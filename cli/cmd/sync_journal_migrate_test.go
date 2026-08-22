package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
)

func writeLegacySyncJournalV1(t *testing.T, store syncJournalStore, plan syncPlan, mutate func(map[string]json.RawMessage)) (syncJournalLocation, []byte) {
	t.Helper()
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	location := handle.location
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["version"] = json.RawMessage("1")
	delete(raw, "schema")
	delete(raw, "status")
	delete(raw, "run_stats")
	delete(raw, "schema_migrations")
	if mutate != nil {
		mutate(raw)
	}
	legacy, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(location.JournalPath, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	return location, legacy
}

func TestSyncJournalLegacyV1InspectAndListAreReadOnly(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, before := writeLegacySyncJournalV1(t, store, plan, nil)

	journal, err := store.Inspect(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Version != 1 || !journal.MigrationRequired || journal.Status != syncJournalStatusActive || len(journal.Migrations) != 0 {
		t.Fatalf("legacy inspect contract: %#v", journal)
	}
	encodedLegacy, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encodedLegacy), `"migration_required":true`) {
		t.Fatalf("legacy inspect JSON omits migration_required: %s", encodedLegacy)
	}
	afterInspect, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, afterInspect) {
		t.Fatal("read-only inspect rewrote legacy journal")
	}

	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Version != 1 || !entries[0].MigrationRequired || entries[0].Status != syncJournalStatusActive {
		t.Fatalf("legacy list migration state: %#v", entries)
	}
	afterList, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, afterList) {
		t.Fatal("read-only list rewrote legacy journal")
	}
	if err := store.write(location, journal); !errors.Is(err, errSyncJournalMigrationRequired) {
		t.Fatalf("legacy journal write was not blocked: %v", err)
	}
}

func TestSyncJournalMigrateV1PreservesUnknownJSONAndRecordsSource(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, source := writeLegacySyncJournalV1(t, store, plan, func(raw map[string]json.RawMessage) {
		raw["legacy_extension"] = json.RawMessage(`{"keep":true,"values":[1,2,3]}`)
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw["items"], &items); err != nil {
			t.Fatal(err)
		}
		items[0]["legacy_item_extension"] = json.RawMessage(`{"opaque":"preserve-me"}`)
		encoded, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		raw["items"] = encoded
	})
	sourceDigest := sha256.Sum256(source)

	result, err := store.Migrate(plan.PlanID[:12])
	if err != nil {
		t.Fatal(err)
	}
	if !result.Migrated || result.FromVersion != 1 || result.ToVersion != syncJournalVersion || result.Status != syncJournalStatusActive {
		t.Fatalf("migration result: %#v", result)
	}

	migratedBytes, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(migratedBytes, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["legacy_extension"]; !ok {
		t.Fatal("top-level unknown JSON was lost during migration")
	}
	var schema string
	if err := json.Unmarshal(raw["schema"], &schema); err != nil || schema != syncJournalSchemaID {
		t.Fatalf("migrated journal schema identity: %q err=%v", schema, err)
	}
	if _, ok := raw["status"]; !ok {
		t.Fatal("migrated journal is missing status")
	}
	if _, ok := raw["run_stats"]; !ok {
		t.Fatal("migrated journal is missing run_stats")
	}
	if _, ok := raw["migration_required"]; ok {
		t.Fatal("current migrated journal persisted derived migration_required")
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw["items"], &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0]["legacy_item_extension"] == nil {
		t.Fatalf("item-level unknown JSON was lost during migration: %#v", items)
	}

	journal, err := store.Inspect(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Version != syncJournalVersion || journal.Schema != syncJournalSchemaID || journal.MigrationRequired || len(journal.Migrations) != 1 {
		t.Fatalf("migrated journal schema metadata: %#v", journal)
	}
	record := journal.Migrations[0]
	if record.FromVersion != 1 || record.ToVersion != syncJournalVersion || record.MigratedAt.IsZero() || record.SourceSHA256 != hex.EncodeToString(sourceDigest[:]) || !record.BackupRequired {
		t.Fatalf("migration audit record: %#v", record)
	}
	backupPath, err := syncJournalMigrationBackupPath(location, record)
	if err != nil {
		t.Fatal(err)
	}
	backupBytes, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backupBytes, source) {
		t.Fatal("migration backup does not preserve the exact pre-migration journal bytes")
	}

	beforeSecond := append([]byte(nil), migratedBytes...)
	second, err := store.Migrate(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if second.Migrated || second.FromVersion != syncJournalVersion || second.ToVersion != syncJournalVersion {
		t.Fatalf("idempotent migration result: %#v", second)
	}
	afterSecond, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeSecond, afterSecond) {
		t.Fatal("current-version migrate rewrote journal")
	}
}

