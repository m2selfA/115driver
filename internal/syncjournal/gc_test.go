package syncjournal

import (
	"testing"
	"time"
)

func TestResolveGCRetentionPreservesHistoricalFallbacks(t *testing.T) {
	if got := ResolveGCRetention(time.Hour, 2*time.Hour); got != time.Hour {
		t.Fatalf("explicit retention = %v", got)
	}
	if got := ResolveGCRetention(0, 2*time.Hour); got != 2*time.Hour {
		t.Fatalf("configured retention = %v", got)
	}
	if got := ResolveGCRetention(0, 0); got != DefaultGCRetention {
		t.Fatalf("default retention = %v", got)
	}
}

func TestBuildTrashRetentionWindowMatchesSessionStoreEligibility(t *testing.T) {
	now := time.Date(2026, 8, 22, 6, 0, 0, 0, time.UTC)
	trashedAt := now.Add(-2 * time.Hour)
	window := BuildTrashRetentionWindow(trashedAt, now, 3*time.Hour)
	if window.Age != 2*time.Hour || window.Retention != 3*time.Hour || !window.EligibleAt.Equal(trashedAt.Add(3*time.Hour)) || window.Eligible {
		t.Fatalf("configured trash window=%#v", window)
	}
	eligible := BuildTrashRetentionWindow(trashedAt, now.Add(2*time.Hour), 3*time.Hour)
	if !eligible.Eligible {
		t.Fatalf("expired trash was not eligible: %#v", eligible)
	}
	fallback := BuildTrashRetentionWindow(trashedAt, now, 0)
	if fallback.Retention != DefaultTrashRetention || !fallback.EligibleAt.Equal(trashedAt.Add(DefaultTrashRetention)) {
		t.Fatalf("default trash window=%#v", fallback)
	}
	future := BuildTrashRetentionWindow(now.Add(time.Hour), now, time.Hour)
	if future.Age != 0 || future.Eligible {
		t.Fatalf("future trash age was not clamped: %#v", future)
	}
}

func TestSelectGCCandidatesFailsClosedForUnsafeJournalStates(t *testing.T) {
	now := time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)
	old := now.Add(-48 * time.Hour)
	entries := []ListEntry{
		{PlanID: "completed", State: StatusCompleted, UpdatedAt: old},
		{PlanID: "failed", State: StatusFailed, UpdatedAt: old},
		{PlanID: "active", State: StatusActive, UpdatedAt: old},
		{PlanID: "locked", State: StatusCompleted, UpdatedAt: old, InUse: true},
		{PlanID: "recovery", State: StatusFailed, UpdatedAt: old, RecoveryRequired: true},
		{PlanID: "reconcile", State: StatusFailed, UpdatedAt: old, ReconcileRequired: true},
		{PlanID: "fresh", State: StatusCompleted, UpdatedAt: now.Add(-30 * time.Minute)},
		{PlanID: "future", State: StatusCompleted, UpdatedAt: now.Add(time.Hour)},
		{PlanID: "protected", State: StatusCompleted, UpdatedAt: old},
	}
	protected := map[string]struct{}{"protected": {}}
	got := SelectGCCandidates(entries, now, time.Hour, protected)
	if len(got) != 2 || got[0].PlanID != "completed" || got[0].State != StatusCompleted || got[1].PlanID != "failed" || got[1].State != StatusFailed {
		t.Fatalf("GC candidates = %#v", got)
	}
}
