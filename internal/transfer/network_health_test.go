package transfer

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNetworkHealthTrackerScoresCooldownAndRecovery(t *testing.T) {
	tracker, err := NewNetworkHealthTracker(NetworkHealthOptions{Cooldown: 10 * time.Second, CooldownMax: 40 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	tracker.now = func() time.Time { return now }
	path := testNetworkPath(7, "10.0.0.7")

	initial := tracker.Snapshot(path)
	if initial.Score != 100 || initial.InCooldown || initial.Failures != 0 {
		t.Fatalf("unexpected initial health: %#v", initial)
	}
	tracker.RecordFailure(path)
	first := tracker.Snapshot(path)
	if first.Score != 75 || !first.InCooldown || first.ConsecutiveFailures != 1 || first.Failures != 1 {
		t.Fatalf("unexpected first failure health: %#v", first)
	}
	if got := first.CooldownUntil.Sub(now); got != 10*time.Second {
		t.Fatalf("unexpected first cooldown: %s", got)
	}

	now = now.Add(10 * time.Second)
	if !tracker.Available(path) {
		t.Fatal("interface did not automatically become eligible after cooldown")
	}
	tracker.RecordFailure(path)
	second := tracker.Snapshot(path)
	if second.Score != 50 || second.ConsecutiveFailures != 2 || second.CooldownUntil.Sub(now) != 20*time.Second {
		t.Fatalf("unexpected exponential cooldown: %#v", second)
	}

	now = now.Add(20 * time.Second)
	tracker.RecordSuccess(path)
	recovered := tracker.Snapshot(path)
	if recovered.Score != 60 || recovered.ConsecutiveFailures != 0 || recovered.InCooldown || recovered.Successes != 1 {
		t.Fatalf("unexpected recovered health: %#v", recovered)
	}
}

func TestNetworkHealthTrackerCapsCooldown(t *testing.T) {
	tracker, err := NewNetworkHealthTracker(NetworkHealthOptions{Cooldown: 10 * time.Second, CooldownMax: 25 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tracker.now = func() time.Time { return now }
	path := testNetworkPath(8, "10.0.0.8")
	for i := 0; i < 4; i++ {
		tracker.RecordFailure(path)
		snapshot := tracker.Snapshot(path)
		if snapshot.CooldownUntil.Sub(now) > 25*time.Second {
			t.Fatalf("cooldown exceeded cap: %#v", snapshot)
		}
		now = snapshot.CooldownUntil
	}
}

func TestNetworkHealthOptionsValidation(t *testing.T) {
	if _, err := NewNetworkHealthTracker(NetworkHealthOptions{}); err == nil {
		t.Fatal("expected zero cooldown to fail")
	}
	if _, err := NewNetworkHealthTracker(NetworkHealthOptions{Cooldown: 10 * time.Second, CooldownMax: 5 * time.Second}); err == nil {
		t.Fatal("expected max cooldown below base to fail")
	}
}

func TestShouldPenalizeNetworkPathOnlyMarkedFailures(t *testing.T) {
	if shouldPenalizeNetworkPath(context.Background(), ErrUnexpectedDownloadStatus) {
		t.Fatal("HTTP status error must not penalize network health")
	}
	if shouldPenalizeNetworkPath(context.Background(), ErrDownloadSizeMismatch) {
		t.Fatal("size mismatch must not penalize network health")
	}
	networkErr := markNetworkPathFailure(errors.New("connection reset"))
	if !shouldPenalizeNetworkPath(context.Background(), networkErr) {
		t.Fatal("marked network failure should penalize health")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if shouldPenalizeNetworkPath(ctx, networkErr) {
		t.Fatal("caller cancellation must not penalize interface health")
	}
}
