package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/spf13/cobra"
)

type syncJournalMigrationResult struct {
	PlanID      string `json:"plan_id"`
	FromVersion int    `json:"from_version"`
	ToVersion   int    `json:"to_version"`
	Migrated    bool   `json:"migrated"`
	Status      string `json:"status"`
}

type syncJournalMigrationBatchItem struct {
	PlanID        string `json:"plan_id"`
	FromVersion   int    `json:"from_version"`
	ToVersion     int    `json:"to_version"`
	Migrated      bool   `json:"migrated"`
	RolledBack    bool   `json:"rolled_back,omitempty"`
	RollbackError string `json:"rollback_error,omitempty"`
	Status        string `json:"status,omitempty"`
	Error         string `json:"error,omitempty"`
}

type syncJournalMigrationBatchResult struct {
	Total          int                             `json:"total"`
	Candidates     int                             `json:"candidates"`
	AlreadyCurrent int                             `json:"already_current"`
	Migrated       int                             `json:"migrated"`
	RolledBack     int                             `json:"rolled_back"`
	RollbackFailed int                             `json:"rollback_failed"`
	Failed         int                             `json:"failed"`
	Preflight      syncJournalDoctorReport         `json:"preflight"`
	Postflight     *syncJournalDoctorReport        `json:"postflight,omitempty"`
	Items          []syncJournalMigrationBatchItem `json:"items"`
}

var errSyncJournalBulkMigrationPreflight = errors.New("bulk sync journal migration preflight failed")

type syncJournalMigrationStepFunc func([]byte, syncExecutionJournal, time.Time) ([]byte, syncExecutionJournal, error)

type syncJournalMigrationTraceEntry struct {
	Record       syncJournalMigrationRecord
	Source       []byte
	TargetSHA256 string
}

var syncJournalMigrationSteps = map[int]syncJournalMigrationStepFunc{
	1: migrateSyncJournalV1ToV2,
}

func validateSyncJournalVersion(version int) error {
	if version > syncJournalVersion {
		return fmt.Errorf("%w: version %d is newer than supported version %d; upgrade 115driver before opening this journal", errSyncJournalNewerVersion, version, syncJournalVersion)
	}
	if version < syncJournalMinReadableVersion {
		return fmt.Errorf("%w: version %d (supported readable range is %d..%d)", errSyncJournalUnsupportedVersion, version, syncJournalMinReadableVersion, syncJournalVersion)
	}
	return nil
}

func validateSyncJournalSchemaEnvelope(data []byte, version int) error {
	if version < 2 {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("%w: decode schema envelope: %v", errSyncJournalInvalidSchema, err)
	}
	schemaRaw, ok := raw["schema"]
	if !ok {
		return fmt.Errorf("%w: schema v%d is missing schema identity", errSyncJournalInvalidSchema, version)
	}
	var schema string
	if err := json.Unmarshal(schemaRaw, &schema); err != nil || schema != syncJournalSchemaID {
		return fmt.Errorf("%w: schema v%d identity must be %q", errSyncJournalInvalidSchema, version, syncJournalSchemaID)
	}
	statusRaw, ok := raw["status"]
	if !ok {
		return fmt.Errorf("%w: schema v%d is missing required field %q", errSyncJournalInvalidSchema, version, "status")
	}
	var status string
	if err := json.Unmarshal(statusRaw, &status); err != nil || status == "" {
		return fmt.Errorf("%w: schema v%d field %q must be a non-empty string", errSyncJournalInvalidSchema, version, "status")
	}
	runStatsRaw, ok := raw["run_stats"]
	if !ok {
		return fmt.Errorf("%w: schema v%d is missing required field %q", errSyncJournalInvalidSchema, version, "run_stats")
	}
	var runStats map[string]json.RawMessage
	if err := json.Unmarshal(runStatsRaw, &runStats); err != nil || runStats == nil {
		return fmt.Errorf("%w: schema v%d field %q must be an object", errSyncJournalInvalidSchema, version, "run_stats")
	}
	for _, key := range []string{"runs", "resume_runs", "interrupted_runs", "last_duration_ms", "total_duration_ms"} {
		if _, ok := runStats[key]; !ok {
			return fmt.Errorf("%w: schema v%d field %q is missing %q", errSyncJournalInvalidSchema, version, "run_stats", key)
		}
	}
	return nil
}