func TestSyncJournalOpenAutoMigratesLegacyUnderLock(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	_, _ = writeLegacySyncJournalV1(t, store, plan, nil)

	handle, err := store.Open(plan.PlanID[:12])
	if err != nil {
		t.Fatal(err)
	}
	if handle.snapshot().Version != syncJournalVersion || len(handle.snapshot().Migrations) != 1 {
		handle.Close()
		t.Fatalf("Open did not migrate legacy journal: %#v", handle.snapshot())
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := store.Inspect(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Version != syncJournalVersion {
		t.Fatalf("auto-migrated journal was not persisted: v%d", journal.Version)
	}
}

func TestOpenSyncResumeJournalRejectsInvalidRequestBeforeLegacyMigration(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, before := writeLegacySyncJournalV1(t, store, plan, nil)

	if _, err := openSyncResumeJournal(store, plan.PlanID, filepath.Join(plan.LocalRoot, "wrong"), plan.RemoteRoot, plan.PlanID, syncDeleteBudget{}); err == nil || !strings.Contains(err.Error(), "local root mismatch") {
		t.Fatalf("invalid resume request was accepted: %v", err)
	}
	after, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("invalid resume request migrated legacy journal before validation")
	}
}

func TestOpenSyncResumeJournalMigratesOnlyAfterReadOnlyValidation(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	_, _ = writeLegacySyncJournalV1(t, store, plan, nil)

	handle, err := openSyncResumeJournal(store, plan.PlanID, plan.LocalRoot, plan.RemoteRoot, plan.PlanID, syncDeleteBudget{})
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	if snapshot := handle.snapshot(); snapshot.Version != syncJournalVersion || len(snapshot.Migrations) != 1 {
		t.Fatalf("validated resume did not migrate legacy journal under lock: %#v", snapshot)
	}
}

func TestOpenSyncRecoveryJournalRejectsWrongStateBeforeLegacyMigration(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, before := writeLegacySyncJournalV1(t, store, plan, nil)

	if _, err := openSyncRecoveryJournal(store, plan.PlanID); err == nil || !strings.Contains(err.Error(), "not recovery-required") {
		t.Fatalf("active journal was accepted for recover: %v", err)
	}
	after, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("invalid recover request migrated legacy journal before state validation")
	}
}

func TestSyncJournalTrashDoesNotMigrateLegacyJournal(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	_, _ = writeLegacySyncJournalV1(t, store, plan, nil)
	trash, err := store.Trash(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(trash, "journal.json"))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := decodeSyncJournalData(data)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Version != 1 || len(journal.Migrations) != 0 {
		t.Fatalf("trash path unnecessarily migrated legacy journal: %#v", journal)
	}
}

