package cmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/spf13/cobra"
)

func findCLISyncJournalAliasDiagnosis(t *testing.T, report syncJournalAliasDiagnosisReport, reviewID string) syncJournalAliasDiagnosisEntry {
	t.Helper()
	for _, entry := range report.Entries {
		if entry.ReviewID == reviewID {
			return entry
		}
	}
	t.Fatalf("alias diagnosis omitted %s: %#v", reviewID, report)
	return syncJournalAliasDiagnosisEntry{}
}

func prepareCLISyncJournalOrphan(t *testing.T) (syncjournalpkg.Store, syncjournalpkg.Store, syncjournalpkg.ReviewAlias) {
	t.Helper()
	owner := testSyncJournalStore(t).sharedCurrentStore()
	reviewID := "sha256:" + strings.Repeat("7", 64)
	rawPlanID := strings.Repeat("8", 64)
	alias, err := owner.WriteReviewAlias(reviewID, rawPlanID)
	if err != nil {
		t.Fatal(err)
	}
	offline := owner
	offline.AccountID = 0
	return owner, offline, alias
}

func TestSyncJournalAliasCommandsAreOffline(t *testing.T) {
	for _, command := range []*cobra.Command{syncJournalAliasesCmd, syncJournalAliasesDiagnoseCmd, syncJournalAliasesPlanCmd, syncJournalAliasesReconcileCmd, syncJournalAliasesReconcileBatchCmd} {
		if !commandSkipsAuthentication(command) {
			t.Fatalf("%s unexpectedly requires authentication", command.CommandPath())
		}
	}
}

func TestSyncJournalAliasReconcileArgsFailBeforeStoreAccess(t *testing.T) {
	old := syncJournalAliasExpectRepairID
	t.Cleanup(func() { syncJournalAliasExpectRepairID = old })
	validReview := "sha256:" + strings.Repeat("a", 64)

	syncJournalAliasExpectRepairID = ""
	if err := syncJournalAliasesReconcileArgs(syncJournalAliasesReconcileCmd, []string{validReview}); err == nil || !strings.Contains(err.Error(), "expect-repair-id") {
		t.Fatalf("missing repair ID args error=%v", err)
	}
	syncJournalAliasExpectRepairID = "sha256:not-hex"
	if err := syncJournalAliasesReconcileArgs(syncJournalAliasesReconcileCmd, []string{validReview}); err == nil || !strings.Contains(err.Error(), "invalid --expect-repair-id") {
		t.Fatalf("malformed repair ID args error=%v", err)
	}
	syncJournalAliasExpectRepairID = "sha256:" + strings.Repeat("b", 64)
	if err := syncJournalAliasesReconcileArgs(syncJournalAliasesReconcileCmd, []string{"bad-review"}); err == nil || !strings.Contains(err.Error(), "invalid review_id") {
		t.Fatalf("malformed review ID args error=%v", err)
	}
	if err := syncJournalAliasesReconcileArgs(syncJournalAliasesReconcileCmd, []string{validReview}); err != nil {
		t.Fatalf("valid alias reconcile args rejected: %v", err)
	}
}

func TestDiagnoseCLISyncJournalAliasesOfflineUsesSharedRepairToken(t *testing.T) {
	owner, offline, alias := prepareCLISyncJournalOrphan(t)
	report, err := diagnoseCLISyncJournalAliases(offline)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanned != 1 || report.Orphan != 1 || report.Issues != 1 || len(report.Entries) != 1 {
		t.Fatalf("offline alias diagnosis=%#v", report)
	}
	entry := findCLISyncJournalAliasDiagnosis(t, report, alias.ReviewID)
	wantRepairID, err := syncjournalpkg.ReviewAliasRepairID(alias)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Status != string(syncjournalpkg.ReviewAliasDiagnosisOrphan) || entry.RepairID != wantRepairID || entry.Error != "" {
		t.Fatalf("offline orphan projection=%#v want repair_id=%q", entry, wantRepairID)
	}
	if resolved, err := owner.ResolveReviewAlias(alias.ReviewID); err != nil || resolved != alias.PlanID {
		t.Fatalf("offline diagnosis mutated alias: resolved=%q err=%v", resolved, err)
	}
}

func TestReconcileCLISyncJournalAliasOfflineRemovesOnlyReviewedOrphan(t *testing.T) {
	owner, offline, alias := prepareCLISyncJournalOrphan(t)
	repairID, err := syncjournalpkg.ReviewAliasRepairID(alias)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reconcileCLISyncJournalAlias(offline, alias.ReviewID, repairID)
	if err != nil || !result.Repaired || result.Status != "removed" || result.ReviewID != alias.ReviewID || result.RepairID != repairID {
		t.Fatalf("offline orphan reconcile result=%#v err=%v", result, err)
	}
	if _, err := owner.ResolveReviewAlias(alias.ReviewID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("offline orphan alias survived: %v", err)
	}
}

func TestReconcileCLISyncJournalAliasRejectsStaleTokenWithoutMutation(t *testing.T) {
	owner, offline, alias := prepareCLISyncJournalOrphan(t)
	wrong := "sha256:" + strings.Repeat("9", 64)
	result, err := reconcileCLISyncJournalAlias(offline, alias.ReviewID, wrong)
	if result.Repaired || !errors.Is(err, errSyncJournalAliasRepairChanged) {
		t.Fatalf("stale-token reconcile result=%#v err=%v", result, err)
	}
	if resolved, err := owner.ResolveReviewAlias(alias.ReviewID); err != nil || resolved != alias.PlanID {
		t.Fatalf("stale token mutated alias: resolved=%q err=%v", resolved, err)
	}
}

func TestReconcileCLISyncJournalAliasRefusesSoftDeletedShadow(t *testing.T) {
	store := testSyncJournalStore(t)
	owner := store.sharedCurrentStore()
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	reviewID := "sha256:" + strings.Repeat("4", 64)
	alias, err := owner.WriteReviewAlias(reviewID, plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	location, err := owner.Location(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := syncjournalpkg.MoveDirectoryToSessionTrash(owner.Root, location.Dir, plan.PlanID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	repairID, err := syncjournalpkg.ReviewAliasRepairID(alias)
	if err != nil {
		t.Fatal(err)
	}
	offline := owner
	offline.AccountID = 0
	result, err := reconcileCLISyncJournalAlias(offline, reviewID, repairID)
	if result.Repaired || !errors.Is(err, syncjournalpkg.ErrReviewAliasTrashed) {
		t.Fatalf("soft-deleted reconcile result=%#v err=%v", result, err)
	}
	if resolved, err := owner.ResolveReviewAlias(reviewID); err != nil || resolved != plan.PlanID {
		t.Fatalf("soft-deleted reconcile mutated alias: resolved=%q err=%v", resolved, err)
	}
}

func TestReconcileCLISyncJournalAliasHonorsRawPlanLock(t *testing.T) {
	owner, offline, alias := prepareCLISyncJournalOrphan(t)
	repairID, err := syncjournalpkg.ReviewAliasRepairID(alias)
	if err != nil {
		t.Fatal(err)
	}
	location, err := owner.Location(alias.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	result, err := reconcileCLISyncJournalAlias(offline, alias.ReviewID, repairID)
	if result.Repaired || !errors.Is(err, transfer.ErrSessionLocked) {
		t.Fatalf("locked orphan reconcile result=%#v err=%v", result, err)
	}
	if resolved, err := owner.ResolveReviewAlias(alias.ReviewID); err != nil || resolved != alias.PlanID {
		t.Fatalf("locked reconcile mutated alias: resolved=%q err=%v", resolved, err)
	}
}