func validateSyncJournalMigrationHistory(journal syncExecutionJournal) error {
	previousTo := 0
	for index, record := range journal.Migrations {
		if record.FromVersion < syncJournalMinReadableVersion || record.ToVersion != record.FromVersion+1 || record.ToVersion > journal.Version {
			return fmt.Errorf("sync journal schema migration %d has invalid version edge %d -> %d", index, record.FromVersion, record.ToVersion)
		}
		if previousTo != 0 && record.FromVersion != previousTo {
			return fmt.Errorf("sync journal schema migration %d is not contiguous", index)
		}
		if record.MigratedAt.IsZero() {
			return fmt.Errorf("sync journal schema migration %d is missing migrated_at", index)
		}
		digest, err := hex.DecodeString(record.SourceSHA256)
		if err != nil || len(digest) != sha256.Size {
			return fmt.Errorf("sync journal schema migration %d has invalid source_sha256", index)
		}
		previousTo = record.ToVersion
	}
	if len(journal.Migrations) > 0 && previousTo != journal.Version {
		return fmt.Errorf("sync journal schema migration history ends at version %d but journal is version %d", previousTo, journal.Version)
	}
	return nil
}

func decodeSyncJournalData(data []byte) (syncExecutionJournal, error) {
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return syncExecutionJournal{}, fmt.Errorf("decode sync execution journal header: %w", err)
	}
	if err := validateSyncJournalVersion(header.Version); err != nil {
		return syncExecutionJournal{}, err
	}
	if err := validateSyncJournalSchemaEnvelope(data, header.Version); err != nil {
		return syncExecutionJournal{}, err
	}
	var journal syncExecutionJournal
	if err := json.Unmarshal(data, &journal); err != nil {
		return syncExecutionJournal{}, fmt.Errorf("decode sync execution journal: %w", err)
	}
	storedStatus := journal.Status
	storedMigrationRequired := journal.MigrationRequired
	if err := restoreSyncJournalPlan(&journal); err != nil {
		return syncExecutionJournal{}, err
	}
	if header.Version >= 2 && storedStatus != journal.Status {
		return syncExecutionJournal{}, fmt.Errorf("%w: persisted status %q does not match derived status %q", errSyncJournalInvalidSchema, storedStatus, journal.Status)
	}
	if header.Version == syncJournalVersion && storedMigrationRequired {
		return syncExecutionJournal{}, fmt.Errorf("%w: current schema v%d cannot be marked migration_required", errSyncJournalInvalidSchema, header.Version)
	}
	if err := validateSyncJournalRunStats(journal.RunStats); err != nil {
		return syncExecutionJournal{}, err
	}
	if err := validateSyncJournalMigrationHistory(journal); err != nil {
		return syncExecutionJournal{}, fmt.Errorf("%w: %v", errSyncJournalInvalidSchema, err)
	}
	return journal, nil
}

func migrateSyncJournalData(data []byte, now time.Time) ([]byte, syncExecutionJournal, bool, error) {
	upgraded, journal, migrated, _, err := migrateSyncJournalDataWithTrace(data, now)
	return upgraded, journal, migrated, err
}

func migrateSyncJournalDataWithTrace(data []byte, now time.Time) ([]byte, syncExecutionJournal, bool, []syncJournalMigrationTraceEntry, error) {
	journal, err := decodeSyncJournalData(data)
	if err != nil {
		return nil, syncExecutionJournal{}, false, nil, err
	}
	if journal.Version == syncJournalVersion {
		return data, journal, false, nil, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}

	upgraded := data
	trace := make([]syncJournalMigrationTraceEntry, 0, syncJournalVersion-journal.Version)
	for journal.Version < syncJournalVersion {
		fromVersion := journal.Version
		step := syncJournalMigrationSteps[fromVersion]
		if step == nil {
			return nil, syncExecutionJournal{}, false, nil, fmt.Errorf("%w: no migration step from version %d toward %d", errSyncJournalUnsupportedVersion, fromVersion, syncJournalVersion)
		}
		source := append([]byte(nil), upgraded...)
		migrationCount := len(journal.Migrations)
		upgraded, journal, err = step(upgraded, journal, now)
		if err != nil {
			return nil, syncExecutionJournal{}, false, nil, err
		}
		if journal.Version != fromVersion+1 {
			return nil, syncExecutionJournal{}, false, nil, fmt.Errorf("%w: migration step from version %d advanced to %d instead of %d", errSyncJournalUnsupportedVersion, fromVersion, journal.Version, fromVersion+1)
		}
		if len(journal.Migrations) != migrationCount+1 {
			return nil, syncExecutionJournal{}, false, nil, fmt.Errorf("%w: migration step v%d -> v%d did not append exactly one audit record", errSyncJournalInvalidSchema, fromVersion, journal.Version)
		}
		targetDigest := sha256.Sum256(upgraded)
		trace = append(trace, syncJournalMigrationTraceEntry{
			Record: journal.Migrations[len(journal.Migrations)-1], Source: source,
			TargetSHA256: hex.EncodeToString(targetDigest[:]),
		})
	}
	return upgraded, journal, len(trace) > 0, trace, nil
}

