package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultMCPListSyncExecutionsLimit = 50
	maxMCPListSyncExecutionsLimit     = 128
)

type ListSyncExecutionsArgs struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum results to return; default 50, maximum 128"`
	Status string `json:"status,omitempty" jsonschema:"optional effective journal status filter: active, failed, completed, reconcile-required, or recovery-required"`
}

type MCPSyncExecutionListItem struct {
	PlanID            string `json:"plan_id" jsonschema:"reviewed MCP plan_sync plan_id; never the raw internal journal plan ID"`
	Version           int    `json:"journal_version"`
	State             string `json:"state"`
	Status            string `json:"status"`
	Direction         string `json:"direction"`
	ConflictPolicy    string `json:"conflict_policy"`
	DeleteExtraneous  bool   `json:"delete"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
	StaleForMillis    int64  `json:"stale_for_ms"`
	Total             int    `json:"total"`
	Completed         int    `json:"completed"`
	Pending           int    `json:"pending"`
	Failed            int    `json:"failed"`
	Blocked           int    `json:"blocked"`
	RecoveryRequired  bool   `json:"recovery_required"`
	ReconcileRequired bool   `json:"reconcile_required"`
	InUse             bool   `json:"in_use"`
	Runs              int    `json:"runs"`
	ResumeRuns        int    `json:"resume_runs"`
	InterruptedRuns   int    `json:"interrupted_runs"`
}

type ListSyncExecutionsOutput struct {
	Returned          int                        `json:"returned"`
	MigrationRequired int                        `json:"migration_required" jsonschema:"number of readable legacy journals omitted because CLI migration is required"`
	Items             []MCPSyncExecutionListItem `json:"items"`
	ErrorCode         string                     `json:"error_code,omitempty"`
	Error             string                     `json:"error,omitempty" jsonschema:"sanitized control-plane error"`
}

func normalizeListSyncExecutionsArgs(args ListSyncExecutionsArgs) (int, string, error) {
	limit := args.Limit
	if limit < 0 {
		return 0, "", fmt.Errorf("limit must be >= 0")
	}
	if limit == 0 {
		limit = defaultMCPListSyncExecutionsLimit
	}
	if limit > maxMCPListSyncExecutionsLimit {
		return 0, "", fmt.Errorf("limit must not exceed %d", maxMCPListSyncExecutionsLimit)
	}
	status := strings.ToLower(strings.TrimSpace(args.Status))
	switch status {
	case "", syncjournalpkg.StatusActive, syncjournalpkg.StatusFailed, syncjournalpkg.StatusCompleted, syncjournalpkg.StatusReconcileRequired, syncjournalpkg.StatusRecoveryRequired:
		return limit, status, nil
	default:
		return 0, "", fmt.Errorf("unsupported status filter %q", status)
	}
}

func mcpSyncExecutionListItem(reviewedPlanID string, entry syncjournalpkg.ListEntry) MCPSyncExecutionListItem {
	return MCPSyncExecutionListItem{
		PlanID: reviewedPlanID, Version: entry.Version, State: entry.State, Status: entry.Status,
		Direction: entry.Direction, ConflictPolicy: entry.ConflictPolicy, DeleteExtraneous: entry.DeleteExtraneous,
		CreatedAt: entry.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: entry.UpdatedAt.UTC().Format(time.RFC3339Nano),
		StaleForMillis: entry.StaleForMillis, Total: entry.Total, Completed: entry.Completed, Pending: entry.Pending,
		Failed: entry.Failed, Blocked: entry.Blocked, RecoveryRequired: entry.RecoveryRequired,
		ReconcileRequired: entry.ReconcileRequired, InUse: entry.InUse,
		Runs: entry.RunStats.Runs, ResumeRuns: entry.RunStats.ResumeRuns, InterruptedRuns: entry.RunStats.InterruptedRuns,
	}
}

func (ft *FileTools) listSyncExecutions(ctx context.Context, req *mcp.CallToolRequest, args ListSyncExecutionsArgs) (*mcp.CallToolResult, ListSyncExecutionsOutput, error) {
	limit, status, err := normalizeListSyncExecutionsArgs(args)
	if err != nil {
		return toolError(err.Error()), ListSyncExecutionsOutput{}, nil
	}
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil {
		output := ListSyncExecutionsOutput{Items: []MCPSyncExecutionListItem{}, ErrorCode: "journal_unavailable", Error: "persistent sync journal store is unavailable"}
		return mcpTypedJSONResult("list_sync_executions", output, output, true)
	}
	if store == nil {
		output := ListSyncExecutionsOutput{Items: []MCPSyncExecutionListItem{}}
		return mcpTypedJSONResult("list_sync_executions", output, output, false)
	}
	scan, err := store.ScanCurrent(maxMCPSyncJournalScan)
	if err != nil {
		output := ListSyncExecutionsOutput{Items: []MCPSyncExecutionListItem{}, ErrorCode: "journal_read_failed", Error: "persistent sync journals could not be read safely"}
		if errors.Is(err, syncjournalpkg.ErrScanLimit) {
			output.ErrorCode = "journal_scan_limit_exceeded"
			output.Error = "persistent sync journal scan exceeded its safety limit"
		}
		return mcpTypedJSONResult("list_sync_executions", output, output, true)
	}
	output := ListSyncExecutionsOutput{MigrationRequired: scan.MigrationRequired, Items: make([]MCPSyncExecutionListItem, 0, min(limit, len(scan.Records)))}
	now := time.Now().UTC()
	for _, record := range scan.Records {
		entry := syncjournalpkg.BuildListEntry(record.Journal, now, record.InUse)
		if status != "" && entry.Status != status {
			continue
		}
		envelope, envelopeErr := buildMCPSyncPlanEnvelope(record.Journal.Plan)
		if envelopeErr != nil {
			failed := ListSyncExecutionsOutput{Items: []MCPSyncExecutionListItem{}, ErrorCode: "journal_read_failed", Error: "persistent sync journal could not be projected safely"}
			return mcpTypedJSONResult("list_sync_executions", failed, failed, true)
		}
		output.Items = append(output.Items, mcpSyncExecutionListItem(envelope.PlanID, entry))
		if len(output.Items) >= limit {
			break
		}
	}
	output.Returned = len(output.Items)
	return mcpTypedJSONResult("list_sync_executions", output, output, false)
}
