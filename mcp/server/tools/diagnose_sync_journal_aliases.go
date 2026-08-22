package tools

import (
	"context"
	"errors"
	"fmt"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultMCPDiagnoseSyncJournalAliasesLimit = 50
	maxMCPDiagnoseSyncJournalAliasesLimit     = 128
)

type DiagnoseSyncJournalAliasesArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum alias diagnostics to return; default 50, maximum 128; aggregate counts still cover the bounded full scan"`
}

type MCPSyncJournalAliasDiagnostic struct {
	PlanID    string `json:"plan_id" jsonschema:"reviewed MCP plan_sync plan_id; never the raw internal journal plan ID"`
	Status    string `json:"status" jsonschema:"live, orphan, soft-deleted-shadow, identity-mismatch, or invalid-target"`
	RepairID  string `json:"repair_id,omitempty" jsonschema:"opaque content-addressed repair token emitted only for a proven orphan alias"`
	InUse     bool   `json:"in_use,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	Error     string `json:"error,omitempty" jsonschema:"sanitized alias diagnostic error"`
}

type DiagnoseSyncJournalAliasesOutput struct {
	Scanned          int                             `json:"scanned"`
	Returned         int                             `json:"returned"`
	Live             int                             `json:"live"`
	Orphan           int                             `json:"orphan"`
	SoftDeleted      int                             `json:"soft_deleted"`
	IdentityMismatch int                             `json:"identity_mismatch"`
	Invalid          int                             `json:"invalid"`
	Issues           int                             `json:"issues"`
	Items            []MCPSyncJournalAliasDiagnostic `json:"items"`
	ErrorCode        string                          `json:"error_code,omitempty"`
	Error            string                          `json:"error,omitempty" jsonschema:"sanitized alias scan error"`
}

func normalizeDiagnoseSyncJournalAliasesLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, fmt.Errorf("limit must be >= 0")
	}
	if limit == 0 {
		return defaultMCPDiagnoseSyncJournalAliasesLimit, nil
	}
	if limit > maxMCPDiagnoseSyncJournalAliasesLimit {
		return 0, fmt.Errorf("limit must not exceed %d", maxMCPDiagnoseSyncJournalAliasesLimit)
	}
	return limit, nil
}

func diagnoseMCPReviewAliases(store syncjournalpkg.Store) (syncjournalpkg.ReviewAliasDiagnosisScan, error) {
	return store.DiagnoseReviewAliases(maxMCPSyncJournalScan, maxMCPSyncJournalScan, func(alias syncjournalpkg.ReviewAlias, journal syncjournalpkg.Journal) (bool, error) {
		envelope, err := buildMCPSyncPlanEnvelope(journal.Plan)
		if err != nil {
			return false, err
		}
		return envelope.PlanID == alias.ReviewID, nil
	})
}

func (ft *FileTools) diagnoseSyncJournalAliases(ctx context.Context, req *mcp.CallToolRequest, args DiagnoseSyncJournalAliasesArgs) (*mcp.CallToolResult, DiagnoseSyncJournalAliasesOutput, error) {
	limit, err := normalizeDiagnoseSyncJournalAliasesLimit(args.Limit)
	if err != nil {
		return toolError(err.Error()), DiagnoseSyncJournalAliasesOutput{}, nil
	}
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil || store == nil {
		output := DiagnoseSyncJournalAliasesOutput{Items: []MCPSyncJournalAliasDiagnostic{}, ErrorCode: "journal_unavailable", Error: "persistent sync journal store is unavailable"}
		return mcpTypedJSONResult("diagnose_sync_journal_aliases", output, output, true)
	}
	scan, err := diagnoseMCPReviewAliases(*store)
	if err != nil {
		output := DiagnoseSyncJournalAliasesOutput{Items: []MCPSyncJournalAliasDiagnostic{}, ErrorCode: "journal_alias_diagnosis_failed", Error: "persistent sync journal aliases could not be diagnosed safely"}
		switch {
		case errors.Is(err, syncjournalpkg.ErrScanLimit):
			output.ErrorCode = "journal_alias_scan_limit_exceeded"
			output.Error = "persistent sync journal alias scan exceeded its safety limit"
		case errors.Is(err, syncjournalpkg.ErrTrashScanLimit):
			output.ErrorCode = "journal_trash_scan_limit_exceeded"
			output.Error = "trashed sync journal scan exceeded its safety limit"
		case errors.Is(err, syncjournalpkg.ErrInvalidSchema):
			output.ErrorCode = "journal_alias_diagnosis_incomplete"
			output.Error = "persistent sync journal alias lifecycle could not be proven from complete current/trash evidence"
		}
		return mcpTypedJSONResult("diagnose_sync_journal_aliases", output, output, true)
	}

	output := DiagnoseSyncJournalAliasesOutput{
		Scanned: scan.Scanned, Live: scan.Live, Orphan: scan.Orphan, SoftDeleted: scan.SoftDeleted,
		IdentityMismatch: scan.IdentityMismatch, Invalid: scan.Invalid, Issues: scan.Issues,
		Items: make([]MCPSyncJournalAliasDiagnostic, 0, min(limit, len(scan.Entries))),
	}
	for _, diagnosis := range scan.Entries {
		item := MCPSyncJournalAliasDiagnostic{
			PlanID: diagnosis.Alias.ReviewID, Status: string(diagnosis.Status), InUse: diagnosis.InUse,
		}
		switch diagnosis.Status {
		case syncjournalpkg.ReviewAliasDiagnosisOrphan:
			repairID, repairErr := syncjournalpkg.ReviewAliasRepairID(diagnosis.Alias)
			if repairErr != nil {
				failed := DiagnoseSyncJournalAliasesOutput{Items: []MCPSyncJournalAliasDiagnostic{}, ErrorCode: "journal_alias_diagnosis_incomplete", Error: "persistent sync journal alias repair snapshot could not be verified safely"}
				return mcpTypedJSONResult("diagnose_sync_journal_aliases", failed, failed, true)
			}
			item.RepairID = repairID
		case syncjournalpkg.ReviewAliasDiagnosisIdentityMismatch:
			item.ErrorCode = "journal_alias_conflict"
			item.Error = "reviewed alias does not match the target journal's content-addressed plan identity"
		case syncjournalpkg.ReviewAliasDiagnosisInvalidTarget:
			item.ErrorCode = "journal_read_failed"
			item.Error = "persistent sync journal alias target could not be read safely"
			if errors.Is(diagnosis.Err, syncjournalpkg.ErrMigrationRequired) {
				item.ErrorCode = "journal_migration_required"
				item.Error = "persistent sync journal alias target requires CLI migration"
			}
		}
		if len(output.Items) < limit {
			output.Items = append(output.Items, item)
		}
	}
	output.Returned = len(output.Items)
	return mcpTypedJSONResult("diagnose_sync_journal_aliases", output, output, false)
}
