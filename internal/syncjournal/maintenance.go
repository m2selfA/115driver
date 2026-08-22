package syncjournal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

var (
	ErrCleanupCandidateChanged    = errors.New("sync journal cleanup candidate changed")
	ErrCleanupMigrationInProgress = errors.New("sync journal cleanup blocked by bulk migration")
	ErrCleanupGuardRequired       = errors.New("sync journal cleanup guard is required")
	ErrTrashRecoveryRemoval       = errors.New("sync journal recovery state requires explicit forced removal")
)

// CleanupGuard serializes sync-journal cleanup with CLI GC and bulk migration.
// Reviewed cleanup/restore take gc -> migration-batch before any raw journal or
// review-alias locks. Bulk migration itself owns migration-batch -> sorted raw
// journal locks, while repair owns sorted raw journal -> sorted alias locks; all
// process locks are fail-fast, so contention must leave the losing operation
// mutation-free rather than relying on wait-based deadlock avoidance.
type CleanupGuard struct {
	root      string
	gcLock    *transfer.SessionLock
	batchLock *transfer.SessionLock
}

func (guard *CleanupGuard) Close() error {
	if guard == nil {
		return nil
	}
	batchErr := guard.batchLock.Close()
	gcErr := guard.gcLock.Close()
	guard.batchLock = nil
	guard.gcLock = nil
	return errors.Join(batchErr, gcErr)
}

// AcquireCleanupGuard prevents concurrent CLI GC and bulk migration while a
// reviewed MCP cleanup is re-planned and applied. An existing migration marker
// fails closed even after the migration lock itself is acquired.
func (store Store) AcquireCleanupGuard() (*CleanupGuard, error) {
	root, err := store.RootPath()
	if err != nil {
		return nil, err
	}
	gcLock, err := transfer.AcquireSessionLock(filepath.Join(root, "gc.lock"), "")
	if err != nil {
		return nil, fmt.Errorf("lock sync journal GC: %w", err)
	}
	migrationDir := filepath.Join(root, "migration")
	batchLock, err := transfer.AcquireSessionLock(
		filepath.Join(migrationDir, "migrate-all.lock"),
		filepath.Join(migrationDir, "migrate-all-lease.json"),
	)
	if err != nil {
		_ = gcLock.Close()
		return nil, fmt.Errorf("lock sync journal bulk migration: %w", err)
	}
	guard := &CleanupGuard{root: root, gcLock: gcLock, batchLock: batchLock}
	markerPresent, err := store.MigrationBatchMarkerPresent()
	if err != nil {
		_ = guard.Close()
		return nil, err
	}
	if markerPresent {
		_ = guard.Close()
		return nil, ErrCleanupMigrationInProgress
	}
	return guard, nil
}

func (store Store) validateCleanupGuard(guard *CleanupGuard) error {
	if guard == nil || guard.gcLock == nil || guard.batchLock == nil {
		return ErrCleanupGuardRequired
	}
	root, err := store.RootPath()
	if err != nil {
		return err
	}
	if filepath.Clean(root) != filepath.Clean(guard.root) {
		return ErrCleanupGuardRequired
	}
	return nil
}

// MoveDirectoryToSessionTrash atomically moves one locked sync-journal
// directory into the common Session Store trash namespace and stamps the
// destination with the move time. Session Store trash GC uses directory mtime,
// so stamping is part of the soft-delete contract rather than cosmetic
// metadata. A stamp failure attempts to roll the rename back.
func MoveDirectoryToSessionTrash(root, sourceDir, planID string, now time.Time) (string, error) {
	planID, err := NormalizePlanID(planID)
	if err != nil {
		return "", err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	rootAbs, err := filepath.Abs(strings.TrimSpace(root))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(root) == "" {
		return "", errors.New("sync journal root is empty")
	}
	sourceAbs, err := filepath.Abs(sourceDir)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(rootAbs, sourceAbs)
	if err != nil {
		return "", err
	}
	if relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("sync journal source is outside the session store root")
	}
	firstPart := relative
	if index := strings.IndexRune(relative, os.PathSeparator); index >= 0 {
		firstPart = relative[:index]
	}
	if strings.EqualFold(firstPart, "trash") {
		return "", errors.New("sync journal source is already inside session trash")
	}
	info, err := os.Lstat(sourceAbs)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("sync journal source is not a real directory")
	}
	trashRoot := filepath.Join(rootAbs, "trash")
	if err := os.MkdirAll(trashRoot, 0o700); err != nil {
		return "", err
	}
	target := filepath.Join(trashRoot, fmt.Sprintf("sync-journal-%s-%s", now.Format("20060102T150405.000000000Z"), planID))
	if err := os.Rename(sourceAbs, target); err != nil {
		return "", fmt.Errorf("move sync journal to trash: %w", err)
	}
	if err := os.Chtimes(target, now, now); err != nil {
		rollbackErr := os.Rename(target, sourceAbs)
		if rollbackErr != nil {
			return "", errors.Join(fmt.Errorf("stamp trashed sync journal: %w", err), fmt.Errorf("restore sync journal after trash stamp failure: %w", rollbackErr))
		}
		return "", fmt.Errorf("stamp trashed sync journal: %w", err)
	}
	return target, nil
}