func migrateSyncJournalV1ToV2(data []byte, journal syncExecutionJournal, now time.Time) ([]byte, syncExecutionJournal, error) {
	return migrateSyncJournalStep(data, journal, 2, now, func(raw map[string]json.RawMessage, current syncExecutionJournal) error {
		if _, ok := raw["run_stats"]; !ok {
			return putSyncJournalRaw(raw, "run_stats", current.RunStats)
		}
		return nil
	})
}

func migrateSyncJournalStep(data []byte, journal syncExecutionJournal, toVersion int, now time.Time, migrateFields func(map[string]json.RawMessage, syncExecutionJournal) error) ([]byte, syncExecutionJournal, error) {
	if toVersion != journal.Version+1 || toVersion > syncJournalVersion {
		return nil, syncExecutionJournal{}, fmt.Errorf("%w: invalid migration step %d -> %d", errSyncJournalUnsupportedVersion, journal.Version, toVersion)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, syncExecutionJournal{}, fmt.Errorf("decode sync execution journal for migration: %w", err)
	}
	if migrateFields != nil {
		if err := migrateFields(raw, journal); err != nil {
			return nil, syncExecutionJournal{}, err
		}
	}
	sourceDigest := sha256.Sum256(data)
	record := syncJournalMigrationRecord{
		FromVersion: journal.Version, ToVersion: toVersion, MigratedAt: now,
		SourceSHA256: hex.EncodeToString(sourceDigest[:]), BackupRequired: true,
	}
	journal.Version = toVersion
	if journal.Version >= 2 {
		journal.Schema = syncJournalSchemaID
	}
	journal.MigrationRequired = journal.Version < syncJournalVersion
	journal.Status = syncJournalEffectiveStatus(journal)
	journal.Migrations = append(journal.Migrations, record)
	if err := putSyncJournalRaw(raw, "version", journal.Version); err != nil {
		return nil, syncExecutionJournal{}, err
	}
	if journal.Version >= 2 {
		if err := putSyncJournalRaw(raw, "schema", journal.Schema); err != nil {
			return nil, syncExecutionJournal{}, err
		}
	}
	if journal.MigrationRequired {
		if err := putSyncJournalRaw(raw, "migration_required", true); err != nil {
			return nil, syncExecutionJournal{}, err
		}
	} else {
		delete(raw, "migration_required")
	}
	if err := putSyncJournalRaw(raw, "status", journal.Status); err != nil {
		return nil, syncExecutionJournal{}, err
	}
	if err := putSyncJournalRaw(raw, "schema_migrations", journal.Migrations); err != nil {
		return nil, syncExecutionJournal{}, err
	}
	upgraded, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, syncExecutionJournal{}, fmt.Errorf("encode migrated sync execution journal: %w", err)
	}
	migrated, err := decodeSyncJournalData(upgraded)
	if err != nil {
		return nil, syncExecutionJournal{}, fmt.Errorf("validate migrated sync execution journal: %w", err)
	}
	return upgraded, migrated, nil
}

func putSyncJournalRaw(raw map[string]json.RawMessage, key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	raw[key] = encoded
	return nil
}

