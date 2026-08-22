package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type ExecuteSyncJournalCleanupArgs struct {
	OlderThanHours  int64  `json:"older_than_hours,omitempty" jsonschema:"same minimum journal age used by plan_sync_journal_cleanup"`
	Limit           int    `json:"limit,omitempty" jsonschema:"same candidate limit used by plan_sync_journal_cleanup"`
	ExpectCleanupID string `json:"expect_cleanup_id" jsonschema:"required cleanup_id returned by plan_sync_journal_cleanup"`
}

func (args ExecuteSyncJournalCleanupArgs) planArgs() PlanSyncJournalCleanupArgs {
	return PlanSyncJournalCleanupArgs{OlderThanHours: args.OlderThanHours, Limit: args.Limit}
}

type MCPSyncJournalCleanupExecutionItem struct {
	Index  int    `json:"index"`
	PlanID string `json:"plan_id" jsonschema:"reviewed MCP plan_sync plan_id"`
	Status string `json:"status" jsonschema:"trashed, failed, or skipped"`
	Error  string `json:"error,omitempty" jsonschema:"sanitized cleanup item error"`
}

type ExecuteSyncJournalCleanupOutput struct {
	CleanupID string                               `json:"cleanup_id,omitempty"`
	Requested int                                  `json:"requested"`
	Trashed   int                                  `json:"trashed"`
	Failed    int                                  `json:"failed"`
	Skipped   int                                  `json:"skipped"`
	Partial   bool                                 `json:"partial"`
	Items     []MCPSyncJournalCleanupExecutionItem `json:"items"`
	ErrorCode string                               `json:"error_code,omitempty"`
	Error     string                               `json:"error,omitempty" jsonschema:"sanitized cleanup execution error"`
}

type mcpSyncJournalCleanupRawCandidate struct {
	reviewID  string
	planID    string
	state     string
	updatedAt time.Time
}

func normalizeMCPExpectedCleanupID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(strings.ToLower(value), prefix) {
		return "", fmt.Errorf("expect_cleanup_id must use sha256:<64 hex> format")
	}
	raw := value[len(prefix):]
	if len(raw) != 64 {
		return "", fmt.Errorf("expect_cleanup_id must use sha256:<64 hex> format")
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("expect_cleanup_id must use sha256:<64 hex> format")
	}
	return prefix + strings.ToLower(raw), nil
}

func resolveMCPSyncJournalCleanupRawCandidates(store syncjournalpkg.Store, now time.Time, preview PlanSyncJournalCleanupOutput) ([]mcpSyncJournalCleanupRawCandidate, error) {
	if preview.Selected != len(preview.Items) {
		return nil, errors.New("cleanup preview selected count is inconsistent")
	}
	scan, err := store.ScanCurrent(maxMCPSyncJournalScan)
	if err != nil {
		return nil, err
	}
	type projected struct {
		rawID     string
		state     string
		updatedAt time.Time
	}
	byReview := make(map[string]projected, len(scan.Records))
	for _, record := range scan.Records {
		envelope, envelopeErr := buildMCPSyncPlanEnvelope(record.Journal.Plan)
		if envelopeErr != nil {
			return nil, envelopeErr
		}
		if _, duplicate := byReview[envelope.PlanID]; duplicate {
			return nil, errors.New("multiple current journals project to one reviewed plan ID")
		}
		byReview[envelope.PlanID] = projected{
			rawID: record.Journal.PlanID, state: record.Journal.State, updatedAt: record.Journal.UpdatedAt,
		}
	}
	resolved := make([]mcpSyncJournalCleanupRawCandidate, 0, len(preview.Items))
	for _, item := range preview.Items {
		current, ok := byReview[item.PlanID]
		if !ok {
			return nil, syncjournalpkg.ErrCleanupCandidateChanged
		}
		updatedAt, err := time.Parse(time.RFC3339Nano, item.UpdatedAt)
		if err != nil || !current.updatedAt.Equal(updatedAt) || current.state != item.State {
			return nil, syncjournalpkg.ErrCleanupCandidateChanged
		}
		resolved = append(resolved, mcpSyncJournalCleanupRawCandidate{
			reviewID: item.PlanID, planID: current.rawID, state: current.state, updatedAt: current.updatedAt,
		})
	}
	_ = now // retained in the signature so caller and preview use one review instant.
	return resolved, nil
}

func executeSyncJournalCleanupCallResult(output ExecuteSyncJournalCleanupOutput) (*mcp.CallToolResult, ExecuteSyncJournalCleanupOutput, error) {
	isError := output.ErrorCode != "" || output.Failed > 0 || output.Skipped > 0
	return mcpTypedJSONResult("execute_sync_journal_cleanup", output, output, isError)
}

func syncJournalCleanupExecutionError(code, message, expectedID string) (*mcp.CallToolResult, ExecuteSyncJournalCleanupOutput, error) {
	output := ExecuteSyncJournalCleanupOutput{
		CleanupID: expectedID, Items: []MCPSyncJournalCleanupExecutionItem{}, ErrorCode: code, Error: message,
	}
	return executeSyncJournalCleanupCallResult(output)
}

