package syncjournal

import (
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"strings"
	"testing"
	"time"
)

func TestExecutionLifecycleTransitions(t *testing.T) {
	now := time.Unix(100, 0)
	plan := currentTestPlan()
	plan.Items[0].Action = "delete-local"
	plan.Items[0].Destructive = true
	plan.Items[0].RemotePresent = false
	plan.PlanID = syncplanpkg.Fingerprint(plan)
	journal, err := New(plan, strings.Repeat("a", 64), 42, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := BeginItem(&journal, 0, plan.Items[0], now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if journal.Items[0].State != "running" || journal.Items[0].Attempts != 1 || journal.State != StatusActive {
		t.Fatalf("begin item = %#v", journal.Items[0])
	}
	if err := SetItemPhase(&journal, 0, PhaseMutationStarted, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := FailItem(&journal, 0, "boom", now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if journal.Items[0].State != "failed" || journal.Items[0].Phase != PhaseMutationStarted || journal.State != StatusFailed || EffectiveStatus(journal) != StatusReconcileRequired {
		t.Fatalf("failed crossed mutation = state=%#v status=%q", journal, EffectiveStatus(journal))
	}
	if err := RequireRecovery(&journal, "ambiguous", now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if journal.State != StatusRecoveryRequired || EffectiveStatus(journal) != StatusRecoveryRequired {
		t.Fatalf("recovery latch = %#v", journal)
	}
}

func TestExecutionLifecycleSuccessRequiresPostcondition(t *testing.T) {
	journal, err := New(currentTestPlan(), strings.Repeat("a", 64), 42, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := BeginItem(&journal, 0, journal.Plan.Items[0], time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := SucceedItem(&journal, 0, journal.Plan.Items[0], nil, time.Time{}); err == nil {
		t.Fatal("non-skip success without postcondition was accepted")
	}
	post := &Postcondition{Side: "remote", Exists: true, Kind: journal.Plan.Items[0].Kind, RemoteID: "remote-id"}
	if err := SucceedItem(&journal, 0, journal.Plan.Items[0], post, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if journal.Items[0].State != "succeeded" || journal.Items[0].Phase != PhaseDone || journal.Items[0].Post == nil {
		t.Fatalf("successful item = %#v", journal.Items[0])
	}
}

func TestBeginItemAllowsCompletedResidualSkip(t *testing.T) {
	plan := currentTestPlan()
	journal, err := New(plan, strings.Repeat("a", 64), 42, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	journal.Items[0].State = "succeeded"
	journal.Items[0].Phase = PhaseDone
	journal.Items[0].Post = &Postcondition{Side: "remote", Exists: true, Kind: "file"}
	residual := plan.Items[0]
	residual.Action = "skip"
	if err := BeginItem(&journal, 0, residual, time.Time{}); err != nil {
		t.Fatalf("completed residual skip rejected: %v", err)
	}
	if err := SucceedItem(&journal, 0, residual, nil, time.Time{}); err != nil {
		t.Fatalf("completed residual skip after-item rejected: %v", err)
	}
	if journal.Items[0].Attempts != 0 || journal.Items[0].State != "succeeded" || journal.Items[0].Post == nil {
		t.Fatalf("completed residual skip mutated journal: %#v", journal.Items[0])
	}
}

func TestExecutionLifecycleSkipIsIdempotent(t *testing.T) {
	plan := currentTestPlan()
	plan.Items[0].Action = "skip"
	plan.Items[0].Reason = "test"
	plan.ChangeActions = 0
	plan.PlanID = ""
	plan.PlanID = syncplanpkg.Fingerprint(plan)
	journal, err := New(plan, strings.Repeat("a", 64), 42, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := BeginItem(&journal, 0, plan.Items[0], time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := SucceedItem(&journal, 0, plan.Items[0], nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if journal.Items[0].State != "skipped" || journal.Items[0].Phase != PhaseDone {
		t.Fatalf("skip lifecycle = %#v", journal.Items[0])
	}
}
