package syncjournal

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

const ReviewAliasRepairSchemaID = "115driver.sync-journal-alias-repair/v1"

type reviewAliasRepairFingerprint struct {
	Schema       string `json:"schema"`
	ReviewID     string `json:"review_id"`
	RawPlanID    string `json:"raw_plan_id"`
	ProfileScope string `json:"profile_scope"`
	AccountID    int64  `json:"account_id"`
	CreatedAt    int64  `json:"created_at_unix_nano"`
	UpdatedAt    int64  `json:"updated_at_unix_nano"`
	Status       string `json:"status"`
}

// ReviewAliasRepairID returns a content-addressed token for one exact orphan
// alias snapshot. The token deliberately binds hidden storage identity,
// profile/account binding, and both persisted timestamps so a later reconcile
// can require explicit review of any state change without exposing those fields.
func ReviewAliasRepairID(alias ReviewAlias) (string, error) {
	reviewID, err := NormalizeReviewID(alias.ReviewID)
	if err != nil || reviewID != alias.ReviewID {
		return "", fmt.Errorf("%w: invalid review alias repair review ID", ErrInvalidSchema)
	}
	planID, err := NormalizePlanID(alias.PlanID)
	if err != nil || planID != alias.PlanID {
		return "", fmt.Errorf("%w: invalid review alias repair plan ID", ErrInvalidSchema)
	}
	scope, err := normalizeProfileScope(alias.ProfileScope)
	if err != nil || scope != alias.ProfileScope {
		return "", fmt.Errorf("%w: invalid review alias repair profile scope", ErrInvalidSchema)
	}
	if alias.Version != ReviewAliasVersion || alias.Schema != ReviewAliasSchemaID || alias.AccountID <= 0 || alias.CreatedAt.IsZero() || alias.UpdatedAt.IsZero() {
		return "", fmt.Errorf("%w: incomplete review alias repair snapshot", ErrInvalidSchema)
	}
	payload := reviewAliasRepairFingerprint{
		Schema: ReviewAliasRepairSchemaID, ReviewID: alias.ReviewID, RawPlanID: alias.PlanID,
		ProfileScope: alias.ProfileScope, AccountID: alias.AccountID,
		CreatedAt: alias.CreatedAt.UnixNano(), UpdatedAt: alias.UpdatedAt.UnixNano(),
		Status: string(ReviewAliasDiagnosisOrphan),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode review alias repair fingerprint: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}
