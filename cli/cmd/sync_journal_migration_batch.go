package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

const (
	syncJournalMigrationBatchSchema             = "115driver.sync-journal-migration-batch"
	syncJournalMigrationBatchMinReadableVersion = 1
	syncJournalMigrationBatchVersion            = 2
	syncJournalMigrationBatchRecoverySchema     = "115driver.sync-journal-migration-batch-recovery/v1"

	syncJournalMigrationBatchPrepared         = "prepared"
	syncJournalMigrationBatchMigrating        = "migrating"
	syncJournalMigrationBatchRollingBack      = "rolling-back"
	syncJournalMigrationBatchRecoveryRequired = "recovery-required"
	syncJournalMigrationBatchCompleted        = "completed"

	syncJournalMigrationCandidatePending        = "pending"
	syncJournalMigrationCandidateMigrated       = "migrated"
	syncJournalMigrationCandidateWriteFailed    = "write-failed"
	syncJournalMigrationCandidateRolledBack     = "rolled-back"
	syncJournalMigrationCandidateRollbackFailed = "rollback-failed"

	syncJournalMigrationActualOriginal = "original"
	syncJournalMigrationActualMigrated = "migrated"
	syncJournalMigrationActualUnknown  = "unknown"
)

var (
	errSyncJournalMigrationBatchNotFound         = errors.New("sync journal migration batch marker not found")
	errSyncJournalMigrationBatchExists           = errors.New("sync journal migration batch marker already exists")
	errSyncJournalMigrationBatchRecoveryRequired = errors.New("sync journal migration batch requires recovery")
)

type syncJournalMigrationBatchCandidate struct {
	PlanID          string `json:"plan_id"`
	FromVersion     int    `json:"from_version"`
	ToVersion       int    `json:"to_version"`
	SourceSHA256    string `json:"source_sha256"`
	TargetSHA256    string `json:"target_sha256"`
	BackupToVersion int    `json:"backup_to_version"`
	State           string `json:"state"`
	Error           string `json:"error,omitempty"`
}

type syncJournalMigrationBatchMarker struct {
	Schema       string                               `json:"schema"`
	Version      int                                  `json:"version"`
	BatchID      string                               `json:"batch_id"`
	ProfileScope string                               `json:"profile_scope"`
	State        string                               `json:"state"`
	StartedAt    time.Time                            `json:"started_at"`
	UpdatedAt    time.Time                            `json:"updated_at"`
	Candidates   []syncJournalMigrationBatchCandidate `json:"candidates"`
}

type syncJournalMigrationBatchLocation struct {
	Dir        string
	MarkerPath string
	LockPath   string
	LeasePath  string
}

type syncJournalMigrationBatchCandidateDiagnostic struct {
	PlanID       string `json:"plan_id"`
	Actual       string `json:"actual"`
	BackupStatus string `json:"backup_status,omitempty"`
	Error        string `json:"error,omitempty"`
}

type syncJournalMigrationBatchDiagnostic struct {
	Exists       bool                                           `json:"exists"`
	BatchID      string                                         `json:"batch_id,omitempty"`
	State        string                                         `json:"state,omitempty"`
	InUse        bool                                           `json:"in_use,omitempty"`
	Interrupted  bool                                           `json:"interrupted,omitempty"`
	Candidates   int                                            `json:"candidates,omitempty"`
	Original     int                                            `json:"original,omitempty"`
	Migrated     int                                            `json:"migrated,omitempty"`
	Unknown      int                                            `json:"unknown,omitempty"`
	BackupIssues int                                            `json:"backup_issues,omitempty"`
	Error        string                                         `json:"error,omitempty"`
	Items        []syncJournalMigrationBatchCandidateDiagnostic `json:"items,omitempty"`
}

type syncJournalMigrationBatchRecoveryResult struct {
	Schema        string `json:"schema"`
	BatchID       string `json:"batch_id"`
	Candidates    int    `json:"candidates"`
	Original      int    `json:"original"`
	Migrated      int    `json:"migrated"`
	Restored      int    `json:"restored"`
	Finalized     bool   `json:"finalized"`
	MarkerRemoved bool   `json:"marker_removed"`
}

