package syncexec

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestBuildGraphAddsDirectoryBarrierAndSerializesSiblingDestructiveActions(t *testing.T) {
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "dir", Action: "upload", Kind: "directory"},
		{RelativePath: "dir/file.bin", Action: "upload", Kind: "file"},
		{RelativePath: "old/a.bin", Action: "delete-remote", Kind: "file", Destructive: true},
		{RelativePath: "old/b.bin", Action: "replace-remote", Kind: "file", Destructive: true},
	}}
	graph, err := BuildGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := graph.Dependencies[1]; !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("child dependencies = %#v, want [0]", got)
	}
	if got := graph.Dependencies[3]; !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("destructive sibling dependencies = %#v, want [2]", got)
	}
}

func TestBuildGraphRejectsOverlappingDestructiveDirectoryMutations(t *testing.T) {
	for name, plan := range map[string]syncplanpkg.Plan{
		"remote-child-delete": {Items: []syncplanpkg.Item{
			{RelativePath: "old", Action: "delete-remote", Kind: "directory", Destructive: true},
			{RelativePath: "old/child.bin", Action: "delete-remote", Kind: "file", Destructive: true},
		}},
		"local-child-download": {Items: []syncplanpkg.Item{
			{RelativePath: "node", Action: "replace-local", Kind: "file", ReplacesKind: "directory", Destructive: true},
			{RelativePath: "node/child.bin", Action: "download", Kind: "file"},
		}},
		"wrong-covered-reason": {Items: []syncplanpkg.Item{
			{RelativePath: "old", Action: "delete-local", Kind: "directory", Destructive: true},
			{RelativePath: "old/child.bin", Action: "skip", Kind: "file", Reason: "covered-by-delete-local:somewhere-else"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildGraph(plan); err == nil || !strings.Contains(err.Error(), "covered skips") {
				t.Fatalf("overlapping destructive subtree error = %v", err)
			}
		})
	}
}

func TestBuildGraphAcceptsReviewedCoveredDestructiveDirectoryDescendants(t *testing.T) {
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "old", Action: "delete-remote", Kind: "directory", Destructive: true},
		{RelativePath: "old/child.bin", Action: "skip", Kind: "file", Reason: "covered-by-delete-remote:old"},
		{RelativePath: "old/sub", Action: "skip", Kind: "directory", Reason: "covered-by-delete-remote:old"},
	}}
	graph, err := BuildGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{1, 2} {
		if !reflect.DeepEqual(graph.Dependencies[index], []int{0}) {
			t.Fatalf("covered descendant %d dependencies = %#v, want [0]", index, graph.Dependencies[index])
		}
	}
}

func TestExecutePreflightAndPrepareAreHardWriteBarriers(t *testing.T) {
	plan := syncplanpkg.Plan{
		Ready: true,
		Items: []syncplanpkg.Item{{RelativePath: "file.bin", Action: "upload", Kind: "file"}},
	}
	var calls []string
	deps := Deps{
		Preflight: func(context.Context) error {
			calls = append(calls, "preflight")
			return nil
		},
		Prepare: func() error {
			calls = append(calls, "prepare")
			return nil
		},
		UploadFile: func(context.Context, syncplanpkg.Item) error {
			calls = append(calls, "upload")
			return nil
		},
	}
	summary, err := Execute(context.Background(), plan, false, deps)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"preflight", "prepare", "upload"}) || !summary.PreflightPassed || summary.PreflightChecked != 1 || summary.Succeeded != 1 {
		t.Fatalf("execution barrier calls=%v summary=%#v", calls, summary)
	}

	calls = nil
	deps.Preflight = func(context.Context) error {
		calls = append(calls, "preflight")
		return errors.New("stale")
	}
	summary, err = Execute(context.Background(), plan, false, deps)
	if err == nil || !strings.Contains(err.Error(), "preflight") || !reflect.DeepEqual(calls, []string{"preflight"}) || summary.Processed != 0 || summary.PreflightPassed {
		t.Fatalf("failed preflight crossed write barrier: err=%v calls=%v summary=%#v", err, calls, summary)
	}
}

func TestExecuteContinueOnErrorBlocksFailedBranchAndRunsIndependentBranch(t *testing.T) {
	plan := syncplanpkg.Plan{
		Ready: true,
		Items: []syncplanpkg.Item{
			{RelativePath: "bad", Action: "upload", Kind: "directory"},
			{RelativePath: "bad/child.bin", Action: "upload", Kind: "file"},
			{RelativePath: "good", Action: "upload", Kind: "directory"},
			{RelativePath: "good/child.bin", Action: "upload", Kind: "file"},
		},
	}
	var calls []string
	deps := Deps{
		Preflight: func(context.Context) error { return nil },
		CreateRemoteDirectory: func(_ context.Context, item syncplanpkg.Item) error {
			calls = append(calls, "mkdir:"+item.RelativePath)
			if item.RelativePath == "bad" {
				return errors.New("synthetic failure")
			}
			return nil
		},
		UploadFile: func(_ context.Context, item syncplanpkg.Item) error {
			calls = append(calls, "upload:"+item.RelativePath)
			return nil
		},
	}
	summary, err := ExecuteWithJobsPolicy(context.Background(), plan, false, 2, true, deps)
	if err == nil || summary.Failed != 1 || summary.Blocked != 1 || summary.Succeeded != 2 {
		t.Fatalf("continue-on-error summary=%#v err=%v", summary, err)
	}
	if strings.Contains(strings.Join(calls, ","), "bad/child.bin") || !strings.Contains(strings.Join(calls, ","), "good/child.bin") {
		t.Fatalf("branch blocking calls=%v", calls)
	}
}

func TestValidateSafetyRequiresReadyPlanAndDestructiveApproval(t *testing.T) {
	if !errors.Is(ValidateSafety(syncplanpkg.Plan{Ready: false}, true), ErrPlanNotReady) {
		t.Fatal("unready plan was accepted")
	}
	plan := syncplanpkg.Plan{Ready: true, DestructiveActions: 1}
	if !errors.Is(ValidateSafety(plan, false), ErrDestructiveApproval) {
		t.Fatal("destructive plan without approval was accepted")
	}
	if err := ValidateSafety(plan, true); err != nil {
		t.Fatalf("approved destructive plan rejected: %v", err)
	}
}
