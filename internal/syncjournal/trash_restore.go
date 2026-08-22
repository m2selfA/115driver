package syncjournal

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

const syncJournalTrashPrefix = "sync-journal-"

var (
	ErrTrashScanLimit       = errors.New("sync journal trash scan limit exceeded")
	ErrTrashEntryChanged    = errors.New("sync journal trash entry changed")
	ErrRestoreCurrentExists = errors.New("sync journal current entry already exists")
)

type TrashedCurrentRecord struct {
	Journal   Journal
	TrashName string
	TrashedAt time.Time
	ReviewIDs []string
}

type TrashedCurrentScan struct {
	Records           []TrashedCurrentRecord
	MigrationRequired int
	Invalid           int
}

// HasTrashedCurrentPlan reports whether the shared Session Store trash contains
// a current-v2 sync journal for this exact raw plan and profile/account binding.
// It intentionally ignores the optional review-alias sidecar: orphan-alias
// repair must still fail closed in the crash window after the journal rename but
// before that sidecar is written. Malformed matching journal evidence is an
// error, never proof of absence.
func (store Store) HasTrashedCurrentPlan(planID string, maxJournals int) (bool, error) {
	planID, err := NormalizePlanID(planID)
	if err != nil {
		return false, err
	}
	if maxJournals <= 0 {
		return false, errors.New("sync journal trash scan limit must be > 0")
	}
	root, err := store.trashRoot()
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	seen := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidatePlanID, ok := parseSyncJournalTrashPlanID(entry.Name())
		if !ok {
			continue
		}
		seen++
		if seen > maxJournals {
			return false, fmt.Errorf("%w: maximum %d journal directories", ErrTrashScanLimit, maxJournals)
		}
		if candidatePlanID != planID {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				continue
			}
			return false, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, fmt.Errorf("%w: matching trashed sync journal is not a real directory", ErrInvalidSchema)
		}
		data, readErr := os.ReadFile(filepath.Join(path, "journal.json"))
		if readErr != nil {
			if os.IsNotExist(readErr) {
				return false, fmt.Errorf("%w: matching trashed sync journal is missing journal.json", ErrInvalidSchema)
			}
			return false, readErr
		}
		journal, decodeErr := DecodeCurrent(data)
		if decodeErr != nil {
			return false, fmt.Errorf("inspect matching trashed sync journal: %w", decodeErr)
		}
		if journal.PlanID != planID {
			return false, fmt.Errorf("%w: matching trash name does not match journal plan ID", ErrInvalidSchema)
		}
		if bindingErr := store.ValidateBinding(journal); bindingErr != nil {
			if errors.Is(bindingErr, ErrBindingMismatch) {
				// The shared trash namespace spans profiles/accounts; a same-plan
				// entry belonging elsewhere must not block this profile's repair.
				continue
			}
			return false, bindingErr
		}
		return true, nil
	}
	return false, nil
}

func parseSyncJournalTrashPlanID(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, syncJournalTrashPrefix) || len(name) <= len(syncJournalTrashPrefix)+64 {
		return "", false
	}
	raw := name[len(name)-64:]
	if name[len(name)-65] != '-' {
		return "", false
	}
	planID, err := NormalizePlanID(raw)
	if err != nil || planID != raw {
		return "", false
	}
	return planID, true
}

func (store Store) trashRoot() (string, error) {
	root := strings.TrimSpace(store.Root)
	if root == "" {
		return "", errors.New("sync journal root is empty")
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(absolute, "trash"), nil
}

func (store Store) readTrashedCurrentName(name string) (TrashedCurrentRecord, error) {
	planID, ok := parseSyncJournalTrashPlanID(name)
	if !ok {
		return TrashedCurrentRecord{}, ErrNotFound
	}
	trashRoot, err := store.trashRoot()
	if err != nil {
		return TrashedCurrentRecord{}, err
	}
	path := filepath.Join(trashRoot, name)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return TrashedCurrentRecord{}, ErrNotFound
		}
		return TrashedCurrentRecord{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return TrashedCurrentRecord{}, fmt.Errorf("%w: trashed sync journal is not a real directory", ErrInvalidSchema)
	}
	data, err := os.ReadFile(filepath.Join(path, "journal.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return TrashedCurrentRecord{}, ErrNotFound
		}
		return TrashedCurrentRecord{}, err
	}
	journal, err := DecodeCurrent(data)
	if err != nil {
		return TrashedCurrentRecord{}, err
	}
	if err := store.ValidateBinding(journal); err != nil {
		return TrashedCurrentRecord{}, err
	}
	if journal.PlanID != planID {
		return TrashedCurrentRecord{}, fmt.Errorf("%w: trash name does not match journal plan ID", ErrInvalidSchema)
	}
	reviewIDs, _, err := ReadTrashReviewAliases(path)
	if err != nil {
		return TrashedCurrentRecord{}, err
	}
	return TrashedCurrentRecord{
		Journal: journal, TrashName: name, TrashedAt: info.ModTime().UTC(),
		ReviewIDs: append([]string(nil), reviewIDs...),
	}, nil
}

