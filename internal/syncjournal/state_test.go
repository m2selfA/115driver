package syncjournal

import (
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestEffectiveStatusFailsClosedForInterruptedDestructiveItems(t *testing.T) {
	journal := Journal{
		State: StatusFailed,
		Plan:  syncplanpkg.Plan{Items: []syncplanpkg.Item{{Action: "delete-remote", Destructive: true}}},
		Items: []Item{{Index: 0, State: "failed", Phase: "pending"}},
	}
	if got := EffectiveStatus(journal); got != StatusFailed {
		t.Fatalf("pending destructive status = %q, want %q", got, StatusFailed)
	}
	journal.Items[0].Phase = "delete-started"
	if got := EffectiveStatus(journal); got != StatusReconcileRequired {
		t.Fatalf("interrupted destructive status = %q, want %q", got, StatusReconcileRequired)
	}
	journal.State = StatusRecoveryRequired
	if got := EffectiveStatus(journal); got != StatusRecoveryRequired {
		t.Fatalf("manual recovery status = %q, want %q", got, StatusRecoveryRequired)
	}
	journal.State = StatusFailed
	journal.Items[0].Index = 99
	if got := EffectiveStatus(journal); got != StatusReconcileRequired {
		t.Fatalf("invalid item index did not fail closed: %q", got)
	}
}

func TestEffectiveStatusRequiresVerificationAfterNonDestructiveMutationDone(t *testing.T) {
	journal := Journal{
		State: StatusFailed,
		Plan:  syncplanpkg.Plan{Items: []syncplanpkg.Item{{Action: "upload", Kind: "file"}}},
		Items: []Item{{Index: 0, State: "failed", Phase: PhaseMutationStarted}},
	}
	if PostconditionVerificationRequired(journal) || ReconciliationRequired(journal) || EffectiveStatus(journal) != StatusFailed {
		t.Fatalf("mutation-started non-destructive journal was incorrectly reconciliation-gated: %#v", journal)
	}
	journal.Items[0].Phase = PhaseMutationDone
	if !PostconditionVerificationRequired(journal) || !ReconciliationRequired(journal) || EffectiveStatus(journal) != StatusReconcileRequired {
		t.Fatalf("mutation-done non-destructive journal did not require verification: %#v", journal)
	}
}

func TestCloneSeparatesMutableJournalState(t *testing.T) {
	now := time.Unix(123, 0).UTC()
	finished := now.Add(time.Second)
	completed := finished.Add(time.Second)
	journal := Journal{
		CompletedAt: &completed,
		RunStats:    RunStats{LastStartedAt: &now, LastFinishedAt: &finished},
		Migrations:  []MigrationRecord{{FromVersion: 1, ToVersion: 2}},
		Plan:        syncplanpkg.Plan{Items: []syncplanpkg.Item{{RelativePath: "old.bin", Action: "delete-remote"}}},
		Items:       []Item{{Index: 0, RelativePath: "old.bin", Post: &Postcondition{Side: "remote", Exists: false}}},
	}
	clone := Clone(journal)
	clone.Migrations[0].ToVersion = 9
	clone.Plan.Items[0].RelativePath = "changed.bin"
	clone.Items[0].RelativePath = "changed.bin"
	clone.Items[0].Post.Side = "local"
	*clone.CompletedAt = completed.Add(time.Hour)
	*clone.RunStats.LastStartedAt = now.Add(time.Hour)

	if journal.Migrations[0].ToVersion != 2 || journal.Plan.Items[0].RelativePath != "old.bin" || journal.Items[0].RelativePath != "old.bin" || journal.Items[0].Post.Side != "remote" {
		t.Fatalf("clone mutation escaped into source: %#v", journal)
	}
	if !journal.CompletedAt.Equal(completed) || !journal.RunStats.LastStartedAt.Equal(now) {
		t.Fatalf("clone time pointer mutation escaped into source: %#v", journal.RunStats)
	}
}
