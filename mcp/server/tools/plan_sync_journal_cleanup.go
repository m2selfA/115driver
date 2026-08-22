package tools

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultMCPPlanSyncJournalCleanupLimit = 50
	maxMCPPlanSyncJournalCleanupLimit     = 128
	maxMCPPlanSyncJournalCleanupHours     = int64(10 * 365 * 24)
)

type PlanSyncJournalCleanupArgs struct {
	OlderThanHours int64 `json:"older_than_hours,omitempty" jsonschema:"minimum journal age in hours; 0 uses transfer.sessions.retention and then the historical 30-day fallback; maximum 87600"`
	Limit          int   `json:"limit,omitempty" jsonschema:"maximum eligible journals in this cleanup preview; default 50, maximum 128"`
}

type MCPSyncJournalCleanupCandidate struct {
	PlanID         string `json:"plan_id" jsonschema:"reviewed MCP plan_sync plan_id; never the raw internal journal plan ID"`
	State          string `json:"state"`
	UpdatedAt      string `json:"updated_at"`
	StaleForMillis int64  `json:"stale_for_ms"`
	TotalItems     int    `json:"total_items"`
	Runs           int    `json:"runs"`
	ResumeRuns     int    `json:"resume_runs"`
}

type PlanSyncJournalCleanupOutput struct {
	CleanupID         string                           `json:"cleanup_id,omitempty" jsonschema:"content-addressed sha256 review token for this exact cleanup candidate set"`
	RetentionMillis   int64                            `json:"retention_ms"`
	Limit             int                              `json:"limit"`
	ScannedCurrent    int                              `json:"scanned_current"`
	MigrationRequired int                              `json:"migration_required"`
	Eligible          int                              `json:"eligible"`
	Selected          int                              `json:"selected"`
	Items             []MCPSyncJournalCleanupCandidate `json:"items"`
	ErrorCode         string                           `json:"error_code,omitempty"`
	Error             string                           `json:"error,omitempty" jsonschema:"sanitized cleanup planning error"`
}

type mcpSyncJournalCleanupFingerprint struct {
	Schema          string                                 `json:"schema"`
	RetentionMillis int64                                  `json:"retention_ms"`
	Limit           int                                    `json:"limit"`
	Items           []mcpSyncJournalCleanupFingerprintItem `json:"items"`
}

type mcpSyncJournalCleanupFingerprintItem struct {
	PlanID    string `json:"plan_id"`
	State     string `json:"state"`
	UpdatedAt int64  `json:"updated_at_unix_nano"`
}

func normalizePlanSyncJournalCleanupArgs(args PlanSyncJournalCleanupArgs, configuredRetention time.Duration) (time.Duration, int, error) {
	if args.OlderThanHours < 0 {
		return 0, 0, fmt.Errorf("older_than_hours must be >= 0")
	}
	if args.OlderThanHours > maxMCPPlanSyncJournalCleanupHours {
		return 0, 0, fmt.Errorf("older_than_hours must not exceed %d", maxMCPPlanSyncJournalCleanupHours)
	}
	requested := time.Duration(0)
	if args.OlderThanHours > 0 {
		requested = time.Duration(args.OlderThanHours) * time.Hour
	}
	retention := syncjournalpkg.ResolveGCRetention(requested, configuredRetention)
	limit := args.Limit
	if limit < 0 {
		return 0, 0, fmt.Errorf("limit must be >= 0")
	}
	if limit == 0 {
		limit = defaultMCPPlanSyncJournalCleanupLimit
	}
	if limit > maxMCPPlanSyncJournalCleanupLimit {
		return 0, 0, fmt.Errorf("limit must not exceed %d", maxMCPPlanSyncJournalCleanupLimit)
	}
	return retention, limit, nil
}

func mcpSyncJournalCleanupID(retention time.Duration, limit int, items []mcpSyncJournalCleanupFingerprintItem) (string, error) {
	payload := mcpSyncJournalCleanupFingerprint{
		Schema: "115driver.mcp-sync-journal-cleanup/v1", RetentionMillis: retention.Milliseconds(), Limit: limit,
		Items: append([]mcpSyncJournalCleanupFingerprintItem(nil), items...),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:]), nil
}