func (store syncJournalStore) writePrivate(path string, data []byte) error {
	if store.writePrivateFile != nil {
		return store.writePrivateFile(path, data)
	}
	return transfer.WritePrivateFileAtomic(path, data)
}

func (store syncJournalStore) validateJournalBinding(journal syncExecutionJournal) error {
	if journal.ProfileScope != store.ProfileScope {
		return errors.New("sync execution journal belongs to a different profile scope")
	}
	if store.AccountID != 0 && journal.AccountID != 0 && store.AccountID != journal.AccountID {
		return fmt.Errorf("sync execution journal belongs to account %d, current account is %d", journal.AccountID, store.AccountID)
	}
	return nil
}

type syncJournalPreparedMigration struct {
	Location syncJournalLocation
	Before   syncExecutionJournal
	After    syncExecutionJournal
	Result   syncJournalMigrationResult
	Original []byte
	Upgraded []byte
	Trace    []syncJournalMigrationTraceEntry
}

func (store syncJournalStore) prepareMigrationLocked(location syncJournalLocation) (syncJournalPreparedMigration, error) {
	prepared := syncJournalPreparedMigration{Location: location}
	data, err := os.ReadFile(location.JournalPath)
	if err != nil {
		if os.IsNotExist(err) {
			return prepared, errSyncJournalNotFound
		}
		return prepared, err
	}
	before, err := decodeSyncJournalData(data)
	if err != nil {
		return prepared, err
	}
	if err := store.validateJournalBinding(before); err != nil {
		return prepared, err
	}
	expectedPlanID := filepath.Base(location.Dir)
	if before.PlanID != expectedPlanID {
		return prepared, fmt.Errorf("%w: journal plan_id %q does not match storage path %q", errSyncJournalInvalidSchema, before.PlanID, expectedPlanID)
	}
	prepared.Before = before
	prepared.Original = append([]byte(nil), data...)
	prepared.Result = syncJournalMigrationResult{
		PlanID: before.PlanID, FromVersion: before.Version, ToVersion: before.Version,
		Status: syncJournalEffectiveStatus(before),
	}
	upgraded, after, migrated, trace, err := migrateSyncJournalDataWithTrace(data, time.Now().UTC())
	if err != nil {
		return prepared, err
	}
	prepared.After = after
	if !migrated {
		prepared.After = before
		return prepared, nil
	}
	prepared.Upgraded = upgraded
	prepared.Trace = trace
	prepared.Result.ToVersion = after.Version
	prepared.Result.Migrated = true
	prepared.Result.Status = syncJournalEffectiveStatus(after)
	return prepared, nil
}

func (store syncJournalStore) ensurePreparedMigrationBackups(prepared syncJournalPreparedMigration) error {
	for _, entry := range prepared.Trace {
		if !entry.Record.BackupRequired {
			continue
		}
		if err := ensureSyncJournalMigrationBackup(prepared.Location, entry.Record, entry.Source); err != nil {
			return fmt.Errorf("persist sync journal migration backup for v%d -> v%d: %w", entry.Record.FromVersion, entry.Record.ToVersion, err)
		}
	}
	return nil
}

func ensurePreparedMigrationSourceUnchanged(prepared syncJournalPreparedMigration) error {
	current, err := os.ReadFile(prepared.Location.JournalPath)
	if err != nil {
		return fmt.Errorf("revalidate sync journal before migration rewrite: %w", err)
	}
	if !bytes.Equal(current, prepared.Original) {
		return fmt.Errorf("%w: sync journal %s changed after migration preparation; refusing schema rewrite over non-source bytes", errSyncJournalInvalidSchema, prepared.Result.PlanID)
	}
	return nil
}

func preparedMigrationRollbackRequired(prepared syncJournalPreparedMigration) (bool, error) {
	current, err := os.ReadFile(prepared.Location.JournalPath)
	if err != nil {
		return false, fmt.Errorf("revalidate sync journal before migration rollback: %w", err)
	}
	switch {
	case bytes.Equal(current, prepared.Original):
		return false, nil
	case bytes.Equal(current, prepared.Upgraded):
		return true, nil
	default:
		return false, fmt.Errorf("%w: sync journal %s changed after migration rewrite; refusing rollback over unknown bytes", errSyncJournalMigrationBatchRecoveryRequired, prepared.Result.PlanID)
	}
}

