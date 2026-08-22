package tools

import (
	"context"
	"crypto/subtle"
	"errors"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func sameMCPContentToken(left, right string) bool {
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

type ReconcileSyncJournalAliasArgs struct {
	PlanID         string `json:"plan_id" jsonschema:"reviewed MCP plan_sync plan_id for an orphan returned by diagnose_sync_journal_aliases"`
	ExpectRepairID string `json:"expect_repair_id" jsonschema:"required repair_id returned for that orphan by diagnose_sync_journal_aliases"`
}

type ReconcileSyncJournalAliasOutput struct {
	PlanID    string `json:"plan_id,omitempty"`
	RepairID  string `json:"repair_id,omitempty"`
	Repaired  bool   `json:"repaired"`
	Status    string `json:"status,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	Error     string `json:"error,omitempty"`
}

func reconcileSyncJournalAliasResult(output ReconcileSyncJournalAliasOutput) (*mcp.CallToolResult, ReconcileSyncJournalAliasOutput, error) {
	return mcpTypedJSONResult("reconcile_sync_journal_alias", output, output, output.ErrorCode != "")
}

func reconcileSyncJournalAliasError(planID, repairID, code, message string) (*mcp.CallToolResult, ReconcileSyncJournalAliasOutput, error) {
	return reconcileSyncJournalAliasResult(ReconcileSyncJournalAliasOutput{
		PlanID: planID, RepairID: repairID, Repaired: false, ErrorCode: code, Error: message,
	})
}

func (ft *FileTools) reconcileSyncJournalAlias(ctx context.Context, req *mcp.CallToolRequest, args ReconcileSyncJournalAliasArgs) (*mcp.CallToolResult, ReconcileSyncJournalAliasOutput, error) {
	reviewedPlanID, err := normalizeMCPExpectedPlanID(args.PlanID)
	if err != nil || reviewedPlanID == "" {
		return reconcileSyncJournalAliasError("", "", "invalid_plan_id", "plan_id must be a reviewed sha256:<64hex> plan_sync ID")
	}
	expectedRepairID, err := normalizeMCPExpectedPlanID(args.ExpectRepairID)
	if err != nil || expectedRepairID == "" {
		return reconcileSyncJournalAliasError(reviewedPlanID, "", "invalid_repair_id", "expect_repair_id must be the sha256:<64hex> token returned by diagnose_sync_journal_aliases")
	}
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil || store == nil {
		return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "journal_unavailable", "persistent sync journal store is unavailable")
	}
	scan, err := diagnoseMCPReviewAliases(*store)
	if err != nil {
		return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "journal_alias_diagnosis_failed", "persistent sync journal aliases could not be diagnosed safely")
	}
	var diagnosis *syncjournalpkg.ReviewAliasDiagnosis
	for index := range scan.Entries {
		if scan.Entries[index].Alias.ReviewID == reviewedPlanID {
			diagnosis = &scan.Entries[index]
			break
		}
	}
	if diagnosis == nil {
		return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "alias_not_found", "no reviewed sync journal alias exists for this plan_id")
	}
	switch diagnosis.Status {
	case syncjournalpkg.ReviewAliasDiagnosisOrphan:
		// Continue through the content-addressed and exact-snapshot gates below.
	case syncjournalpkg.ReviewAliasDiagnosisSoftDeleted:
		return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "journal_trashed", "the alias still shadows a soft-deleted journal; restore the journal instead of removing the alias")
	case syncjournalpkg.ReviewAliasDiagnosisIdentityMismatch:
		return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "journal_alias_conflict", "the alias target does not match the reviewed plan identity and cannot be repaired automatically")
	case syncjournalpkg.ReviewAliasDiagnosisLive:
		return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "alias_not_orphan", "the reviewed alias still points to a live journal")
	default:
		return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "journal_read_failed", "the reviewed alias target cannot be proven safe for automatic repair")
	}

	freshRepairID, err := syncjournalpkg.ReviewAliasRepairID(diagnosis.Alias)
	if err != nil {
		return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "journal_read_failed", "the orphan alias repair snapshot could not be verified safely")
	}
	if !sameMCPContentToken(freshRepairID, expectedRepairID) {
		return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "repair_changed", "the orphan alias snapshot changed; run diagnose_sync_journal_aliases again")
	}
	removed, err := store.RemoveOrphanReviewAliasExact(diagnosis.Alias)
	if err != nil {
		switch {
		case errors.Is(err, syncjournalpkg.ErrReviewAliasChanged):
			return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "repair_changed", "the orphan alias snapshot changed; run diagnose_sync_journal_aliases again")
		case errors.Is(err, syncjournalpkg.ErrReviewAliasTrashed):
			return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "journal_trashed", "the sync journal became soft-deleted; restore it instead of removing the alias")
		case errors.Is(err, syncjournalpkg.ErrReviewAliasConflict):
			return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "journal_alias_conflict", "the reviewed alias binding changed and cannot be repaired automatically")
		case errors.Is(err, syncjournalpkg.ErrReviewAliasInUse), errors.Is(err, transfer.ErrSessionLocked):
			return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "journal_in_use", "the reviewed alias or target journal is being updated by another process")
		default:
			return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "repair_failed", "the orphan alias could not be removed safely")
		}
	}
	if !removed {
		return reconcileSyncJournalAliasError(reviewedPlanID, expectedRepairID, "repair_changed", "the orphan alias no longer matches the reviewed repair snapshot")
	}
	return reconcileSyncJournalAliasResult(ReconcileSyncJournalAliasOutput{
		PlanID: reviewedPlanID, RepairID: expectedRepairID, Repaired: true, Status: "removed",
	})
}