func TestSyncJournalMigrationPreservesDestructiveRecoveryEvidence(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncDestructiveJournalPlan(t)
	_, _ = writeLegacySyncJournalV1(t, store, plan, func(raw map[string]json.RawMessage) {
		raw["state"] = json.RawMessage(`"failed"`)
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(raw["items"], &items); err != nil {
			t.Fatal(err)
		}
		items[0]["state"] = json.RawMessage(`"failed"`)
		items[0]["phase"] = json.RawMessage(`"delete-started"`)
		items[0]["attempts"] = json.RawMessage(`3`)
		encoded, err := json.Marshal(items)
		if err != nil {
			t.Fatal(err)
		}
		raw["items"] = encoded
	})

	result, err := store.Migrate(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Migrated || result.Status != syncJournalStatusReconcileRequired {
		t.Fatalf("destructive migration result: %#v", result)
	}
	journal, err := store.Inspect(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != syncJournalStatusFailed || journal.Status != syncJournalStatusReconcileRequired || journal.Items[0].State != syncJournalStatusFailed || journal.Items[0].Phase != "delete-started" || journal.Items[0].Attempts != 3 {
		t.Fatalf("destructive recovery evidence changed during migration: %#v", journal)
	}
}

func TestSyncJournalMigrationRequiresExclusiveJournalLock(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, before := writeLegacySyncJournalV1(t, store, plan, nil)
	lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if _, err := store.Migrate(plan.PlanID); !errors.Is(err, transfer.ErrSessionLocked) {
		t.Fatalf("migration ignored existing journal lock: %v", err)
	}
	after, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("locked migration changed legacy journal")
	}
}

func TestSyncJournalMigrationWriteFailureLeavesLegacyBytesUntouched(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, before := writeLegacySyncJournalV1(t, store, plan, nil)
	failing := store
	failing.writePrivateFile = func(string, []byte) error { return errors.New("injected migration write failure") }

	if _, err := failing.Migrate(plan.PlanID); err == nil || !strings.Contains(err.Error(), "injected migration write failure") {
		t.Fatalf("expected injected migration failure, got %v", err)
	}
	after, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed atomic migration changed legacy journal bytes")
	}
	journal, err := store.Inspect(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Version != 1 {
		t.Fatalf("failed migration changed stored schema version: %d", journal.Version)
	}
}

func TestSyncJournalHalfMigratedV2FailsClosed(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	_, legacy := writeLegacySyncJournalV1(t, store, plan, nil)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(legacy, &raw); err != nil {
		t.Fatal(err)
	}
	raw["version"] = json.RawMessage("2")
	partial, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeSyncJournalData(partial); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), "missing schema identity") {
		t.Fatalf("half-migrated v2 journal was accepted: %v", err)
	}
}

func TestSyncJournalV2EnvelopeRequiresStatusAndRunStats(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(handle.location.JournalPath)
	_ = handle.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"status", "run_stats"} {
		t.Run(key, func(t *testing.T) {
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(data, &raw); err != nil {
				t.Fatal(err)
			}
			delete(raw, key)
			broken, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeSyncJournalData(broken); !errors.Is(err, errSyncJournalInvalidSchema) || !strings.Contains(err.Error(), key) {
				t.Fatalf("schema v2 without %s was accepted: %v", key, err)
			}
		})
	}
}