func (store syncJournalStore) persistPreparedMigrationJournal(prepared syncJournalPreparedMigration) error {
	if !prepared.Result.Migrated {
		return nil
	}
	if len(prepared.Upgraded) == 0 {
		return fmt.Errorf("%w: prepared migration for %s has no encoded journal", errSyncJournalInvalidSchema, prepared.Result.PlanID)
	}
	if err := ensurePreparedMigrationSourceUnchanged(prepared); err != nil {
		return err
	}
	if err := store.writePrivate(prepared.Location.JournalPath, prepared.Upgraded); err != nil {
		return fmt.Errorf("persist sync journal schema migration: %w", err)
	}
	return nil
}

func (store syncJournalStore) persistPreparedMigration(prepared syncJournalPreparedMigration) error {
	if err := store.ensurePreparedMigrationBackups(prepared); err != nil {
		return err
	}
	return store.persistPreparedMigrationJournal(prepared)
}

func (store syncJournalStore) rollbackPreparedMigration(prepared syncJournalPreparedMigration) error {
	if !prepared.Result.Migrated {
		return nil
	}
	if len(prepared.Original) == 0 {
		return fmt.Errorf("%w: prepared migration rollback for %s has no original journal", errSyncJournalInvalidSchema, prepared.Result.PlanID)
	}
	rollbackRequired, err := preparedMigrationRollbackRequired(prepared)
	if err != nil {
		return err
	}
	if !rollbackRequired {
		return nil
	}
	if err := store.writePrivate(prepared.Location.JournalPath, prepared.Original); err != nil {
		return fmt.Errorf("restore original sync journal after migration failure: %w", err)
	}
	current, err := os.ReadFile(prepared.Location.JournalPath)
	if err != nil {
		return fmt.Errorf("verify original sync journal after migration rollback: %w", err)
	}
	if !bytes.Equal(current, prepared.Original) {
		return fmt.Errorf("%w: sync journal %s did not remain at exact source bytes after rollback", errSyncJournalMigrationBatchRecoveryRequired, prepared.Result.PlanID)
	}
	return nil
}

func (store syncJournalStore) migrateLocationLocked(location syncJournalLocation) (syncExecutionJournal, syncJournalMigrationResult, error) {
	prepared, err := store.prepareMigrationLocked(location)
	if err != nil {
		return syncExecutionJournal{}, syncJournalMigrationResult{}, err
	}
	if err := store.persistPreparedMigration(prepared); err != nil {
		return syncExecutionJournal{}, syncJournalMigrationResult{}, err
	}
	return prepared.After, prepared.Result, nil
}

func (store syncJournalStore) Migrate(prefix string) (syncJournalMigrationResult, error) {
	planID, err := store.resolvePrefix(prefix)
	if err != nil {
		return syncJournalMigrationResult{}, err
	}
	if err := store.ensureJournalMutationAllowed(planID); err != nil {
		return syncJournalMigrationResult{}, err
	}
	location, err := store.location(planID)
	if err != nil {
		return syncJournalMigrationResult{}, err
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		return syncJournalMigrationResult{}, err
	}
	defer lock.Close()
	_, result, err := store.migrateLocationLocked(location)
	return result, err
}

