package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/internal/transfer"
)

type interruptedMigrationBatchFixture struct {
	marker    syncJournalMigrationBatchMarker
	location  syncJournalMigrationBatchLocation
	plans     []syncPlan
	locations []syncJournalLocation
	originals [][]byte
	prepared  []syncJournalPreparedMigration
}

func createInterruptedMigrationBatchFixture(t *testing.T, store syncJournalStore, migratedCount int) interruptedMigrationBatchFixture {
	t.Helper()
	plans := []syncPlan{testSyncJournalPlan(t), testSyncJournalPlan(t)}
	fixture := interruptedMigrationBatchFixture{
		plans: plans, locations: make([]syncJournalLocation, len(plans)), originals: make([][]byte, len(plans)),
		prepared: make([]syncJournalPreparedMigration, 0, len(plans)),
	}
	for index, plan := range plans {
		location, original := writeLegacySyncJournalV1(t, store, plan, nil)
		fixture.locations[index] = location
		fixture.originals[index] = append([]byte(nil), original...)
		lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := store.prepareMigrationLocked(location)
		if closeErr := lock.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := store.ensurePreparedMigrationBackups(prepared); err != nil {
			t.Fatal(err)
		}
		fixture.prepared = append(fixture.prepared, prepared)
	}
	marker, err := newSyncJournalMigrationBatchMarker(store, fixture.prepared)
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
	for index := 0; index < migratedCount; index++ {
		if err := store.persistPreparedMigrationJournal(fixture.prepared[index]); err != nil {
			t.Fatal(err)
		}
		if err := updateSyncJournalMigrationBatchCandidate(&marker, fixture.prepared[index].Result.PlanID, syncJournalMigrationCandidateMigrated, ""); err != nil {
			t.Fatal(err)
		}
		if err := store.writeMigrationBatchMarker(batchLocation, marker); err != nil {
			t.Fatal(err)
		}
	}
	fixture.marker = marker
	fixture.location = batchLocation
	return fixture
}

func TestSyncJournalMigrationBatchRecoveryMachineSchemaAndExitCode(t *testing.T) {
	store := testSyncJournalStore(t)
	result, err := store.RecoverMigrationBatch()
	if !errors.Is(err, errSyncJournalMigrationBatchNotFound) {
		t.Fatalf("empty migration store returned unexpected recovery error: %v", err)
	}
	if result.Schema != syncJournalMigrationBatchRecoverySchema {
		t.Fatalf("batch recovery result schema: %#v", result)
	}
	exitErr, ok := syncJournalExitErrorData(err, result).(*exitError)
	if !ok || exitErr.code != output.ExitArgs {
		t.Fatalf("batch recovery error classification: %T %#v", exitErr, exitErr)
	}
	data, ok := exitErr.data.(syncJournalMigrationBatchRecoveryResult)
	if !ok || data.Schema != syncJournalMigrationBatchRecoverySchema {
		t.Fatalf("batch recovery error data contract: %#v", exitErr.data)
	}
	encoded, marshalErr := json.Marshal(data)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if !strings.Contains(string(encoded), `"schema":"`+syncJournalMigrationBatchRecoverySchema+`"`) {
		t.Fatalf("batch recovery JSON schema contract: %s", encoded)
	}
}

func TestSyncJournalDoctorDetectsInterruptedMigrationBatchByExactBytes(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 1)

	report, err := store.Diagnose()
	if err != nil {
		t.Fatal(err)
	}
	if !report.InterruptedMigrationBatch || report.MigrationBatch == nil || !report.MigrationBatch.Interrupted {
		t.Fatalf("doctor missed interrupted migration batch: %#v", report)
	}
	if report.MigrationBatch.Candidates != 2 || report.MigrationBatch.Original != 1 || report.MigrationBatch.Migrated != 1 || report.MigrationBatch.Unknown != 0 || report.MigrationBatch.BackupIssues != 0 {
		t.Fatalf("interrupted migration batch diagnosis: %#v", report.MigrationBatch)
	}
	if report.MigrationBatch.BatchID != fixture.marker.BatchID || report.Issues < 1 || report.AllCurrentAndValid {
		t.Fatalf("interrupted batch aggregate contract: %#v", report)
	}
}

