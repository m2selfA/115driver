package syncjournal

import (
	"strings"
	"time"
)

func CountKey(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "unset"
	}
	return value
}

// BuildListEntry derives the persisted journal summary used by CLI and MCP
// control planes from one validated journal. It performs no I/O and preserves
// the historical CLI counting semantics.
func BuildListEntry(journal Journal, now time.Time, inUse bool) ListEntry {
	staleFor := now.UTC().Sub(journal.UpdatedAt)
	if staleFor < 0 {
		staleFor = 0
	}
	entry := ListEntry{
		Schema: ListEntrySchema,
		PlanID: journal.PlanID, Version: journal.Version, MigrationRequired: journal.Version < Version,
		State: journal.State, Status: EffectiveStatus(journal), RunStats: journal.RunStats,
		Direction: journal.Plan.Direction, ConflictPolicy: journal.Plan.ConflictPolicy, DeleteExtraneous: journal.Plan.DeleteExtraneous,
		LocalRoot: journal.Plan.LocalRoot, RemoteRoot: journal.Plan.RemoteRoot,
		CreatedAt: journal.CreatedAt, UpdatedAt: journal.UpdatedAt, StaleForMillis: staleFor.Milliseconds(),
		Total: len(journal.Items), ActionCounts: make(map[string]int), StateCounts: make(map[string]int), PhaseCounts: make(map[string]int),
		RecoveryRequired: RecoveryRequired(journal), ReconcileRequired: ReconciliationRequired(journal), InUse: inUse,
	}
	for _, item := range journal.Items {
		entry.ActionCounts[CountKey(item.Action)]++
		entry.StateCounts[CountKey(item.State)]++
		entry.PhaseCounts[CountKey(item.Phase)]++
		switch item.State {
		case "succeeded", "skipped":
			entry.Completed++
		case "failed":
			entry.Failed++
		case "blocked":
			entry.Blocked++
		default:
			entry.Pending++
		}
	}
	return entry
}
