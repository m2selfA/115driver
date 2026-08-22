package syncjournal

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/SheltonZhu/115driver/internal/transfer"
)

func prepareExactAliasRepairBatch(t *testing.T) (Store, []ReviewAlias, []syncplanpkg.Plan) {
	t.Helper()
	store := Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	plans := make([]syncplanpkg.Plan, 2)
	aliases := make([]ReviewAlias, 2)
	for index := range plans {
		plan := currentTestPlan()
		plan.RemoteRootID = strings.Repeat(string(rune('a'+index)), index+1)
		plan.PlanID = ""
		plan.PlanID = syncplanpkg.Fingerprint(plan)
		plans[index] = plan
		reviewID := "sha256:" + strings.Repeat(string(rune('1'+index)), 64)
		alias, err := store.WriteReviewAlias(reviewID, plan.PlanID)
		if err != nil {
			t.Fatal(err)
		}
		aliases[index] = alias
	}
	return store, aliases, plans
}

func assertExactAliasBatchPresent(t *testing.T, store Store, aliases []ReviewAlias) {
	t.Helper()
	for _, alias := range aliases {
		resolved, err := store.ResolveReviewAlias(alias.ReviewID)
		if err != nil || resolved != alias.PlanID {
			t.Fatalf("alias %s missing or changed: resolved=%q err=%v", alias.ReviewID, resolved, err)
		}
	}
}

func TestRemoveOrphanReviewAliasesExactRemovesWholePreflightedBatch(t *testing.T) {
	store, aliases, _ := prepareExactAliasRepairBatch(t)
	result, err := store.RemoveOrphanReviewAliasesExact([]ReviewAlias{aliases[1], aliases[0]})
	if err != nil || result.Requested != 2 || result.Removed != 2 || result.RolledBack || result.RecoveryRequired {
		t.Fatalf("exact batch removal result=%#v err=%v", result, err)
	}
	for _, alias := range aliases {
		if _, err := store.ResolveReviewAlias(alias.ReviewID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("removed alias %s still exists: %v", alias.ReviewID, err)
		}
	}
}

func TestRemoveOrphanReviewAliasesExactSnapshotMismatchRemovesNothing(t *testing.T) {
	store, aliases, _ := prepareExactAliasRepairBatch(t)
	changed := append([]ReviewAlias(nil), aliases...)
	changed[1].UpdatedAt = changed[1].UpdatedAt.Add(time.Nanosecond)
	result, err := store.RemoveOrphanReviewAliasesExact(changed)
	if !errors.Is(err, ErrReviewAliasChanged) || result.Removed != 0 || result.RecoveryRequired {
		t.Fatalf("stale exact batch result=%#v err=%v", result, err)
	}
	assertExactAliasBatchPresent(t, store, aliases)
}

func TestRemoveOrphanReviewAliasesExactLockContentionRemovesNothing(t *testing.T) {
	store, aliases, _ := prepareExactAliasRepairBatch(t)
	planID := aliases[0].PlanID
	if aliases[1].PlanID > planID {
		planID = aliases[1].PlanID
	}
	location, err := store.Location(planID)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	result, err := store.RemoveOrphanReviewAliasesExact(aliases)
	if !errors.Is(err, transfer.ErrSessionLocked) || result.Removed != 0 || result.RecoveryRequired {
		t.Fatalf("locked exact batch result=%#v err=%v", result, err)
	}
	assertExactAliasBatchPresent(t, store, aliases)
}

