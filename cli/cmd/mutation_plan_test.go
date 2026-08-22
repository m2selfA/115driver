package cmd

import (
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

func TestBuildMoveOrCopyPlanUsesReadOnlyResolverAndDeduplicates(t *testing.T) {
	plan, err := buildMoveOrCopyPlan("copy", testRemotePathClient(), []string{"/a.txt", "/folder", "/a.txt"}, "/dest")
	if err != nil {
		t.Fatal(err)
	}
	if plan.DestinationID != "d-dest" || plan.InputCount != 3 || plan.UniqueItemCount != 2 {
		t.Fatalf("unexpected plan summary: %#v", plan)
	}
	got := []string{plan.Items[0].Destination, plan.Items[1].Destination}
	if want := []string{"/dest/a.txt", "/dest/folder"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected destinations: got %#v want %#v", got, want)
	}
}

func TestBuildMoveOrCopyPlanReusesScopedParentResolution(t *testing.T) {
	client := &fakeRemotePathClient{
		dirIDs: map[string]string{"dest": "d-dest", "shared": "d-shared"},
		lists: map[string][]driver.File{
			"d-shared": {
				{FileID: "f-a", Name: "a.txt"},
				{FileID: "f-b", Name: "b.txt"},
			},
		},
	}

	plan, err := buildMoveOrCopyPlan("copy", client, []string{"/shared/a.txt", "/shared/b.txt"}, "/dest")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 2 {
		t.Fatalf("unexpected plan items: %#v", plan.Items)
	}
	if got := client.dirCalls["shared"]; got != 1 {
		t.Fatalf("shared parent resolved %d times, want 1", got)
	}
}

func TestBuildRemoteDeletePlanSummarizesDirectorySubtreeWithoutDeleteAPI(t *testing.T) {
	client := &fakeMetadataClient{
		dirIDs: map[string]string{"folder": "d1"},
		lists: map[string][]driver.File{
			"d1": {{FileID: "f2", Name: "child.bin", Size: 5}},
		},
	}
	plan, err := buildRemoteDeletePlan(client, []string{"/folder"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 {
		t.Fatalf("unexpected delete plan: %#v", plan)
	}
	item := plan.Items[0]
	if item.ID != "d1" || item.Kind != "directory" || !item.Subtree || item.Files != 1 || item.Directories != 1 || item.Size != 5 {
		t.Fatalf("unexpected delete item: %#v", item)
	}
}

func TestBuildMkdirPlanStopsAfterResolverNetworkFailure(t *testing.T) {
	networkErr := &net.DNSError{Err: "temporary resolver failure", Name: "webapi.115.com", IsTemporary: true}
	client := &fakeRemotePathClient{dirErrs: map[string]error{"a": networkErr}}

	_, err := buildMkdirPlan(client, "/a/b", true)
	if err == nil || commandErrorCode(err) != output.ExitNetwork {
		t.Fatalf("mkdir plan network error = %T %v, code=%d; want ExitNetwork", err, err, commandErrorCode(err))
	}
	if client.listCalls != 0 {
		t.Fatalf("mkdir plan made %d list calls after resolver network failure, want 0", client.listCalls)
	}
}

func TestBuildMkdirPlanReportsParentsToReuseAndCreateWithoutMkdirAPI(t *testing.T) {
	client := &fakeRemotePathClient{
		dirIDs: map[string]string{"a": "d-a"},
		lists:  map[string][]driver.File{"d-a": {}},
	}
	plan, err := buildMkdirPlan(client, "/a/b/c", true)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Action != "create" || !reflect.DeepEqual(plan.Reuse, []string{"/a"}) || !reflect.DeepEqual(plan.Create, []string{"/a/b", "/a/b/c"}) {
		t.Fatalf("unexpected mkdir -p plan: %#v", plan)
	}
}

func TestBuildMkdirPlanRejectsRemoteFileConflict(t *testing.T) {
	client := &fakeRemotePathClient{
		dirIDs: map[string]string{},
		lists: map[string][]driver.File{
			"0": {{FileID: "f1", Name: "conflict", IsDirectory: false}},
		},
	}
	if _, err := buildMkdirPlan(client, "/conflict", false); err == nil || commandErrorCode(err) != output.ExitArgs {
		t.Fatalf("expected file conflict argument error, got %v", err)
	}
}

type fakeOfflinePlanClient struct {
	*fakeRemotePathClient
	tasks []*driver.OfflineTask
}

func (client *fakeOfflinePlanClient) ListOfflineTask(page int64) (driver.OfflineTaskResp, error) {
	if page != 1 {
		return driver.OfflineTaskResp{Page: page, PageCount: 1}, nil
	}
	return driver.OfflineTaskResp{Page: 1, PageCount: 1, Total: int64(len(client.tasks)), Tasks: client.tasks}, nil
}

type fakePagedOfflineClient struct {
	pages map[int64]driver.OfflineTaskResp
	calls []int64
}

func (client *fakePagedOfflineClient) ListOfflineTask(page int64) (driver.OfflineTaskResp, error) {
	client.calls = append(client.calls, page)
	return client.pages[page], nil
}

func TestLoadAllOfflineTasksUsesFirstPageAsBoundedSnapshot(t *testing.T) {
	client := &fakePagedOfflineClient{pages: map[int64]driver.OfflineTaskResp{
		1: {Page: 1, PageCount: 2, Total: 3, Tasks: []*driver.OfflineTask{{InfoHash: "A"}, {InfoHash: "B"}}},
		2: {Page: 2, PageCount: 99, Total: 100, Tasks: []*driver.OfflineTask{{InfoHash: "b"}, {InfoHash: "C"}}},
		3: {Page: 3, PageCount: 99, Total: 100, Tasks: []*driver.OfflineTask{{InfoHash: "D"}}},
	}}
	tasks, total, err := loadAllOfflineTasks(client)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(tasks) != 3 {
		t.Fatalf("bounded offline snapshot = total %d tasks %#v, want 3 unique tasks", total, tasks)
	}
	if want := []int64{1, 2}; !reflect.DeepEqual(client.calls, want) {
		t.Fatalf("offline snapshot pages = %#v, want %#v", client.calls, want)
	}
	if tasks[0].InfoHash != "A" || tasks[1].InfoHash != "B" || tasks[2].InfoHash != "C" {
		t.Fatalf("offline snapshot order/dedup = %#v", tasks)
	}
}

func TestLoadAllOfflineTasksRejectsWrongResponsePage(t *testing.T) {
	client := &fakePagedOfflineClient{pages: map[int64]driver.OfflineTaskResp{
		1: {Page: 1, PageCount: 2, Total: 2, Tasks: []*driver.OfflineTask{{InfoHash: "A"}}},
		2: {Page: 1, PageCount: 2, Total: 2, Tasks: []*driver.OfflineTask{{InfoHash: "B"}}},
	}}
	if _, _, err := loadAllOfflineTasks(client); err == nil || !strings.Contains(err.Error(), "requested page 2") {
		t.Fatalf("wrong response page error = %v, want diagnostic failure", err)
	}
}

func TestLoadAllOfflineTasksStopsWhenLiveListShrinks(t *testing.T) {
	client := &fakePagedOfflineClient{pages: map[int64]driver.OfflineTaskResp{
		1: {Page: 1, PageCount: 4, Total: 10, Tasks: []*driver.OfflineTask{{InfoHash: "A"}}},
		2: {Page: 2, PageCount: 2, Total: 2, Tasks: []*driver.OfflineTask{{InfoHash: "B"}}},
		3: {Page: 3, PageCount: 3, Total: 3, Tasks: []*driver.OfflineTask{{InfoHash: "C"}}},
	}}
	tasks, _, err := loadAllOfflineTasks(client)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || !reflect.DeepEqual(client.calls, []int64{1, 2}) {
		t.Fatalf("shrinking snapshot tasks=%#v calls=%#v", tasks, client.calls)
	}
}

func TestOfflineDryRunPlansUseOnlyReadAPIs(t *testing.T) {
	oldSaveDir := offlineSaveDir
	offlineSaveDir = "/save"
	t.Cleanup(func() { offlineSaveDir = oldSaveDir })
	client := &fakeOfflinePlanClient{
		fakeRemotePathClient: &fakeRemotePathClient{dirIDs: map[string]string{"save": "d-save"}, lists: map[string][]driver.File{}},
		tasks:                []*driver.OfflineTask{{InfoHash: "ABC", Name: "task", Size: 12, Status: 1, Percent: 0.5}},
	}
	addPlan, err := buildOfflineAddPlan(client, []string{"magnet:?xt=urn:btih:ABC"})
	if err != nil {
		t.Fatal(err)
	}
	if addPlan.SaveDirectory.ID != "d-save" || addPlan.ServerValidation != "deferred-until-submit" {
		t.Fatalf("unexpected offline add plan: %#v", addPlan)
	}
	rmPlan, err := buildOfflineRemovePlan(client, []string{"abc", "missing"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rmPlan.Items) != 2 || !rmPlan.Items[0].Visible || rmPlan.Items[1].Visible {
		t.Fatalf("unexpected offline remove plan: %#v", rmPlan)
	}
}

func TestRunSessionsRemoveDryRunLeavesSessionInspectable(t *testing.T) {
	store := transfer.SessionStore{Root: t.TempDir()}
	id := createSessionForRemoveTest(t, store, "dry-run.bin")
	oldJSON, oldPrinter, oldAbort := jsonOutput, printer, sessionsRmAbortRemote
	jsonOutput = true
	printer = output.NewPrinter(false)
	sessionsRmAbortRemote = false
	t.Cleanup(func() {
		jsonOutput, printer, sessionsRmAbortRemote = oldJSON, oldPrinter, oldAbort
	})
	cmd := newBatchInputTestCommand(t, "")
	if err := runSessionsRemoveDryRun(cmd, store, []string{id}); err != nil {
		t.Fatal(err)
	}
	entry, err := store.InspectSession(id)
	if err != nil || entry.ID != id {
		t.Fatalf("dry-run changed session state: entry=%#v err=%v", entry, err)
	}
}