// TrashCurrentReviewed moves one exact current-v2 journal into the shared
// Session Store trash after revalidating the reviewed cleanup snapshot under
// the journal lock. If a matching MCP review alias exists it is removed under
// its own lock so a collected journal cannot leave a stale execution binding.
// The caller must hold AcquireCleanupGuard for the entire cleanup transaction.
func (store Store) TrashCurrentReviewed(guard *CleanupGuard, reviewID, planID, expectedState string, expectedUpdatedAt time.Time, olderThan time.Duration, now time.Time) (string, error) {
	if err := store.validateCleanupGuard(guard); err != nil {
		return "", err
	}
	reviewID, err := NormalizeReviewID(reviewID)
	if err != nil {
		return "", err
	}
	planID, err = NormalizePlanID(planID)
	if err != nil {
		return "", err
	}
	expectedState = strings.ToLower(strings.TrimSpace(expectedState))
	olderThan = ResolveGCRetention(olderThan, store.Retention)
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	if expectedUpdatedAt.IsZero() {
		return "", fmt.Errorf("%w: expected update time is empty", ErrCleanupCandidateChanged)
	}

	location, err := store.Location(planID)
	if err != nil {
		return "", err
	}
	journalLock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		return "", err
	}
	defer journalLock.Close()

	journal, err := store.ReadCurrent(location)
	if err != nil {
		return "", err
	}
	entry := BuildListEntry(journal, now, false)
	if journal.PlanID != planID || entry.State != expectedState || !journal.UpdatedAt.Equal(expectedUpdatedAt) {
		return "", ErrCleanupCandidateChanged
	}
	if len(SelectGCCandidates([]ListEntry{entry}, now, olderThan, nil)) != 1 {
		return "", ErrCleanupCandidateChanged
	}
	if markerPresent, markerErr := store.MigrationBatchMarkerPresent(); markerErr != nil {
		return "", markerErr
	} else if markerPresent {
		return "", ErrCleanupMigrationInProgress
	}

	existingReviewIDs, err := store.reviewIDsForPlan(planID)
	if err != nil {
		return "", err
	}
	if mapped, mapErr := store.ResolveReviewAlias(reviewID); mapErr == nil {
		if mapped != planID {
			return "", fmt.Errorf("%w: reviewed plan maps to a different sync journal", ErrReviewAliasConflict)
		}
	} else if !errors.Is(mapErr, ErrNotFound) {
		return "", mapErr
	}
	reviewIDs := append([]string(nil), existingReviewIDs...)
	foundRequested := false
	for _, existing := range reviewIDs {
		if existing == reviewID {
			foundRequested = true
			break
		}
	}
	if !foundRequested {
		// Preserve the reviewed identity that authorized this cleanup even when
		// the journal predates persistent aliases. Restoring the soft-deleted
		// journal can then re-establish the exact MCP identity that collected it.
		reviewIDs = append(reviewIDs, reviewID)
	}
	aliasSet, err := store.lockReviewAliasSet(reviewIDs, planID, true)
	if err != nil {
		return "", err
	}
	defer aliasSet.Close()
	for _, existing := range existingReviewIDs {
		if _, ok := aliasSet.existing[existing]; !ok {
			return "", ErrCleanupCandidateChanged
		}
	}

	return store.trashLockedCurrent(location, journalLock, planID, now, reviewIDs, aliasSet)
}