func TestRemoveOrphanReviewAliasesExactSoftDeletedCandidateRemovesNothing(t *testing.T) {
	store, aliases, plans := prepareExactAliasRepairBatch(t)
	handle, err := store.CreateCurrent(plans[1])
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	location, err := store.Location(plans[1].PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MoveDirectoryToSessionTrash(store.Root, location.Dir, plans[1].PlanID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	result, err := store.RemoveOrphanReviewAliasesExact(aliases)
	if !errors.Is(err, ErrReviewAliasTrashed) || result.Removed != 0 || result.RecoveryRequired {
		t.Fatalf("soft-deleted exact batch result=%#v err=%v", result, err)
	}
	assertExactAliasBatchPresent(t, store, aliases)
}

func TestRemoveOrphanReviewAliasesExactRollsBackEarlierRemoval(t *testing.T) {
	store, aliases, _ := prepareExactAliasRepairBatch(t)
	removeCalls := 0
	result, err := store.removeOrphanReviewAliasesExactWith(aliases, func(path string) error {
		removeCalls++
		if removeCalls == 2 {
			return errors.New("injected alias remove failure")
		}
		return os.Remove(path)
	}, store.writeReviewAliasRecordLocked)
	if err == nil || errors.Is(err, ErrReviewAliasRepairRollback) || result.Removed != 0 || !result.RolledBack || result.RecoveryRequired {
		t.Fatalf("rollback exact batch result=%#v err=%v", result, err)
	}
	assertExactAliasBatchPresent(t, store, aliases)
}

func TestRemoveOrphanReviewAliasesExactOfflineProfileSupportsPersistedMultiAccountAliases(t *testing.T) {
	root := t.TempDir()
	scope := strings.Repeat("b", 64)
	firstStore := Store{Root: root, ProfileScope: scope, AccountID: 41}
	secondStore := Store{Root: root, ProfileScope: scope, AccountID: 42}
	first, err := firstStore.WriteReviewAlias("sha256:"+strings.Repeat("6", 64), strings.Repeat("8", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, err := secondStore.WriteReviewAlias("sha256:"+strings.Repeat("7", 64), strings.Repeat("9", 64))
	if err != nil {
		t.Fatal(err)
	}
	offline := Store{Root: root, ProfileScope: scope}
	result, err := offline.RemoveOrphanReviewAliasesExact([]ReviewAlias{second, first})
	if err != nil || result.Requested != 2 || result.Removed != 2 || result.RolledBack || result.RecoveryRequired {
		t.Fatalf("offline multi-account exact batch result=%#v err=%v", result, err)
	}
	for _, pair := range []struct {
		store Store
		alias ReviewAlias
	}{{firstStore, first}, {secondStore, second}} {
		if _, err := pair.store.ResolveReviewAlias(pair.alias.ReviewID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("offline multi-account alias %s survived: %v", pair.alias.ReviewID, err)
		}
	}
}

func TestRemoveOrphanReviewAliasesExactAuthenticatedStoreRejectsForeignAccountAlias(t *testing.T) {
	root := t.TempDir()
	scope := strings.Repeat("c", 64)
	owner := Store{Root: root, ProfileScope: scope, AccountID: 41}
	foreign := Store{Root: root, ProfileScope: scope, AccountID: 42}
	first, err := owner.WriteReviewAlias("sha256:"+strings.Repeat("a", 64), strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	second, err := foreign.WriteReviewAlias("sha256:"+strings.Repeat("c", 64), strings.Repeat("d", 64))
	if err != nil {
		t.Fatal(err)
	}
	result, err := owner.RemoveOrphanReviewAliasesExact([]ReviewAlias{first, second})
	if !errors.Is(err, ErrBindingMismatch) || result.Removed != 0 {
		t.Fatalf("authenticated mixed-account batch result=%#v err=%v", result, err)
	}
	if _, err := owner.ResolveReviewAlias(first.ReviewID); err != nil {
		t.Fatalf("strict batch changed owner alias: %v", err)
	}
	if _, err := foreign.ResolveReviewAlias(second.ReviewID); err != nil {
		t.Fatalf("strict batch changed foreign alias: %v", err)
	}
}

func TestRemoveOrphanReviewAliasesExactReportsRollbackFailure(t *testing.T) {
	store, aliases, _ := prepareExactAliasRepairBatch(t)
	removeCalls := 0
	result, err := store.removeOrphanReviewAliasesExactWith(aliases, func(path string) error {
		removeCalls++
		if removeCalls == 2 {
			return errors.New("injected alias remove failure")
		}
		return os.Remove(path)
	}, func(alias ReviewAlias) error {
		return errors.New("injected alias rollback failure")
	})
	if !errors.Is(err, ErrReviewAliasRepairRollback) || !result.RecoveryRequired || result.Removed != 1 {
		t.Fatalf("rollback-failed exact batch result=%#v err=%v", result, err)
	}
	missing := 0
	for _, alias := range aliases {
		if _, resolveErr := store.ResolveReviewAlias(alias.ReviewID); errors.Is(resolveErr, ErrNotFound) {
			missing++
		}
	}
	if missing != result.Removed {
		t.Fatalf("rollback-failed missing=%d result.Removed=%d", missing, result.Removed)
	}
}
