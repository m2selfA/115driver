package cmd

import (
	"reflect"
	"testing"
)

func TestBuildSyncExecutionGraphAddsDirectoryBarriers(t *testing.T) {
	plan := syncPlan{Items: []syncPlanItem{
		{RelativePath: "upload-dir", Action: "upload", Kind: "directory"},
		{RelativePath: "upload-dir/file.bin", Action: "upload", Kind: "file"},
		{RelativePath: "download-dir", Action: "download", Kind: "directory"},
		{RelativePath: "download-dir/file.bin", Action: "download", Kind: "file"},
		{RelativePath: "remote-replace", Action: "replace-remote", Kind: "directory", ReplacesKind: "file"},
		{RelativePath: "remote-replace/child.bin", Action: "upload", Kind: "file"},
		{RelativePath: "local-replace", Action: "replace-local", Kind: "directory", ReplacesKind: "file"},
		{RelativePath: "local-replace/child.bin", Action: "download", Kind: "file"},
	}}
	graph, err := buildSyncExecutionGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][]int{
		1: {0},
		3: {2},
		5: {4},
		7: {6},
	}
	for index := range plan.Items {
		if got := graph.Dependencies[index]; !reflect.DeepEqual(got, want[index]) {
			t.Fatalf("dependencies[%d]: got %#v want %#v", index, got, want[index])
		}
	}
}

func TestBuildSyncExecutionGraphSerializesSameParentDestructiveActions(t *testing.T) {
	plan := syncPlan{Items: []syncPlanItem{
		{RelativePath: "remote/a.bin", Action: "replace-remote", Kind: "file"},
		{RelativePath: "remote/b.bin", Action: "replace-remote", Kind: "file"},
		{RelativePath: "local/a.bin", Action: "replace-local", Kind: "file"},
		{RelativePath: "local/b.bin", Action: "replace-local", Kind: "file"},
		{RelativePath: "other/c.bin", Action: "replace-remote", Kind: "file"},
	}}
	graph, err := buildSyncExecutionGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := graph.Dependencies[1]; !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("second remote replacement dependencies: %#v", got)
	}
	if got := graph.Dependencies[3]; !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("second local replacement dependencies: %#v", got)
	}
	if got := graph.Dependencies[4]; len(got) != 0 {
		t.Fatalf("different-parent destructive action was serialized unnecessarily: %#v", got)
	}
}

func TestBuildSyncExecutionGraphKeepsCoveredSkipsBehindReplacement(t *testing.T) {
	plan := syncPlan{Items: []syncPlanItem{
		{RelativePath: "node", Action: "replace-remote", Kind: "file", ReplacesKind: "directory"},
		{RelativePath: "node/old.bin", Action: "skip", Kind: "file", Reason: "covered-by-replace-remote:node"},
		{RelativePath: "node/sub", Action: "skip", Kind: "directory", Reason: "covered-by-replace-remote:node"},
		{RelativePath: "node/sub/old.bin", Action: "skip", Kind: "file", Reason: "covered-by-replace-remote:node"},
	}}
	graph, err := buildSyncExecutionGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{1, 2, 3} {
		if got := graph.Dependencies[index]; !reflect.DeepEqual(got, []int{0}) {
			t.Fatalf("covered skip %d dependencies: got %#v want [0]", index, got)
		}
	}
}

func TestBuildSyncExecutionGraphKeepsCoveredSkipsBehindMirrorDelete(t *testing.T) {
	plan := syncPlan{Items: []syncPlanItem{
		{RelativePath: "old", Action: "delete-remote", Kind: "directory", Destructive: true},
		{RelativePath: "old/child.bin", Action: "skip", Kind: "file", Reason: "covered-by-delete-remote:old"},
		{RelativePath: "old/sub", Action: "skip", Kind: "directory", Reason: "covered-by-delete-remote:old"},
	}}
	graph, err := buildSyncExecutionGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []int{1, 2} {
		if got := graph.Dependencies[index]; !reflect.DeepEqual(got, []int{0}) {
			t.Fatalf("covered mirror-delete skip %d dependencies: got %#v want [0]", index, got)
		}
	}
	if syncItemCreatesRemoteDirectory(plan.Items[0]) {
		t.Fatal("remote directory deletion was incorrectly treated as a directory-creation barrier")
	}
}