func TestSyncJournalDoctorDoesNotMisclassifyActiveMigrationBatchAsInterrupted(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 1)
	batchLock, _, err := store.acquireMigrationBatchLock()
	if err != nil {
		t.Fatal(err)
	}
	defer batchLock.Close()

	report, err := store.Diagnose()
	if err != nil {
		t.Fatal(err)
	}
	if report.InterruptedMigrationBatch || report.MigrationBatch == nil || !report.MigrationBatch.InUse || report.MigrationBatch.Interrupted {
		t.Fatalf("active migration batch was misclassified: %#v", report.MigrationBatch)
	}
	if report.MigrationBatch.BatchID != fixture.marker.BatchID || report.MigrationBatch.Original != 1 || report.MigrationBatch.Migrated != 1 {
		t.Fatalf("active migration batch diagnosis lost exact state: %#v", report.MigrationBatch)
	}
	if report.Warnings < 1 {
		t.Fatalf("active migration batch was not surfaced as a doctor warning: %#v", report)
	}
}

func TestSyncJournalMigrationBatchProtectsCandidateFromOrdinaryMutations(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 1)

	if _, err := store.Migrate(fixture.plans[1].PlanID); !errors.Is(err, errSyncJournalMigrationBatchExists) {
		t.Fatalf("single migration mutated a batch-protected source candidate: %v", err)
	}
	if _, err := store.Open(fixture.plans[0].PlanID); !errors.Is(err, errSyncJournalMigrationBatchExists) {
		t.Fatalf("Open exposed a batch-protected migrated candidate for mutation: %v", err)
	}
	if _, err := store.ForceTrash(fixture.plans[0].PlanID); !errors.Is(err, errSyncJournalMigrationBatchExists) {
		t.Fatalf("forced trash removed a batch-protected candidate: %v", err)
	}
	for index, location := range fixture.locations {
		data, err := os.ReadFile(location.JournalPath)
		if err != nil {
			t.Fatal(err)
		}
		actual := syncJournalMigrationBytesActual(fixture.marker.Candidates[index], data)
		if index == 0 && actual != syncJournalMigrationActualMigrated {
			t.Fatalf("protected migrated candidate changed: %s", actual)
		}
		if index == 1 && actual != syncJournalMigrationActualOriginal {
			t.Fatalf("protected source candidate changed: %s", actual)
		}
	}
}

func TestSyncJournalMigrationBatchProtectsCandidateFromRecreation(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 0)
	if err := os.RemoveAll(fixture.locations[0].Dir); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(fixture.plans[0]); !errors.Is(err, errSyncJournalMigrationBatchExists) {
		t.Fatalf("Create recreated a batch-protected missing candidate: %v", err)
	}
	if _, err := os.Stat(fixture.locations[0].JournalPath); !os.IsNotExist(err) {
		t.Fatalf("batch-protected missing candidate was recreated: %v", err)
	}
}

func TestSyncJournalMigrationBatchProtectsFailedCandidateFromGC(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, _ := writeLegacySyncJournalV1(t, store, plan, func(raw map[string]json.RawMessage) {
		raw["state"] = json.RawMessage(`"failed"`)
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw["items"], &items); err != nil {
			t.Fatal(err)
		}
		items[0]["state"] = json.RawMessage(`"failed"`)
		encoded, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		raw["items"] = encoded
	})
	lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := store.prepareMigrationLocked(location)
	if closeErr := lock.Close(); err == nil && closeErr != nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ensurePreparedMigrationBackups(prepared); err != nil {
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
	time.Sleep(2 * time.Millisecond)
	actions, err := store.GC(time.Nanosecond, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("GC proposed a batch-protected failed journal: %#v", actions)
	}
	if _, err := store.Inspect(plan.PlanID); err != nil {
		t.Fatalf("GC protection damaged candidate: %v", err)
	}
}

