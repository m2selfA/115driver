package syncjournal

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

var ErrReviewAliasRepairRollback = errors.New("sync journal review alias repair rollback failed")

type ReviewAliasBatchRemovalResult struct {
	Requested        int
	Removed          int
	RolledBack       bool
	RecoveryRequired bool
}

type lockedRawPlanSet struct {
	locks []*transfer.SessionLock
}

func (set *lockedRawPlanSet) Close() error {
	if set == nil {
		return nil
	}
	var joined error
	for index := len(set.locks) - 1; index >= 0; index-- {
		joined = errors.Join(joined, set.locks[index].Close())
	}
	set.locks = nil
	return joined
}

func (store Store) validateExactRepairAlias(alias ReviewAlias) error {
	if store.AccountID == 0 {
		return store.validateReviewAliasMode(alias, alias.ReviewID, false)
	}
	return store.validateReviewAlias(alias, alias.ReviewID)
}

func (store Store) readExactRepairAlias(reviewID string) (ReviewAlias, error) {
	if store.AccountID == 0 {
		return store.readReviewAliasProfileBound(reviewID)
	}
	return store.readReviewAlias(reviewID)
}

func (store Store) writeExactRepairAliasRecordLocked(alias ReviewAlias) error {
	aliasStore := store
	if aliasStore.AccountID == 0 {
		aliasStore.AccountID = alias.AccountID
	}
	return aliasStore.writeReviewAliasRecordLocked(alias)
}

func canonicalExactRepairAliases(store Store, expected []ReviewAlias) ([]ReviewAlias, error) {
	ordered := append([]ReviewAlias(nil), expected...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ReviewID < ordered[j].ReviewID })
	lastReviewID := ""
	for _, alias := range ordered {
		if err := store.validateExactRepairAlias(alias); err != nil {
			return nil, err
		}
		if alias.ReviewID == lastReviewID {
			return nil, fmt.Errorf("%w: duplicate review alias in exact repair batch", ErrInvalidSchema)
		}
		lastReviewID = alias.ReviewID
	}
	return ordered, nil
}

func (store Store) lockExactRepairRawPlans(expected []ReviewAlias) (*lockedRawPlanSet, error) {
	planSet := make(map[string]struct{}, len(expected))
	for _, alias := range expected {
		planSet[alias.PlanID] = struct{}{}
	}
	planIDs := make([]string, 0, len(planSet))
	for planID := range planSet {
		planIDs = append(planIDs, planID)
	}
	sort.Strings(planIDs)
	set := &lockedRawPlanSet{locks: make([]*transfer.SessionLock, 0, len(planIDs))}
	for _, planID := range planIDs {
		location, err := store.Location(planID)
		if err != nil {
			_ = set.Close()
			return nil, err
		}
		lock, err := transfer.AcquireSessionLock(location.LockPath, "")
		if err != nil {
			_ = set.Close()
			return nil, err
		}
		set.locks = append(set.locks, lock)
	}
	return set, nil
}

func (store Store) lockExactRepairAliases(expected []ReviewAlias) (*lockedReviewAliasSet, error) {
	ids := make([]string, 0, len(expected))
	for _, alias := range expected {
		ids = append(ids, alias.ReviewID)
	}
	canonical, err := canonicalTrashReviewAliases(ids)
	if err != nil {
		return nil, err
	}
	set := &lockedReviewAliasSet{ids: canonical, locks: make([]*transfer.SessionLock, 0, len(canonical)), existing: make(map[string]ReviewAlias, len(canonical))}
	for _, reviewID := range canonical {
		lockPath, err := store.reviewAliasLockPath(reviewID)
		if err != nil {
			_ = set.Close()
			return nil, err
		}
		lock, err := transfer.AcquireSessionLock(lockPath, "")
		if err != nil {
			_ = set.Close()
			if errors.Is(err, transfer.ErrSessionLocked) {
				return nil, ErrReviewAliasInUse
			}
			return nil, fmt.Errorf("lock sync journal review alias: %w", err)
		}
		set.locks = append(set.locks, lock)
	}
	return set, nil
}

