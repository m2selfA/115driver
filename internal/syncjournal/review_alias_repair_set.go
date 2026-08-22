package syncjournal

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

const ReviewAliasRepairSetSchemaID = "115driver.sync-journal-alias-repair-set/v1"

const (
	DefaultReviewAliasRepairBatchLimit = 50
	MaxReviewAliasRepairBatchLimit     = 128
)

type reviewAliasRepairSetFingerprint struct {
	Schema    string   `json:"schema"`
	Limit     int      `json:"limit"`
	RepairIDs []string `json:"repair_ids"`
}

type ReviewAliasRepairCandidate struct {
	Alias    ReviewAlias
	RepairID string
}

type ReviewAliasRepairPlan struct {
	RepairSetID string
	Scanned     int
	Eligible    int
	Limit       int
	Candidates  []ReviewAliasRepairCandidate
}

// BuildReviewAliasRepairPlan is the shared CLI/MCP authority for turning one
// lifecycle diagnosis into a reviewed orphan-only batch. It binds every current
// orphan into RepairSetID while exposing only the sorted selected prefix to the
// caller. Aggregate drift fails closed instead of producing a token from an
// internally inconsistent diagnosis snapshot.
func BuildReviewAliasRepairPlan(scan ReviewAliasDiagnosisScan, limit int) (ReviewAliasRepairPlan, error) {
	if limit <= 0 {
		return ReviewAliasRepairPlan{}, fmt.Errorf("review alias repair plan limit must be > 0")
	}
	if scan.Scanned != len(scan.Entries) {
		return ReviewAliasRepairPlan{}, fmt.Errorf("%w: review alias diagnosis scanned aggregate mismatch", ErrInvalidSchema)
	}
	orphans := make([]ReviewAlias, 0, scan.Orphan)
	live, softDeleted, identityMismatch, invalid := 0, 0, 0, 0
	for _, diagnosis := range scan.Entries {
		switch diagnosis.Status {
		case ReviewAliasDiagnosisLive:
			live++
		case ReviewAliasDiagnosisOrphan:
			orphans = append(orphans, diagnosis.Alias)
		case ReviewAliasDiagnosisSoftDeleted:
			softDeleted++
		case ReviewAliasDiagnosisIdentityMismatch:
			identityMismatch++
		case ReviewAliasDiagnosisInvalidTarget:
			invalid++
		default:
			return ReviewAliasRepairPlan{}, fmt.Errorf("%w: unknown review alias diagnosis status %q", ErrInvalidSchema, diagnosis.Status)
		}
	}
	issues := len(orphans) + softDeleted + identityMismatch + invalid
	if scan.Live != live || scan.Orphan != len(orphans) || scan.SoftDeleted != softDeleted || scan.IdentityMismatch != identityMismatch || scan.Invalid != invalid || scan.Issues != issues {
		return ReviewAliasRepairPlan{}, fmt.Errorf("%w: review alias diagnosis aggregate mismatch", ErrInvalidSchema)
	}
	sort.Slice(orphans, func(i, j int) bool { return orphans[i].ReviewID < orphans[j].ReviewID })
	repairSetID, err := ReviewAliasRepairSetID(limit, orphans)
	if err != nil {
		return ReviewAliasRepairPlan{}, err
	}
	selectedCount := min(limit, len(orphans))
	plan := ReviewAliasRepairPlan{
		RepairSetID: repairSetID, Scanned: scan.Scanned, Eligible: len(orphans), Limit: limit,
		Candidates: make([]ReviewAliasRepairCandidate, 0, selectedCount),
	}
	for _, alias := range orphans[:selectedCount] {
		repairID, err := ReviewAliasRepairID(alias)
		if err != nil {
			return ReviewAliasRepairPlan{}, err
		}
		plan.Candidates = append(plan.Candidates, ReviewAliasRepairCandidate{Alias: alias, RepairID: repairID})
	}
	return plan, nil
}

// ReviewAliasRepairSetID returns a content-addressed token for the complete
// currently diagnosed orphan set plus the caller's selected execution limit.
// Every orphan contributes its exact ReviewAliasRepairID, so changes to an
// unselected orphan still invalidate a previously reviewed batch token.
func ReviewAliasRepairSetID(limit int, aliases []ReviewAlias) (string, error) {
	if limit <= 0 {
		return "", fmt.Errorf("review alias repair set limit must be > 0")
	}
	ordered := append([]ReviewAlias(nil), aliases...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ReviewID < ordered[j].ReviewID })
	repairIDs := make([]string, 0, len(ordered))
	lastReviewID := ""
	for _, alias := range ordered {
		if alias.ReviewID == lastReviewID {
			return "", fmt.Errorf("%w: duplicate review alias in repair set", ErrInvalidSchema)
		}
		repairID, err := ReviewAliasRepairID(alias)
		if err != nil {
			return "", err
		}
		repairIDs = append(repairIDs, repairID)
		lastReviewID = alias.ReviewID
	}
	encoded, err := json.Marshal(reviewAliasRepairSetFingerprint{
		Schema: ReviewAliasRepairSetSchemaID, Limit: limit, RepairIDs: repairIDs,
	})
	if err != nil {
		return "", fmt.Errorf("encode review alias repair set fingerprint: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}