func TestSyncJournalCorruptMigrationBatchMarkerBlocksJournalMutation(t *testing.T) {
	store := testSyncJournalStore(t)
	batchLocation, err := store.migrationBatchLocation()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(batchLocation.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(batchLocation.MarkerPath, []byte(`{"schema":"broken"}`), 0600); err != nil {
		t.Fatal(err)
	}
	plan := testSyncJournalPlan(t)
	if _, err := store.Create(plan); !errors.Is(err, errSyncJournalMigrationBatchExists) {
		t.Fatalf("corrupt migration marker did not conservatively block journal creation: %v", err)
	}
	actions, err := store.GC(time.Nanosecond, true)
	if !errors.Is(err, errSyncJournalMigrationBatchExists) || len(actions) != 0 {
		t.Fatalf("corrupt migration marker did not conservatively block GC: actions=%#v err=%v", actions, err)
	}
}

func TestSyncJournalMigrationBatchV1MarkerRemainsReadableAndRecoverable(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 1)
	legacy := fixture.marker
	legacy.Version = 1
	legacy.BatchID, _ = syncJournalMigrationBatchIDForVersion(legacy.Version, legacy.StartedAt, legacy.Candidates)
	if err := store.writeMigrationBatchMarker(fixture.location, legacy); err != nil {
		t.Fatalf("legacy v1 marker was not writable/readable: %v", err)
	}
	result, err := store.RecoverMigrationBatch()
	if err != nil {
		t.Fatalf("legacy v1 marker was not recoverable: %v", err)
	}
	if result.Original != 1 || result.Migrated != 1 || result.Restored != 1 || !result.MarkerRemoved {
		t.Fatalf("legacy v1 recovery result: %#v", result)
	}
}

func TestSyncJournalMigrationBatchIDDetectsCandidateHashTampering(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 0)
	tampered := fixture.marker
	tampered.Candidates = append([]syncJournalMigrationBatchCandidate(nil), fixture.marker.Candidates...)
	tampered.Candidates[0].TargetSHA256 = strings.Repeat("0", 64)
	if err := validateSyncJournalMigrationBatchMarker(tampered, store); err == nil || !strings.Contains(err.Error(), "batch ID") {
		t.Fatalf("marker candidate hash tampering did not invalidate batch identity: %v", err)
	}
}

func TestSyncJournalMigrationBatchIDDetectsVersionEdgeTampering(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 0)
	tampered := fixture.marker
	tampered.Candidates = append([]syncJournalMigrationBatchCandidate(nil), fixture.marker.Candidates...)
	tampered.Candidates[0].ToVersion++
	if err := validateSyncJournalMigrationBatchMarker(tampered, store); err == nil || !strings.Contains(err.Error(), "batch ID") {
		t.Fatalf("marker version-edge tampering did not invalidate batch identity: %v", err)
	}
}

func TestSyncJournalMigrationBatchMarkerRemovalRevalidatesExactBytes(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 0)
	tampered := append(append([]byte(nil), fixture.originals[0]...), '\n')
	if err := os.WriteFile(fixture.locations[0].JournalPath, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.removeMigrationBatchMarkerIfExact(fixture.location, fixture.marker, syncJournalMigrationActualOriginal); !errors.Is(err, errSyncJournalMigrationBatchRecoveryRequired) {
		t.Fatalf("marker removal ignored changed candidate bytes: %v", err)
	}
	if _, err := os.Stat(fixture.location.MarkerPath); err != nil {
		t.Fatalf("failed exact-byte revalidation removed recovery marker: %v", err)
	}
}

func TestSyncJournalRecoverMigrationBatchRollsBackMixedExactState(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 1)

	result, err := store.RecoverMigrationBatch()
	if err != nil {
		t.Fatal(err)
	}
	if result.Schema != syncJournalMigrationBatchRecoverySchema || result.BatchID != fixture.marker.BatchID || result.Candidates != 2 || result.Original != 1 || result.Migrated != 1 || result.Restored != 1 || result.Finalized || !result.MarkerRemoved {
		t.Fatalf("mixed batch recovery result: %#v", result)
	}
	for index, location := range fixture.locations {
		data, err := os.ReadFile(location.JournalPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, fixture.originals[index]) {
			t.Fatalf("mixed batch recovery did not restore exact source bytes for %s", fixture.plans[index].PlanID)
		}
		journal, err := store.Inspect(fixture.plans[index].PlanID)
		if err != nil {
			t.Fatal(err)
		}
		if journal.Version != 1 || !journal.MigrationRequired {
			t.Fatalf("mixed batch recovery did not restore legacy schema: %#v", journal)
		}
	}
	batch, err := store.DiagnoseMigrationBatch()
	if err != nil {
		t.Fatal(err)
	}
	if batch.Exists {
		t.Fatalf("recovered migration marker remained: %#v", batch)
	}
}

