package cmd

import (
	"errors"
	"testing"
	"time"
)

func TestSyncJournalRunStatsTrackResumeAndInterruptedRuns(t *testing.T) {
	store := testSyncJournalStore(t)
	plan := testSyncJournalPlan(t)
	handle, err := store.Create(plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()

	if err := handle.beginRun(false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := handle.finishRun(newSyncExecutionSummary(plan, false), errors.New("first run failed")); err != nil {
		t.Fatal(err)
	}
	first := handle.snapshot().RunStats
	if first.Runs != 1 || first.ResumeRuns != 0 || first.InterruptedRuns != 0 || first.LastStartedAt == nil || first.LastFinishedAt == nil || first.LastDurationMillis <= 0 || first.TotalDurationMillis != first.LastDurationMillis {
		t.Fatalf("unexpected first-run stats: %#v", first)
	}

	if err := handle.beginRun(true); err != nil {
		t.Fatal(err)
	}
	second := handle.snapshot().RunStats
	if second.Runs != 2 || second.ResumeRuns != 1 || second.InterruptedRuns != 0 || second.LastStartedAt == nil || second.LastFinishedAt != nil {
		t.Fatalf("unexpected active resume stats: %#v", second)
	}

	// Simulate process loss: another resume begins before the previous run records a finish.
	if err := handle.beginRun(true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := handle.finishRun(newSyncExecutionSummary(plan, false), errors.New("third run failed")); err != nil {
		t.Fatal(err)
	}
	final := handle.snapshot().RunStats
	if final.Runs != 3 || final.ResumeRuns != 2 || final.InterruptedRuns != 1 || final.LastStartedAt == nil || final.LastFinishedAt == nil || final.LastDurationMillis <= 0 || final.TotalDurationMillis <= first.TotalDurationMillis {
		t.Fatalf("unexpected resumed/interrupted stats: %#v", final)
	}

	persisted, err := store.Inspect(plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.RunStats.Runs != final.Runs || persisted.RunStats.ResumeRuns != final.ResumeRuns || persisted.RunStats.InterruptedRuns != final.InterruptedRuns || persisted.RunStats.TotalDurationMillis != final.TotalDurationMillis {
		t.Fatalf("persisted run stats differ: memory=%#v disk=%#v", final, persisted.RunStats)
	}

	copy := handle.snapshot()
	originalStart := *copy.RunStats.LastStartedAt
	*copy.RunStats.LastStartedAt = originalStart.Add(24 * time.Hour)
	if got := *handle.snapshot().RunStats.LastStartedAt; !got.Equal(originalStart) {
		t.Fatalf("snapshot run timestamp shares backing pointer: got %s want %s", got, originalStart)
	}
}