func (store Store) preflightExactOrphanRepairBatch(expected []ReviewAlias, aliasLocks *lockedReviewAliasSet) error {
	byReviewID := make(map[string]ReviewAlias, len(expected))
	trashTargets := make(map[string]map[int64]struct{})
	for _, alias := range expected {
		byReviewID[alias.ReviewID] = alias
		accounts := trashTargets[alias.PlanID]
		if accounts == nil {
			accounts = make(map[int64]struct{})
			trashTargets[alias.PlanID] = accounts
		}
		accounts[alias.AccountID] = struct{}{}
	}
	for _, reviewID := range aliasLocks.ids {
		expectedAlias := byReviewID[reviewID]
		currentAlias, err := store.readExactRepairAlias(reviewID)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				return ErrReviewAliasChanged
			}
			return err
		}
		if currentAlias.PlanID != expectedAlias.PlanID {
			return fmt.Errorf("%w: reviewed plan maps to a different sync journal", ErrReviewAliasConflict)
		}
		if !sameReviewAliasSnapshot(currentAlias, expectedAlias) {
			return ErrReviewAliasChanged
		}
		aliasLocks.existing[reviewID] = currentAlias
		location, err := store.Location(expectedAlias.PlanID)
		if err != nil {
			return err
		}
		aliasStore := store
		aliasStore.AccountID = expectedAlias.AccountID
		if _, err := aliasStore.ReadCurrent(location); err == nil {
			return ErrReviewAliasChanged
		} else if !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	trashed, err := store.scanReviewAliasTrashTargets(trashTargets, maxMaintenanceReviewAliases)
	if err != nil {
		return err
	}
	for _, alias := range expected {
		if _, ok := trashed[reviewAliasTrashKey(alias.PlanID, alias.AccountID)]; ok {
			return ErrReviewAliasTrashed
		}
	}
	return nil
}

func countMissingExactRepairAliases(store Store, expected []ReviewAlias) int {
	missing := 0
	for _, alias := range expected {
		if _, err := store.readExactRepairAlias(alias.ReviewID); errors.Is(err, ErrNotFound) {
			missing++
		}
	}
	return missing
}

// RemoveOrphanReviewAliasesExact is the exact-preflight, all-or-rollback batch
// companion to RemoveOrphanReviewAliasExact. It acquires every raw-plan lock in
// sorted plan order and then every alias lock in sorted review-ID order,
// revalidates every exact alias/current/trash precondition, and only then removes
// the first alias. Catchable remove failures roll back already-removed aliases
// while all locks remain held; rollback failure is surfaced as RecoveryRequired.
// Abrupt process termination can bypass rollback after a strict subset has been
// removed. That case is intentionally crash-convergent rather than power-loss
// atomic because every removed alias was already proven orphan. A fresh
// diagnosis/review is authoritative for whatever orphan aliases remain.
func (store Store) RemoveOrphanReviewAliasesExact(expected []ReviewAlias) (ReviewAliasBatchRemovalResult, error) {
	return store.removeOrphanReviewAliasesExactWith(expected, os.Remove, store.writeExactRepairAliasRecordLocked)
}

func (store Store) removeOrphanReviewAliasesExactWith(expected []ReviewAlias, removeFile func(string) error, restoreAlias func(ReviewAlias) error) (ReviewAliasBatchRemovalResult, error) {
	result := ReviewAliasBatchRemovalResult{Requested: len(expected)}
	if len(expected) == 0 {
		return result, nil
	}
	if removeFile == nil || restoreAlias == nil {
		return result, errors.New("sync journal review alias repair callbacks are nil")
	}
	ordered, err := canonicalExactRepairAliases(store, expected)
	if err != nil {
		return result, err
	}
	rawLocks, err := store.lockExactRepairRawPlans(ordered)
	if err != nil {
		return result, err
	}
	defer rawLocks.Close()
	aliasLocks, err := store.lockExactRepairAliases(ordered)
	if err != nil {
		return result, err
	}
	defer aliasLocks.Close()
	if err := store.preflightExactOrphanRepairBatch(ordered, aliasLocks); err != nil {
		return result, err
	}

	removed := make([]ReviewAlias, 0, len(ordered))
	for _, alias := range ordered {
		path, err := store.reviewAliasPath(alias.ReviewID)
		if err == nil {
			err = removeFile(path)
		}
		if err == nil {
			removed = append(removed, alias)
			continue
		}
		var rollbackErr error
		for index := len(removed) - 1; index >= 0; index-- {
			rollbackErr = errors.Join(rollbackErr, restoreAlias(removed[index]))
		}
		if rollbackErr != nil {
			result.Removed = countMissingExactRepairAliases(store, ordered)
			result.RecoveryRequired = true
			return result, errors.Join(ErrReviewAliasRepairRollback, fmt.Errorf("remove orphan sync journal review alias: %w", err), rollbackErr)
		}
		result.RolledBack = len(removed) > 0
		return result, fmt.Errorf("remove orphan sync journal review alias: %w", err)
	}
	result.Removed = len(ordered)
	return result, nil
}