func (ft *FileTools) executeSyncJournalCleanup(ctx context.Context, req *mcp.CallToolRequest, args ExecuteSyncJournalCleanupArgs) (*mcp.CallToolResult, ExecuteSyncJournalCleanupOutput, error) {
	expectedID, err := normalizeMCPExpectedCleanupID(args.ExpectCleanupID)
	if err != nil {
		return toolError(err.Error()), ExecuteSyncJournalCleanupOutput{}, nil
	}
	if expectedID == "" {
		return toolError("expect_cleanup_id is required"), ExecuteSyncJournalCleanupOutput{}, nil
	}
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil || store == nil {
		return syncJournalCleanupExecutionError("journal_unavailable", "persistent sync journal store is unavailable", expectedID)
	}
	// Purge only previously expired shared Session Store state/trash before the
	// scope cleanup guard is acquired. Current sync journals still require the
	// reviewed cleanup_id path below.
	_ = ft.runMCPSessionOpportunisticGC()
	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		switch {
		case errors.Is(err, syncjournalpkg.ErrCleanupMigrationInProgress):
			return syncJournalCleanupExecutionError("journal_migration_in_progress", "sync journal cleanup is disabled while a bulk migration marker exists", expectedID)
		case errors.Is(err, transfer.ErrSessionLocked):
			return syncJournalCleanupExecutionError("journal_cleanup_in_use", "sync journal cleanup or bulk migration is already in progress", expectedID)
		default:
			return syncJournalCleanupExecutionError("journal_cleanup_unavailable", "sync journal cleanup lock could not be acquired safely", expectedID)
		}
	}
	defer guard.Close()

	now := time.Now().UTC()
	preview, err := planMCPSyncJournalCleanup(*store, now, args.planArgs())
	if err != nil {
		if errors.Is(err, syncjournalpkg.ErrScanLimit) {
			return syncJournalCleanupExecutionError("journal_scan_limit_exceeded", "persistent sync journal scan exceeded its safety limit", expectedID)
		}
		return syncJournalCleanupExecutionError("journal_cleanup_plan_failed", "persistent sync journal cleanup preview could not be rebuilt safely", expectedID)
	}
	if preview.CleanupID != expectedID {
		return syncJournalCleanupExecutionError("cleanup_changed", "sync journal cleanup candidates changed; run plan_sync_journal_cleanup again", expectedID)
	}
	rawCandidates, err := resolveMCPSyncJournalCleanupRawCandidates(*store, now, preview)
	if err != nil {
		return syncJournalCleanupExecutionError("cleanup_changed", "sync journal cleanup candidates changed; run plan_sync_journal_cleanup again", expectedID)
	}

	retention := time.Duration(preview.RetentionMillis) * time.Millisecond
	output := ExecuteSyncJournalCleanupOutput{
		CleanupID: expectedID, Requested: len(rawCandidates), Items: make([]MCPSyncJournalCleanupExecutionItem, 0, len(rawCandidates)),
	}
	for index, candidate := range rawCandidates {
		_, trashErr := store.TrashCurrentReviewed(guard, candidate.reviewID, candidate.planID, candidate.state, candidate.updatedAt, retention, now)
		if trashErr == nil {
			output.Trashed++
			output.Items = append(output.Items, MCPSyncJournalCleanupExecutionItem{Index: index, PlanID: candidate.reviewID, Status: "trashed"})
			continue
		}
		code := "journal_cleanup_failed"
		message := "sync journal could not be moved to session trash safely"
		switch {
		case errors.Is(trashErr, syncjournalpkg.ErrCleanupCandidateChanged):
			code = "cleanup_candidate_changed"
			message = "a reviewed cleanup candidate changed before it could be trashed"
		case errors.Is(trashErr, syncjournalpkg.ErrCleanupMigrationInProgress):
			code = "journal_migration_in_progress"
			message = "bulk sync journal migration started before cleanup completed"
		case errors.Is(trashErr, transfer.ErrSessionLocked), errors.Is(trashErr, syncjournalpkg.ErrReviewAliasInUse):
			code = "journal_in_use"
			message = "a reviewed cleanup candidate became active before it could be trashed"
		case errors.Is(trashErr, syncjournalpkg.ErrReviewAliasConflict):
			code = "journal_alias_conflict"
			message = "a reviewed cleanup candidate has a conflicting persistent review binding"
		}
		output.Failed++
		output.ErrorCode = code
		output.Error = message
		output.Items = append(output.Items, MCPSyncJournalCleanupExecutionItem{Index: index, PlanID: candidate.reviewID, Status: "failed", Error: message})
		for skippedIndex := index + 1; skippedIndex < len(rawCandidates); skippedIndex++ {
			output.Skipped++
			output.Items = append(output.Items, MCPSyncJournalCleanupExecutionItem{
				Index: skippedIndex, PlanID: rawCandidates[skippedIndex].reviewID, Status: "skipped", Error: "not attempted after an earlier cleanup failure",
			})
		}
		break
	}
	output.Partial = output.Trashed > 0 && (output.Failed > 0 || output.Skipped > 0)
	return executeSyncJournalCleanupCallResult(output)
}