// ScanTrashedCurrent returns bounded current-schema sync journals from the
// common Session Store trash namespace for this profile/account. Foreign
// account entries are silently omitted rather than exposed through aggregate
// counts; legacy readable entries are counted for migration awareness.
func (store Store) ScanTrashedCurrent(maxJournals int) (TrashedCurrentScan, error) {
	if maxJournals <= 0 {
		return TrashedCurrentScan{}, errors.New("sync journal trash scan limit must be > 0")
	}
	root, err := store.trashRoot()
	if err != nil {
		return TrashedCurrentScan{}, err
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return TrashedCurrentScan{Records: []TrashedCurrentRecord{}}, nil
	}
	if err != nil {
		return TrashedCurrentScan{}, err
	}
	scan := TrashedCurrentScan{Records: make([]TrashedCurrentRecord, 0)}
	seen := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, ok := parseSyncJournalTrashPlanID(entry.Name()); !ok {
			continue
		}
		seen++
		if seen > maxJournals {
			return TrashedCurrentScan{}, fmt.Errorf("%w: maximum %d journal directories", ErrTrashScanLimit, maxJournals)
		}
		record, readErr := store.readTrashedCurrentName(entry.Name())
		if readErr == nil {
			scan.Records = append(scan.Records, record)
			continue
		}
		switch {
		case errors.Is(readErr, ErrMigrationRequired):
			scan.MigrationRequired++
		case errors.Is(readErr, ErrBindingMismatch):
			// Do not reveal trash belonging to another authenticated account.
		default:
			scan.Invalid++
		}
	}
	sort.Slice(scan.Records, func(i, j int) bool {
		if scan.Records[i].TrashedAt.Equal(scan.Records[j].TrashedAt) {
			return scan.Records[i].TrashName < scan.Records[j].TrashName
		}
		return scan.Records[i].TrashedAt.After(scan.Records[j].TrashedAt)
	})
	return scan, nil
}

// RestoreTrashedCurrent restores one exact raw sync-journal trash record to
// its canonical current-v2 location without creating any MCP review alias. The
// caller must hold AcquireCleanupGuard so Session Store trash GC and CLI bulk
// migration cannot race the restore.
func (store Store) RestoreTrashedCurrent(guard *CleanupGuard, trashName, expectedPlanID string, expectedUpdatedAt, expectedTrashedAt time.Time, expectedReviewIDs []string) (Journal, error) {
	return store.restoreTrashedCurrent(guard, "", trashName, expectedPlanID, expectedUpdatedAt, expectedTrashedAt, expectedReviewIDs)
}

// RestoreTrashedCurrentReviewed performs the same raw restore and additionally
// recreates/verifies the MCP review alias before releasing the raw journal lock.
func (store Store) RestoreTrashedCurrentReviewed(guard *CleanupGuard, reviewID, trashName, expectedPlanID string, expectedUpdatedAt, expectedTrashedAt time.Time, expectedReviewIDs []string) (Journal, error) {
	reviewID, err := NormalizeReviewID(reviewID)
	if err != nil {
		return Journal{}, err
	}
	return store.restoreTrashedCurrent(guard, reviewID, trashName, expectedPlanID, expectedUpdatedAt, expectedTrashedAt, expectedReviewIDs)
}

func optionalCanonicalReviewIDs(reviewIDs []string) ([]string, error) {
	if len(reviewIDs) == 0 {
		return nil, nil
	}
	return canonicalTrashReviewAliases(reviewIDs)
}

