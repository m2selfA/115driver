package syncjournal

import (
	"strings"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

// Clone returns an independent in-memory journal snapshot while preserving the
// exact persisted schema. Immutable prepared-digest pointers inside Plan items
// intentionally retain the historical shallow-copy behavior.
func Clone(journal Journal) Journal {
	clone := journal
	clone.Migrations = append([]MigrationRecord(nil), journal.Migrations...)
	if journal.CompletedAt != nil {
		completedAt := *journal.CompletedAt
		clone.CompletedAt = &completedAt
	}
	if journal.RunStats.LastStartedAt != nil {
		startedAt := *journal.RunStats.LastStartedAt
		clone.RunStats.LastStartedAt = &startedAt
	}
	if journal.RunStats.LastFinishedAt != nil {
		finishedAt := *journal.RunStats.LastFinishedAt
		clone.RunStats.LastFinishedAt = &finishedAt
	}
	clone.Plan.Items = append([]syncplanpkg.Item(nil), journal.Plan.Items...)
	clone.Items = append([]Item(nil), journal.Items...)
	for index := range clone.Items {
		if clone.Items[index].Post != nil {
			post := *clone.Items[index].Post
			clone.Items[index].Post = &post
		}
	}
	return clone
}

func IsDestructivePlanItem(item syncplanpkg.Item) bool {
	return item.Destructive || item.Action == "replace-remote" || item.Action == "replace-local" || item.Action == "delete-remote" || item.Action == "delete-local"
}

func RecoveryRequired(journal Journal) bool {
	return journal.State == StatusRecoveryRequired
}

// DestructiveReconciliationRequired reports whether an unfinished destructive
// item has crossed a mutation phase and therefore must be reconciled before it
// can be replayed. Malformed item indexes fail closed.
func DestructiveReconciliationRequired(journal Journal) bool {
	for _, stored := range journal.Items {
		if stored.Index < 0 || stored.Index >= len(journal.Plan.Items) {
			return true
		}
		if !IsDestructivePlanItem(journal.Plan.Items[stored.Index]) || stored.State == "succeeded" || stored.State == "skipped" {
			continue
		}
		if PhaseRequiresReconciliation(stored.Phase) {
			return true
		}
	}
	return false
}

// PostconditionVerificationRequired reports a non-destructive write that
// returned successfully and crossed mutation-done but has not yet persisted a
// verified terminal postcondition. Such an item must be observed, never blindly
// replayed, because the content mutation may already have completed.
func PostconditionVerificationRequired(journal Journal) bool {
	for _, stored := range journal.Items {
		if stored.Index < 0 || stored.Index >= len(journal.Plan.Items) {
			return true
		}
		if stored.State == "succeeded" || stored.State == "skipped" || stored.Phase != PhaseMutationDone {
			continue
		}
		if !IsDestructivePlanItem(journal.Plan.Items[stored.Index]) {
			return true
		}
	}
	return false
}

// ReconciliationRequired is the shared control-plane gate for both interrupted
// destructive mutations and non-destructive writes whose terminal
// postcondition still needs verification.
func ReconciliationRequired(journal Journal) bool {
	return DestructiveReconciliationRequired(journal) || PostconditionVerificationRequired(journal)
}

func EffectiveStatus(journal Journal) string {
	if RecoveryRequired(journal) {
		return StatusRecoveryRequired
	}
	if ReconciliationRequired(journal) {
		return StatusReconcileRequired
	}
	state := strings.ToLower(strings.TrimSpace(journal.State))
	if state == "" {
		return StatusUnknown
	}
	return state
}
