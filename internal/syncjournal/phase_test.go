package syncjournal

import (
	"testing"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestMutationPhasesShareJournalContract(t *testing.T) {
	for name, tc := range map[string]struct {
		item   syncplanpkg.Item
		stage  MutationStage
		before string
		after  string
	}{
		"upload":         {item: syncplanpkg.Item{Action: "upload"}, stage: MutationStageWrite, before: PhaseMutationStarted, after: PhaseMutationDone},
		"download":       {item: syncplanpkg.Item{Action: "download"}, stage: MutationStageWrite, before: PhaseMutationStarted, after: PhaseMutationDone},
		"replace-winner": {item: syncplanpkg.Item{Action: "replace-remote"}, stage: MutationStageWrite, before: PhaseWinnerStarted, after: PhaseWinnerCreated},
		"remove-loser":   {item: syncplanpkg.Item{Action: "replace-local"}, stage: MutationStageRemove, before: PhaseRemoveStarted, after: PhaseLoserRemoved},
		"delete":         {item: syncplanpkg.Item{Action: "delete-remote"}, stage: MutationStageDelete, before: PhaseDeleteStarted, after: PhaseDeleted},
	} {
		t.Run(name, func(t *testing.T) {
			before, after, err := MutationPhases(tc.item, tc.stage)
			if err != nil || before != tc.before || after != tc.after {
				t.Fatalf("MutationPhases(%q, %q) = %q, %q, %v", tc.item.Action, tc.stage, before, after, err)
			}
		})
	}
	for _, tc := range []struct {
		item  syncplanpkg.Item
		stage MutationStage
	}{
		{item: syncplanpkg.Item{Action: "upload"}, stage: MutationStageDelete},
		{item: syncplanpkg.Item{Action: "delete-local"}, stage: MutationStageRemove},
		{item: syncplanpkg.Item{Action: "skip"}, stage: MutationStageWrite},
		{item: syncplanpkg.Item{Action: "upload"}, stage: MutationStage("future")},
	} {
		if _, _, err := MutationPhases(tc.item, tc.stage); err == nil {
			t.Fatalf("invalid MutationPhases(%q, %q) was accepted", tc.item.Action, tc.stage)
		}
	}
}

func TestPhaseValidationAndReconciliationContract(t *testing.T) {
	valid := []string{"", PhasePending, PhaseDone, PhaseMutationStarted, PhaseMutationDone, PhaseWinnerStarted, PhaseWinnerCreated, PhaseRemoveStarted, PhaseLoserRemoved, PhaseDeleteStarted, PhaseDeleted}
	for _, phase := range valid {
		if !IsValidPhase(phase) {
			t.Fatalf("known phase %q was rejected", phase)
		}
	}
	if IsValidPhase("mystery") {
		t.Fatal("unknown phase was accepted")
	}
	if PhaseRequiresReconciliation("") || PhaseRequiresReconciliation(PhasePending) {
		t.Fatal("pristine phase unexpectedly requires reconciliation")
	}
	for _, phase := range []string{PhaseDone, PhaseMutationStarted, PhaseMutationDone, PhaseWinnerStarted, PhaseWinnerCreated, PhaseRemoveStarted, PhaseLoserRemoved, PhaseDeleteStarted, PhaseDeleted} {
		if !PhaseRequiresReconciliation(phase) {
			t.Fatalf("crossed mutation phase %q did not require reconciliation", phase)
		}
	}
}

func TestMutationFailureRequiresRecoveryMatrix(t *testing.T) {
	for name, tc := range map[string]struct {
		item  syncplanpkg.Item
		stage MutationStage
		want  bool
	}{
		"ordinary-upload":    {item: syncplanpkg.Item{Action: "upload"}, stage: MutationStageWrite, want: false},
		"replacement-winner": {item: syncplanpkg.Item{Action: "replace-remote"}, stage: MutationStageWrite, want: true},
		"replacement-remove": {item: syncplanpkg.Item{Action: "replace-local"}, stage: MutationStageRemove, want: true},
		"remote-delete":      {item: syncplanpkg.Item{Action: "delete-remote"}, stage: MutationStageDelete, want: true},
		"local-delete":       {item: syncplanpkg.Item{Action: "delete-local"}, stage: MutationStageDelete, want: true},
		"wrong-stage":        {item: syncplanpkg.Item{Action: "upload"}, stage: MutationStageRemove, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := MutationFailureRequiresRecovery(tc.item, tc.stage); got != tc.want {
				t.Fatalf("MutationFailureRequiresRecovery(%q, %q) = %v, want %v", tc.item.Action, tc.stage, got, tc.want)
			}
		})
	}
}
