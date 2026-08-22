package syncjournal

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

func TestRemoveOrphanReviewAliasRefusesSoftDeletedJournalCrashWindow(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("1", 64), AccountID: 42}
	plan := currentTestPlan()
	handle, err := store.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	reviewID := "sha256:" + strings.Repeat("2", 64)
	if _, err := handle.BindReviewAlias(reviewID); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	location, err := store.Location(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	// Model a process crash immediately after the current directory was moved,
	// before review-alias sidecar creation and live-alias removal.
	trashPath, err := MoveDirectoryToSessionTrash(store.Root, location.Dir, plan.PlanID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := ReadTrashReviewAliases(trashPath); err != nil || found {
		t.Fatalf("crash-window trash unexpectedly has alias sidecar: found=%v err=%v", found, err)
	}

	removed, err := store.RemoveOrphanReviewAlias(reviewID, plan.PlanID)
	if removed || !errors.Is(err, ErrReviewAliasTrashed) {
		t.Fatalf("soft-deleted alias repair removed=%v err=%v", removed, err)
	}
	resolved, err := store.ResolveReviewAlias(reviewID)
	if err != nil || resolved != plan.PlanID {
		t.Fatalf("soft-deleted alias was mutated: resolved=%q err=%v", resolved, err)
	}
	if present, err := store.HasTrashedCurrentPlan(plan.PlanID, 16); err != nil || !present {
		t.Fatalf("soft-deleted journal presence=%v err=%v", present, err)
	}
}

func TestReviewAliasRoundTripBindingAndConflict(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	reviewID := "sha256:" + strings.Repeat("b", 64)
	planID := strings.Repeat("c", 64)
	alias, err := store.WriteReviewAlias(reviewID, planID)
	if err != nil {
		t.Fatal(err)
	}
	if alias.ReviewID != reviewID || alias.PlanID != planID || alias.AccountID != 42 || alias.ProfileScope != store.ProfileScope {
		t.Fatalf("review alias=%#v", alias)
	}
	resolved, err := store.ResolveReviewAlias(reviewID)
	if err != nil || resolved != planID {
		t.Fatalf("resolve alias=%q err=%v", resolved, err)
	}
	again, err := store.WriteReviewAlias(strings.ToUpper(reviewID), strings.ToUpper(planID))
	if err != nil || again.PlanID != planID {
		t.Fatalf("idempotent alias=%#v err=%v", again, err)
	}
	if _, err := store.WriteReviewAlias(reviewID, strings.Repeat("d", 64)); !errors.Is(err, ErrReviewAliasConflict) {
		t.Fatalf("alias conflict error=%v", err)
	}
	path, err := store.reviewAliasPath(reviewID)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("review alias permissions=%#o", info.Mode().Perm())
	}

	wrongAccount := store
	wrongAccount.AccountID = 43
	if _, err := wrongAccount.ResolveReviewAlias(reviewID); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("wrong-account alias error=%v", err)
	}
	wrongScope := store
	wrongScope.ProfileScope = strings.Repeat("e", 64)
	if _, err := wrongScope.ResolveReviewAlias(reviewID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong-scope alias should be isolated by path, got %v", err)
	}
}

func TestHandleBindReviewAliasUsesLockedCurrentJournal(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	plan := currentTestPlan()
	handle, err := store.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	reviewID := "sha256:" + strings.Repeat("f", 64)
	alias, err := handle.BindReviewAlias(reviewID)
	if err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if alias.ReviewID != reviewID || alias.PlanID != plan.PlanID {
		handle.Close()
		t.Fatalf("bound review alias=%#v", alias)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := handle.BindReviewAlias(reviewID); err == nil {
		t.Fatal("closed sync journal handle accepted review alias binding")
	}
	resolved, err := store.ResolveReviewAlias(reviewID)
	if err != nil || resolved != plan.PlanID {
		t.Fatalf("resolved bound review alias=%q err=%v", resolved, err)
	}
}

func TestReviewAliasRejectsMalformedIDsAndUnknownAccount(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	if _, err := store.WriteReviewAlias("bad", strings.Repeat("c", 64)); err == nil {
		t.Fatal("malformed review ID was accepted")
	}
	if _, err := store.WriteReviewAlias("sha256:"+strings.Repeat("b", 64), "bad"); err == nil {
		t.Fatal("malformed internal plan ID was accepted")
	}
	store.AccountID = 0
	if _, err := store.WriteReviewAlias("sha256:"+strings.Repeat("b", 64), strings.Repeat("c", 64)); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("unknown-account alias error=%v", err)
	}
}