func (store syncJournalStore) MigrateAll() (syncJournalMigrationBatchResult, error) {
	report, err := store.Diagnose()
	result := syncJournalMigrationBatchResult{Preflight: report, Items: make([]syncJournalMigrationBatchItem, 0)}
	if err != nil {
		return result, err
	}
	result.Total = report.Total
	result.Candidates = report.MigrationRequired
	result.AlreadyCurrent = report.Healthy

	blockers := 0
	for _, entry := range report.Entries {
		switch entry.Health {
		case syncJournalDoctorHealthy:
			continue
		case syncJournalDoctorMigrationRequired:
			if entry.InUse {
				blockers++
			}
		default:
			blockers++
		}
	}
	if blockers > 0 {
		return result, fmt.Errorf("%w: %d blocker(s); run '115driver sync journal doctor' for details", errSyncJournalBulkMigrationPreflight, blockers)
	}
	if report.InterruptedMigrationBatch {
		return result, fmt.Errorf("%w: interrupted bulk migration requires '115driver sync journal migrate --recover-batch'", errSyncJournalBulkMigrationPreflight)
	}
	batchLock, batchLocation, batchLockErr := store.acquireMigrationBatchLock()
	if batchLockErr != nil {
		return result, fmt.Errorf("%w: acquire bulk migration lock: %v", errSyncJournalBulkMigrationPreflight, batchLockErr)
	}
	defer batchLock.Close()
	if _, markerErr := store.readMigrationBatchMarker(batchLocation); markerErr == nil {
		return result, fmt.Errorf("%w: %w", errSyncJournalBulkMigrationPreflight, errSyncJournalMigrationBatchExists)
	} else if !errors.Is(markerErr, errSyncJournalMigrationBatchNotFound) {
		return result, fmt.Errorf("%w: inspect bulk migration marker: %v", errSyncJournalBulkMigrationPreflight, markerErr)
	}

	type lockedCandidate struct {
		entry    syncJournalDoctorEntry
		location syncJournalLocation
		lock     *transfer.SessionLock
	}
	locked := make([]lockedCandidate, 0, report.MigrationRequired)
	defer func() {
		for index := len(locked) - 1; index >= 0; index-- {
			_ = locked[index].lock.Close()
		}
	}()
	candidates := make([]syncJournalDoctorEntry, 0, report.MigrationRequired)
	for _, entry := range report.Entries {
		if entry.Health == syncJournalDoctorMigrationRequired {
			candidates = append(candidates, entry)
		}
	}
	// Keep the lock order local to the destructive batch implementation instead
	// of relying on Diagnose retaining its current output ordering forever.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].PlanID == candidates[j].PlanID {
			return candidates[i].JournalPath < candidates[j].JournalPath
		}
		return candidates[i].PlanID < candidates[j].PlanID
	})
	for _, entry := range candidates {
		location, locationErr := store.location(entry.PlanID)
		if locationErr != nil {
			return result, fmt.Errorf("%w: resolve migration candidate %s: %v", errSyncJournalBulkMigrationPreflight, entry.PlanID, locationErr)
		}
		lock, lockErr := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
		if lockErr != nil {
			return result, fmt.Errorf("%w: lock migration candidate %s: %v", errSyncJournalBulkMigrationPreflight, entry.PlanID, lockErr)
		}
		locked = append(locked, lockedCandidate{entry: entry, location: location, lock: lock})
	}

	prepared := make([]syncJournalPreparedMigration, 0, len(locked))
	for _, candidate := range locked {
		migration, prepareErr := store.prepareMigrationLocked(candidate.location)
		if prepareErr != nil {
			return result, fmt.Errorf("%w: prepare migration candidate %s: %v", errSyncJournalBulkMigrationPreflight, candidate.entry.PlanID, prepareErr)
		}
		prepared = append(prepared, migration)
	}
	if len(prepared) == 0 {
		postflight, postErr := store.Diagnose()
		if postErr != nil {
			return result, fmt.Errorf("postflight sync journal diagnosis: %w", postErr)
		}
		result.Postflight = &postflight
		return result, nil
	}
	for _, migration := range prepared {
		if backupErr := store.ensurePreparedMigrationBackups(migration); backupErr != nil {
			return result, fmt.Errorf("%w: prepare migration backup for %s: %v", errSyncJournalBulkMigrationPreflight, migration.Result.PlanID, backupErr)
		}
	}
	marker, markerErr := newSyncJournalMigrationBatchMarker(store, prepared)
	if markerErr != nil {
		return result, fmt.Errorf("%w: create migration batch marker: %v", errSyncJournalBulkMigrationPreflight, markerErr)
	}
	marker.State = syncJournalMigrationBatchMigrating
	if markerErr := store.createMigrationBatchMarker(batchLocation, marker); markerErr != nil {
		return result, fmt.Errorf("%w: persist migration batch marker: %v", errSyncJournalBulkMigrationPreflight, markerErr)
	}

	for migrationIndex, migration := range prepared {
		item := syncJournalMigrationBatchItem{
			PlanID: migration.Result.PlanID, FromVersion: migration.Result.FromVersion, ToVersion: migration.Result.ToVersion,
			Migrated: migration.Result.Migrated, Status: migration.Result.Status,
		}
		journalWritten := false
		persistErr := store.persistPreparedMigrationJournal(migration)
		if persistErr == nil {
			journalWritten = migration.Result.Migrated
			_ = updateSyncJournalMigrationBatchCandidate(&marker, migration.Result.PlanID, syncJournalMigrationCandidateMigrated, "")
			if checkpointErr := store.writeMigrationBatchMarker(batchLocation, marker); checkpointErr != nil {
				persistErr = fmt.Errorf("persist migration batch checkpoint after %s: %w", migration.Result.PlanID, checkpointErr)
			}
		}
		if persistErr != nil {
			item.Migrated = journalWritten
			item.Error = persistErr.Error()
			result.Failed++
			result.Items = append(result.Items, item)
			_ = updateSyncJournalMigrationBatchCandidate(&marker, migration.Result.PlanID, syncJournalMigrationCandidateWriteFailed, persistErr.Error())
			marker.State = syncJournalMigrationBatchRollingBack
			_ = store.writeMigrationBatchMarker(batchLocation, marker)
			rollbackErrors := make([]error, 0)
			rollbackStart := migrationIndex - 1
			if journalWritten {
				rollbackStart = migrationIndex
				result.Migrated++
			}
			for rollbackIndex := rollbackStart; rollbackIndex >= 0; rollbackIndex-- {
				if rollbackIndex >= len(result.Items) || !result.Items[rollbackIndex].Migrated {
					continue
				}
				rollbackErr := store.rollbackPreparedMigration(prepared[rollbackIndex])
				if rollbackErr != nil {
					result.Items[rollbackIndex].RollbackError = rollbackErr.Error()
					result.RollbackFailed++
					_ = updateSyncJournalMigrationBatchCandidate(&marker, prepared[rollbackIndex].Result.PlanID, syncJournalMigrationCandidateRollbackFailed, rollbackErr.Error())
					rollbackErrors = append(rollbackErrors, fmt.Errorf("rollback sync journal %s: %w", prepared[rollbackIndex].Result.PlanID, rollbackErr))
					continue
				}
				result.Items[rollbackIndex].Migrated = false
				result.Items[rollbackIndex].RolledBack = true
				result.RolledBack++
				result.Migrated--
				_ = updateSyncJournalMigrationBatchCandidate(&marker, prepared[rollbackIndex].Result.PlanID, syncJournalMigrationCandidateRolledBack, "")
			}
			if len(rollbackErrors) > 0 {
				marker.State = syncJournalMigrationBatchRecoveryRequired
				_ = store.writeMigrationBatchMarker(batchLocation, marker)
			} else if revalidateErr := store.removeMigrationBatchMarkerIfExact(batchLocation, marker, syncJournalMigrationActualOriginal); revalidateErr != nil {
				rollbackErrors = append(rollbackErrors, revalidateErr)
			}
			migrationErr := fmt.Errorf("migrate sync journal %s: %w", migration.Result.PlanID, persistErr)
			if len(rollbackErrors) > 0 {
				return result, errors.Join(append([]error{migrationErr}, rollbackErrors...)...)
			}
			return result, migrationErr
		}
		if migration.Result.Migrated {
			result.Migrated++
		} else {
			result.AlreadyCurrent++
		}
		result.Items = append(result.Items, item)
	}
	marker.State = syncJournalMigrationBatchCompleted
	if markerErr := store.writeMigrationBatchMarker(batchLocation, marker); markerErr != nil {
		return result, fmt.Errorf("persist completed migration batch marker: %w", markerErr)
	}
	if markerErr := store.removeMigrationBatchMarkerIfExact(batchLocation, marker, syncJournalMigrationActualMigrated); markerErr != nil {
		return result, fmt.Errorf("finalize completed migration batch: %w", markerErr)
	}
	for index := len(locked) - 1; index >= 0; index-- {
		_ = locked[index].lock.Close()
	}
	locked = nil
	postflight, err := store.Diagnose()
	if err != nil {
		return result, fmt.Errorf("postflight sync journal diagnosis: %w", err)
	}
	result.Postflight = &postflight
	if postflight.Issues != 0 {
		return result, fmt.Errorf("postflight sync journal diagnosis found %d issue(s)", postflight.Issues)
	}
	return result, nil
}

