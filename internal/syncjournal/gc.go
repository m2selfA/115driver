package syncjournal

import "time"

const (
	DefaultGCRetention    = 30 * 24 * time.Hour
	DefaultTrashRetention = 7 * 24 * time.Hour
)

type GCCandidate struct {
	PlanID string
	State  string
}

// ResolveGCRetention preserves the historical sync-journal GC contract: an
// explicit positive age wins, then configured retention, then 30 days.
func ResolveGCRetention(requested, configured time.Duration) time.Duration {
	if requested > 0 {
		return requested
	}
	if configured > 0 {
		return configured
	}
	return DefaultGCRetention
}

func ResolveTrashRetention(configured time.Duration) time.Duration {
	if configured > 0 {
		return configured
	}
	return DefaultTrashRetention
}

type TrashRetentionWindow struct {
	Age        time.Duration
	Retention  time.Duration
	EligibleAt time.Time
	Eligible   bool
}

// BuildTrashRetentionWindow mirrors Session Store trash GC eligibility. The
// eligible timestamp is deterministic, but actual purge remains opportunistic
// and can happen later depending on when maintenance next runs.
func BuildTrashRetentionWindow(trashedAt, now time.Time, configured time.Duration) TrashRetentionWindow {
	trashedAt = trashedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	retention := ResolveTrashRetention(configured)
	age := now.Sub(trashedAt)
	if age < 0 {
		age = 0
	}
	eligibleAt := trashedAt.Add(retention)
	return TrashRetentionWindow{
		Age: age, Retention: retention, EligibleAt: eligibleAt,
		Eligible: !now.Before(eligibleAt),
	}
}

// SelectGCCandidates is the shared pure eligibility policy for sync-journal
// maintenance. Store-specific migration protection is supplied by the caller;
// active/locked, recovery/reconcile, and nonterminal journals always fail
// closed. The input order is preserved for stable CLI/MCP previews.
func SelectGCCandidates(entries []ListEntry, now time.Time, olderThan time.Duration, protected map[string]struct{}) []GCCandidate {
	olderThan = ResolveGCRetention(olderThan, 0)
	now = now.UTC()
	candidates := make([]GCCandidate, 0)
	for _, entry := range entries {
		if _, ok := protected[entry.PlanID]; ok {
			continue
		}
		if entry.InUse || entry.RecoveryRequired || entry.ReconcileRequired {
			continue
		}
		if entry.State != StatusCompleted && entry.State != StatusFailed {
			continue
		}
		age := now.Sub(entry.UpdatedAt)
		if age < 0 || age < olderThan {
			continue
		}
		candidates = append(candidates, GCCandidate{PlanID: entry.PlanID, State: entry.State})
	}
	return candidates
}
