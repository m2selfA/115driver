package syncjournal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

const (
	ReviewAliasSchemaID = "115driver.sync-journal-review-alias"
	ReviewAliasVersion  = 1
)

var (
	ErrReviewAliasConflict = errors.New("sync journal review alias conflict")
	ErrReviewAliasInUse    = errors.New("sync journal review alias is in use")
	ErrReviewAliasTrashed  = errors.New("sync journal review alias points to a soft-deleted journal")
	ErrReviewAliasChanged  = errors.New("sync journal review alias changed")
)

type ReviewAlias struct {
	Version      int       `json:"version"`
	Schema       string    `json:"schema"`
	ReviewID     string    `json:"review_id"`
	PlanID       string    `json:"plan_id"`
	ProfileScope string    `json:"profile_scope"`
	AccountID    int64     `json:"account_id"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func NormalizeReviewID(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return "", fmt.Errorf("sync journal review ID must use sha256:<64 hex> format")
	}
	raw, err := NormalizePlanID(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("invalid sync journal review ID: %w", err)
	}
	return prefix + raw, nil
}

func (store Store) reviewAliasPath(reviewID string) (string, error) {
	reviewID, err := NormalizeReviewID(reviewID)
	if err != nil {
		return "", err
	}
	root, err := store.RootPath()
	if err != nil {
		return "", err
	}
	raw := strings.TrimPrefix(reviewID, "sha256:")
	return filepath.Join(root, "review-aliases", raw[:2], raw+".json"), nil
}

func (store Store) reviewAliasLockPath(reviewID string) (string, error) {
	path, err := store.reviewAliasPath(reviewID)
	if err != nil {
		return "", err
	}
	return path + ".lock", nil
}

func (store Store) validateReviewAliasMode(alias ReviewAlias, expectedReviewID string, requireAccountMatch bool) error {
	reviewID, err := NormalizeReviewID(alias.ReviewID)
	if err != nil {
		return fmt.Errorf("%w: invalid review alias ID: %v", ErrInvalidSchema, err)
	}
	expectedReviewID, err = NormalizeReviewID(expectedReviewID)
	if err != nil {
		return err
	}
	if alias.Version != ReviewAliasVersion || alias.Schema != ReviewAliasSchemaID || reviewID != expectedReviewID {
		return fmt.Errorf("%w: invalid sync journal review alias envelope", ErrInvalidSchema)
	}
	planID, err := NormalizePlanID(alias.PlanID)
	if err != nil || planID != alias.PlanID {
		return fmt.Errorf("%w: invalid sync journal review alias plan ID", ErrInvalidSchema)
	}
	scope, err := normalizeProfileScope(store.ProfileScope)
	if err != nil {
		return err
	}
	if alias.ProfileScope != scope {
		return fmt.Errorf("%w: review alias belongs to a different profile scope", ErrBindingMismatch)
	}
	if alias.AccountID <= 0 {
		return fmt.Errorf("%w: review alias has no valid account binding", ErrBindingMismatch)
	}
	if requireAccountMatch && (store.AccountID <= 0 || store.AccountID != alias.AccountID) {
		return fmt.Errorf("%w: review alias belongs to a different account", ErrBindingMismatch)
	}
	if alias.CreatedAt.IsZero() || alias.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: review alias timestamps are incomplete", ErrInvalidSchema)
	}
	return nil
}

func (store Store) validateReviewAlias(alias ReviewAlias, expectedReviewID string) error {
	return store.validateReviewAliasMode(alias, expectedReviewID, true)
}

func (store Store) readReviewAliasMode(reviewID string, requireAccountMatch bool) (ReviewAlias, error) {
	path, err := store.reviewAliasPath(reviewID)
	if err != nil {
		return ReviewAlias{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ReviewAlias{}, ErrNotFound
		}
		return ReviewAlias{}, err
	}
	var alias ReviewAlias
	if err := json.Unmarshal(data, &alias); err != nil {
		return ReviewAlias{}, fmt.Errorf("%w: decode sync journal review alias: %v", ErrInvalidSchema, err)
	}
	if err := store.validateReviewAliasMode(alias, reviewID, requireAccountMatch); err != nil {
		return ReviewAlias{}, err
	}
	return alias, nil
}

func (store Store) readReviewAlias(reviewID string) (ReviewAlias, error) {
	return store.readReviewAliasMode(reviewID, true)
}

func (store Store) readReviewAliasProfileBound(reviewID string) (ReviewAlias, error) {
	return store.readReviewAliasMode(reviewID, false)
}

// WriteReviewAlias is the low-level alias writer retained for repair/import
// paths and tests that intentionally create aliases without a live journal.
// Production current-journal lifecycle code should use Handle.BindReviewAlias
// or call bindReviewAlias only while already holding the raw journal lock.
func (store Store) WriteReviewAlias(reviewID, planID string) (ReviewAlias, error) {
	return store.bindReviewAlias(reviewID, planID)
}

func (store Store) bindReviewAlias(reviewID, planID string) (ReviewAlias, error) {
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
	lockPath, err := store.reviewAliasLockPath(reviewID)
	if err != nil {
		return ReviewAlias{}, err
	}
	lock, err := transfer.AcquireSessionLock(lockPath, "")
	if err != nil {
		if errors.Is(err, transfer.ErrSessionLocked) {
			return ReviewAlias{}, ErrReviewAliasInUse
		}
		return ReviewAlias{}, fmt.Errorf("lock sync journal review alias: %w", err)
	}
	defer lock.Close()
	if existing, err := store.readReviewAlias(reviewID); err == nil {
		if existing.PlanID != planID {
			return ReviewAlias{}, fmt.Errorf("%w: reviewed plan maps to a different sync journal", ErrReviewAliasConflict)
		}
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return ReviewAlias{}, err
	}
	scope, err := normalizeProfileScope(store.ProfileScope)
	if err != nil {
		return ReviewAlias{}, err
	}
	now := time.Now().UTC()
	alias := ReviewAlias{
		Version: ReviewAliasVersion, Schema: ReviewAliasSchemaID, ReviewID: reviewID, PlanID: planID,
		ProfileScope: scope, AccountID: store.AccountID, CreatedAt: now, UpdatedAt: now,
	}
	encoded, err := json.Marshal(alias)
	if err != nil {
		return ReviewAlias{}, err
	}
	path, err := store.reviewAliasPath(reviewID)
	if err != nil {
		return ReviewAlias{}, err
	}
	if err := transfer.WritePrivateFileAtomic(path, encoded); err != nil {
		return ReviewAlias{}, fmt.Errorf("write sync journal review alias: %w", err)
	}
	written, err := store.readReviewAlias(reviewID)
	if err != nil {
		return ReviewAlias{}, err
	}
	if written.PlanID != planID {
		return ReviewAlias{}, fmt.Errorf("%w: reviewed plan alias changed during write", ErrReviewAliasConflict)
	}
	return written, nil
}

// BindReviewAlias creates/verifies a reviewed-plan binding while this Handle's
// raw journal lock is held. Production execution/recovery paths should prefer
// this method over Store.WriteReviewAlias so journal lifetime and alias creation
// share the canonical journal -> alias lock order.
func (handle *Handle) BindReviewAlias(reviewID string) (ReviewAlias, error) {
	if handle == nil || handle.lock == nil {
		return ReviewAlias{}, errors.New("sync journal handle is not locked")
	}
	handle.mu.Lock()
	planID := handle.journal.PlanID
	store := handle.store
	handle.mu.Unlock()
	if planID == "" {
		return ReviewAlias{}, errors.New("sync journal handle has no plan ID")
	}
	return store.bindReviewAlias(reviewID, planID)
}

func sameReviewAliasSnapshot(left, right ReviewAlias) bool {
	return left.Version == right.Version && left.Schema == right.Schema && left.ReviewID == right.ReviewID &&
		left.PlanID == right.PlanID && left.ProfileScope == right.ProfileScope && left.AccountID == right.AccountID &&
		left.CreatedAt.Equal(right.CreatedAt) && left.UpdatedAt.Equal(right.UpdatedAt)
}

// RemoveOrphanReviewAlias removes a stale reviewed-plan binding only when the
// mapped current journal is still absent after acquiring the raw journal lock
// and then the alias lock. The raw lock deliberately omits its lease path so an
// absence check cannot create a phantom journal directory merely to write a
// heartbeat file.
func (store Store) RemoveOrphanReviewAlias(reviewID, expectedPlanID string) (bool, error) {
	return store.removeOrphanReviewAlias(reviewID, expectedPlanID, nil)
}

// RemoveOrphanReviewAliasExact adds an anti-TOCTOU snapshot gate for reviewed
// maintenance flows. Every persisted alias field/timestamp must still match the
// previously diagnosed record before absence is re-proven and deletion occurs.
func (store Store) RemoveOrphanReviewAliasExact(expected ReviewAlias) (bool, error) {
	if err := store.validateReviewAlias(expected, expected.ReviewID); err != nil {
		return false, err
	}
	return store.removeOrphanReviewAlias(expected.ReviewID, expected.PlanID, &expected)
}

func (store Store) removeOrphanReviewAlias(reviewID, expectedPlanID string, expected *ReviewAlias) (bool, error) {
	reviewID, err := NormalizeReviewID(reviewID)
	if err != nil {
		return false, err
	}
	expectedPlanID, err = NormalizePlanID(expectedPlanID)
	if err != nil {
		return false, err
	}
	location, err := store.Location(expectedPlanID)
	if err != nil {
		return false, err
	}
	journalLock, err := transfer.AcquireSessionLock(location.LockPath, "")
	if err != nil {
		return false, err
	}
	defer journalLock.Close()

	aliasLockPath, err := store.reviewAliasLockPath(reviewID)
	if err != nil {
		return false, err
	}
	aliasLock, err := transfer.AcquireSessionLock(aliasLockPath, "")
	if err != nil {
		if errors.Is(err, transfer.ErrSessionLocked) {
			return false, ErrReviewAliasInUse
		}
		return false, fmt.Errorf("lock sync journal review alias: %w", err)
	}
	defer aliasLock.Close()

	alias, err := store.readReviewAlias(reviewID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if alias.PlanID != expectedPlanID {
		return false, fmt.Errorf("%w: reviewed plan maps to a different sync journal", ErrReviewAliasConflict)
	}
	if expected != nil && !sameReviewAliasSnapshot(alias, *expected) {
		return false, ErrReviewAliasChanged
	}
	if _, err := store.ReadCurrent(location); err == nil {
		return false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return false, err
	}
	trashed, err := store.HasTrashedCurrentPlan(expectedPlanID, maxMaintenanceReviewAliases)
	if err != nil {
		return false, err
	}
	if trashed {
		return false, ErrReviewAliasTrashed
	}
	aliasPath, err := store.reviewAliasPath(reviewID)
	if err != nil {
		return false, err
	}
	if err := os.Remove(aliasPath); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("remove orphan sync journal review alias: %w", err)
	}
	return true, nil
}

func (store Store) ResolveReviewAlias(reviewID string) (string, error) {
	alias, err := store.readReviewAlias(reviewID)
	if err != nil {
		return "", err
	}
	return alias.PlanID, nil
}