func TestSyncJournalFutureVersionFailsClosedWithoutRewrite(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	location := handle.location
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["version"] = json.RawMessage("3")
	future, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(location.JournalPath, future, 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Inspect(plan.PlanID); !errors.Is(err, errSyncJournalNewerVersion) {
		t.Fatalf("future journal inspect did not fail closed: %v", err)
	}
	if _, err := store.Migrate(plan.PlanID); !errors.Is(err, errSyncJournalNewerVersion) {
		t.Fatalf("future journal migrate did not fail closed: %v", err)
	}
	if _, err := store.Open(plan.PlanID); !errors.Is(err, errSyncJournalNewerVersion) {
		t.Fatalf("future journal Open did not fail closed: %v", err)
	}
	after, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(future, after) {
		t.Fatal("future-version journal was rewritten")
	}
}

func TestSyncJournalRejectsInvalidMigrationHistory(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	location := handle.location
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	raw["schema_migrations"] = json.RawMessage(`[{"from_version":1,"to_version":2,"migrated_at":"2026-08-21T00:00:00Z","source_sha256":"not-a-sha"}]`)
	corrupt, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(location.JournalPath, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Inspect(plan.PlanID); err == nil || !strings.Contains(err.Error(), "invalid source_sha256") {
		t.Fatalf("invalid migration history was accepted: %v", err)
	}
}

func TestSyncJournalStorageLayoutVersionIsIndependentFromSchemaVersion(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, err := store.location(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if syncJournalVersion == 1 {
		t.Fatal("schema version test requires a post-v1 schema")
	}
	if !strings.Contains(filepath.ToSlash(location.Dir), "/sync/"+syncJournalLayoutVersion+"/") {
		t.Fatalf("journal layout unexpectedly follows schema version: %s", location.Dir)
	}
}

func TestSyncJournalMigrationRegistryCoversEveryReadableUpgradeEdge(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	_, legacy := writeLegacySyncJournalV1(t, store, plan, nil)
	journal, err := decodeSyncJournalData(legacy)
	if err != nil {
		t.Fatal(err)
	}
	data := legacy
	now := time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)
	for version := syncJournalMinReadableVersion; version < syncJournalVersion; version++ {
		step := syncJournalMigrationSteps[version]
		if step == nil {
			t.Fatalf("missing registered sync journal migration step from v%d to v%d", version, version+1)
		}
		if journal.Version != version {
			t.Fatalf("migration test journal is v%d before v%d step", journal.Version, version)
		}
		data, journal, err = step(data, journal, now)
		if err != nil {
			t.Fatalf("migration step v%d -> v%d failed: %v", version, version+1, err)
		}
		if journal.Version != version+1 {
			t.Fatalf("migration step v%d advanced to v%d", version, journal.Version)
		}
	}
	if journal.Version != syncJournalVersion {
		t.Fatalf("migration registry ended at v%d want v%d", journal.Version, syncJournalVersion)
	}
}

func TestSyncJournalMigrateAllPreflightsAndMigratesLegacyJournals(t *testing.T) {
	store := testSyncJournalStore(t)
	currentPlan := testSyncJournalPlan(t)
	current, err := store.Create(currentPlan)
	if err != nil {
		t.Fatal(err)
	}
	if err := current.Close(); err != nil {
		t.Fatal(err)
	}
	legacyA := testSyncJournalPlan(t)
	_, _ = writeLegacySyncJournalV1(t, store, legacyA, nil)
	legacyB := testSyncJournalPlan(t)
	_, _ = writeLegacySyncJournalV1(t, store, legacyB, nil)

	result, err := store.MigrateAll()
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 3 || result.Candidates != 2 || result.AlreadyCurrent != 1 || result.Migrated != 2 || result.Failed != 0 || len(result.Items) != 2 {
		t.Fatalf("bulk migration result: %#v", result)
	}
	if result.Postflight == nil || result.Postflight.Issues != 0 || result.Postflight.InUse != 0 || !result.Postflight.AllCurrentAndValid || result.Postflight.Healthy != 3 {
		t.Fatalf("bulk migration postflight: %#v", result.Postflight)
	}
	for _, plan := range []syncPlan{legacyA, legacyB} {
		journal, err := store.Inspect(plan.PlanID)
		if err != nil {
			t.Fatal(err)
		}
		if journal.Version != syncJournalVersion || journal.MigrationRequired {
			t.Fatalf("bulk migration left legacy journal: %#v", journal)
		}
	}
}

func TestSyncJournalMigrateAllRefusesKnownCorruptionBeforeAnyWrite(t *testing.T) {
	store := testSyncJournalStore(t)
	legacyPlan := testSyncJournalPlan(t)
	legacyLocation, legacyBefore := writeLegacySyncJournalV1(t, store, legacyPlan, nil)
	badPlan := testSyncJournalPlan(t)
	badHandle, err := store.Create(badPlan)
	if err != nil {
		t.Fatal(err)
	}
	badLocation := badHandle.location
	if err := badHandle.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteSyncJournalJSON(t, badLocation, func(raw map[string]json.RawMessage) { delete(raw, "run_stats") })

	result, err := store.MigrateAll()
	if !errors.Is(err, errSyncJournalBulkMigrationPreflight) || result.Migrated != 0 {
		t.Fatalf("bulk migration did not fail closed on corrupt store: result=%#v err=%v", result, err)
	}
	legacyAfter, err := os.ReadFile(legacyLocation.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(legacyBefore, legacyAfter) {
		t.Fatal("bulk migration changed a legacy journal before corruption preflight failed")
	}
}

func TestSyncJournalMigrateAllRefusesInUseCandidateBeforeAnyWrite(t *testing.T) {
	store := testSyncJournalStore(t)
	legacyA := testSyncJournalPlan(t)
	locationA, beforeA := writeLegacySyncJournalV1(t, store, legacyA, nil)
	legacyB := testSyncJournalPlan(t)
	locationB, beforeB := writeLegacySyncJournalV1(t, store, legacyB, nil)
	lock, err := transfer.AcquireSessionLock(locationB.LockPath, locationB.LeasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	result, err := store.MigrateAll()
	if !errors.Is(err, errSyncJournalBulkMigrationPreflight) || result.Migrated != 0 {
		t.Fatalf("bulk migration did not fail closed on in-use candidate: result=%#v err=%v", result, err)
	}
	for _, pair := range []struct {
		location syncJournalLocation
		before   []byte
	}{{locationA, beforeA}, {locationB, beforeB}} {
		after, err := os.ReadFile(pair.location.JournalPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pair.before, after) {
			t.Fatal("bulk migration rewrote a journal before in-use preflight failed")
		}
	}
}

func TestSyncJournalMigrateAllSharesInternalRepairRawLockNamespace(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	location, before := writeLegacySyncJournalV1(t, store, plan, nil)
	lifecycleStore := syncjournalpkg.Store{Root: store.Root, ProfileScope: store.ProfileScope, AccountID: store.AccountID}
	lifecycleLocation, err := lifecycleStore.Location(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(lifecycleLocation.LockPath) != filepath.Clean(location.LockPath) {
		t.Fatalf("CLI migration and internal repair raw lock namespaces diverged: cli=%q internal=%q", location.LockPath, lifecycleLocation.LockPath)
	}
	lock, err := transfer.AcquireSessionLock(lifecycleLocation.LockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	result, err := store.MigrateAll()
	if !errors.Is(err, errSyncJournalBulkMigrationPreflight) || result.Migrated != 0 {
		t.Fatalf("bulk migration did not fail closed on internal repair lock: result=%#v err=%v", result, err)
	}
	after, err := os.ReadFile(location.JournalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("bulk migration rewrote a journal while the internal repair raw lock was held")
	}
}

func TestSyncJournalMigrateAllRollsBackEarlierWritesOnLaterWriteFailure(t *testing.T) {
	store := testSyncJournalStore(t)
	plans := []syncPlan{testSyncJournalPlan(t), testSyncJournalPlan(t)}
	originals := make(map[string][]byte, len(plans))
	locations := make(map[string]syncJournalLocation, len(plans))
	for _, plan := range plans {
		location, before := writeLegacySyncJournalV1(t, store, plan, nil)
		locations[plan.PlanID] = location
		originals[plan.PlanID] = append([]byte(nil), before...)
	}

	failing := store
	calls := 0
	failing.writePrivateFile = func(path string, data []byte) error {
		calls++
		if calls == 2 {
			return errors.New("injected second migration write failure")
		}
		return transfer.WritePrivateFileAtomic(path, data)
	}
	result, err := failing.MigrateAll()
	if err == nil || !strings.Contains(err.Error(), "injected second migration write failure") {
		t.Fatalf("expected bulk migration write failure, got result=%#v err=%v", result, err)
	}
	if result.Failed != 1 || result.RolledBack != 1 || result.RollbackFailed != 0 || result.Migrated != 0 || len(result.Items) != 2 {
		t.Fatalf("bulk rollback result: %#v", result)
	}
	rolledBack := 0
	for _, item := range result.Items {
		if item.RolledBack {
			rolledBack++
			if item.Migrated || item.RollbackError != "" {
				t.Fatalf("rolled-back item contract: %#v", item)
			}
		}
	}
	if rolledBack != 1 {
		t.Fatalf("expected one rolled-back item: %#v", result.Items)
	}
	for _, plan := range plans {
		after, readErr := os.ReadFile(locations[plan.PlanID].JournalPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(originals[plan.PlanID], after) {
			t.Fatalf("bulk rollback did not restore exact legacy bytes for %s", plan.PlanID)
		}
		journal, inspectErr := store.Inspect(plan.PlanID)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		if journal.Version != 1 || !journal.MigrationRequired {
			t.Fatalf("bulk rollback did not restore legacy schema for %s: %#v", plan.PlanID, journal)
		}
	}
}

func TestSyncJournalMigrateAllSurfacesRollbackFailure(t *testing.T) {
	store := testSyncJournalStore(t)
	plans := []syncPlan{testSyncJournalPlan(t), testSyncJournalPlan(t)}
	originalByPath := make(map[string][]byte, len(plans))
	for _, plan := range plans {
		location, before := writeLegacySyncJournalV1(t, store, plan, nil)
		originalByPath[location.JournalPath] = append([]byte(nil), before...)
	}

	failing := store
	calls := 0
	firstWrittenPath := ""
	failing.writePrivateFile = func(path string, data []byte) error {
		calls++
		switch calls {
		case 1:
			firstWrittenPath = path
			return transfer.WritePrivateFileAtomic(path, data)
		case 2:
			return errors.New("injected second migration write failure")
		case 3:
			return errors.New("injected rollback write failure")
		default:
			return transfer.WritePrivateFileAtomic(path, data)
		}
	}
	result, err := failing.MigrateAll()
	if err == nil || !strings.Contains(err.Error(), "injected second migration write failure") || !strings.Contains(err.Error(), "injected rollback write failure") {
		t.Fatalf("rollback failure was not joined into bulk migration error: result=%#v err=%v", result, err)
	}
	if result.Failed != 1 || result.RolledBack != 0 || result.RollbackFailed != 1 || result.Migrated != 1 || len(result.Items) != 2 {
		t.Fatalf("bulk rollback-failure result: %#v", result)
	}
	if firstWrittenPath == "" {
		t.Fatal("bulk migration never persisted its first candidate")
	}
	firstBytes, readErr := os.ReadFile(firstWrittenPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if bytes.Equal(firstBytes, originalByPath[firstWrittenPath]) {
		t.Fatal("failed rollback unexpectedly restored the first migrated journal")
	}
	var firstJournal syncExecutionJournal
	if err := json.Unmarshal(firstBytes, &firstJournal); err != nil {
		t.Fatal(err)
	}
	if firstJournal.Version != syncJournalVersion {
		t.Fatalf("failed rollback did not leave the first journal at current schema: v%d", firstJournal.Version)
	}
	rollbackMarked := false
	for _, item := range result.Items {
		if item.RollbackError != "" {
			rollbackMarked = true
			if !item.Migrated || item.RolledBack || !strings.Contains(item.RollbackError, "injected rollback write failure") {
				t.Fatalf("rollback failure item contract: %#v", item)
			}
		}
	}
	if !rollbackMarked {
		t.Fatalf("rollback failure item was not marked: %#v", result.Items)
	}
	for path, before := range originalByPath {
		if path == firstWrittenPath {
			continue
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("failed second candidate changed despite atomic write failure: %s", path)
		}
	}
}

func TestValidateSyncJournalMigrateArgsSupportsSingleAllOrBatchRecovery(t *testing.T) {
	oldAll := syncJournalMigrateAll
	oldRecover := syncJournalMigrateRecoverBatch
	defer func() {
		syncJournalMigrateAll = oldAll
		syncJournalMigrateRecoverBatch = oldRecover
	}()
	syncJournalMigrateAll = false
	syncJournalMigrateRecoverBatch = false
	if err := validateSyncJournalMigrateArgs(syncJournalMigrateCmd, []string{"abcdef12"}); err != nil {
		t.Fatalf("single journal migrate args rejected: %v", err)
	}
	if err := validateSyncJournalMigrateArgs(syncJournalMigrateCmd, nil); err == nil {
		t.Fatal("migrate without plan ID or mode flag was accepted")
	}
	syncJournalMigrateAll = true
	if err := validateSyncJournalMigrateArgs(syncJournalMigrateCmd, nil); err != nil {
		t.Fatalf("migrate --all args rejected: %v", err)
	}
	if err := validateSyncJournalMigrateArgs(syncJournalMigrateCmd, []string{"abcdef12"}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("migrate --all accepted a plan ID: %v", err)
	}
	syncJournalMigrateAll = false
	syncJournalMigrateRecoverBatch = true
	if err := validateSyncJournalMigrateArgs(syncJournalMigrateCmd, nil); err != nil {
		t.Fatalf("migrate --recover-batch args rejected: %v", err)
	}
	if err := validateSyncJournalMigrateArgs(syncJournalMigrateCmd, []string{"abcdef12"}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("migrate --recover-batch accepted a plan ID: %v", err)
	}
	syncJournalMigrateAll = true
	if err := validateSyncJournalMigrateArgs(syncJournalMigrateCmd, nil); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("migrate accepted --all with --recover-batch: %v", err)
	}
}

func TestSyncJournalMigrateCommandSkipsAuthenticationAndDocumentsSafety(t *testing.T) {
	if !commandSkipsAuthentication(syncJournalMigrateCmd) {
		t.Fatal("sync journal migrate unexpectedly requires remote authentication")
	}
	if !strings.Contains(syncJournalMigrateCmd.Long, "never executes sync data actions") || !strings.Contains(syncJournalMigrateCmd.Long, "source-byte backup") || !strings.Contains(syncJournalMigrateCmd.Long, "persistent batch marker") || !strings.Contains(syncJournalMigrateCmd.Long, "--recover-batch") || !strings.Contains(syncJournalMigrateCmd.Long, "Unknown bytes fail closed") {
		t.Fatalf("migration help does not document safety contract: %q", syncJournalMigrateCmd.Long)
	}
}
