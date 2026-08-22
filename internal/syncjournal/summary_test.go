package syncjournal

import (
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestBuildListEntryPreservesSharedCountAndStatusSemantics(t *testing.T) {
	now := time.Unix(200, 0).UTC()
	journal := Journal{
		Version: Version, State: StatusFailed, CreatedAt: time.Unix(100, 0).UTC(), UpdatedAt: time.Unix(150, 0).UTC(),
		RunStats: RunStats{Runs: 3, ResumeRuns: 1, InterruptedRuns: 1},
		Plan: syncplanpkg.Plan{
			Direction: syncplanpkg.DirectionBoth, ConflictPolicy: syncplanpkg.ConflictLocal, DeleteExtraneous: true,
			LocalRoot: "/local", RemoteRoot: "/remote",
			Items: []syncplanpkg.Item{
				{Action: "upload"},
				{Action: "skip"},
				{Action: "delete-remote", Destructive: true},
				{Action: "download"},
				{Action: "download"},
			},
		},
		Items: []Item{
			{Index: 0, Action: "upload", State: "succeeded", Phase: PhaseDone},
			{Index: 1, Action: "skip", State: "skipped", Phase: PhaseDone},
			{Index: 2, Action: "delete-remote", State: "failed", Phase: PhaseDeleteStarted},
			{Index: 3, Action: "download", State: "blocked", Phase: PhasePending},
			{Index: 4, Action: "download", State: "pending", Phase: ""},
		},
	}
	entry := BuildListEntry(journal, now, true)
	if entry.Schema != ListEntrySchema || entry.Status != StatusReconcileRequired || entry.State != StatusFailed || !entry.ReconcileRequired || entry.RecoveryRequired || !entry.InUse {
		t.Fatalf("unexpected shared list status: %#v", entry)
	}
	if entry.Total != 5 || entry.Completed != 2 || entry.Failed != 1 || entry.Blocked != 1 || entry.Pending != 1 || entry.StaleForMillis != 50_000 {
		t.Fatalf("unexpected shared list counts: %#v", entry)
	}
	if entry.ActionCounts["download"] != 2 || entry.StateCounts["failed"] != 1 || entry.PhaseCounts["unset"] != 1 || entry.PhaseCounts[PhaseDone] != 2 {
		t.Fatalf("unexpected shared list maps: %#v", entry)
	}
}

func TestBuildListEntryClampsFutureUpdatedAtStaleness(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	entry := BuildListEntry(Journal{UpdatedAt: now.Add(time.Hour)}, now, false)
	if entry.StaleForMillis != 0 {
		t.Fatalf("future journal staleness = %d, want 0", entry.StaleForMillis)
	}
}