func validateSyncJournalMigrateArgs(_ *cobra.Command, args []string) error {
	if syncJournalMigrateAll && syncJournalMigrateRecoverBatch {
		return fmt.Errorf("--all and --recover-batch are mutually exclusive")
	}
	if syncJournalMigrateAll || syncJournalMigrateRecoverBatch {
		if len(args) != 0 {
			return fmt.Errorf("--all/--recover-batch cannot be combined with a plan_id")
		}
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("sync journal migrate requires one plan_id unless --all or --recover-batch is set")
	}
	return nil
}

func printSyncJournalMigrationBatch(result syncJournalMigrationBatchResult) {
	if jsonOutput {
		return
	}
	fmt.Printf("Sync journal migrate --all: total=%d candidates=%d migrated=%d rolled-back=%d rollback-failed=%d current=%d failed=%d\n", result.Total, result.Candidates, result.Migrated, result.RolledBack, result.RollbackFailed, result.AlreadyCurrent, result.Failed)
	for _, item := range result.Items {
		extra := ""
		if item.Error != "" {
			extra = " [" + item.Error + "]"
		}
		if item.RolledBack {
			extra += " [rolled back]"
		}
		if item.RollbackError != "" {
			extra += " [rollback: " + item.RollbackError + "]"
		}
		fmt.Printf("%s  v%d -> v%d  migrated=%t  %s%s\n", item.PlanID[:12], item.FromVersion, item.ToVersion, item.Migrated, item.Status, extra)
	}
}