func TestBuildSyncExecutionGraphSerializesDeleteWithSameParentReplacement(t *testing.T) {
	plan := syncPlan{Items: []syncPlanItem{
		{RelativePath: "remote/a.bin", Action: "delete-remote", Kind: "file", Destructive: true},
		{RelativePath: "remote/b.bin", Action: "replace-remote", Kind: "file", Destructive: true},
		{RelativePath: "local/a.bin", Action: "delete-local", Kind: "file", Destructive: true},
		{RelativePath: "local/b.bin", Action: "replace-local", Kind: "file", Destructive: true},
	}}
	graph, err := buildSyncExecutionGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	if got := graph.Dependencies[1]; !reflect.DeepEqual(got, []int{0}) {
		t.Fatalf("remote destructive sequence not serialized: %#v", got)
	}
	if got := graph.Dependencies[3]; !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("local destructive sequence not serialized: %#v", got)
	}
}

func TestBuildSyncExecutionGraphLeavesIndependentFilesReadyTogether(t *testing.T) {
	plan := syncPlan{Items: []syncPlanItem{
		{RelativePath: "a.bin", Action: "upload", Kind: "file"},
		{RelativePath: "b.bin", Action: "upload", Kind: "file"},
		{RelativePath: "c.bin", Action: "download", Kind: "file"},
	}}
	graph, err := buildSyncExecutionGraph(plan)
	if err != nil {
		t.Fatal(err)
	}
	for index, dependencies := range graph.Dependencies {
		if len(dependencies) != 0 {
			t.Fatalf("independent file %d unexpectedly depends on %#v", index, dependencies)
		}
	}
}

func TestValidateSyncExecutionGraphRejectsCycle(t *testing.T) {
	graph := syncExecutionGraph{
		Dependencies: [][]int{{1}, {0}},
		Dependents:   [][]int{{1}, {0}},
	}
	if err := validateSyncExecutionGraph(graph); err == nil {
		t.Fatal("dependency cycle was accepted")
	}
}

func TestResolveSyncJobsAndSharedWorkerBudget(t *testing.T) {
	if _, err := resolveSyncJobs(0); err == nil {
		t.Fatal("--jobs=0 was accepted")
	}
	if jobs, err := resolveSyncJobs(4); err != nil || jobs != 4 {
		t.Fatalf("valid jobs rejected: jobs=%d err=%v", jobs, err)
	}
	tests := []struct {
		workers       int
		jobs          int
		fileTransfers int
		wantWorkers   int
		wantSlots     int
		wantErr       bool
	}{
		{workers: 8, jobs: 4, fileTransfers: 4, wantWorkers: 2, wantSlots: 4},
		{workers: 8, jobs: 4, fileTransfers: 2, wantWorkers: 4, wantSlots: 2},
		{workers: 2, jobs: 4, fileTransfers: 2, wantWorkers: 1, wantSlots: 2},
		{workers: 1, jobs: 4, fileTransfers: 2, wantWorkers: 1, wantSlots: 1},
		{workers: 8, jobs: 4, fileTransfers: 1, wantWorkers: 8, wantSlots: 1},
		{workers: 8, jobs: 1, fileTransfers: 4, wantWorkers: 8, wantSlots: 1},
		{workers: 8, jobs: 4, fileTransfers: 0, wantWorkers: 8, wantSlots: 0},
		{workers: 0, jobs: 4, fileTransfers: 2, wantErr: true},
	}
	for _, test := range tests {
		workers, slots, err := syncTransferBudget(test.workers, test.jobs, test.fileTransfers)
		if test.wantErr {
			if err == nil {
				t.Fatalf("worker budget workers=%d jobs=%d transfers=%d unexpectedly succeeded with workers=%d slots=%d", test.workers, test.jobs, test.fileTransfers, workers, slots)
			}
			continue
		}
		if err != nil || workers != test.wantWorkers || slots != test.wantSlots {
			t.Fatalf("worker budget workers=%d jobs=%d transfers=%d: got workers=%d slots=%d err=%v want workers=%d slots=%d", test.workers, test.jobs, test.fileTransfers, workers, slots, err, test.wantWorkers, test.wantSlots)
		}
		legacyWorkers, legacyErr := syncTransferWorkerLimit(test.workers, test.jobs, test.fileTransfers)
		if legacyErr != nil || legacyWorkers != workers {
			t.Fatalf("worker-limit wrapper mismatch: got=%d err=%v want=%d", legacyWorkers, legacyErr, workers)
		}
	}
}
