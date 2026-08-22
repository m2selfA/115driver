package syncjournal

import (
	"strings"
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func projectionPlan(items ...syncplanpkg.Item) syncplanpkg.Plan {
	plan := syncplanpkg.Plan{
		Operation: "sync", DryRun: true, Mode: syncplanpkg.ModeConservative,
		Direction: syncplanpkg.DirectionUpload, ConflictPolicy: syncplanpkg.ConflictError,
		Ready: true, LocalRoot: "/local", RemoteRoot: "/remote", RemoteRootID: "root", Items: items,
	}
	plan.ChangeActions = syncplanpkg.ChangeCount(plan)
	for _, item := range items {
		if IsDestructivePlanItem(item) {
			plan.DestructiveActions++
		}
	}
	plan.RequiresAllowDestructive = plan.DestructiveActions > 0
	plan.PlanID = syncplanpkg.Fingerprint(plan)
	return plan
}

func TestExpectedPlanProjectsCompletedPostcondition(t *testing.T) {
	plan := projectionPlan(syncplanpkg.Item{
		RelativePath: "file.bin", Action: "upload", Kind: "file", LocalPresent: true,
		LocalPath: "/local/file.bin", RemotePath: "/remote/file.bin", LocalSize: 7, LocalSHA1: strings.Repeat("A", 40),
	})
	journal, err := New(plan, strings.Repeat("a", 64), 42, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	journal.Items[0].State = "succeeded"
	journal.Items[0].Phase = PhaseDone
	journal.Items[0].Post = &Postcondition{Side: "remote", Exists: true, Kind: "file", RemoteID: "remote-id", Size: 7, SHA1: strings.Repeat("b", 40), ModTimeUnixNano: 123}
	expected, err := ExpectedPlan(journal)
	if err != nil {
		t.Fatal(err)
	}
	item := expected.Items[0]
	if item.Action != "skip" || item.RemoteID != "remote-id" || !item.RemotePresent || item.RemoteSize != 7 || item.RemoteSHA1 != strings.Repeat("B", 40) || item.RemoteModTimeUnixNano != 123 {
		t.Fatalf("expected projected item = %#v", item)
	}
}

func TestExpectedPlanClearsCoveredDescendantAfterCompletedDeleteRoot(t *testing.T) {
	plan := projectionPlan(
		syncplanpkg.Item{RelativePath: "old", Action: "delete-remote", Kind: "directory", RemotePresent: true, RemotePath: "/remote/old", RemoteID: "old-id", Destructive: true},
		syncplanpkg.Item{RelativePath: "old/child.bin", Action: "skip", Kind: "file", RemotePresent: true, RemotePath: "/remote/old/child.bin", RemoteID: "child-id", RemoteSize: 5, RemoteSHA1: strings.Repeat("C", 40)},
	)
	journal, err := New(plan, strings.Repeat("a", 64), 42, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	journal.Items[0].State = "succeeded"
	journal.Items[0].Phase = PhaseDone
	journal.Items[0].Post = &Postcondition{Side: "remote", Exists: false}
	expected, err := ExpectedPlan(journal)
	if err != nil {
		t.Fatal(err)
	}
	if expected.Items[1].RemotePresent || expected.Items[1].RemoteID != "" || expected.Items[1].RemoteSize != 0 || expected.Items[1].RemoteSHA1 != "" {
		t.Fatalf("covered descendant retained removed remote snapshot: %#v", expected.Items[1])
	}
}

func TestResidualPlanKeepsCompletedSkippedAndWinnerOnlyReplacement(t *testing.T) {
	plan := projectionPlan(
		syncplanpkg.Item{RelativePath: "done.bin", Action: "upload", Kind: "file", LocalPresent: true, LocalPath: "/local/done.bin", RemotePath: "/remote/done.bin"},
		syncplanpkg.Item{RelativePath: "replace.bin", Action: "replace-remote", Kind: "file", ReplacesKind: "directory", Destructive: true, LocalPresent: true, RemotePresent: true, LocalPath: "/local/replace.bin", RemotePath: "/remote/replace.bin", RemoteID: "loser"},
	)
	journal, err := New(plan, strings.Repeat("a", 64), 42, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	journal.Items[0].State = "succeeded"
	journal.Items[0].Phase = PhaseDone
	journal.Items[0].Post = &Postcondition{Side: "remote", Exists: true, Kind: "file", RemoteID: "done"}
	journal.Items[1].State = "failed"
	journal.Items[1].Phase = PhaseLoserRemoved
	residual := ResidualPlan(journal)
	if residual.Items[0].Action != "skip" || residual.Items[1].Action != "upload" || residual.Items[1].Destructive || residual.Items[1].ReplacesKind != "" {
		t.Fatalf("residual plan items = %#v", residual.Items)
	}
	if residual.DestructiveActions != 0 || residual.ChangeActions != 1 || residual.RequiresAllowDestructive {
		t.Fatalf("residual plan counters = %#v", residual)
	}
}