func TestSyncJournalRecoverMigrationBatchFinalizesAllTargetWithoutBackups(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 2)
	for _, prepared := range fixture.prepared {
		if err := os.RemoveAll(prepared.Location.Dir + string(os.PathSeparator) + syncJournalMigrationBackupDirName); err != nil {
			t.Fatal(err)
		}
	}

	result, err := store.RecoverMigrationBatch()
	if err != nil {
		t.Fatal(err)
	}
	if result.Original != 0 || result.Migrated != 2 || result.Restored != 0 || !result.Finalized || !result.MarkerRemoved {
		t.Fatalf("all-target batch finalization result: %#v", result)
	}
	for _, plan := range fixture.plans {
		journal, err := store.Inspect(plan.PlanID)
		if err != nil {
			t.Fatal(err)
		}
		if journal.Version != syncJournalVersion || journal.MigrationRequired {
			t.Fatalf("all-target finalization changed migrated journal: %#v", journal)
		}
	}
}

func TestSyncJournalRecoverMigrationBatchClearsAllSource(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 0)
	result, err := store.RecoverMigrationBatch()
	if err != nil {
		t.Fatal(err)
	}
	if result.Original != 2 || result.Migrated != 0 || result.Restored != 0 || result.Finalized || !result.MarkerRemoved {
		t.Fatalf("all-source batch recovery result: %#v", result)
	}
	for index, location := range fixture.locations {
		data, err := os.ReadFile(location.JournalPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, fixture.originals[index]) {
			t.Fatalf("all-source recovery changed journal %s", fixture.plans[index].PlanID)
		}
	}
}

func TestSyncJournalRecoverMigrationBatchFailsClosedOnUnknownBytes(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 1)
	data, err := os.ReadFile(fixture.locations[0].JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(fixture.locations[0].JournalPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	result, err := store.RecoverMigrationBatch()
	if !errors.Is(err, errSyncJournalMigrationBatchRecoveryRequired) || result.Restored != 0 {
		t.Fatalf("unknown batch bytes were not rejected: result=%#v err=%v", result, err)
	}
	if _, statErr := os.Stat(fixture.location.MarkerPath); statErr != nil {
		t.Fatalf("unknown-state recovery removed marker: %v", statErr)
	}
	batch, err := store.DiagnoseMigrationBatch()
	if err != nil {
		t.Fatal(err)
	}
	if !batch.Exists || batch.Unknown != 1 {
		t.Fatalf("unknown batch state not preserved for diagnosis: %#v", batch)
	}
}

func TestSyncJournalRecoverMigrationBatchRequiresBackupOnlyForMixedRollback(t *testing.T) {
	store := testSyncJournalStore(t)
	fixture := createInterruptedMigrationBatchFixture(t, store, 1)
	candidate := fixture.marker.Candidates[0]
	backupRecord := syncJournalMigrationCandidateBackupRecord(candidate)
	backupPath, err := syncJournalMigrationBackupPath(fixture.locations[0], backupRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(backupPath); err != nil {
		t.Fatal(err)
	}

	result, err := store.RecoverMigrationBatch()
	if !errors.Is(err, errSyncJournalMigrationBatchRecoveryRequired) || result.Restored != 0 {
		t.Fatalf("mixed recovery without backup was not blocked: result=%#v err=%v", result, err)
	}
	if _, statErr := os.Stat(fixture.location.MarkerPath); statErr != nil {
		t.Fatalf("failed mixed recovery removed marker: %v", statErr)
	}
}

func TestSyncJournalMigrateAllLeavesNoBatchMarkerOnSuccess(t *testing.T) {
	store := testSyncJournalStore(t)
	for range 2 {
		plan := testSyncJournalPlan(t)
		_, _ = writeLegacySyncJournalV1(t, store, plan, nil)
	}
	result, err := store.MigrateAll()
	if err != nil {
		t.Fatal(err)
	}
	if result.Migrated != 2 || result.Failed != 0 {
		t.Fatalf("bulk migration result: %#v", result)
	}
	batch, err := store.DiagnoseMigrationBatch()
	if err != nil {
		t.Fatal(err)
	}
	if batch.Exists {
		t.Fatalf("successful bulk migration left crash marker: %#v", batch)
	}
}