func equalReviewIDSets(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsReviewID(reviewIDs []string, expected string) bool {
	for _, reviewID := range reviewIDs {
		if reviewID == expected {
			return true
		}
	}
	return false
}

func (store Store) restoreTrashedCurrent(guard *CleanupGuard, reviewID, trashName, expectedPlanID string, expectedUpdatedAt, expectedTrashedAt time.Time, expectedReviewIDs []string) (Journal, error) {
	if err := store.validateCleanupGuard(guard); err != nil {
		return Journal{}, err
	}
	expectedPlanID, err := NormalizePlanID(expectedPlanID)
	if err != nil {
		return Journal{}, err
	}
	if expectedUpdatedAt.IsZero() || expectedTrashedAt.IsZero() {
		return Journal{}, ErrTrashEntryChanged
	}
	expectedReviewIDs, err = optionalCanonicalReviewIDs(expectedReviewIDs)
	if err != nil {
		return Journal{}, err
	}
	location, err := store.Location(expectedPlanID)
	if err != nil {
		return Journal{}, err
	}
	// Lock by raw plan ID without a lease so proving current absence cannot
	// create the very current directory that must remain absent for restore.
	journalLock, err := transfer.AcquireSessionLock(location.LockPath, "")
	if err != nil {
		return Journal{}, err
	}
	defer journalLock.Close()
	if _, err := os.Lstat(location.Dir); err == nil {
		return Journal{}, ErrRestoreCurrentExists
	} else if !os.IsNotExist(err) {
		return Journal{}, err
	}
	record, err := store.readTrashedCurrentName(trashName)
	if err != nil {
		return Journal{}, err
	}
	if record.Journal.PlanID != expectedPlanID || !record.Journal.UpdatedAt.Equal(expectedUpdatedAt) || !record.TrashedAt.Equal(expectedTrashedAt) || !equalReviewIDSets(record.ReviewIDs, expectedReviewIDs) {
		return Journal{}, ErrTrashEntryChanged
	}
	aliasIDs := append([]string(nil), record.ReviewIDs...)
	if reviewID != "" {
		if len(aliasIDs) > 0 && !containsReviewID(aliasIDs, reviewID) {
			return Journal{}, ErrTrashEntryChanged
		}
		if len(aliasIDs) == 0 {
			// Backward-compatible restore for trash created before the private
			// multi-alias sidecar existed.
			aliasIDs = []string{reviewID}
		}
	}
	aliasStore, err := store.aliasLifecycleStoreForJournal(record.Journal, len(aliasIDs) > 0)
	if err != nil {
		return Journal{}, err
	}
	aliasSet, err := aliasStore.lockReviewAliasSet(aliasIDs, expectedPlanID, true)
	if err != nil {
		return Journal{}, err
	}
	defer aliasSet.Close()
	if marker, markerErr := store.MigrationBatchMarkerPresent(); markerErr != nil {
		return Journal{}, markerErr
	} else if marker {
		return Journal{}, ErrCleanupMigrationInProgress
	}
	trashRoot, err := store.trashRoot()
	if err != nil {
		return Journal{}, err
	}
	source := filepath.Join(trashRoot, record.TrashName)
	if err := os.MkdirAll(filepath.Dir(location.Dir), 0o700); err != nil {
		return Journal{}, err
	}
	if err := os.Rename(source, location.Dir); err != nil {
		return Journal{}, fmt.Errorf("restore sync journal from trash: %w", err)
	}
	rollback := func(cause error) (Journal, error) {
		aliasRollbackErr := aliasStore.rollbackLockedReviewAliases(aliasSet)
		moveRollbackErr := os.Rename(location.Dir, source)
		if moveRollbackErr != nil {
			moveRollbackErr = fmt.Errorf("return sync journal to trash after restore failure: %w", moveRollbackErr)
		}
		return Journal{}, errors.Join(cause, aliasRollbackErr, moveRollbackErr)
	}
	if err := aliasStore.restoreLockedReviewAliases(aliasSet, expectedPlanID); err != nil {
		return rollback(fmt.Errorf("restore sync journal review aliases: %w", err))
	}
	journal, err := store.ReadCurrent(location)
	if err != nil {
		return rollback(fmt.Errorf("verify restored sync journal: %w", err))
	}
	if len(record.ReviewIDs) > 0 {
		if err := os.Remove(filepath.Join(location.Dir, trashReviewAliasesFile)); err != nil && !os.IsNotExist(err) {
			return rollback(fmt.Errorf("remove restored trash review sidecar: %w", err))
		}
	}
	return journal, nil
}
