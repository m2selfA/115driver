package cmd

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
)

func prepareCLISyncJournalAliasBatchOrphans(t *testing.T) (syncjournalpkg.Store, syncjournalpkg.Store, []syncjournalpkg.ReviewAlias) {
	t.Helper()
	owner := testSyncJournalStore(t).sharedCurrentStore()
	aliases := make([]syncjournalpkg.ReviewAlias, 0, 2)
	for _, pair := range [][2]string{
		{"sha256:" + strings.Repeat("4", 64), strings.Repeat("6", 64)},
		{"sha256:" + strings.Repeat("5", 64), strings.Repeat("7", 64)},
	} {
		alias, err := owner.WriteReviewAlias(pair[0], pair[1])
		if err != nil {
			t.Fatal(err)
		}
		aliases = append(aliases, alias)
	}
	offline := owner
	offline.AccountID = 0
	return owner, offline, aliases
}

func TestPlanCLISyncJournalAliasRepairBindsCompleteOrphanSet(t *testing.T) {
	owner, offline, aliases := prepareCLISyncJournalAliasBatchOrphans(t)
	plan, selected, err := planCLISyncJournalAliasRepair(offline, 1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Scanned != 2 || plan.Eligible != 2 || plan.Selected != 1 || len(plan.Entries) != 1 || len(selected) != 1 || plan.RepairSetID == "" {
		t.Fatalf("CLI alias repair plan=%#v selected=%#v", plan, selected)
	}
	wantSetID, err := syncjournalpkg.ReviewAliasRepairSetID(1, aliases)
	if err != nil || plan.RepairSetID != wantSetID {
		t.Fatalf("CLI repair set token=%q want=%q err=%v", plan.RepairSetID, wantSetID, err)
	}

	// Rewrite only the unselected orphan with the same raw target. The shared
	// set token must still change because it binds every current orphan.
	unselected := aliases[1]
	repairStore := owner
	repairStore.AccountID = unselected.AccountID
	if removed, err := repairStore.RemoveOrphanReviewAliasExact(unselected); err != nil || !removed {
		t.Fatalf("rewrite unselected orphan remove=%v err=%v", removed, err)
	}
	if _, err := owner.WriteReviewAlias(unselected.ReviewID, unselected.PlanID); err != nil {
		t.Fatal(err)
	}
	changed, _, err := planCLISyncJournalAliasRepair(offline, 1)
	if err != nil {
		t.Fatal(err)
	}
	if changed.RepairSetID == plan.RepairSetID {
		t.Fatalf("unselected orphan rewrite did not change CLI repair_set_id: %q", plan.RepairSetID)
	}
}

func TestReconcileCLISyncJournalAliasBatchRejectsStaleSetBeforeMutation(t *testing.T) {
	owner, offline, aliases := prepareCLISyncJournalAliasBatchOrphans(t)
	plan, _, err := planCLISyncJournalAliasRepair(offline, 1)
	if err != nil {
		t.Fatal(err)
	}
	unselected := aliases[1]
	repairStore := owner
	repairStore.AccountID = unselected.AccountID
	if removed, err := repairStore.RemoveOrphanReviewAliasExact(unselected); err != nil || !removed {
		t.Fatalf("rewrite unselected orphan remove=%v err=%v", removed, err)
	}
	if _, err := owner.WriteReviewAlias(unselected.ReviewID, unselected.PlanID); err != nil {
		t.Fatal(err)
	}

	result, err := reconcileCLISyncJournalAliasBatch(offline, 1, plan.RepairSetID)
	if !errors.Is(err, errSyncJournalAliasRepairChanged) || result.RepairSetID != plan.RepairSetID || result.Removed != 0 || result.Requested != 0 || result.Unchanged != 0 || result.Unknown != 0 || len(result.Items) != 0 || result.Partial || result.RecoveryRequired {
		t.Fatalf("stale CLI batch result=%#v err=%v", result, err)
	}
	if resolved, err := owner.ResolveReviewAlias(aliases[0].ReviewID); err != nil || resolved != aliases[0].PlanID {
		t.Fatalf("stale CLI batch removed selected orphan: resolved=%q err=%v", resolved, err)
	}
}

func TestReconcileCLISyncJournalAliasBatchChangedSetHidesReplacementCandidates(t *testing.T) {
	owner, offline, _ := prepareCLISyncJournalAliasBatchOrphans(t)
	preview, selected, err := planCLISyncJournalAliasRepair(offline, 1)
	if err != nil || len(selected) != 1 {
		t.Fatalf("initial CLI batch preview=%#v selected=%#v err=%v", preview, selected, err)
	}
	repairStore := owner
	repairStore.AccountID = selected[0].AccountID
	if removed, err := repairStore.RemoveOrphanReviewAliasExact(selected[0]); err != nil || !removed {
		t.Fatalf("remove original CLI selected orphan=%v err=%v", removed, err)
	}
	newReviewID := "sha256:" + strings.Repeat("0", 64)
	if _, err := owner.WriteReviewAlias(newReviewID, strings.Repeat("9", 64)); err != nil {
		t.Fatal(err)
	}
	fresh, _, err := planCLISyncJournalAliasRepair(offline, 1)
	if err != nil || len(fresh.Entries) != 1 || fresh.Entries[0].ReviewID != newReviewID || fresh.RepairSetID == preview.RepairSetID {
		t.Fatalf("fresh CLI replacement preview=%#v err=%v", fresh, err)
	}

	result, err := reconcileCLISyncJournalAliasBatch(offline, 1, preview.RepairSetID)
	if !errors.Is(err, errSyncJournalAliasRepairChanged) || result.RepairSetID != preview.RepairSetID || result.Requested != 0 || result.Removed != 0 || result.Unchanged != 0 || result.Unknown != 0 || len(result.Items) != 0 || result.Partial || result.RecoveryRequired {
		t.Fatalf("changed CLI set result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	payload := string(encoded)
	if strings.Contains(payload, newReviewID) || strings.Contains(payload, fresh.RepairSetID) || strings.Contains(payload, fresh.Entries[0].RepairID) {
		t.Fatalf("changed CLI set leaked replacement state: %s", payload)
	}
	if _, err := owner.ResolveReviewAlias(newReviewID); err != nil {
		t.Fatalf("changed CLI set mutated replacement candidate: %v", err)
	}
}

func TestReconcileCLISyncJournalAliasBatchCrashStateRequiresFreshPlan(t *testing.T) {
	owner, offline, aliases := prepareCLISyncJournalAliasBatchOrphans(t)
	preview, selected, err := planCLISyncJournalAliasRepair(offline, 2)
	if err != nil || len(selected) != 2 {
		t.Fatalf("initial CLI crash-state preview=%#v selected=%#v err=%v", preview, selected, err)
	}
	crashStore := owner
	crashStore.AccountID = selected[0].AccountID
	if removed, err := crashStore.RemoveOrphanReviewAliasExact(selected[0]); err != nil || !removed {
		t.Fatalf("simulate CLI post-delete crash remove=%v err=%v", removed, err)
	}
	fresh, freshSelected, err := planCLISyncJournalAliasRepair(offline, 2)
	if err != nil || fresh.RepairSetID == preview.RepairSetID || len(freshSelected) != 1 {
		t.Fatalf("fresh CLI crash-state preview=%#v selected=%#v err=%v", fresh, freshSelected, err)
	}

	stale, staleErr := reconcileCLISyncJournalAliasBatch(offline, 2, preview.RepairSetID)
	if !errors.Is(staleErr, errSyncJournalAliasRepairChanged) || stale.RepairSetID != preview.RepairSetID || stale.Requested != 0 || stale.Removed != 0 || stale.Unchanged != 0 || stale.Unknown != 0 || stale.Partial || stale.RecoveryRequired || len(stale.Items) != 0 {
		t.Fatalf("stale CLI post-crash execution=%#v err=%v", stale, staleErr)
	}
	encoded, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), fresh.RepairSetID) || strings.Contains(string(encoded), fresh.Entries[0].RepairID) || strings.Contains(string(encoded), fresh.Entries[0].ReviewID) {
		t.Fatalf("stale CLI post-crash execution leaked fresh review state: %s", encoded)
	}
	exitErr, ok := syncJournalExitErrorData(staleErr, stale).(*exitError)
	if !ok || exitErr.code != output.ExitArgs {
		t.Fatalf("stale CLI post-crash exit contract=%T %#v", exitErr, exitErr)
	}
	exitData, ok := exitErr.data.(syncJournalAliasRepairBatchResult)
	if !ok || exitData.RepairSetID != preview.RepairSetID || exitData.Requested != 0 || len(exitData.Items) != 0 {
		t.Fatalf("stale CLI post-crash structured error data=%#v", exitErr.data)
	}
	if resolved, err := owner.ResolveReviewAlias(aliases[1].ReviewID); err != nil || resolved != aliases[1].PlanID {
		t.Fatalf("stale CLI post-crash execution mutated remaining orphan: resolved=%q err=%v", resolved, err)
	}

	result, err := reconcileCLISyncJournalAliasBatch(offline, 2, fresh.RepairSetID)
	if err != nil || result.Requested != 1 || result.Removed != 1 || result.Unchanged != 0 || result.Unknown != 0 || result.Partial || result.RecoveryRequired || len(result.Items) != 1 || result.Items[0].Status != "removed" {
		t.Fatalf("fresh CLI post-crash execution=%#v err=%v", result, err)
	}
	if _, err := owner.ResolveReviewAlias(aliases[1].ReviewID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("fresh CLI post-crash execution left remaining orphan: %v", err)
	}
}

func TestReconcileCLISyncJournalAliasBatchLockContentionMatchesMCPContract(t *testing.T) {
	owner, offline, aliases := prepareCLISyncJournalAliasBatchOrphans(t)
	preview, _, err := planCLISyncJournalAliasRepair(offline, 2)
	if err != nil {
		t.Fatal(err)
	}
	location, err := owner.Location(aliases[1].PlanID)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	result, err := reconcileCLISyncJournalAliasBatch(offline, 2, preview.RepairSetID)
	if !errors.Is(err, transfer.ErrSessionLocked) || result.RepairSetID != preview.RepairSetID || result.Requested != 2 || result.Removed != 0 || result.Unchanged != 2 || result.Unknown != 0 || result.Partial || result.RecoveryRequired || len(result.Items) != 2 {
		t.Fatalf("contended CLI batch result=%#v err=%v", result, err)
	}
	for _, item := range result.Items {
		if item.Status != "unchanged" {
			t.Fatalf("contended CLI batch item status=%q", item.Status)
		}
	}
	for _, alias := range aliases {
		if resolved, err := owner.ResolveReviewAlias(alias.ReviewID); err != nil || resolved != alias.PlanID {
			t.Fatalf("contended CLI batch mutated alias %s: resolved=%q err=%v", alias.ReviewID, resolved, err)
		}
	}
}

func TestCLIAliasRepairBatchFailureDistinguishesRollbackFromRecovery(t *testing.T) {
	selected := []syncjournalpkg.ReviewAlias{
		{ReviewID: "sha256:" + strings.Repeat("1", 64)},
		{ReviewID: "sha256:" + strings.Repeat("2", 64)},
	}
	expectedID := "sha256:" + strings.Repeat("3", 64)
	rolledBack := cliAliasRepairBatchFailure(expectedID, selected, syncjournalpkg.ReviewAliasBatchRemovalResult{Requested: 2, RolledBack: true})
	if rolledBack.RepairSetID != expectedID || rolledBack.Requested != 2 || rolledBack.Removed != 0 || rolledBack.Unchanged != 2 || rolledBack.Unknown != 0 || rolledBack.Partial || rolledBack.RecoveryRequired || len(rolledBack.Items) != 2 {
		t.Fatalf("rolled-back CLI projection=%#v", rolledBack)
	}
	for _, item := range rolledBack.Items {
		if item.Status != "unchanged" {
			t.Fatalf("rolled-back CLI item status=%q", item.Status)
		}
	}
	recovery := cliAliasRepairBatchFailure(expectedID, selected, syncjournalpkg.ReviewAliasBatchRemovalResult{Requested: 2, Removed: 1, RecoveryRequired: true})
	if recovery.RepairSetID != expectedID || recovery.Requested != 2 || recovery.Removed != 1 || recovery.Unchanged != 0 || recovery.Unknown != 2 || !recovery.Partial || !recovery.RecoveryRequired || len(recovery.Items) != 2 {
		t.Fatalf("recovery-required CLI projection=%#v", recovery)
	}
	for _, item := range recovery.Items {
		if item.Status != "unknown" {
			t.Fatalf("recovery-required CLI item status=%q", item.Status)
		}
	}
}

func TestReconcileCLISyncJournalAliasBatchRemovesOnlySelectedPrefix(t *testing.T) {
	owner, offline, aliases := prepareCLISyncJournalAliasBatchOrphans(t)
	plan, _, err := planCLISyncJournalAliasRepair(offline, 1)
	if err != nil {
		t.Fatal(err)
	}
	result, err := reconcileCLISyncJournalAliasBatch(offline, 1, plan.RepairSetID)
	if err != nil || result.Requested != 1 || result.Removed != 1 || result.Unchanged != 0 || result.Unknown != 0 || result.Partial || result.RecoveryRequired {
		t.Fatalf("CLI batch result=%#v err=%v", result, err)
	}
	if _, err := owner.ResolveReviewAlias(aliases[0].ReviewID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("selected CLI orphan survived: %v", err)
	}
	if resolved, err := owner.ResolveReviewAlias(aliases[1].ReviewID); err != nil || resolved != aliases[1].PlanID {
		t.Fatalf("unselected CLI orphan changed: resolved=%q err=%v", resolved, err)
	}
}

func TestReconcileCLISyncJournalAliasBatchOfflineSupportsMultiplePersistedAccounts(t *testing.T) {
	root := t.TempDir()
	scope := strings.Repeat("e", 64)
	firstStore := syncjournalpkg.Store{Root: root, ProfileScope: scope, AccountID: 51}
	secondStore := syncjournalpkg.Store{Root: root, ProfileScope: scope, AccountID: 52}
	first, err := firstStore.WriteReviewAlias("sha256:"+strings.Repeat("1", 64), strings.Repeat("2", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondStore.WriteReviewAlias("sha256:"+strings.Repeat("3", 64), strings.Repeat("4", 64))
	if err != nil {
		t.Fatal(err)
	}
	offline := syncjournalpkg.Store{Root: root, ProfileScope: scope}
	plan, _, err := planCLISyncJournalAliasRepair(offline, 2)
	if err != nil || plan.Selected != 2 {
		t.Fatalf("offline multi-account plan=%#v err=%v", plan, err)
	}
	result, err := reconcileCLISyncJournalAliasBatch(offline, 2, plan.RepairSetID)
	if err != nil || result.Removed != 2 || result.RecoveryRequired {
		t.Fatalf("offline multi-account batch=%#v err=%v", result, err)
	}
	for _, pair := range []struct {
		store syncjournalpkg.Store
		alias syncjournalpkg.ReviewAlias
	}{{firstStore, first}, {secondStore, second}} {
		if _, err := pair.store.ResolveReviewAlias(pair.alias.ReviewID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
			t.Fatalf("offline multi-account CLI alias %s survived: %v", pair.alias.ReviewID, err)
		}
	}
}

func TestSyncJournalAliasBatchHelpFreezesCrashAndProfileScopeContract(t *testing.T) {
	for _, needle := range []string{"complete current orphan set", "offline profile administration", "persisted account bindings"} {
		if !strings.Contains(syncJournalAliasesPlanCmd.Long, needle) {
			t.Errorf("alias repair plan help lost %q: %s", needle, syncJournalAliasesPlanCmd.Long)
		}
	}
	for _, needle := range []string{"fresh plan", "not power-loss atomic", "monotonically converges", "offline/profile-scoped", "persisted account bindings"} {
		if !strings.Contains(syncJournalAliasesReconcileBatchCmd.Long, needle) {
			t.Errorf("alias repair execute help lost %q: %s", needle, syncJournalAliasesReconcileBatchCmd.Long)
		}
	}
}

func TestSyncJournalAliasBatchRollbackFailureUsesControlPlaneExitCode(t *testing.T) {
	if got := syncJournalExitCode(syncjournalpkg.ErrReviewAliasRepairRollback); got != output.ExitArgs {
		t.Fatalf("alias rollback recovery exit=%d want=%d", got, output.ExitArgs)
	}
}

func TestSyncJournalAliasBatchArgsFailBeforeStoreAccess(t *testing.T) {
	oldPlanLimit := syncJournalAliasRepairPlanLimit
	oldBatchLimit := syncJournalAliasRepairBatchLimit
	oldSetID := syncJournalAliasExpectRepairSetID
	t.Cleanup(func() {
		syncJournalAliasRepairPlanLimit = oldPlanLimit
		syncJournalAliasRepairBatchLimit = oldBatchLimit
		syncJournalAliasExpectRepairSetID = oldSetID
	})

	syncJournalAliasRepairPlanLimit = maxCLISyncJournalAliasRepairLimit + 1
	if err := syncJournalAliasesPlanArgs(syncJournalAliasesPlanCmd, nil); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized plan limit error=%v", err)
	}
	syncJournalAliasRepairBatchLimit = defaultCLISyncJournalAliasRepairLimit
	syncJournalAliasExpectRepairSetID = ""
	if err := syncJournalAliasesReconcileBatchArgs(syncJournalAliasesReconcileBatchCmd, nil); err == nil || !strings.Contains(err.Error(), "expect-repair-set-id") {
		t.Fatalf("missing batch repair token error=%v", err)
	}
	syncJournalAliasExpectRepairSetID = "sha256:not-hex"
	if err := syncJournalAliasesReconcileBatchArgs(syncJournalAliasesReconcileBatchCmd, nil); err == nil || !strings.Contains(err.Error(), "invalid --expect-repair-set-id") {
		t.Fatalf("malformed batch repair token error=%v", err)
	}
	syncJournalAliasExpectRepairSetID = "sha256:" + strings.Repeat("a", 64)
	if err := syncJournalAliasesReconcileBatchArgs(syncJournalAliasesReconcileBatchCmd, nil); err != nil {
		t.Fatalf("valid batch repair args rejected: %v", err)
	}
}
