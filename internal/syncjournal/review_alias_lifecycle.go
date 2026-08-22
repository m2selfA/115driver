package syncjournal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

const maxMaintenanceReviewAliases = 4096

// aliasLifecycleStoreForJournal keeps offline CLI journal administration
// account-safe without requiring a network login. Current-v2 journals persist
// their account binding, so an otherwise unknown-account Store may borrow that
// exact account only after the journal itself has been read from the already
// profile-scoped location. Alias-bearing state with no account on either side
// fails closed.
func (store Store) aliasLifecycleStoreForJournal(journal Journal, aliasesExpected bool) (Store, error) {
	if store.AccountID != 0 {
		if journal.AccountID != 0 && store.AccountID != journal.AccountID {
			return Store{}, fmt.Errorf("%w: journal belongs to a different account", ErrBindingMismatch)
		}
		return store, nil
	}
	if journal.AccountID > 0 {
		store.AccountID = journal.AccountID
		return store, nil
	}
	if aliasesExpected {
		return Store{}, fmt.Errorf("%w: reviewed sync journal aliases require a persisted account binding", ErrBindingMismatch)
	}
	return store, nil
}

type lockedReviewAliasSet struct {
	ids      []string
	locks    []*transfer.SessionLock
	existing map[string]ReviewAlias
}

func (set *lockedReviewAliasSet) Close() error {
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

func (store Store) lockReviewAliasSet(reviewIDs []string, expectedPlanID string, allowMissing bool) (*lockedReviewAliasSet, error) {
	expectedPlanID, err := NormalizePlanID(expectedPlanID)
	if err != nil {
		return nil, err
	}
	if len(reviewIDs) == 0 {
		return &lockedReviewAliasSet{ids: []string{}, existing: make(map[string]ReviewAlias)}, nil
	}
	canonical, err := canonicalTrashReviewAliases(reviewIDs)
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
	for _, reviewID := range canonical {
		alias, err := store.readReviewAlias(reviewID)
		if err != nil {
			if allowMissing && errors.Is(err, ErrNotFound) {
				continue
			}
			_ = set.Close()
			return nil, err
		}
		if alias.PlanID != expectedPlanID {
			_ = set.Close()
			return nil, fmt.Errorf("%w: reviewed plan maps to a different sync journal", ErrReviewAliasConflict)
		}
		set.existing[reviewID] = alias
	}
	return set, nil
}

func (store Store) reviewIDsForPlan(planID string) ([]string, error) {
	planID, err := NormalizePlanID(planID)
	if err != nil {
		return nil, err
	}
	scan, err := store.ScanReviewAliases(maxMaintenanceReviewAliases)
	if err != nil {
		return nil, err
	}
	ids := append([]string(nil), scan.ByPlanID[planID]...)
	sort.Strings(ids)
	return ids, nil
}

func (store Store) newReviewAliasRecord(reviewID, planID string, now time.Time) (ReviewAlias, error) {
	reviewID, err := NormalizeReviewID(reviewID)
	if err != nil {
		return ReviewAlias{}, err
	}
	planID, err = NormalizePlanID(planID)
	if err != nil {
		return ReviewAlias{}, err
	}
	if store.AccountID <= 0 {
		return ReviewAlias{}, fmt.Errorf("%w: review alias requires a known account", ErrBindingMismatch)
	}
	scope, err := normalizeProfileScope(store.ProfileScope)
	if err != nil {
		return ReviewAlias{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	return ReviewAlias{
		Version: ReviewAliasVersion, Schema: ReviewAliasSchemaID, ReviewID: reviewID, PlanID: planID,
		ProfileScope: scope, AccountID: store.AccountID, CreatedAt: now, UpdatedAt: now,
	}, nil
}

// writeReviewAliasRecordLocked writes one already-locked alias record without
// acquiring the alias lock again. Lifecycle code uses this to make multi-alias
// rollback/restore deterministic while the raw journal lock remains held.
func (store Store) writeReviewAliasRecordLocked(alias ReviewAlias) error {
	if err := store.validateReviewAlias(alias, alias.ReviewID); err != nil {
		return err
	}
	path, err := store.reviewAliasPath(alias.ReviewID)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(alias)
	if err != nil {
		return err
	}
	if err := transfer.WritePrivateFileAtomic(path, encoded); err != nil {
		return fmt.Errorf("write sync journal review alias: %w", err)
	}
	written, err := store.readReviewAlias(alias.ReviewID)
	if err != nil {
		return err
	}
	if written.PlanID != alias.PlanID {
		return fmt.Errorf("%w: reviewed plan alias changed during write", ErrReviewAliasConflict)
	}
	return nil
}

func (store Store) removeLockedReviewAliases(set *lockedReviewAliasSet) error {
	if set == nil {
		return nil
	}
	for _, reviewID := range set.ids {
		if _, exists := set.existing[reviewID]; !exists {
			continue
		}
		path, err := store.reviewAliasPath(reviewID)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove sync journal review alias: %w", err)
		}
	}
	return nil
}

func (store Store) restoreLockedReviewAliases(set *lockedReviewAliasSet, planID string) error {
	if set == nil {
		return nil
	}
	for _, reviewID := range set.ids {
		if existing, ok := set.existing[reviewID]; ok {
			if existing.PlanID != planID {
				return fmt.Errorf("%w: reviewed plan maps to a different sync journal", ErrReviewAliasConflict)
			}
			continue
		}
		alias, err := store.newReviewAliasRecord(reviewID, planID, time.Now().UTC())
		if err != nil {
			return err
		}
		if err := store.writeReviewAliasRecordLocked(alias); err != nil {
			return err
		}
	}
	return nil
}

func (store Store) rollbackLockedReviewAliases(set *lockedReviewAliasSet) error {
	if set == nil {
		return nil
	}
	var joined error
	for _, reviewID := range set.ids {
		path, err := store.reviewAliasPath(reviewID)
		if err != nil {
			joined = errors.Join(joined, err)
			continue
		}
		if original, existed := set.existing[reviewID]; existed {
			if err := store.writeReviewAliasRecordLocked(original); err != nil {
				joined = errors.Join(joined, err)
			}
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}
