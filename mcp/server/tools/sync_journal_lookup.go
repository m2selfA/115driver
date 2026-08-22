package tools

import (
	"context"
	"errors"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
)

type mcpSyncJournalLookupResult struct {
	Record         *syncjournalpkg.CurrentRecord
	ReviewedPlanID string
	ErrorCode      string
	Error          string
}

func (result mcpSyncJournalLookupResult) found() bool {
	return result.Record != nil && result.ErrorCode == ""
}

// lookupMCPSyncJournal resolves the externally reviewed MCP plan ID to the
// profile/account-bound raw current journal without ever returning the raw ID
// to tool callers. New executions use the private review alias; older current-v2
// journals fall back to the bounded generic-plan projection scan.
func (ft *FileTools) lookupMCPSyncJournal(ctx context.Context, reviewedPlanID string) mcpSyncJournalLookupResult {
	store, err := ft.resolveSyncJournalStore(ctx)
	if err != nil {
		return mcpSyncJournalLookupResult{ReviewedPlanID: reviewedPlanID, ErrorCode: "journal_unavailable", Error: "persistent sync journal store is unavailable"}
	}
	if store == nil {
		return mcpSyncJournalLookupResult{ReviewedPlanID: reviewedPlanID, ErrorCode: "journal_not_found", Error: "no persistent sync journal exists for the reviewed plan"}
	}

	rawPlanID, aliasErr := store.ResolveReviewAlias(reviewedPlanID)
	if aliasErr == nil {
		record, readErr := store.InspectCurrentRecord(rawPlanID)
		if readErr == nil {
			return mcpSyncJournalLookupResult{Record: &record, ReviewedPlanID: reviewedPlanID}
		}
		switch {
		case errors.Is(readErr, syncjournalpkg.ErrMigrationRequired):
			return mcpSyncJournalLookupResult{ReviewedPlanID: reviewedPlanID, ErrorCode: "journal_migration_required", Error: "the sync journal uses an older schema; migrate it with the 115driver CLI"}
		case errors.Is(readErr, syncjournalpkg.ErrNotFound):
			// A stale private alias must not hide a compatible current-v2 journal
			// created before aliases existed. Continue to the bounded projection
			// scan; read-only lookup never mutates or repairs the alias itself.
		default:
			return mcpSyncJournalLookupResult{ReviewedPlanID: reviewedPlanID, ErrorCode: "journal_read_failed", Error: "persistent sync journal could not be read safely"}
		}
	}
	if aliasErr != nil && !errors.Is(aliasErr, syncjournalpkg.ErrNotFound) {
		return mcpSyncJournalLookupResult{ReviewedPlanID: reviewedPlanID, ErrorCode: "journal_read_failed", Error: "persistent sync journal alias could not be read safely"}
	}

	scan, scanErr := store.ScanCurrent(maxMCPSyncJournalScan)
	if scanErr != nil {
		code := "journal_read_failed"
		message := "persistent sync journals could not be read safely"
		if errors.Is(scanErr, syncjournalpkg.ErrScanLimit) {
			code = "journal_scan_limit_exceeded"
			message = "persistent sync journal scan exceeded its safety limit"
		}
		return mcpSyncJournalLookupResult{ReviewedPlanID: reviewedPlanID, ErrorCode: code, Error: message}
	}
	var matched *syncjournalpkg.CurrentRecord
	for index := range scan.Records {
		envelope, envelopeErr := buildMCPSyncPlanEnvelope(scan.Records[index].Journal.Plan)
		if envelopeErr != nil {
			return mcpSyncJournalLookupResult{ReviewedPlanID: reviewedPlanID, ErrorCode: "journal_read_failed", Error: "persistent sync journal could not be projected safely"}
		}
		if envelope.PlanID != reviewedPlanID {
			continue
		}
		if matched != nil {
			return mcpSyncJournalLookupResult{ReviewedPlanID: reviewedPlanID, ErrorCode: "journal_ambiguous", Error: "multiple persistent sync journals matched the reviewed plan"}
		}
		copy := scan.Records[index]
		matched = &copy
	}
	if matched == nil {
		return mcpSyncJournalLookupResult{ReviewedPlanID: reviewedPlanID, ErrorCode: "journal_not_found", Error: "no current persistent sync journal exists for the reviewed plan"}
	}
	return mcpSyncJournalLookupResult{Record: matched, ReviewedPlanID: reviewedPlanID}
}
