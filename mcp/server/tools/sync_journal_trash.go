package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultMCPSyncJournalTrashListLimit = 50
	maxMCPSyncJournalTrashListLimit     = 128
)

type ListSyncJournalTrashArgs struct {
	Limit int `json:"limit,omitempty" jsonschema:"maximum trashed sync journals to return; default 50, maximum 128"`
}

type MCPSyncJournalTrashItem struct {
	PlanID            string `json:"plan_id" jsonschema:"reviewed MCP plan_sync plan_id"`
	RestoreID         string `json:"restore_id" jsonschema:"content-addressed token for this exact trashed journal snapshot"`
	State             string `json:"state"`
	Status            string `json:"status"`
	TrashedAt         string `json:"trashed_at"`
	UpdatedAt         string `json:"updated_at"`
	TrashAgeMS        int64  `json:"trash_age_ms"`
	TrashRetentionMS  int64  `json:"trash_retention_ms"`
	PurgeEligibleAt   string `json:"purge_eligible_at" jsonschema:"time this entry becomes eligible for opportunistic Session Store GC; not a guaranteed deletion time"`
	PurgeEligible     bool   `json:"purge_eligible"`
	TotalItems        int    `json:"total_items"`
	Runs              int    `json:"runs"`
	ResumeRuns        int    `json:"resume_runs"`
	RecoveryRequired  bool   `json:"recovery_required"`
	ReconcileRequired bool   `json:"reconcile_required"`
}

type ListSyncJournalTrashOutput struct {
	Scanned           int                       `json:"scanned"`
	Returned          int                       `json:"returned"`
	MigrationRequired int                       `json:"migration_required"`
	InvalidSkipped    int                       `json:"invalid_skipped"`
	Items             []MCPSyncJournalTrashItem `json:"items"`
	ErrorCode         string                    `json:"error_code,omitempty"`
	Error             string                    `json:"error,omitempty" jsonschema:"sanitized trash listing error"`
}

type mcpSyncJournalRestoreFingerprint struct {
	Schema          string   `json:"schema"`
	ReviewedPlan    string   `json:"reviewed_plan_id"`
	RawPlanID       string   `json:"raw_plan_id"`
	TrashName       string   `json:"trash_name"`
	JournalState    string   `json:"journal_state"`
	JournalStatus   string   `json:"journal_status"`
	UpdatedAt       int64    `json:"updated_at_unix_nano"`
	TrashedAt       int64    `json:"trashed_at_unix_nano"`
	ReviewedPlanIDs []string `json:"reviewed_plan_ids,omitempty"`
}

func normalizeMCPJournalTrashLimit(limit int) (int, error) {
	if limit < 0 {
		return 0, fmt.Errorf("limit must be >= 0")
	}
	if limit == 0 {
		return defaultMCPSyncJournalTrashListLimit, nil
	}
	if limit > maxMCPSyncJournalTrashListLimit {
		return 0, fmt.Errorf("limit must not exceed %d", maxMCPSyncJournalTrashListLimit)
	}
	return limit, nil
}