func (store syncJournalStore) migrationBatchLocation() (syncJournalMigrationBatchLocation, error) {
	root, err := store.root()
	if err != nil {
		return syncJournalMigrationBatchLocation{}, err
	}
	dir := filepath.Join(root, "migration")
	return syncJournalMigrationBatchLocation{
		Dir: dir, MarkerPath: filepath.Join(dir, "migrate-all.json"),
		LockPath: filepath.Join(dir, "migrate-all.lock"), LeasePath: filepath.Join(dir, "migrate-all-lease.json"),
	}, nil
}

func validateSyncJournalMigrationBatchSHA(value string) bool {
	decoded, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(value)))
	return err == nil && len(decoded) == sha256.Size
}

func syncJournalMigrationBatchIDForVersion(version int, startedAt time.Time, candidates []syncJournalMigrationBatchCandidate) (string, error) {
	if version < syncJournalMigrationBatchMinReadableVersion || version > syncJournalMigrationBatchVersion {
		return "", fmt.Errorf("unsupported sync journal migration batch marker version %d", version)
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(startedAt.UTC().Format(time.RFC3339Nano)))
	for _, candidate := range candidates {
		_, _ = hash.Write([]byte(candidate.PlanID))
		_, _ = hash.Write([]byte(candidate.SourceSHA256))
		_, _ = hash.Write([]byte(candidate.TargetSHA256))
		if version >= 2 {
			_, _ = fmt.Fprintf(hash, "\x00%d\x00%d\x00%d\x00", candidate.FromVersion, candidate.ToVersion, candidate.BackupToVersion)
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncJournalMigrationBatchID(startedAt time.Time, candidates []syncJournalMigrationBatchCandidate) string {
	batchID, _ := syncJournalMigrationBatchIDForVersion(syncJournalMigrationBatchVersion, startedAt, candidates)
	return batchID
}

func validateSyncJournalMigrationBatchMarker(marker syncJournalMigrationBatchMarker, store syncJournalStore) error {
	if marker.Schema != syncJournalMigrationBatchSchema || marker.Version < syncJournalMigrationBatchMinReadableVersion || marker.Version > syncJournalMigrationBatchVersion {
		return fmt.Errorf("invalid sync journal migration batch schema/version")
	}
	if marker.ProfileScope != store.ProfileScope {
		return fmt.Errorf("sync journal migration batch belongs to a different profile scope")
	}
	if marker.BatchID == "" || marker.StartedAt.IsZero() || marker.UpdatedAt.IsZero() || len(marker.Candidates) == 0 {
		return fmt.Errorf("invalid sync journal migration batch envelope")
	}
	switch marker.State {
	case syncJournalMigrationBatchPrepared, syncJournalMigrationBatchMigrating, syncJournalMigrationBatchRollingBack, syncJournalMigrationBatchRecoveryRequired, syncJournalMigrationBatchCompleted:
	default:
		return fmt.Errorf("invalid sync journal migration batch state %q", marker.State)
	}
	seen := make(map[string]struct{}, len(marker.Candidates))
	for index, candidate := range marker.Candidates {
		planID, err := normalizeSyncPlanID(candidate.PlanID)
		if err != nil || planID == "" || planID != candidate.PlanID {
			return fmt.Errorf("migration batch candidate %d has invalid plan ID", index)
		}
		if _, ok := seen[planID]; ok {
			return fmt.Errorf("migration batch contains duplicate plan ID %s", planID)
		}
		seen[planID] = struct{}{}
		if candidate.FromVersion < syncJournalMinReadableVersion || candidate.ToVersion <= candidate.FromVersion || candidate.BackupToVersion != candidate.FromVersion+1 {
			return fmt.Errorf("migration batch candidate %s has invalid version range", planID)
		}
		if !validateSyncJournalMigrationBatchSHA(candidate.SourceSHA256) || !validateSyncJournalMigrationBatchSHA(candidate.TargetSHA256) {
			return fmt.Errorf("migration batch candidate %s has invalid source/target SHA-256", planID)
		}
		if strings.EqualFold(candidate.SourceSHA256, candidate.TargetSHA256) {
			return fmt.Errorf("migration batch candidate %s has identical source/target SHA-256", planID)
		}
		switch candidate.State {
		case syncJournalMigrationCandidatePending, syncJournalMigrationCandidateMigrated, syncJournalMigrationCandidateWriteFailed, syncJournalMigrationCandidateRolledBack, syncJournalMigrationCandidateRollbackFailed:
		default:
			return fmt.Errorf("migration batch candidate %s has invalid state %q", planID, candidate.State)
		}
	}
	expectedBatchID, err := syncJournalMigrationBatchIDForVersion(marker.Version, marker.StartedAt, marker.Candidates)
	if err != nil {
		return err
	}
	if marker.BatchID != expectedBatchID {
		return fmt.Errorf("invalid sync journal migration batch ID: expected %s got %s", expectedBatchID, marker.BatchID)
	}
	return nil
}

func newSyncJournalMigrationBatchMarker(store syncJournalStore, prepared []syncJournalPreparedMigration) (syncJournalMigrationBatchMarker, error) {
	if len(prepared) == 0 {
		return syncJournalMigrationBatchMarker{}, errors.New("cannot create an empty sync journal migration batch")
	}
	now := time.Now().UTC()
	marker := syncJournalMigrationBatchMarker{
		Schema: syncJournalMigrationBatchSchema, Version: syncJournalMigrationBatchVersion,
		ProfileScope: store.ProfileScope, State: syncJournalMigrationBatchPrepared, StartedAt: now, UpdatedAt: now,
		Candidates: make([]syncJournalMigrationBatchCandidate, 0, len(prepared)),
	}
	for _, migration := range prepared {
		if !migration.Result.Migrated || len(migration.Trace) == 0 || len(migration.Upgraded) == 0 {
			return syncJournalMigrationBatchMarker{}, fmt.Errorf("migration candidate %s is not fully prepared", migration.Result.PlanID)
		}
		first := migration.Trace[0]
		sourceDigest := sha256.Sum256(migration.Original)
		targetDigest := sha256.Sum256(migration.Upgraded)
		candidate := syncJournalMigrationBatchCandidate{
			PlanID: migration.Result.PlanID, FromVersion: migration.Result.FromVersion, ToVersion: migration.Result.ToVersion,
			SourceSHA256: hex.EncodeToString(sourceDigest[:]), TargetSHA256: hex.EncodeToString(targetDigest[:]),
			BackupToVersion: first.Record.ToVersion, State: syncJournalMigrationCandidatePending,
		}
		if !strings.EqualFold(candidate.SourceSHA256, first.Record.SourceSHA256) {
			return syncJournalMigrationBatchMarker{}, fmt.Errorf("migration candidate %s trace does not match original source SHA-256", migration.Result.PlanID)
		}
		marker.Candidates = append(marker.Candidates, candidate)
	}
	marker.BatchID = syncJournalMigrationBatchID(marker.StartedAt, marker.Candidates)
	return marker, validateSyncJournalMigrationBatchMarker(marker, store)
}

func (store syncJournalStore) writeMigrationBatchMarker(location syncJournalMigrationBatchLocation, marker syncJournalMigrationBatchMarker) error {
	if err := validateSyncJournalMigrationBatchMarker(marker, store); err != nil {
		return err
	}
	marker.UpdatedAt = time.Now().UTC()
	encoded, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return err
	}
	return transfer.WritePrivateFileAtomic(location.MarkerPath, encoded)
}

func (store syncJournalStore) readMigrationBatchMarker(location syncJournalMigrationBatchLocation) (syncJournalMigrationBatchMarker, error) {
	data, err := os.ReadFile(location.MarkerPath)
	if err != nil {
		if os.IsNotExist(err) {
			return syncJournalMigrationBatchMarker{}, errSyncJournalMigrationBatchNotFound
		}
		return syncJournalMigrationBatchMarker{}, err
	}
	var marker syncJournalMigrationBatchMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return syncJournalMigrationBatchMarker{}, fmt.Errorf("decode sync journal migration batch marker: %w", err)
	}
	if err := validateSyncJournalMigrationBatchMarker(marker, store); err != nil {
		return syncJournalMigrationBatchMarker{}, err
	}
	return marker, nil
}

func (store syncJournalStore) createMigrationBatchMarker(location syncJournalMigrationBatchLocation, marker syncJournalMigrationBatchMarker) error {
	if _, err := os.Lstat(location.MarkerPath); err == nil {
		return errSyncJournalMigrationBatchExists
	} else if !os.IsNotExist(err) {
		return err
	}
	return store.writeMigrationBatchMarker(location, marker)
}

func (store syncJournalStore) migrationBatchProtectedPlanIDs() (map[string]struct{}, error) {
	location, err := store.migrationBatchLocation()
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(location.MarkerPath); os.IsNotExist(err) {
		return map[string]struct{}{}, nil
	} else if err != nil {
		return nil, fmt.Errorf("inspect sync journal migration batch marker: %w", err)
	}
	marker, err := store.readMigrationBatchMarker(location)
	if err != nil {
		return nil, fmt.Errorf("%w: existing migration batch marker cannot be validated: %v", errSyncJournalMigrationBatchExists, err)
	}
	protected := make(map[string]struct{}, len(marker.Candidates))
	for _, candidate := range marker.Candidates {
		protected[candidate.PlanID] = struct{}{}
	}
	return protected, nil
}

func (store syncJournalStore) ensureJournalMutationAllowed(planID string) error {
	protected, err := store.migrationBatchProtectedPlanIDs()
	if err != nil {
		return err
	}
	if _, ok := protected[planID]; !ok {
		return nil
	}
	return fmt.Errorf("%w: journal %s is protected by an unresolved bulk migration; run '115driver sync journal migrate --recover-batch' first", errSyncJournalMigrationBatchExists, planID)
}

func syncJournalMigrationCandidateBackupRecord(candidate syncJournalMigrationBatchCandidate) syncJournalMigrationRecord {
	return syncJournalMigrationRecord{
		FromVersion: candidate.FromVersion, ToVersion: candidate.BackupToVersion,
		SourceSHA256: candidate.SourceSHA256, BackupRequired: true,
	}
}

func syncJournalMigrationBytesActual(candidate syncJournalMigrationBatchCandidate, data []byte) string {
	digest := sha256.Sum256(data)
	actual := hex.EncodeToString(digest[:])
	switch {
	case strings.EqualFold(actual, candidate.SourceSHA256):
		return syncJournalMigrationActualOriginal
	case strings.EqualFold(actual, candidate.TargetSHA256):
		return syncJournalMigrationActualMigrated
	default:
		return syncJournalMigrationActualUnknown
	}
}

func (store syncJournalStore) readMigrationBatchCandidateActual(candidate syncJournalMigrationBatchCandidate) (string, syncJournalLocation, error) {
	location, err := store.location(candidate.PlanID)
	if err != nil {
		return syncJournalMigrationActualUnknown, syncJournalLocation{}, err
	}
	data, err := os.ReadFile(location.JournalPath)
	if err != nil {
		return syncJournalMigrationActualUnknown, location, err
	}
	return syncJournalMigrationBytesActual(candidate, data), location, nil
}

func (store syncJournalStore) requireMigrationBatchCandidatesActual(candidates []syncJournalMigrationBatchCandidate, expected string) error {
	for _, candidate := range candidates {
		actual, _, err := store.readMigrationBatchCandidateActual(candidate)
		if err != nil {
			return fmt.Errorf("%w: revalidate journal %s: %v", errSyncJournalMigrationBatchRecoveryRequired, candidate.PlanID, err)
		}
		if actual != expected {
			return fmt.Errorf("%w: journal %s changed during migration batch reconciliation: expected exact %s bytes, found %s", errSyncJournalMigrationBatchRecoveryRequired, candidate.PlanID, expected, actual)
		}
	}
	return nil
}

func (store syncJournalStore) removeMigrationBatchMarkerIfExact(location syncJournalMigrationBatchLocation, marker syncJournalMigrationBatchMarker, expected string) error {
	current, err := store.readMigrationBatchMarker(location)
	if err != nil {
		return fmt.Errorf("%w: revalidate migration batch marker: %v", errSyncJournalMigrationBatchRecoveryRequired, err)
	}
	if current.BatchID != marker.BatchID {
		return fmt.Errorf("%w: migration batch marker changed during reconciliation", errSyncJournalMigrationBatchRecoveryRequired)
	}
	if err := store.requireMigrationBatchCandidatesActual(current.Candidates, expected); err != nil {
		return err
	}
	if err := os.Remove(location.MarkerPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove reconciled migration batch marker: %w", err)
	}
	return nil
}

func (store syncJournalStore) diagnoseMigrationBatchCandidate(candidate syncJournalMigrationBatchCandidate) syncJournalMigrationBatchCandidateDiagnostic {
	diagnostic := syncJournalMigrationBatchCandidateDiagnostic{PlanID: candidate.PlanID, Actual: syncJournalMigrationActualUnknown}
	actual, location, err := store.readMigrationBatchCandidateActual(candidate)
	if err != nil {
		diagnostic.Error = err.Error()
		return diagnostic
	}
	diagnostic.Actual = actual
	if diagnostic.Actual == syncJournalMigrationActualMigrated {
		record := syncJournalMigrationCandidateBackupRecord(candidate)
		if _, backupErr := readSyncJournalMigrationBackup(location, record); backupErr != nil {
			diagnostic.BackupStatus = syncJournalBackupInvalid
			if os.IsNotExist(backupErr) {
				diagnostic.BackupStatus = syncJournalBackupMissing
			}
			diagnostic.Error = backupErr.Error()
		} else {
			diagnostic.BackupStatus = syncJournalBackupOK
		}
	}
	return diagnostic
}

func (store syncJournalStore) DiagnoseMigrationBatch() (syncJournalMigrationBatchDiagnostic, error) {
	location, err := store.migrationBatchLocation()
	if err != nil {
		return syncJournalMigrationBatchDiagnostic{}, err
	}
	marker, err := store.readMigrationBatchMarker(location)
	if errors.Is(err, errSyncJournalMigrationBatchNotFound) {
		return syncJournalMigrationBatchDiagnostic{}, nil
	}
	if err != nil {
		return syncJournalMigrationBatchDiagnostic{Exists: true, Error: err.Error(), Interrupted: true}, nil
	}
	diagnostic := syncJournalMigrationBatchDiagnostic{
		Exists: true, BatchID: marker.BatchID, State: marker.State, Candidates: len(marker.Candidates),
		Items: make([]syncJournalMigrationBatchCandidateDiagnostic, 0, len(marker.Candidates)),
	}
	diagnostic.InUse, _ = transfer.SessionLockInUse(location.LockPath)
	diagnostic.Interrupted = !diagnostic.InUse
	for _, candidate := range marker.Candidates {
		item := store.diagnoseMigrationBatchCandidate(candidate)
		switch item.Actual {
		case syncJournalMigrationActualOriginal:
			diagnostic.Original++
		case syncJournalMigrationActualMigrated:
			diagnostic.Migrated++
		default:
			diagnostic.Unknown++
		}
		if item.BackupStatus == syncJournalBackupMissing || item.BackupStatus == syncJournalBackupInvalid {
			diagnostic.BackupIssues++
		}
		diagnostic.Items = append(diagnostic.Items, item)
	}
	return diagnostic, nil
}

func (store syncJournalStore) acquireMigrationBatchLock() (*transfer.SessionLock, syncJournalMigrationBatchLocation, error) {
	location, err := store.migrationBatchLocation()
	if err != nil {
		return nil, syncJournalMigrationBatchLocation{}, err
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		return nil, location, err
	}
	return lock, location, nil
}

func updateSyncJournalMigrationBatchCandidate(marker *syncJournalMigrationBatchMarker, planID, state, message string) error {
	for index := range marker.Candidates {
		if marker.Candidates[index].PlanID != planID {
			continue
		}
		marker.Candidates[index].State = state
		marker.Candidates[index].Error = message
		return nil
	}
	return fmt.Errorf("migration batch does not contain plan %s", planID)
}

func (store syncJournalStore) RecoverMigrationBatch() (syncJournalMigrationBatchRecoveryResult, error) {
	result := syncJournalMigrationBatchRecoveryResult{Schema: syncJournalMigrationBatchRecoverySchema}
	lock, batchLocation, err := store.acquireMigrationBatchLock()
	if err != nil {
		return result, err
	}
	defer lock.Close()
	marker, err := store.readMigrationBatchMarker(batchLocation)
	if err != nil {
		return result, err
	}
	result.BatchID = marker.BatchID
	result.Candidates = len(marker.Candidates)

	candidates := append([]syncJournalMigrationBatchCandidate(nil), marker.Candidates...)
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].PlanID < candidates[j].PlanID })
	type lockedCandidate struct {
		candidate syncJournalMigrationBatchCandidate
		location  syncJournalLocation
		lock      *transfer.SessionLock
		actual    string
		backup    []byte
	}
	locked := make([]lockedCandidate, 0, len(candidates))
	defer func() {
		for index := len(locked) - 1; index >= 0; index-- {
			_ = locked[index].lock.Close()
		}
	}()
	for _, candidate := range candidates {
		location, locationErr := store.location(candidate.PlanID)
		if locationErr != nil {
			return result, locationErr
		}
		journalLock, lockErr := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
		if lockErr != nil {
			return result, lockErr
		}
		item := lockedCandidate{candidate: candidate, location: location, lock: journalLock}
		locked = append(locked, item)
	}

	for index := range locked {
		actual, _, readErr := store.readMigrationBatchCandidateActual(locked[index].candidate)
		if readErr != nil {
			return result, readErr
		}
		locked[index].actual = actual
		switch locked[index].actual {
		case syncJournalMigrationActualOriginal:
			result.Original++
		case syncJournalMigrationActualMigrated:
			result.Migrated++
		default:
			return result, fmt.Errorf("%w: journal %s no longer matches the exact pre-migration or migrated bytes", errSyncJournalMigrationBatchRecoveryRequired, locked[index].candidate.PlanID)
		}
	}

	if result.Migrated == 0 || result.Original == 0 {
		result.Finalized = result.Migrated == result.Candidates
		expected := syncJournalMigrationActualOriginal
		if result.Finalized {
			expected = syncJournalMigrationActualMigrated
		}
		if err := store.removeMigrationBatchMarkerIfExact(batchLocation, marker, expected); err != nil {
			return result, err
		}
		result.MarkerRemoved = true
		return result, nil
	}

	for index := range locked {
		if locked[index].actual != syncJournalMigrationActualMigrated {
			continue
		}
		record := syncJournalMigrationCandidateBackupRecord(locked[index].candidate)
		backup, backupErr := readSyncJournalMigrationBackup(locked[index].location, record)
		if backupErr != nil {
			return result, fmt.Errorf("%w: migration backup for %s: %v", errSyncJournalMigrationBatchRecoveryRequired, locked[index].candidate.PlanID, backupErr)
		}
		locked[index].backup = backup
	}

	marker.State = syncJournalMigrationBatchRollingBack
	if err := store.writeMigrationBatchMarker(batchLocation, marker); err != nil {
		return result, err
	}
	for index := len(locked) - 1; index >= 0; index-- {
		if locked[index].actual != syncJournalMigrationActualMigrated {
			continue
		}
		actual, _, revalidateErr := store.readMigrationBatchCandidateActual(locked[index].candidate)
		if revalidateErr != nil {
			return result, fmt.Errorf("%w: revalidate journal %s before rollback: %v", errSyncJournalMigrationBatchRecoveryRequired, locked[index].candidate.PlanID, revalidateErr)
		}
		if actual == syncJournalMigrationActualOriginal {
			continue
		}
		if actual != syncJournalMigrationActualMigrated {
			return result, fmt.Errorf("%w: journal %s changed before rollback: found %s bytes", errSyncJournalMigrationBatchRecoveryRequired, locked[index].candidate.PlanID, actual)
		}
		if err := store.writePrivate(locked[index].location.JournalPath, locked[index].backup); err != nil {
			marker.State = syncJournalMigrationBatchRecoveryRequired
			_ = updateSyncJournalMigrationBatchCandidate(&marker, locked[index].candidate.PlanID, syncJournalMigrationCandidateRollbackFailed, err.Error())
			_ = store.writeMigrationBatchMarker(batchLocation, marker)
			return result, fmt.Errorf("restore interrupted migration journal %s: %w", locked[index].candidate.PlanID, err)
		}
		result.Restored++
		_ = updateSyncJournalMigrationBatchCandidate(&marker, locked[index].candidate.PlanID, syncJournalMigrationCandidateRolledBack, "")
		if err := store.writeMigrationBatchMarker(batchLocation, marker); err != nil {
			return result, err
		}
	}
	if err := store.removeMigrationBatchMarkerIfExact(batchLocation, marker, syncJournalMigrationActualOriginal); err != nil {
		return result, err
	}
	result.MarkerRemoved = true
	return result, nil
}