// TrashCurrent moves one raw current-v2 journal into shared Session Store trash
// while preserving every current reviewed-plan alias in the private trash
// sidecar. Unlike reviewed MCP cleanup this method does not require the GC/
// migration guard; callers such as CLI rm/gc retain their existing outer
// maintenance policy. The raw journal lock remains authoritative for content
// state, and force is required for recovery/reconcile-gated journals.
func (store Store) TrashCurrent(planID string, force bool, now time.Time) (string, error) {
	planID, err := NormalizePlanID(planID)
	if err != nil {
		return "", err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	location, err := store.Location(planID)
	if err != nil {
		return "", err
	}
	journalLock, err := transfer.AcquireSessionLock(location.LockPath, location.LeasePath)
	if err != nil {
		return "", err
	}
	defer journalLock.Close()
	journal, err := store.ReadCurrent(location)
	if err != nil {
		return "", err
	}
	if journal.PlanID != planID {
		return "", fmt.Errorf("%w: current journal identity changed", ErrInvalidSchema)
	}
	if !force && (RecoveryRequired(journal) || DestructiveReconciliationRequired(journal)) {
		return "", ErrTrashRecoveryRemoval
	}
	aliasStore, err := store.aliasLifecycleStoreForJournal(journal, false)
	if err != nil {
		return "", err
	}
	reviewIDs := []string(nil)
	// Review aliases can only be created for a known account. If both the
	// offline admin Store and the journal lack one, there is no legitimate alias
	// set to discover; importantly, we also avoid scanning unrelated aliases in
	// the same profile with an unknown account identity.
	if aliasStore.AccountID > 0 {
		reviewIDs, err = aliasStore.reviewIDsForPlan(planID)
		if err != nil {
			return "", err
		}
	}
	aliasSet, err := aliasStore.lockReviewAliasSet(reviewIDs, planID, false)
	if err != nil {
		return "", err
	}
	defer aliasSet.Close()
	return aliasStore.trashLockedCurrent(location, journalLock, planID, now, reviewIDs, aliasSet)
}

func (store Store) trashLockedCurrent(location Location, journalLock *transfer.SessionLock, planID string, now time.Time, reviewIDs []string, aliasSet *lockedReviewAliasSet) (string, error) {
	if journalLock == nil {
		return "", errors.New("sync journal lock is nil")
	}
	if err := journalLock.StopLease(); err != nil {
		return "", err
	}
	target, err := MoveDirectoryToSessionTrash(store.Root, location.Dir, planID, now)
	if err != nil {
		return "", err
	}
	if len(reviewIDs) > 0 {
		if err := WriteTrashReviewAliases(target, reviewIDs); err != nil {
			rollbackErr := os.Rename(target, location.Dir)
			if rollbackErr != nil {
				return "", errors.Join(fmt.Errorf("persist trashed sync journal review aliases: %w", err), fmt.Errorf("restore sync journal after sidecar failure: %w", rollbackErr))
			}
			return "", fmt.Errorf("persist trashed sync journal review aliases: %w", err)
		}
	}
	if err := store.removeLockedReviewAliases(aliasSet); err != nil {
		rollbackMoveErr := os.Rename(target, location.Dir)
		rollbackAliasErr := store.rollbackLockedReviewAliases(aliasSet)
		var sidecarErr error
		if rollbackMoveErr == nil && len(reviewIDs) > 0 {
			if removeErr := os.Remove(filepath.Join(location.Dir, trashReviewAliasesFile)); removeErr != nil && !os.IsNotExist(removeErr) {
				sidecarErr = fmt.Errorf("remove rolled-back trash review sidecar: %w", removeErr)
			}
		}
		if rollbackMoveErr != nil {
			rollbackMoveErr = fmt.Errorf("restore sync journal after alias removal failure: %w", rollbackMoveErr)
		}
		return "", errors.Join(err, rollbackMoveErr, rollbackAliasErr, sidecarErr)
	}
	if len(reviewIDs) > 0 {
		// Sidecar creation changes the directory mtime on some filesystems.
		// Re-stamp after all private trash metadata is durable so Session Store
		// trash GC measures retention from the soft-delete transaction.
		if err := os.Chtimes(target, now, now); err != nil {
			rollbackMoveErr := os.Rename(target, location.Dir)
			rollbackAliasErr := store.rollbackLockedReviewAliases(aliasSet)
			var sidecarErr error
			if rollbackMoveErr == nil {
				if removeErr := os.Remove(filepath.Join(location.Dir, trashReviewAliasesFile)); removeErr != nil && !os.IsNotExist(removeErr) {
					sidecarErr = fmt.Errorf("remove rolled-back trash review sidecar: %w", removeErr)
				}
			}
			return "", errors.Join(fmt.Errorf("stamp trashed sync journal after sidecar write: %w", err), rollbackMoveErr, rollbackAliasErr, sidecarErr)
		}
	}
	return target, nil
}
