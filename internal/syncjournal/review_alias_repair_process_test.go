package syncjournal

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/SheltonZhu/115driver/internal/transfer"
)

const (
	exactAliasRepairChildEnv   = "115DRIVER_TEST_EXACT_ALIAS_REPAIR_CHILD"
	exactAliasRepairFixtureEnv = "115DRIVER_TEST_EXACT_ALIAS_REPAIR_FIXTURE"
)

type exactAliasRepairProcessFixture struct {
	Action                    string        `json:"action,omitempty"`
	Root                      string        `json:"root"`
	ProfileScope              string        `json:"profile_scope"`
	AccountID                 int64         `json:"account_id"`
	PlanID                    string        `json:"plan_id,omitempty"`
	Aliases                   []ReviewAlias `json:"aliases"`
	SignalPath                string        `json:"signal_path"`
	SignalBeforeFirst         bool          `json:"signal_before_first"`
	SignalAfterRemovals       int           `json:"signal_after_removals"`
	FailRemoveAt              int           `json:"fail_remove_at,omitempty"`
	SignalDuringRollbackAfter int           `json:"signal_during_rollback_after,omitempty"`
}

type exactAliasRepairChild struct {
	cmd        *exec.Cmd
	output     *bytes.Buffer
	signalPath string
	stopped    bool
}

func writeExactAliasRepairSignal(path string) error {
	return os.WriteFile(path, []byte("ready\n"), 0o600)
}

func blockExactAliasRepairChild() {
	for {
		time.Sleep(time.Hour)
	}
}

