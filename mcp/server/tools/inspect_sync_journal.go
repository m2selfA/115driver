package tools

import (
	"context"
	"errors"
	"strings"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxMCPSyncJournalScan = 4096

type InspectSyncJournalArgs struct {
	PlanID string `json:"plan_id" jsonschema:"reviewed MCPPlan v1 plan_id in sha256:<64 hex> form"`
}

type MCPSyncJournalItemView struct {
	Index        int    `json:"index"`
	RelativePath string `json:"relative_path"`
	Action       string `json:"action"`
	Kind         string `json:"kind"`
	State        string `json:"state"`
	Phase        string `json:"phase"`
	Attempts     int    `json:"attempts"`
	HasError     bool   `json:"has_error"`
}

type MCPSyncJournalInspection struct {
	Found             bool                     `json:"found"`
	PlanID            string                   `json:"plan_id,omitempty"`
	Schema            string                   `json:"schema,omitempty"`
	Version           int                      `json:"version,omitempty"`
	State             string                   `json:"state,omitempty"`
	Status            string                   `json:"status,omitempty"`
	RecoveryRequired  bool                     `json:"recovery_required"`
	ReconcileRequired bool                     `json:"reconcile_required"`
	InUse             bool                     `json:"in_use"`
	Completed         int                      `json:"completed"`
	Pending           int                      `json:"pending"`
	Running           int                      `json:"running"`
	Failed            int                      `json:"failed"`
	Skipped           int                      `json:"skipped"`
	TotalAttempts     int                      `json:"total_attempts"`
	Runs              int                      `json:"runs"`
	ResumeRuns        int                      `json:"resume_runs"`
	InterruptedRuns   int                      `json:"interrupted_runs"`
	Items             []MCPSyncJournalItemView `json:"items,omitempty"`
	ErrorCode         string                   `json:"error_code,omitempty"`
	Error             string                   `json:"error,omitempty" jsonschema:"sanitized journal inspection error"`
}

func inspectSyncJournalView(reviewedPlanID string, journal syncjournalpkg.Journal, inUse bool) MCPSyncJournalInspection {
	output := MCPSyncJournalInspection{
		Found:             true,
		PlanID:            reviewedPlanID,
		Schema:            journal.Schema,
		Version:           journal.Version,
		State:             journal.State,
		Status:            journal.Status,
		RecoveryRequired:  syncjournalpkg.RecoveryRequired(journal),
		ReconcileRequired: syncjournalpkg.ReconciliationRequired(journal),
		InUse:             inUse,
		Runs:              journal.RunStats.Runs,
		ResumeRuns:        journal.RunStats.ResumeRuns,
		InterruptedRuns:   journal.RunStats.InterruptedRuns,
		Items:             make([]MCPSyncJournalItemView, 0, len(journal.Items)),
	}
	for index, stored := range journal.Items {
		planned := journal.Plan.Items[index]
		view := MCPSyncJournalItemView{
			Index: index, RelativePath: planned.RelativePath, Action: planned.Action, Kind: planned.Kind,
			State: stored.State, Phase: stored.Phase, Attempts: stored.Attempts, HasError: strings.TrimSpace(stored.LastError) != "",
		}
		output.TotalAttempts += stored.Attempts
		switch stored.State {
		case "succeeded":
			output.Completed++
		case "skipped":
			output.Completed++
			output.Skipped++
		case "running":
			output.Running++
		case "failed":
			output.Failed++
		default:
			output.Pending++
		}
		output.Items = append(output.Items, view)
	}
	return output
}

func (ft *FileTools) inspectSyncJournal(ctx context.Context, req *mcp.CallToolRequest, args InspectSyncJournalArgs) (*mcp.CallToolResult, MCPSyncJournalInspection, error) {
	reviewedPlanID, err := normalizeMCPExpectedPlanID(args.PlanID)
	if err != nil || reviewedPlanID == "" {
		return mcpTypedJSONResult("inspect_sync_journal", MCPSyncJournalInspection{
			ErrorCode: "invalid_plan_id", Error: "plan_id must use sha256:<64 hex> format",
		}, MCPSyncJournalInspection{ErrorCode: "invalid_plan_id", Error: "plan_id must use sha256:<64 hex> format"}, true)
	}
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil {
		output := MCPSyncJournalInspection{PlanID: reviewedPlanID, ErrorCode: "journal_unavailable", Error: "sync journal store is unavailable"}
		return mcpTypedJSONResult("inspect_sync_journal", output, output, true)
	}
	if store == nil {
		output := MCPSyncJournalInspection{PlanID: reviewedPlanID, ErrorCode: "journal_store_disabled", Error: "sync journal store is not configured for this MCP server"}
		return mcpTypedJSONResult("inspect_sync_journal", output, output, true)
	}
	internalPlanID, aliasErr := store.ResolveReviewAlias(reviewedPlanID)
	if aliasErr == nil {
		record, recordErr := store.InspectCurrentRecord(internalPlanID)
		if recordErr == nil {
			output := inspectSyncJournalView(reviewedPlanID, record.Journal, record.InUse)
			return mcpTypedJSONResult("inspect_sync_journal", output, output, false)
		}
		switch {
		case errors.Is(recordErr, syncjournalpkg.ErrNotFound):
			// Keep inspection read-only but do not let an orphan alias mask a
			// compatible pre-alias current-v2 journal. Fall through to the same
			// bounded projection scan used when no alias exists.
		case errors.Is(recordErr, syncjournalpkg.ErrBindingMismatch):
			output := MCPSyncJournalInspection{Found: false, PlanID: reviewedPlanID, ErrorCode: "journal_not_found", Error: "no current persistent sync journal exists for the reviewed plan"}
			return mcpTypedJSONResult("inspect_sync_journal", output, output, true)
		case errors.Is(recordErr, syncjournalpkg.ErrMigrationRequired):
			output := MCPSyncJournalInspection{Found: true, PlanID: reviewedPlanID, ErrorCode: "journal_migration_required", Error: "the sync journal uses an older schema; migrate it with the 115driver CLI"}
			return mcpTypedJSONResult("inspect_sync_journal", output, output, true)
		default:
			output := MCPSyncJournalInspection{PlanID: reviewedPlanID, ErrorCode: "journal_read_failed", Error: "persistent sync journal could not be read safely"}
			return mcpTypedJSONResult("inspect_sync_journal", output, output, true)
		}
	}
	if aliasErr != nil && !errors.Is(aliasErr, syncjournalpkg.ErrNotFound) {
		// Binding mismatches are intentionally indistinguishable from absence so
		// this read-only tool does not become a cross-account existence oracle.
		if errors.Is(aliasErr, syncjournalpkg.ErrBindingMismatch) {
			output := MCPSyncJournalInspection{Found: false, PlanID: reviewedPlanID, ErrorCode: "journal_not_found", Error: "no current persistent sync journal exists for the reviewed plan"}
			return mcpTypedJSONResult("inspect_sync_journal", output, output, true)
		}
		output := MCPSyncJournalInspection{PlanID: reviewedPlanID, ErrorCode: "journal_read_failed", Error: "persistent sync journal binding could not be read safely"}
		return mcpTypedJSONResult("inspect_sync_journal", output, output, true)
	}

	// Compatibility fallback for current-v2 journals created before review
	// aliases existed. It is bounded and read-only; new executions always write
	// an alias so normal inspection no longer scales with total journal count.
	scan, err := store.ScanCurrent(maxMCPSyncJournalScan)
	if err != nil {
		code := "journal_read_failed"
		message := "persistent sync journals could not be read safely"
		if errors.Is(err, syncjournalpkg.ErrScanLimit) {
			code = "journal_scan_limit_exceeded"
			message = "persistent sync journal scan exceeded its safety limit"
		}
		output := MCPSyncJournalInspection{PlanID: reviewedPlanID, ErrorCode: code, Error: message}
		return mcpTypedJSONResult("inspect_sync_journal", output, output, true)
	}
	var matched *syncjournalpkg.CurrentRecord
	for index := range scan.Records {
		envelope, envelopeErr := buildMCPSyncPlanEnvelope(scan.Records[index].Journal.Plan)
		if envelopeErr != nil {
			output := MCPSyncJournalInspection{PlanID: reviewedPlanID, ErrorCode: "journal_read_failed", Error: "persistent sync journal could not be projected safely"}
			return mcpTypedJSONResult("inspect_sync_journal", output, output, true)
		}
		if envelope.PlanID != reviewedPlanID {
			continue
		}
		if matched != nil {
			output := MCPSyncJournalInspection{PlanID: reviewedPlanID, ErrorCode: "journal_ambiguous", Error: "multiple persistent sync journals matched the reviewed plan"}
			return mcpTypedJSONResult("inspect_sync_journal", output, output, true)
		}
		matched = &scan.Records[index]
	}
	if matched == nil {
		output := MCPSyncJournalInspection{Found: false, PlanID: reviewedPlanID, ErrorCode: "journal_not_found", Error: "no current persistent sync journal exists for the reviewed plan"}
		return mcpTypedJSONResult("inspect_sync_journal", output, output, true)
	}
	output := inspectSyncJournalView(reviewedPlanID, matched.Journal, matched.InUse)
	return mcpTypedJSONResult("inspect_sync_journal", output, output, false)
}
