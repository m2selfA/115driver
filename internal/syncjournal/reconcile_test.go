package syncjournal

import (
	"strings"
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestClassifyDestructiveEvidence(t *testing.T) {
	for name, tc := range map[string]struct {
		action          string
		targetExists    bool
		winnerMatches   bool
		originalMatches bool
		want            DestructiveDecision
	}{
		"delete-completed":         {action: "delete-remote", targetExists: false, want: DestructiveCompleted},
		"delete-retry":             {action: "delete-local", targetExists: true, originalMatches: true, want: DestructiveRetryFull},
		"delete-ambiguous":         {action: "delete-remote", targetExists: true, want: DestructiveAmbiguous},
		"replace-winner-completed": {action: "replace-remote", targetExists: true, winnerMatches: true, want: DestructiveCompleted},
		"replace-original-retry":   {action: "replace-local", targetExists: true, originalMatches: true, want: DestructiveRetryFull},
		"replace-ambiguous":        {action: "replace-remote", targetExists: true, want: DestructiveAmbiguous},
		"replace-winner-only":      {action: "replace-local", targetExists: false, want: DestructiveWinnerOnly},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ClassifyDestructiveEvidence(tc.action, tc.targetExists, tc.winnerMatches, tc.originalMatches)
			if err != nil || got != tc.want {
				t.Fatalf("ClassifyDestructiveEvidence(%q) = %q, %v; want %q", tc.action, got, err, tc.want)
			}
		})
	}
	if _, err := ClassifyDestructiveEvidence("upload", true, false, false); err == nil {
		t.Fatal("non-destructive action was accepted by destructive classifier")
	}
}

func TestApplyDestructiveDecisionTransitions(t *testing.T) {
	newJournal := func(item syncplanpkg.Item) Journal {
		plan := projectionPlan(item)
		journal, err := New(plan, strings.Repeat("a", 64), 42, time.Unix(100, 0))
		if err != nil {
			t.Fatal(err)
		}
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = PhaseDeleteStarted
		journal.Items[0].LastError = "interrupted"
		return journal
	}

	t.Run("completed", func(t *testing.T) {
		journal := newJournal(syncplanpkg.Item{RelativePath: "old.bin", Action: "delete-remote", Kind: "file", RemotePresent: true, RemoteID: "id", Destructive: true})
		post := &Postcondition{Side: "remote", Exists: false}
		if err := ApplyDestructiveDecision(&journal, 0, DestructiveCompleted, post, time.Unix(200, 0)); err != nil {
			t.Fatal(err)
		}
		if journal.Items[0].State != "succeeded" || journal.Items[0].Phase != PhaseDone || journal.Items[0].Post == nil || journal.Items[0].LastError != "" {
			t.Fatalf("completed destructive transition = %#v", journal.Items[0])
		}
	})

	t.Run("retry-full", func(t *testing.T) {
		journal := newJournal(syncplanpkg.Item{RelativePath: "old.bin", Action: "delete-local", Kind: "file", LocalPresent: true, Destructive: true})
		if err := ApplyDestructiveDecision(&journal, 0, DestructiveRetryFull, nil, time.Unix(200, 0)); err != nil {
			t.Fatal(err)
		}
		if journal.Items[0].State != "pending" || journal.Items[0].Phase != PhasePending || journal.Items[0].Post != nil || journal.Items[0].LastError != "" {
			t.Fatalf("retry-full transition = %#v", journal.Items[0])
		}
	})

	t.Run("winner-only", func(t *testing.T) {
		journal := newJournal(syncplanpkg.Item{RelativePath: "conflict.bin", Action: "replace-local", Kind: "file", ReplacesKind: "directory", LocalPresent: true, RemotePresent: true, Destructive: true})
		if err := ApplyDestructiveDecision(&journal, 0, DestructiveWinnerOnly, nil, time.Unix(200, 0)); err != nil {
			t.Fatal(err)
		}
		if journal.Items[0].State != "pending" || journal.Items[0].Phase != PhaseLoserRemoved || journal.Items[0].Post != nil || journal.Items[0].LastError != "" {
			t.Fatalf("winner-only transition = %#v", journal.Items[0])
		}
	})

	t.Run("ambiguous-refused", func(t *testing.T) {
		journal := newJournal(syncplanpkg.Item{RelativePath: "old.bin", Action: "delete-local", Kind: "file", LocalPresent: true, Destructive: true})
		before := Clone(journal)
		if err := ApplyDestructiveDecision(&journal, 0, DestructiveAmbiguous, nil, time.Unix(200, 0)); err == nil {
			t.Fatal("ambiguous destructive decision was applied")
		}
		if journal.Items[0].State != before.Items[0].State || journal.Items[0].Phase != before.Items[0].Phase || journal.Items[0].LastError != before.Items[0].LastError {
			t.Fatalf("ambiguous decision mutated journal item: before=%#v after=%#v", before.Items[0], journal.Items[0])
		}
	})
}
