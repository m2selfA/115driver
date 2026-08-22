package tools

import (
	"context"
	"errors"
	"fmt"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultMCPPlanSyncJournalAliasRepairLimit = syncjournalpkg.DefaultReviewAliasRepairBatchLimit
	maxMCPPlanSyncJournalAliasRepairLimit     = syncjournalpkg.MaxReviewAliasRepairBatchLimit
)

type PlanSyncJournalAliasRepairArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum proven orphan aliases selected for this reviewed batch; default 50, maximum 128"`
}

type MCPSyncJournalAliasRepairCandidate struct {
	PlanID   string `json:"plan_id" jsonschema:"reviewed MCP plan_sync plan_id"`
	RepairID string `json:"repair_id" jsonschema:"opaque exact-snapshot repair token for this orphan alias"`
}

type PlanSyncJournalAliasRepairOutput struct {
	RepairSetID string                               `json:"repair_set_id,omitempty" jsonschema:"content-addressed token binding the complete currently diagnosed orphan set and selection limit"`
	Scanned     int                                  `json:"scanned"`
	Eligible    int                                  `json:"eligible"`
	Selected    int                                  `json:"selected"`
	Items       []MCPSyncJournalAliasRepairCandidate `json:"items"`
	ErrorCode   string                               `json:"error_code,omitempty"`
	Error       string                               `json:"error,omitempty" jsonschema:"sanitized alias repair planning error"`
}

type ExecuteSyncJournalAliasRepairArgs struct {
	Limit             int    `json:"limit,omitempty" jsonschema:"same candidate limit used by plan_sync_journal_alias_repair"`
	ExpectRepairSetID string `json:"expect_repair_set_id" jsonschema:"required repair_set_id returned by plan_sync_journal_alias_repair"`
}

type MCPSyncJournalAliasRepairExecutionItem struct {
	Index  int    `json:"index"`
	PlanID string `json:"plan_id" jsonschema:"reviewed MCP plan_sync plan_id"`
	Status string `json:"status" jsonschema:"removed, unchanged, or unknown when rollback itself failed"`
}

type ExecuteSyncJournalAliasRepairOutput struct {
	RepairSetID      string                                   `json:"repair_set_id,omitempty"`
	Requested        int                                      `json:"requested"`
	Removed          int                                      `json:"removed"`
	Unchanged        int                                      `json:"unchanged"`
	Unknown          int                                      `json:"unknown"`
	Partial          bool                                     `json:"partial"`
	RecoveryRequired bool                                     `json:"recovery_required,omitempty"`
	Items            []MCPSyncJournalAliasRepairExecutionItem `json:"items"`
	ErrorCode        string                                   `json:"error_code,omitempty"`
	Error            string                                   `json:"error,omitempty" jsonschema:"sanitized alias repair execution error"`
}

func normalizeMCPAliasRepairBatchLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, fmt.Errorf("limit must be >= 0")
	}
	if limit == 0 {
		return defaultMCPPlanSyncJournalAliasRepairLimit, nil
	}
	if limit > maxMCPPlanSyncJournalAliasRepairLimit {
		return 0, fmt.Errorf("limit must not exceed %d", maxMCPPlanSyncJournalAliasRepairLimit)
	}
	return limit, nil
}

func planMCPSyncJournalAliasRepair(store syncjournalpkg.Store, limit int) (PlanSyncJournalAliasRepairOutput, []syncjournalpkg.ReviewAlias, error) {
	limit, err := normalizeMCPAliasRepairBatchLimit(limit)
	if err != nil {
		return PlanSyncJournalAliasRepairOutput{}, nil, err
	}
	scan, err := diagnoseMCPReviewAliases(store)
	if err != nil {
		return PlanSyncJournalAliasRepairOutput{}, nil, err
	}
	sharedPlan, err := syncjournalpkg.BuildReviewAliasRepairPlan(scan, limit)
	if err != nil {
		return PlanSyncJournalAliasRepairOutput{}, nil, err
	}
	selected := make([]syncjournalpkg.ReviewAlias, 0, len(sharedPlan.Candidates))
	output := PlanSyncJournalAliasRepairOutput{
		RepairSetID: sharedPlan.RepairSetID, Scanned: sharedPlan.Scanned, Eligible: sharedPlan.Eligible, Selected: len(sharedPlan.Candidates),
		Items: make([]MCPSyncJournalAliasRepairCandidate, 0, len(sharedPlan.Candidates)),
	}
	for _, candidate := range sharedPlan.Candidates {
		selected = append(selected, candidate.Alias)
		output.Items = append(output.Items, MCPSyncJournalAliasRepairCandidate{PlanID: candidate.Alias.ReviewID, RepairID: candidate.RepairID})
	}
	return output, selected, nil
}

func (ft *FileTools) planSyncJournalAliasRepair(ctx context.Context, req *mcp.CallToolRequest, args PlanSyncJournalAliasRepairArgs) (*mcp.CallToolResult, PlanSyncJournalAliasRepairOutput, error) {
	if _, err := normalizeMCPAliasRepairBatchLimit(args.Limit); err != nil {
		return toolError(err.Error()), PlanSyncJournalAliasRepairOutput{}, nil
	}
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil || store == nil {
		output := PlanSyncJournalAliasRepairOutput{Items: []MCPSyncJournalAliasRepairCandidate{}, ErrorCode: "journal_unavailable", Error: "persistent sync journal store is unavailable"}
		return mcpTypedJSONResult("plan_sync_journal_alias_repair", output, output, true)
	}
	output, _, err := planMCPSyncJournalAliasRepair(*store, args.Limit)
	if err != nil {
		failed := PlanSyncJournalAliasRepairOutput{Items: []MCPSyncJournalAliasRepairCandidate{}, ErrorCode: "journal_alias_diagnosis_failed", Error: "persistent sync journal aliases could not be diagnosed safely"}
		if errors.Is(err, syncjournalpkg.ErrScanLimit) {
			failed.ErrorCode = "journal_alias_scan_limit_exceeded"
			failed.Error = "persistent sync journal alias scan exceeded its safety limit"
		} else if errors.Is(err, syncjournalpkg.ErrTrashScanLimit) {
			failed.ErrorCode = "journal_trash_scan_limit_exceeded"
			failed.Error = "trashed sync journal scan exceeded its safety limit"
		}
		return mcpTypedJSONResult("plan_sync_journal_alias_repair", failed, failed, true)
	}
	return mcpTypedJSONResult("plan_sync_journal_alias_repair", output, output, false)
}