func TestRemoveOrphanReviewAliasRequiresLockedProofOfAbsence(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("f", 64), AccountID: 42}
	reviewID := "sha256:" + strings.Repeat("1", 64)
	missingPlanID := strings.Repeat("2", 64)
	if _, err := store.WriteReviewAlias(reviewID, missingPlanID); err != nil {
		t.Fatal(err)
	}
	location, err := store.Location(missingPlanID)
	if err != nil {
		t.Fatal(err)
	}
	removed, err := store.RemoveOrphanReviewAlias(reviewID, missingPlanID)
	if err != nil || !removed {
		t.Fatalf("remove orphan alias removed=%v err=%v", removed, err)
	}
	if _, err := store.ResolveReviewAlias(reviewID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan alias survived: %v", err)
	}
	if _, err := os.Stat(location.Dir); !os.IsNotExist(err) {
		t.Fatalf("orphan proof created a phantom journal directory: %v", err)
	}
}

func TestRemoveOrphanReviewAliasExactRejectsChangedSnapshot(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("7", 64), AccountID: 42}
	reviewID := "sha256:" + strings.Repeat("6", 64)
	planID := strings.Repeat("8", 64)
	alias, err := store.WriteReviewAlias(reviewID, planID)
	if err != nil {
		t.Fatal(err)
	}
	stale := alias
	stale.UpdatedAt = stale.UpdatedAt.Add(-time.Second)
	removed, err := store.RemoveOrphanReviewAliasExact(stale)
	if removed || !errors.Is(err, ErrReviewAliasChanged) {
		t.Fatalf("changed exact orphan repair removed=%v err=%v", removed, err)
	}
	if resolved, err := store.ResolveReviewAlias(reviewID); err != nil || resolved != planID {
		t.Fatalf("changed exact orphan repair mutated alias: resolved=%q err=%v", resolved, err)
	}
	removed, err = store.RemoveOrphanReviewAliasExact(alias)
	if err != nil || !removed {
		t.Fatalf("matching exact orphan repair removed=%v err=%v", removed, err)
	}
}

func TestRemoveOrphanReviewAliasPreservesExistingOrLockedJournal(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("9", 64), AccountID: 42}
	plan := currentTestPlan()
	handle, err := store.CreateCurrent(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	reviewID := "sha256:" + strings.Repeat("3", 64)
	if _, err := store.WriteReviewAlias(reviewID, plan.PlanID); err != nil {
		t.Fatal(err)
	}
	removed, err := store.RemoveOrphanReviewAlias(reviewID, plan.PlanID)
	if err != nil || removed {
		t.Fatalf("existing journal alias removed=%v err=%v", removed, err)
	}
	if resolved, err := store.ResolveReviewAlias(reviewID); err != nil || resolved != plan.PlanID {
		t.Fatalf("existing journal alias changed: resolved=%q err=%v", resolved, err)
	}

	missingPlanID := strings.Repeat("4", 64)
	lockedReviewID := "sha256:" + strings.Repeat("5", 64)
	if _, err := store.WriteReviewAlias(lockedReviewID, missingPlanID); err != nil {
		t.Fatal(err)
	}
	location, err := store.Location(missingPlanID)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := store.RemoveOrphanReviewAlias(lockedReviewID, missingPlanID); !errors.Is(err, transfer.ErrSessionLocked) {
		t.Fatalf("locked orphan alias error=%v", err)
	}
	if resolved, err := store.ResolveReviewAlias(lockedReviewID); err != nil || resolved != missingPlanID {
		t.Fatalf("locked orphan alias changed: resolved=%q err=%v", resolved, err)
	}
}
