package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

func completeSyncJournalForTest(t *testing.T, store syncJournalStore, plan syncPlan) {
	t.Helper()
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.mutate(func(journal *syncExecutionJournal) error {
		journal.State = "completed"
		journal.Items[0].State = "succeeded"
		journal.Items[0].Phase = "done"
		journal.Items[0].Post = &syncJournalPostcondition{Side: "remote", Exists: true, Kind: "file", Size: 4}
		return nil
	}); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSyncJournalOpportunisticGCThrottles(t *testing.T) {
	store := testSyncJournalStore(t)
	store.Retention = time.Nanosecond
	firstPlan := testSyncJournalPlan(t)
	completeSyncJournalForTest(t, store, firstPlan)
	time.Sleep(2 * time.Millisecond)

	actions, err := store.RunOpportunisticGC(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].PlanID != firstPlan.PlanID {
		t.Fatalf("first opportunistic GC actions: %#v", actions)
	}
	if _, err := store.Inspect(firstPlan.PlanID); !errors.Is(err, errSyncJournalNotFound) {
		t.Fatalf("first completed journal survived opportunistic GC: %v", err)
	}

	secondPlan := testSyncJournalPlan(t)
	completeSyncJournalForTest(t, store, secondPlan)
	time.Sleep(2 * time.Millisecond)
	actions, err = store.RunOpportunisticGC(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("throttled opportunistic GC unexpectedly ran again: %#v", actions)
	}
	if _, err := store.Inspect(secondPlan.PlanID); err != nil {
		t.Fatalf("throttled GC removed second journal: %v", err)
	}
	root, err := store.root()
	if err != nil {
		t.Fatal(err)
	}
	maintenance, err := readSyncJournalMaintenance(filepath.Join(root, "maintenance.json"))
	if err != nil || maintenance.LastGC.IsZero() {
		t.Fatalf("journal maintenance timestamp missing: %#v err=%v", maintenance, err)
	}
}

func TestSyncJournalGCProtectsMigrationBatchJournalAndBackup(t *testing.T) {
	store := testSyncJournalStore(t)
	store.Retention = time.Nanosecond
	plan := testSyncJournalPlan(t)
	location, _ := writeLegacySyncJournalV1(t, store, plan, func(raw map[string]json.RawMessage) {
		raw["state"] = json.RawMessage(`"failed"`)
	})
	lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := store.prepareMigrationLocked(location)
	if err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := store.ensurePreparedMigrationBackups(prepared); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	marker, err := newSyncJournalMigrationBatchMarker(store, []syncJournalPreparedMigration{prepared})
	if err != nil {
		t.Fatal(err)
	}
	marker.State = syncJournalMigrationBatchMigrating
	batchLocation, err := store.migrationBatchLocation()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.createMigrationBatchMarker(batchLocation, marker); err != nil {
		t.Fatal(err)
	}
	backupPath, err := syncJournalMigrationBackupPath(location, prepared.Trace[0].Record)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)

	actions, err := store.RunGCExclusive(0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("GC collected migration-batch evidence: %#v", actions)
	}
	if _, err := store.Inspect(plan.PlanID); err != nil {
		t.Fatalf("protected migration journal was removed: %v", err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("protected migration backup was removed: %v", err)
	}
	if _, err := os.Stat(batchLocation.MarkerPath); err != nil {
		t.Fatalf("GC removed migration batch marker: %v", err)
	}

	if err := os.Remove(batchLocation.MarkerPath); err != nil {
		t.Fatal(err)
	}
	actions, err = store.RunGCExclusive(0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].PlanID != plan.PlanID {
		t.Fatalf("unprotected expired journal was not collected: %#v", actions)
	}
	if _, err := store.Inspect(plan.PlanID); !errors.Is(err, errSyncJournalNotFound) {
		t.Fatalf("unprotected journal survived GC: %v", err)
	}
	if _, err := os.Stat(backupPath); !os.IsNotExist(err) {
		t.Fatalf("active backup path survived journal trash move: %v", err)
	}
}

func TestSyncJournalOpportunisticMaintenanceProtectsInUseJournal(t *testing.T) {
	store := testSyncJournalStore(t)
	store.AutoGC = true
	store.GCInterval = time.Nanosecond
	store.Retention = time.Nanosecond
	store.TrashRetention = time.Hour
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.mutate(func(journal *syncExecutionJournal) error {
		journal.State = "failed"
		journal.Items[0].State = "failed"
		return nil
	}); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if err := runSyncJournalOpportunisticMaintenance(store); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if _, err := store.Inspect(plan.PlanID); err != nil {
		handle.Close()
		t.Fatalf("opportunistic maintenance removed locked journal: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	actions, err := store.RunGCExclusive(0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].PlanID != plan.PlanID {
		t.Fatalf("exclusive GC did not collect unlocked failed journal: %#v", actions)
	}
}