func mcpAliasRepairExecutionItems(selected []syncjournalpkg.ReviewAlias, status string) []MCPSyncJournalAliasRepairExecutionItem {
	items := make([]MCPSyncJournalAliasRepairExecutionItem, len(selected))
	for index, alias := range selected {
		items[index] = MCPSyncJournalAliasRepairExecutionItem{Index: index, PlanID: alias.ReviewID, Status: status}
	}
	return items
}

func aliasRepairExecutionError(expectedID, code, message string, requested int, selected []syncjournalpkg.ReviewAlias, shared syncjournalpkg.ReviewAliasBatchRemovalResult) (*mcp.CallToolResult, ExecuteSyncJournalAliasRepairOutput, error) {
	status := "unchanged"
	if shared.RecoveryRequired {
		status = "unknown"
	}
	output := ExecuteSyncJournalAliasRepairOutput{
		RepairSetID: expectedID, Requested: requested, Removed: shared.Removed,
		RecoveryRequired: shared.RecoveryRequired, Items: mcpAliasRepairExecutionItems(selected, status),
		ErrorCode: code, Error: message,
	}
	if shared.RecoveryRequired {
		output.Unknown = len(selected)
		output.Partial = shared.Removed > 0
	} else {
		output.Unchanged = len(selected)
	}
	return mcpTypedJSONResult("execute_sync_journal_alias_repair", output, output, true)
}

func (ft *FileTools) executeSyncJournalAliasRepair(ctx context.Context, req *mcp.CallToolRequest, args ExecuteSyncJournalAliasRepairArgs) (*mcp.CallToolResult, ExecuteSyncJournalAliasRepairOutput, error) {
	limit, err := normalizeMCPAliasRepairBatchLimit(args.Limit)
	if err != nil {
		return toolError(err.Error()), ExecuteSyncJournalAliasRepairOutput{}, nil
	}
	expectedID, err := normalizeMCPExpectedPlanID(args.ExpectRepairSetID)
	if err != nil || expectedID == "" {
		return toolError("expect_repair_set_id must use sha256:<64 hex> format"), ExecuteSyncJournalAliasRepairOutput{}, nil
	}
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil || store == nil {
		return aliasRepairExecutionError(expectedID, "journal_unavailable", "persistent sync journal store is unavailable", 0, nil, syncjournalpkg.ReviewAliasBatchRemovalResult{})
	}
	preview, selected, err := planMCPSyncJournalAliasRepair(*store, limit)
	if err != nil {
		return aliasRepairExecutionError(expectedID, "journal_alias_diagnosis_failed", "persistent sync journal aliases could not be diagnosed safely", 0, nil, syncjournalpkg.ReviewAliasBatchRemovalResult{})
	}
	if !sameMCPContentToken(preview.RepairSetID, expectedID) {
		// Never return the fresh candidate identities or replacement set token.
		// Changed state requires a new explicit planning/review turn.
		return aliasRepairExecutionError(expectedID, "repair_changed", "the orphan alias repair set changed; run plan_sync_journal_alias_repair again", 0, nil, syncjournalpkg.ReviewAliasBatchRemovalResult{})
	}
	shared, removeErr := store.RemoveOrphanReviewAliasesExact(selected)
	if removeErr != nil {
		code := "repair_changed"
		message := "the reviewed orphan alias set changed before removal; no alias was removed"
		switch {
		case errors.Is(removeErr, syncjournalpkg.ErrReviewAliasRepairRollback):
			code = "recovery_required"
			message = "alias removal failed and private alias rollback could not be completed; inspect alias lifecycle before any further repair"
		case errors.Is(removeErr, syncjournalpkg.ErrReviewAliasTrashed):
			code = "journal_trashed"
			message = "an orphan candidate became soft-deleted; restore the journal instead of removing its alias"
		case errors.Is(removeErr, syncjournalpkg.ErrReviewAliasConflict):
			code = "journal_alias_conflict"
			message = "an orphan candidate alias binding changed and cannot be repaired automatically"
		case errors.Is(removeErr, syncjournalpkg.ErrReviewAliasInUse), errors.Is(removeErr, transfer.ErrSessionLocked):
			code = "journal_in_use"
			message = "an orphan candidate alias or target journal is being updated by another process"
		case !errors.Is(removeErr, syncjournalpkg.ErrReviewAliasChanged):
			code = "repair_failed"
			if shared.RolledBack {
				message = "alias removal failed after mutation began, but the complete reviewed alias set was restored under lock"
			} else {
				message = "the orphan alias set could not be removed safely"
			}
		}
		return aliasRepairExecutionError(expectedID, code, message, len(selected), selected, shared)
	}
	items := mcpAliasRepairExecutionItems(selected, "removed")
	output := ExecuteSyncJournalAliasRepairOutput{RepairSetID: expectedID, Requested: len(selected), Removed: shared.Removed, Items: items}
	return mcpTypedJSONResult("execute_sync_journal_alias_repair", output, output, false)
}