func mcpSyncJournalRestoreID(record syncjournalpkg.TrashedCurrentRecord, reviewedPlanID string) (string, error) {
	reviewedPlanID, err := normalizeMCPExpectedPlanID(reviewedPlanID)
	if err != nil || reviewedPlanID == "" {
		return "", errors.New("reviewed sync plan ID is invalid")
	}
	payload := mcpSyncJournalRestoreFingerprint{
		Schema: "115driver.mcp-sync-journal-restore/v1", ReviewedPlan: reviewedPlanID,
		RawPlanID: record.Journal.PlanID, TrashName: record.TrashName,
		JournalState: record.Journal.State, JournalStatus: record.Journal.Status,
		UpdatedAt: record.Journal.UpdatedAt.UnixNano(), TrashedAt: record.TrashedAt.UnixNano(),
		ReviewedPlanIDs: append([]string(nil), record.ReviewIDs...),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}

func projectMCPSyncJournalTrashItem(record syncjournalpkg.TrashedCurrentRecord, trashRetention time.Duration, now time.Time) (MCPSyncJournalTrashItem, error) {
	envelope, err := buildMCPSyncPlanEnvelope(record.Journal.Plan)
	if err != nil {
		return MCPSyncJournalTrashItem{}, err
	}
	restoreID, err := mcpSyncJournalRestoreID(record, envelope.PlanID)
	if err != nil {
		return MCPSyncJournalTrashItem{}, err
	}
	entry := syncjournalpkg.BuildListEntry(record.Journal, now, false)
	window := syncjournalpkg.BuildTrashRetentionWindow(record.TrashedAt, now, trashRetention)
	return MCPSyncJournalTrashItem{
		PlanID: envelope.PlanID, RestoreID: restoreID, State: record.Journal.State, Status: record.Journal.Status,
		TrashedAt: record.TrashedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: record.Journal.UpdatedAt.UTC().Format(time.RFC3339Nano),
		TrashAgeMS: window.Age.Milliseconds(), TrashRetentionMS: window.Retention.Milliseconds(),
		PurgeEligibleAt: window.EligibleAt.UTC().Format(time.RFC3339Nano), PurgeEligible: window.Eligible,
		TotalItems: len(record.Journal.Items), Runs: entry.RunStats.Runs, ResumeRuns: entry.RunStats.ResumeRuns,
		RecoveryRequired:  syncjournalpkg.RecoveryRequired(record.Journal),
		ReconcileRequired: syncjournalpkg.DestructiveReconciliationRequired(record.Journal) || syncjournalpkg.PostconditionVerificationRequired(record.Journal),
	}, nil
}

func listMCPSyncJournalTrash(store syncjournalpkg.Store, args ListSyncJournalTrashArgs) (ListSyncJournalTrashOutput, error) {
	limit, err := normalizeMCPJournalTrashLimit(args.Limit)
	if err != nil {
		return ListSyncJournalTrashOutput{}, err
	}
	scan, err := store.ScanTrashedCurrent(maxMCPSyncJournalScan)
	if err != nil {
		return ListSyncJournalTrashOutput{}, err
	}
	output := ListSyncJournalTrashOutput{
		Scanned: len(scan.Records), MigrationRequired: scan.MigrationRequired, InvalidSkipped: scan.Invalid,
		Items: make([]MCPSyncJournalTrashItem, 0, min(limit, len(scan.Records))),
	}
	now := time.Now().UTC()
	for index, record := range scan.Records {
		if index >= limit {
			break
		}
		item, err := projectMCPSyncJournalTrashItem(record, store.TrashRetention, now)
		if err != nil {
			return ListSyncJournalTrashOutput{}, fmt.Errorf("project trashed sync journal safely: %w", err)
		}
		output.Items = append(output.Items, item)
	}
	output.Returned = len(output.Items)
	return output, nil
}

func (ft *FileTools) listSyncJournalTrash(ctx context.Context, req *mcp.CallToolRequest, args ListSyncJournalTrashArgs) (*mcp.CallToolResult, ListSyncJournalTrashOutput, error) {
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil || store == nil {
		output := ListSyncJournalTrashOutput{Items: []MCPSyncJournalTrashItem{}, ErrorCode: "journal_unavailable", Error: "persistent sync journal store is unavailable"}
		return mcpTypedJSONResult("list_sync_journal_trash", output, output, true)
	}
	output, err := listMCPSyncJournalTrash(*store, args)
	if err != nil {
		code := "journal_trash_read_failed"
		message := "trashed sync journals could not be read safely"
		if errors.Is(err, syncjournalpkg.ErrTrashScanLimit) {
			code = "journal_trash_scan_limit_exceeded"
			message = "trashed sync journal scan exceeded its safety limit"
		}
		failed := ListSyncJournalTrashOutput{Items: []MCPSyncJournalTrashItem{}, ErrorCode: code, Error: message}
		return mcpTypedJSONResult("list_sync_journal_trash", failed, failed, true)
	}
	return mcpTypedJSONResult("list_sync_journal_trash", output, output, false)
}

type RestoreSyncJournalArgs struct {
	PlanID          string `json:"plan_id" jsonschema:"reviewed MCP plan_sync plan_id"`
	ExpectRestoreID string `json:"expect_restore_id" jsonschema:"required restore_id returned by list_sync_journal_trash"`
}

type RestoreSyncJournalOutput struct {
	PlanID            string `json:"plan_id,omitempty"`
	RestoreID         string `json:"restore_id,omitempty"`
	Restored          bool   `json:"restored"`
	State             string `json:"state,omitempty"`
	Status            string `json:"status,omitempty"`
	RecoveryRequired  bool   `json:"recovery_required"`
	ReconcileRequired bool   `json:"reconcile_required"`
	ErrorCode         string `json:"error_code,omitempty"`
	Error             string `json:"error,omitempty" jsonschema:"sanitized trash restore error"`
}

func normalizeMCPExpectedRestoreID(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(strings.ToLower(value), prefix) {
		return "", errors.New("expect_restore_id must use sha256:<64 hex> format")
	}
	raw := value[len(prefix):]
	if len(raw) != 64 {
		return "", errors.New("expect_restore_id must use sha256:<64 hex> format")
	}
	decoded, err := hex.DecodeString(raw)
	if err != nil || len(decoded) != sha256.Size {
		return "", errors.New("expect_restore_id must use sha256:<64 hex> format")
	}
	return prefix + strings.ToLower(raw), nil
}

func restoreSyncJournalCallResult(output RestoreSyncJournalOutput) (*mcp.CallToolResult, RestoreSyncJournalOutput, error) {
	return mcpTypedJSONResult("restore_sync_journal", output, output, output.ErrorCode != "")
}

func (ft *FileTools) restoreSyncJournal(ctx context.Context, req *mcp.CallToolRequest, args RestoreSyncJournalArgs) (*mcp.CallToolResult, RestoreSyncJournalOutput, error) {
	reviewedPlanID, err := normalizeMCPExpectedPlanID(args.PlanID)
	if err != nil || reviewedPlanID == "" {
		return toolError("plan_id must use sha256:<64 hex> format"), RestoreSyncJournalOutput{}, nil
	}
	expectedRestoreID, err := normalizeMCPExpectedRestoreID(args.ExpectRestoreID)
	if err != nil {
		return toolError(err.Error()), RestoreSyncJournalOutput{}, nil
	}
	if expectedRestoreID == "" {
		return toolError("expect_restore_id is required"), RestoreSyncJournalOutput{}, nil
	}
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil || store == nil {
		return restoreSyncJournalCallResult(RestoreSyncJournalOutput{PlanID: reviewedPlanID, RestoreID: expectedRestoreID, ErrorCode: "journal_unavailable", Error: "persistent sync journal store is unavailable"})
	}
	guard, err := store.AcquireCleanupGuard()
	if err != nil {
		code := "journal_restore_unavailable"
		message := "sync journal restore lock could not be acquired safely"
		switch {
		case errors.Is(err, syncjournalpkg.ErrCleanupMigrationInProgress):
			code = "journal_migration_in_progress"
			message = "sync journal restore is disabled while a bulk migration marker exists"
		case errors.Is(err, transfer.ErrSessionLocked):
			code = "journal_restore_in_use"
			message = "sync journal maintenance or bulk migration is already in progress"
		}
		return restoreSyncJournalCallResult(RestoreSyncJournalOutput{PlanID: reviewedPlanID, RestoreID: expectedRestoreID, ErrorCode: code, Error: message})
	}
	defer guard.Close()

	scan, err := store.ScanTrashedCurrent(maxMCPSyncJournalScan)
	if err != nil {
		return restoreSyncJournalCallResult(RestoreSyncJournalOutput{PlanID: reviewedPlanID, RestoreID: expectedRestoreID, ErrorCode: "journal_trash_read_failed", Error: "trashed sync journals could not be read safely"})
	}
	var matched *syncjournalpkg.TrashedCurrentRecord
	for index := range scan.Records {
		record := &scan.Records[index]
		envelope, envelopeErr := buildMCPSyncPlanEnvelope(record.Journal.Plan)
		if envelopeErr != nil {
			return restoreSyncJournalCallResult(RestoreSyncJournalOutput{PlanID: reviewedPlanID, RestoreID: expectedRestoreID, ErrorCode: "journal_trash_read_failed", Error: "trashed sync journal could not be projected safely"})
		}
		if envelope.PlanID != reviewedPlanID {
			continue
		}
		restoreID, restoreErr := mcpSyncJournalRestoreID(*record, reviewedPlanID)
		if restoreErr != nil {
			return restoreSyncJournalCallResult(RestoreSyncJournalOutput{PlanID: reviewedPlanID, RestoreID: expectedRestoreID, ErrorCode: "journal_trash_read_failed", Error: "trashed sync journal restore token could not be verified safely"})
		}
		if restoreID != expectedRestoreID {
			continue
		}
		if matched != nil {
			return restoreSyncJournalCallResult(RestoreSyncJournalOutput{PlanID: reviewedPlanID, RestoreID: expectedRestoreID, ErrorCode: "journal_restore_ambiguous", Error: "multiple trashed sync journals matched the reviewed restore token"})
		}
		matched = record
	}
	if matched == nil {
		return restoreSyncJournalCallResult(RestoreSyncJournalOutput{PlanID: reviewedPlanID, RestoreID: expectedRestoreID, ErrorCode: "restore_changed", Error: "trashed sync journal changed or no longer matches the reviewed restore token; list_sync_journal_trash again"})
	}
	restored, err := store.RestoreTrashedCurrentReviewed(
		guard, reviewedPlanID, matched.TrashName, matched.Journal.PlanID,
		matched.Journal.UpdatedAt, matched.TrashedAt, matched.ReviewIDs,
	)
	if err != nil {
		code := "journal_restore_failed"
		message := "trashed sync journal could not be restored safely"
		switch {
		case errors.Is(err, syncjournalpkg.ErrRestoreCurrentExists):
			code = "current_exists"
			message = "a current sync journal already exists for this reviewed restore"
		case errors.Is(err, syncjournalpkg.ErrTrashEntryChanged), errors.Is(err, syncjournalpkg.ErrNotFound):
			code = "restore_changed"
			message = "trashed sync journal changed before restore; list_sync_journal_trash again"
		case errors.Is(err, syncjournalpkg.ErrCleanupMigrationInProgress):
			code = "journal_migration_in_progress"
			message = "bulk sync journal migration started before restore completed"
		case errors.Is(err, transfer.ErrSessionLocked), errors.Is(err, syncjournalpkg.ErrReviewAliasInUse):
			code = "journal_in_use"
			message = "the sync journal or reviewed binding became active before restore completed"
		case errors.Is(err, syncjournalpkg.ErrReviewAliasConflict):
			code = "journal_alias_conflict"
			message = "the reviewed plan is already bound to a different persistent sync journal"
		}
		return restoreSyncJournalCallResult(RestoreSyncJournalOutput{PlanID: reviewedPlanID, RestoreID: expectedRestoreID, ErrorCode: code, Error: message})
	}
	output := RestoreSyncJournalOutput{
		PlanID: reviewedPlanID, RestoreID: expectedRestoreID, Restored: true,
		State: restored.State, Status: restored.Status,
		RecoveryRequired:  syncjournalpkg.RecoveryRequired(restored),
		ReconcileRequired: syncjournalpkg.DestructiveReconciliationRequired(restored) || syncjournalpkg.PostconditionVerificationRequired(restored),
	}
	return restoreSyncJournalCallResult(output)
}