// TestExactAliasRepairProcessHelper is invoked only by the parent tests below.
// It intentionally uses the real test binary and real OS file locks so the
// maintenance matrix exercises cross-process behavior rather than goroutine-only
// scheduling within one process.
func TestExactAliasRepairProcessHelper(t *testing.T) {
	if os.Getenv(exactAliasRepairChildEnv) != "1" {
		return
	}
	fixturePath := os.Getenv(exactAliasRepairFixtureEnv)
	encoded, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture exactAliasRepairProcessFixture
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatal(err)
	}
	store := Store{Root: fixture.Root, ProfileScope: fixture.ProfileScope, AccountID: fixture.AccountID}
	if fixture.Action == "hold-exact-raw-locks" {
		runExactAliasRepairRawLockHelper(t, store, fixture)
		return
	}
	if fixture.Action == "trash-rename-before-sidecar" {
		runExactAliasRepairTrashRenameCrashHelper(t, store, fixture)
		return
	}
	removeCalls := 0
	rollbackCalls := 0
	_, err = store.removeOrphanReviewAliasesExactWith(fixture.Aliases, func(path string) error {
		removeCalls++
		if fixture.SignalBeforeFirst && removeCalls == 1 {
			if err := writeExactAliasRepairSignal(fixture.SignalPath); err != nil {
				return err
			}
			blockExactAliasRepairChild()
		}
		if fixture.FailRemoveAt > 0 && removeCalls == fixture.FailRemoveAt {
			return errors.New("injected exact alias repair child remove failure")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if fixture.SignalAfterRemovals > 0 && removeCalls == fixture.SignalAfterRemovals {
			if err := writeExactAliasRepairSignal(fixture.SignalPath); err != nil {
				return err
			}
			blockExactAliasRepairChild()
		}
		return nil
	}, func(alias ReviewAlias) error {
		if err := store.writeExactRepairAliasRecordLocked(alias); err != nil {
			return err
		}
		rollbackCalls++
		if fixture.SignalDuringRollbackAfter > 0 && rollbackCalls == fixture.SignalDuringRollbackAfter {
			if err := writeExactAliasRepairSignal(fixture.SignalPath); err != nil {
				return err
			}
			blockExactAliasRepairChild()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func runExactAliasRepairRawLockHelper(t *testing.T, store Store, fixture exactAliasRepairProcessFixture) {
	t.Helper()
	locks, err := store.lockExactRepairRawPlans(fixture.Aliases)
	if err != nil {
		t.Fatal(err)
	}
	defer locks.Close()
	if err := writeExactAliasRepairSignal(fixture.SignalPath); err != nil {
		t.Fatal(err)
	}
	blockExactAliasRepairChild()
}

func runExactAliasRepairTrashRenameCrashHelper(t *testing.T, store Store, fixture exactAliasRepairProcessFixture) {
	t.Helper()
	location, err := store.Location(fixture.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	journalLock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer journalLock.Close()
	journal, err := store.ReadCurrent(location)
	if err != nil {
		t.Fatal(err)
	}
	aliasStore, err := store.aliasLifecycleStoreForJournal(journal, false)
	if err != nil {
		t.Fatal(err)
	}
	reviewIDs := make([]string, 0, len(fixture.Aliases))
	for _, alias := range fixture.Aliases {
		reviewIDs = append(reviewIDs, alias.ReviewID)
	}
	aliasSet, err := aliasStore.lockReviewAliasSet(reviewIDs, fixture.PlanID, false)
	if err != nil {
		t.Fatal(err)
	}
	defer aliasSet.Close()
	if err := journalLock.StopLease(); err != nil {
		t.Fatal(err)
	}
	if _, err := MoveDirectoryToSessionTrash(store.Root, location.Dir, fixture.PlanID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := writeExactAliasRepairSignal(fixture.SignalPath); err != nil {
		t.Fatal(err)
	}
	blockExactAliasRepairChild()
}

func startExactAliasRepairChild(t *testing.T, fixture exactAliasRepairProcessFixture) *exactAliasRepairChild {
	t.Helper()
	dir := t.TempDir()
	fixture.SignalPath = filepath.Join(dir, "phase.ready")
	fixturePath := filepath.Join(dir, "fixture.json")
	encoded, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixturePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	output := &bytes.Buffer{}
	cmd := exec.Command(executable, "-test.run=^TestExactAliasRepairProcessHelper$")
	cmd.Env = append(os.Environ(),
		exactAliasRepairChildEnv+"=1",
		exactAliasRepairFixtureEnv+"="+fixturePath,
	)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &exactAliasRepairChild{cmd: cmd, output: output, signalPath: fixture.SignalPath}
	t.Cleanup(func() { child.stop(t) })
	return child
}

func (child *exactAliasRepairChild) waitForSignal(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(child.signalPath); err == nil {
			return
		} else if !os.IsNotExist(err) {
			child.stop(t)
			t.Fatalf("inspect exact alias repair child signal: %v", err)
		}
		if time.Now().After(deadline) {
			child.stop(t)
			t.Fatalf("exact alias repair child did not reach requested phase; output=%s", child.output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (child *exactAliasRepairChild) stop(t *testing.T) {
	t.Helper()
	if child == nil || child.stopped {
		return
	}
	child.stopped = true
	if child.cmd.Process != nil {
		_ = child.cmd.Process.Kill()
	}
	if err := child.cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Errorf("wait for exact alias repair child: %v; output=%s", err, child.output.String())
		}
	}
}

func prepareExactAliasRepairProcessBatch(t *testing.T, count int) (Store, []ReviewAlias) {
	t.Helper()
	store := Store{Root: t.TempDir(), ProfileScope: fmt.Sprintf("%064x", 0xa11a5), AccountID: 42}
	aliases := make([]ReviewAlias, 0, count)
	for index := 0; index < count; index++ {
		plan := currentTestPlan()
		plan.RemoteRootID = fmt.Sprintf("process-batch-%02d", index)
		plan.PlanID = ""
		plan.PlanID = syncplanpkg.Fingerprint(plan)
		alias, err := store.WriteReviewAlias(fmt.Sprintf("sha256:%064x", index+1), plan.PlanID)
		if err != nil {
			t.Fatal(err)
		}
		aliases = append(aliases, alias)
	}
	return store, aliases
}

func assertExactAliasMissing(t *testing.T, store Store, alias ReviewAlias) {
	t.Helper()
	if _, err := store.ResolveReviewAlias(alias.ReviewID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("alias %s unexpectedly exists: %v", alias.ReviewID, err)
	}
}

func TestRemoveOrphanReviewAliasesExactCrossProcessContentionMatrix(t *testing.T) {
	store, aliases := prepareExactAliasRepairProcessBatch(t, 4)
	child := startExactAliasRepairChild(t, exactAliasRepairProcessFixture{
		Root: store.Root, ProfileScope: store.ProfileScope, AccountID: store.AccountID,
		Aliases: []ReviewAlias{aliases[1], aliases[0]}, SignalBeforeFirst: true,
	})
	child.waitForSignal(t)

	identical, err := store.RemoveOrphanReviewAliasesExact([]ReviewAlias{aliases[0], aliases[1]})
	if !errors.Is(err, transfer.ErrSessionLocked) || identical.Removed != 0 || identical.RecoveryRequired {
		t.Fatalf("identical cross-process batch result=%#v err=%v", identical, err)
	}
	overlap, err := store.RemoveOrphanReviewAliasesExact([]ReviewAlias{aliases[2], aliases[1]})
	if !errors.Is(err, transfer.ErrSessionLocked) || overlap.Removed != 0 || overlap.RecoveryRequired {
		t.Fatalf("overlapping cross-process batch result=%#v err=%v", overlap, err)
	}
	disjoint, err := store.RemoveOrphanReviewAliasesExact([]ReviewAlias{aliases[3], aliases[2]})
	if err != nil || disjoint.Removed != 2 || disjoint.RecoveryRequired {
		t.Fatalf("disjoint cross-process batch result=%#v err=%v", disjoint, err)
	}
	assertExactAliasBatchPresent(t, store, aliases[:2])
	assertExactAliasMissing(t, store, aliases[2])
	assertExactAliasMissing(t, store, aliases[3])

	child.stop(t)
	assertExactAliasBatchPresent(t, store, aliases[:2])
}

func TestRemoveOrphanReviewAliasesExactCrashBeforeFirstDeleteLeavesBatchIntact(t *testing.T) {
	store, aliases := prepareExactAliasRepairProcessBatch(t, 2)
	child := startExactAliasRepairChild(t, exactAliasRepairProcessFixture{
		Root: store.Root, ProfileScope: store.ProfileScope, AccountID: store.AccountID,
		Aliases: []ReviewAlias{aliases[1], aliases[0]}, SignalBeforeFirst: true,
	})
	child.waitForSignal(t)
	child.stop(t)

	assertExactAliasBatchPresent(t, store, aliases)
	result, err := store.RemoveOrphanReviewAliasesExact(aliases)
	if err != nil || result.Removed != 2 || result.RecoveryRequired {
		t.Fatalf("post-crash exact batch did not reacquire released process locks: result=%#v err=%v", result, err)
	}
}

func TestRemoveOrphanReviewAliasesExactCrashAfterFirstDeleteConvergesWithFreshReview(t *testing.T) {
	store, aliases := prepareExactAliasRepairProcessBatch(t, 2)
	before, err := store.DiagnoseReviewAliases(8, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldPlan, err := BuildReviewAliasRepairPlan(before, 2)
	if err != nil || oldPlan.Eligible != 2 || len(oldPlan.Candidates) != 2 {
		t.Fatalf("initial repair plan=%#v err=%v", oldPlan, err)
	}

	child := startExactAliasRepairChild(t, exactAliasRepairProcessFixture{
		Root: store.Root, ProfileScope: store.ProfileScope, AccountID: store.AccountID,
		Aliases: []ReviewAlias{aliases[1], aliases[0]}, SignalAfterRemovals: 1,
	})
	child.waitForSignal(t)
	child.stop(t)

	assertExactAliasMissing(t, store, aliases[0])
	assertExactAliasBatchPresent(t, store, aliases[1:])
	after, err := store.DiagnoseReviewAliases(8, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	if after.Scanned != 1 || after.Orphan != 1 || after.Issues != 1 || len(after.Entries) != 1 || after.Entries[0].Alias.ReviewID != aliases[1].ReviewID {
		t.Fatalf("post-crash diagnosis=%#v", after)
	}
	freshPlan, err := BuildReviewAliasRepairPlan(after, 2)
	if err != nil {
		t.Fatal(err)
	}
	if freshPlan.RepairSetID == oldPlan.RepairSetID || freshPlan.Eligible != 1 || len(freshPlan.Candidates) != 1 || freshPlan.Candidates[0].Alias.ReviewID != aliases[1].ReviewID {
		t.Fatalf("fresh repair plan did not replace stale pre-crash review: old=%#v fresh=%#v", oldPlan, freshPlan)
	}
	result, err := store.RemoveOrphanReviewAliasesExact([]ReviewAlias{freshPlan.Candidates[0].Alias})
	if err != nil || result.Removed != 1 || result.RecoveryRequired {
		t.Fatalf("fresh post-crash repair result=%#v err=%v", result, err)
	}
	final, err := store.DiagnoseReviewAliases(8, 8, nil)
	if err != nil || final.Scanned != 0 || final.Orphan != 0 || final.Issues != 0 {
		t.Fatalf("final converged diagnosis=%#v err=%v", final, err)
	}
}

func TestRemoveOrphanReviewAliasesExactCrashAfterWholeDeleteConvergesCleanly(t *testing.T) {
	store, aliases := prepareExactAliasRepairProcessBatch(t, 2)
	before, err := store.DiagnoseReviewAliases(8, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldPlan, err := BuildReviewAliasRepairPlan(before, 2)
	if err != nil {
		t.Fatal(err)
	}
	child := startExactAliasRepairChild(t, exactAliasRepairProcessFixture{
		Root: store.Root, ProfileScope: store.ProfileScope, AccountID: store.AccountID,
		Aliases: aliases, SignalAfterRemovals: len(aliases),
	})
	child.waitForSignal(t)
	child.stop(t)
	for _, alias := range aliases {
		assertExactAliasMissing(t, store, alias)
	}
	final, err := store.DiagnoseReviewAliases(8, 8, nil)
	if err != nil || final.Scanned != 0 || final.Orphan != 0 || final.Issues != 0 {
		t.Fatalf("post-whole-delete crash diagnosis=%#v err=%v", final, err)
	}
	freshPlan, err := BuildReviewAliasRepairPlan(final, 2)
	if err != nil {
		t.Fatal(err)
	}
	if freshPlan.RepairSetID == oldPlan.RepairSetID || freshPlan.Eligible != 0 || len(freshPlan.Candidates) != 0 {
		t.Fatalf("whole-delete crash did not invalidate stale repair review: old=%#v fresh=%#v", oldPlan, freshPlan)
	}
	result, err := store.RemoveOrphanReviewAliasesExact(aliases)
	if !errors.Is(err, ErrReviewAliasChanged) || result.Removed != 0 || result.RecoveryRequired {
		t.Fatalf("post-whole-delete stale exact repair result=%#v err=%v", result, err)
	}
}

func TestRemoveOrphanReviewAliasesExactCrashDuringRollbackConvergesWithFreshReview(t *testing.T) {
	store, aliases := prepareExactAliasRepairProcessBatch(t, 3)
	before, err := store.DiagnoseReviewAliases(8, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldPlan, err := BuildReviewAliasRepairPlan(before, 3)
	if err != nil {
		t.Fatal(err)
	}
	child := startExactAliasRepairChild(t, exactAliasRepairProcessFixture{
		Root: store.Root, ProfileScope: store.ProfileScope, AccountID: store.AccountID,
		Aliases: aliases, FailRemoveAt: 3, SignalDuringRollbackAfter: 1,
	})
	child.waitForSignal(t)
	child.stop(t)

	assertExactAliasMissing(t, store, aliases[0])
	assertExactAliasBatchPresent(t, store, aliases[1:])
	after, err := store.DiagnoseReviewAliases(8, 8, nil)
	if err != nil {
		t.Fatal(err)
	}
	freshPlan, err := BuildReviewAliasRepairPlan(after, 3)
	if err != nil {
		t.Fatal(err)
	}
	if after.Scanned != 2 || after.Orphan != 2 || after.Issues != 2 || freshPlan.RepairSetID == oldPlan.RepairSetID || freshPlan.Eligible != 2 || len(freshPlan.Candidates) != 2 {
		t.Fatalf("rollback-crash did not converge to remaining orphan set: diagnosis=%#v old=%#v fresh=%#v", after, oldPlan, freshPlan)
	}
	expected := []ReviewAlias{freshPlan.Candidates[0].Alias, freshPlan.Candidates[1].Alias}
	result, err := store.RemoveOrphanReviewAliasesExact(expected)
	if err != nil || result.Removed != 2 || result.RecoveryRequired {
		t.Fatalf("fresh repair after rollback crash result=%#v err=%v", result, err)
	}
	final, err := store.DiagnoseReviewAliases(8, 8, nil)
	if err != nil || final.Scanned != 0 || final.Issues != 0 {
		t.Fatalf("rollback-crash final diagnosis=%#v err=%v", final, err)
	}
}

func TestExactRepairRawPlanLocksBlockReviewedCleanupWithoutMutation(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: fmt.Sprintf("%064x", 0xabc1), AccountID: 42, Retention: time.Hour}
	planID, updatedAt := completeCurrentForMaintenanceTest(t, store, 48*time.Hour)
	reviewID := fmt.Sprintf("sha256:%064x", 0xabc2)
	alias, err := store.WriteReviewAlias(reviewID, planID)
	if err != nil {
		t.Fatal(err)
	}
	child := startExactAliasRepairChild(t, exactAliasRepairProcessFixture{
		Action: "hold-exact-raw-locks", Root: store.Root, ProfileScope: store.ProfileScope,
		AccountID: store.AccountID, Aliases: []ReviewAlias{alias},
	})
	child.waitForSignal(t)

	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	_, cleanupErr := store.TrashCurrentReviewed(guard, reviewID, planID, StatusCompleted, updatedAt, time.Hour, time.Now().UTC())
	closeErr := guard.Close()
	if !errors.Is(cleanupErr, transfer.ErrSessionLocked) || closeErr != nil {
		t.Fatalf("reviewed cleanup under exact-repair raw lock err=%v close=%v", cleanupErr, closeErr)
	}
	if current, err := store.InspectCurrent(planID); err != nil || current.PlanID != planID {
		t.Fatalf("contended reviewed cleanup mutated current: current=%#v err=%v", current, err)
	}
	if resolved, err := store.ResolveReviewAlias(reviewID); err != nil || resolved != planID {
		t.Fatalf("contended reviewed cleanup mutated alias: resolved=%q err=%v", resolved, err)
	}

	child.stop(t)
	guard, err = store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	trashPath, cleanupErr := store.TrashCurrentReviewed(guard, reviewID, planID, StatusCompleted, updatedAt, time.Hour, time.Now().UTC())
	closeErr = guard.Close()
	if cleanupErr != nil || closeErr != nil || trashPath == "" {
		t.Fatalf("reviewed cleanup did not recover after process lock release: path=%q err=%v close=%v", trashPath, cleanupErr, closeErr)
	}
}

func TestExactRepairRawPlanLocksBlockRestoreWithoutMutation(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: fmt.Sprintf("%064x", 0xdef1), AccountID: 42, Retention: time.Hour}
	planID, updatedAt := completeCurrentForMaintenanceTest(t, store, 48*time.Hour)
	reviewID := fmt.Sprintf("sha256:%064x", 0xdef2)
	alias, err := store.WriteReviewAlias(reviewID, planID)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	trashPath, err := store.TrashCurrentReviewed(guard, reviewID, planID, StatusCompleted, updatedAt, time.Hour, time.Now().UTC())
	closeErr := guard.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("prepare restore contention trash path=%q err=%v close=%v", trashPath, err, closeErr)
	}
	scan, err := store.ScanTrashedCurrent(8)
	if err != nil || len(scan.Records) != 1 {
		t.Fatalf("prepare restore contention scan=%#v err=%v", scan, err)
	}
	record := scan.Records[0]

	child := startExactAliasRepairChild(t, exactAliasRepairProcessFixture{
		Action: "hold-exact-raw-locks", Root: store.Root, ProfileScope: store.ProfileScope,
		AccountID: store.AccountID, Aliases: []ReviewAlias{alias},
	})
	child.waitForSignal(t)
	guard, err = store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	_, restoreErr := store.RestoreTrashedCurrentReviewed(
		guard, reviewID, record.TrashName, planID, record.Journal.UpdatedAt, record.TrashedAt, record.ReviewIDs,
	)
	closeErr = guard.Close()
	if !errors.Is(restoreErr, transfer.ErrSessionLocked) || closeErr != nil {
		t.Fatalf("restore under exact-repair raw lock err=%v close=%v", restoreErr, closeErr)
	}
	if _, err := os.Stat(trashPath); err != nil {
		t.Fatalf("contended restore consumed trash: %v", err)
	}
	if _, err := store.InspectCurrent(planID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("contended restore recreated current: %v", err)
	}
	assertExactAliasMissing(t, store, alias)

	child.stop(t)
	guard, err = store.AcquireCleanupGuard()
	if err != nil {
		t.Fatal(err)
	}
	restored, restoreErr := store.RestoreTrashedCurrentReviewed(
		guard, reviewID, record.TrashName, planID, record.Journal.UpdatedAt, record.TrashedAt, record.ReviewIDs,
	)
	closeErr = guard.Close()
	if restoreErr != nil || closeErr != nil || restored.PlanID != planID {
		t.Fatalf("restore did not recover after process lock release: restored=%#v err=%v close=%v", restored, restoreErr, closeErr)
	}
	if resolved, err := store.ResolveReviewAlias(reviewID); err != nil || resolved != planID {
		t.Fatalf("restored alias mismatch after contention: resolved=%q err=%v", resolved, err)
	}
}

func TestRemoveOrphanReviewAliasesExactTrashRenameCrashWindowFailsClosedUntilGCPurge(t *testing.T) {
	store := Store{Root: t.TempDir(), ProfileScope: fmt.Sprintf("%064x", 0xcafe), AccountID: 42, Retention: time.Hour}
	planID, _ := completeCurrentForMaintenanceTest(t, store, 48*time.Hour)
	reviewID := fmt.Sprintf("sha256:%064x", 0xfeed)
	alias, err := store.WriteReviewAlias(reviewID, planID)
	if err != nil {
		t.Fatal(err)
	}
	child := startExactAliasRepairChild(t, exactAliasRepairProcessFixture{
		Action: "trash-rename-before-sidecar", Root: store.Root, ProfileScope: store.ProfileScope,
		AccountID: store.AccountID, PlanID: planID, Aliases: []ReviewAlias{alias},
	})
	child.waitForSignal(t)
	child.stop(t)

	if _, err := store.InspectCurrent(planID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename crash left current journal visible: %v", err)
	}
	assertExactAliasBatchPresent(t, store, []ReviewAlias{alias})
	trashed, err := store.HasTrashedCurrentPlan(planID, 8)
	if err != nil || !trashed {
		t.Fatalf("rename crash trash proof=%v err=%v", trashed, err)
	}
	result, err := store.RemoveOrphanReviewAliasesExact([]ReviewAlias{alias})
	if !errors.Is(err, ErrReviewAliasTrashed) || result.Removed != 0 || result.RecoveryRequired {
		t.Fatalf("rename-before-sidecar crash repair result=%#v err=%v", result, err)
	}
	diagnosis, err := store.DiagnoseReviewAliases(8, 8, nil)
	if err != nil || diagnosis.Scanned != 1 || diagnosis.SoftDeleted != 1 || diagnosis.Orphan != 0 {
		t.Fatalf("rename-before-sidecar crash diagnosis=%#v err=%v", diagnosis, err)
	}

	sessionStore := transfer.SessionStore{Root: store.Root}
	actions, err := sessionStore.RunGCExclusive(transfer.SessionGCOptions{
		Now: time.Now().UTC().Add(2 * time.Hour), Retention: 24 * time.Hour, TrashRetention: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	purged := false
	for _, action := range actions {
		if action.Action == "purge" && action.Reason == "trash-retention" {
			purged = true
		}
	}
	if !purged {
		t.Fatalf("session GC did not purge expired crash-window trash: %#v", actions)
	}
	trashed, err = store.HasTrashedCurrentPlan(planID, 8)
	if err != nil || trashed {
		t.Fatalf("post-GC trash proof=%v err=%v", trashed, err)
	}
	postGC, err := store.DiagnoseReviewAliases(8, 8, nil)
	if err != nil || postGC.Scanned != 1 || postGC.Orphan != 1 || postGC.SoftDeleted != 0 {
		t.Fatalf("post-GC diagnosis=%#v err=%v", postGC, err)
	}
	freshPlan, err := BuildReviewAliasRepairPlan(postGC, 1)
	if err != nil || len(freshPlan.Candidates) != 1 {
		t.Fatalf("post-GC repair plan=%#v err=%v", freshPlan, err)
	}
	result, err = store.RemoveOrphanReviewAliasesExact([]ReviewAlias{freshPlan.Candidates[0].Alias})
	if err != nil || result.Removed != 1 || result.RecoveryRequired {
		t.Fatalf("post-GC exact repair result=%#v err=%v", result, err)
	}
}