var (
	syncJournalMigrateAll          bool
	syncJournalMigrateRecoverBatch bool
)

var syncJournalMigrateCmd = &cobra.Command{
	Use:   "migrate [plan_id]",
	Short: "Atomically upgrade legacy sync journal schemas",
	Long:  "Upgrade one legacy sync journal to the current schema under its exclusive journal lock. Migration changes only journal metadata: it never executes sync data actions or mutates either tree. Each new migration step preserves an exact private source-byte backup before the journal rewrite, and each journal rewrite still uses atomic replacement. Journals written by a newer 115driver version are refused rather than downgraded or overwritten. With --all, a read-only doctor preflight must find only healthy or migration-required journals, then all migration candidates are locked and fully prepared in memory before the first rewrite. A persistent batch marker records exact source and target hashes so process crashes are detectable. Synchronous write failures roll back already-written candidates on a best-effort basis. Use --recover-batch to reconcile an interrupted marker: exact-all-target batches are finalized, exact-all-source batches are cleared, and mixed exact states are restored to source bytes from verified backups. Unknown bytes fail closed.",
	Args:  validateSyncJournalMigrateArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := resolveSyncJournalStore()
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if syncJournalMigrateRecoverBatch {
			result, recoverErr := store.RecoverMigrationBatch()
			if recoverErr != nil {
				return syncJournalExitErrorData(recoverErr, result)
			}
			printer.PrintSuccess(result)
			if !jsonOutput {
				fmt.Printf("Sync journal migration batch recovered: %s candidates=%d original=%d migrated=%d restored=%d finalized=%t marker-removed=%t\n", result.BatchID, result.Candidates, result.Original, result.Migrated, result.Restored, result.Finalized, result.MarkerRemoved)
			}
			return nil
		}
		if syncJournalMigrateAll {
			result, err := store.MigrateAll()
			if err != nil {
				code := output.ExitError
				if errors.Is(err, errSyncJournalBulkMigrationPreflight) {
					code = output.ExitArgs
				}
				if !jsonOutput {
					printSyncJournalMigrationBatch(result)
				}
				return &exitError{code: code, msg: err.Error(), data: result}
			}
			printer.PrintSuccess(result)
			printSyncJournalMigrationBatch(result)
			return nil
		}
		result, err := store.Migrate(args[0])
		if err != nil {
			return syncJournalExitError(err)
		}
		printer.PrintSuccess(result)
		if !jsonOutput {
			if result.Migrated {
				fmt.Printf("Sync journal migrated: %s (v%d -> v%d, status=%s)\n", result.PlanID, result.FromVersion, result.ToVersion, result.Status)
			} else {
				fmt.Printf("Sync journal already uses schema v%d: %s\n", result.ToVersion, result.PlanID)
			}
		}
		return nil
	},
}