func planMCPSyncJournalCleanup(store syncjournalpkg.Store, now time.Time, args PlanSyncJournalCleanupArgs) (PlanSyncJournalCleanupOutput, error) {
	retention, limit, err := normalizePlanSyncJournalCleanupArgs(args, store.Retention)
	if err != nil {
		return PlanSyncJournalCleanupOutput{}, err
	}
	markerPresent, err := store.MigrationBatchMarkerPresent()
	if err != nil {
		return PlanSyncJournalCleanupOutput{}, err
	}
	if markerPresent {
		return PlanSyncJournalCleanupOutput{}, syncjournalpkg.ErrCleanupMigrationInProgress
	}
	scan, err := store.ScanCurrent(maxMCPSyncJournalScan)
	if err != nil {
		return PlanSyncJournalCleanupOutput{}, err
	}
	now = now.UTC()
	entries := make([]syncjournalpkg.ListEntry, 0, len(scan.Records))
	records := make(map[string]syncjournalpkg.CurrentRecord, len(scan.Records))
	for _, record := range scan.Records {
		entry := syncjournalpkg.BuildListEntry(record.Journal, now, record.InUse)
		entries = append(entries, entry)
		records[entry.PlanID] = record
	}
	candidates := syncjournalpkg.SelectGCCandidates(entries, now, retention, nil)
	sort.Slice(candidates, func(i, j int) bool {
		left := records[candidates[i].PlanID].Journal.UpdatedAt
		right := records[candidates[j].PlanID].Journal.UpdatedAt
		if left.Equal(right) {
			return candidates[i].PlanID < candidates[j].PlanID
		}
		return left.Before(right)
	})
	output := PlanSyncJournalCleanupOutput{
		RetentionMillis: retention.Milliseconds(), Limit: limit, ScannedCurrent: len(scan.Records),
		MigrationRequired: scan.MigrationRequired, Eligible: len(candidates),
		Items: make([]MCPSyncJournalCleanupCandidate, 0, min(limit, len(candidates))),
	}
	fingerprintItems := make([]mcpSyncJournalCleanupFingerprintItem, 0, min(limit, len(candidates)))
	for index, candidate := range candidates {
		if index >= limit {
			break
		}
		record := records[candidate.PlanID]
		envelope, envelopeErr := buildMCPSyncPlanEnvelope(record.Journal.Plan)
		if envelopeErr != nil {
			return PlanSyncJournalCleanupOutput{}, fmt.Errorf("project cleanup candidate safely: %w", envelopeErr)
		}
		entry := syncjournalpkg.BuildListEntry(record.Journal, now, record.InUse)
		output.Items = append(output.Items, MCPSyncJournalCleanupCandidate{
			PlanID: envelope.PlanID, State: candidate.State, UpdatedAt: entry.UpdatedAt.UTC().Format(time.RFC3339Nano),
			StaleForMillis: entry.StaleForMillis, TotalItems: entry.Total,
			Runs: entry.RunStats.Runs, ResumeRuns: entry.RunStats.ResumeRuns,
		})
		fingerprintItems = append(fingerprintItems, mcpSyncJournalCleanupFingerprintItem{
			PlanID: envelope.PlanID, State: candidate.State, UpdatedAt: record.Journal.UpdatedAt.UnixNano(),
		})
	}
	output.Selected = len(output.Items)
	cleanupID, err := mcpSyncJournalCleanupID(retention, limit, fingerprintItems)
	if err != nil {
		return PlanSyncJournalCleanupOutput{}, err
	}
	output.CleanupID = cleanupID
	return output, nil
}

func (ft *FileTools) planSyncJournalCleanup(ctx context.Context, req *mcp.CallToolRequest, args PlanSyncJournalCleanupArgs) (*mcp.CallToolResult, PlanSyncJournalCleanupOutput, error) {
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil {
		output := PlanSyncJournalCleanupOutput{Items: []MCPSyncJournalCleanupCandidate{}, ErrorCode: "journal_unavailable", Error: "persistent sync journal store is unavailable"}
		return mcpTypedJSONResult("plan_sync_journal_cleanup", output, output, true)
	}
	if store == nil {
		output := PlanSyncJournalCleanupOutput{Items: []MCPSyncJournalCleanupCandidate{}, ErrorCode: "journal_store_disabled", Error: "persistent sync journal store is not configured"}
		return mcpTypedJSONResult("plan_sync_journal_cleanup", output, output, true)
	}
	output, err := planMCPSyncJournalCleanup(*store, time.Now().UTC(), args)
	if err != nil {
		code := "journal_cleanup_plan_failed"
		message := "persistent sync journal cleanup preview could not be built safely"
		switch {
		case errors.Is(err, syncjournalpkg.ErrScanLimit):
			code = "journal_scan_limit_exceeded"
			message = "persistent sync journal scan exceeded its safety limit"
		case errors.Is(err, syncjournalpkg.ErrCleanupMigrationInProgress):
			code = "journal_migration_in_progress"
			message = "sync journal cleanup is disabled while a bulk migration marker exists"
		}
		failed := PlanSyncJournalCleanupOutput{Items: []MCPSyncJournalCleanupCandidate{}, ErrorCode: code, Error: message}
		return mcpTypedJSONResult("plan_sync_journal_cleanup", failed, failed, true)
	}
	return mcpTypedJSONResult("plan_sync_journal_cleanup", output, output, false)
}
