package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
)

type mcpResidualRemoteObject struct {
	ID      string
	Name    string
	Size    int64
	SHA1    string
	Visible bool
}

type mcpResidualUploadFixture struct {
	ft          *FileTools
	args        ExecuteSyncPlanArgs
	state       mcpSyncPlannedState
	store       syncjournalpkg.Store
	remote      map[string]*mcpResidualRemoteObject
	uploadCalls map[string]int
}

func newMCPResidualUploadFixture(t *testing.T) mcpResidualUploadFixture {
	t.Helper()
	root := t.TempDir()
	payloads := map[string]string{"a.bin": "alpha", "b.bin": "bravo!"}
	remote := make(map[string]*mcpResidualRemoteObject, len(payloads))
	for name, payload := range payloads {
		if err := os.WriteFile(filepath.Join(root, name), []byte(payload), 0o600); err != nil {
			t.Fatal(err)
		}
		remote[name] = &mcpResidualRemoteObject{
			ID: name + "-remote-id", Name: name, Size: int64(len(payload)), SHA1: mcpSyncTestSHA1(payload),
		}
	}
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			remotePath := strings.Trim(req.URL.Query().Get("path"), "/")
			if remotePath == "remote" {
				return mcpResolveJSONResponse(req, `{"state":true,"id":"r"}`), nil
			}
			if strings.HasPrefix(remotePath, "remote/") {
				return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
			}
			t.Fatalf("unexpected residual-resume getid path %q", remotePath)
		case "/natsort/files.php", "/files":
			if req.URL.Query().Get("cid") != "r" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected residual-resume listing: %s", req.URL)
			}
			names := make([]string, 0, len(remote))
			for name, object := range remote {
				if object.Visible {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			rows := make([]string, 0, len(names))
			for _, name := range names {
				object := remote[name]
				rows = append(rows, fmt.Sprintf(`{"fid":%q,"cid":"r","n":%q,"s":%q,"sha":%q}`, object.ID, object.Name, fmt.Sprintf("%d", object.Size), object.SHA1))
			}
			body := fmt.Sprintf(`{"state":true,"cid":"r","count":%d,"offset":0,"limit":500,"data":[%s]}`, len(rows), strings.Join(rows, ","))
			return mcpResolveJSONResponse(req, body), nil
		case "/files/get_info":
			fileID := req.URL.Query().Get("file_id")
			for _, object := range remote {
				if object.Visible && object.ID == fileID {
					body := fmt.Sprintf(`{"state":true,"data":[{"fid":%q,"cid":"r","n":%q,"s":%q,"sha":%q}]}`, object.ID, object.Name, fmt.Sprintf("%d", object.Size), object.SHA1)
					return mcpResolveJSONResponse(req, body), nil
				}
			}
			t.Fatalf("unexpected residual-resume metadata request: %s", req.URL)
		default:
			t.Fatalf("unexpected residual-resume request: %s %s", req.Method, req.URL)
		}
		return nil, errors.New("unreachable")
	})})))
	store := syncjournalpkg.Store{Root: t.TempDir(), ProfileScope: strings.Repeat("f", 64), AccountID: 42}
	ft := NewFileTools(client, WithLocalRoot(root), WithDestructiveTools(true), WithSyncJournalStore(&store))
	uploadCalls := make(map[string]int)
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, dirID, name string, size int64, _ *os.File, options uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls[name]++
		object := remote[name]
		if object == nil || dirID != "r" || size != object.Size || options.PreparedDigest == nil || !strings.EqualFold(options.PreparedDigest.SHA1, object.SHA1) {
			t.Fatalf("unexpected residual prepared upload: dir=%q name=%q size=%d options=%#v", dirID, name, size, options)
		}
		object.Visible = true
		return uploadpkg.Result{BytesUploaded: size}, nil
	}
	planArgs := PlanSyncArgs{LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 20, MaxChecksumBytes: 128}
	state, err := planMCPSyncState(context.Background(), client, root, planArgs)
	if err != nil {
		t.Fatal(err)
	}
	if state.Plan.ChangeActions != 2 {
		t.Fatalf("residual fixture initial plan = %#v", state.Plan.Items)
	}
	return mcpResidualUploadFixture{
		ft: ft,
		args: ExecuteSyncPlanArgs{
			LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 20, MaxChecksumBytes: 128,
			ExpectPlanID: state.Output.Plan.PlanID,
		},
		state: state, store: store, remote: remote, uploadCalls: uploadCalls,
	}
}

func seedMCPResidualUploadJournal(t *testing.T, fixture mcpResidualUploadFixture) {
	t.Helper()
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	indices := map[string]int{}
	for index, item := range fixture.state.Plan.Items {
		indices[item.RelativePath] = index
	}
	aIndex, aOK := indices["a.bin"]
	bIndex, bOK := indices["b.bin"]
	if !aOK || !bOK {
		handle.Close()
		t.Fatalf("residual fixture item indices = %#v", indices)
	}
	aObject := fixture.remote["a.bin"]
	if err := handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		journal.State = syncjournalpkg.StatusFailed
		journal.Items[aIndex].State = "succeeded"
		journal.Items[aIndex].Phase = syncjournalpkg.PhaseDone
		journal.Items[aIndex].Attempts = 1
		journal.Items[aIndex].LastError = ""
		journal.Items[aIndex].Post = &syncjournalpkg.Postcondition{
			Side: "remote", Exists: true, Kind: "file", RemoteID: aObject.ID, Size: aObject.Size, SHA1: aObject.SHA1,
		}
		journal.Items[bIndex].State = "failed"
		journal.Items[bIndex].Phase = syncjournalpkg.PhaseMutationStarted
		journal.Items[bIndex].Attempts = 1
		journal.Items[bIndex].LastError = "synthetic non-destructive interruption"
		return nil
	}); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.WriteReviewAlias(fixture.args.ExpectPlanID, fixture.state.Plan.PlanID); err != nil {
		t.Fatal(err)
	}
	aObject.Visible = true
}

func TestExecuteSyncPlanResumesOnlyResidualActionsAfterPartialSuccess(t *testing.T) {
	fixture := newMCPResidualUploadFixture(t)
	seedMCPResidualUploadJournal(t, fixture)
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" {
		t.Fatalf("residual resume result=%#v output=%#v err=%v", result, output, err)
	}
	if output.PlanID != fixture.args.ExpectPlanID || !output.Summary.JournalPersisted || !output.Summary.JournalResumed || output.Summary.JournalCompletedBefore != 1 {
		t.Fatalf("residual resume summary=%#v", output.Summary)
	}
	if fixture.uploadCalls["a.bin"] != 0 || fixture.uploadCalls["b.bin"] != 1 {
		t.Fatalf("residual upload calls = %#v", fixture.uploadCalls)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != syncjournalpkg.StatusCompleted || journal.Status != syncjournalpkg.StatusCompleted || journal.RunStats.ResumeRuns != 1 || journal.RunStats.Runs != 1 {
		t.Fatalf("residual final journal = %#v", journal)
	}
	attempts := map[string]int{}
	states := map[string]string{}
	for index, item := range journal.Plan.Items {
		attempts[item.RelativePath] = journal.Items[index].Attempts
		states[item.RelativePath] = journal.Items[index].State
	}
	if attempts["a.bin"] != 1 || attempts["b.bin"] != 2 || states["a.bin"] != "succeeded" || states["b.bin"] != "succeeded" {
		t.Fatalf("residual final items attempts=%#v states=%#v", attempts, states)
	}
}

func TestExecuteSyncPlanResidualResumeRejectsChangedCompletedPostconditionBeforeMutation(t *testing.T) {
	fixture := newMCPResidualUploadFixture(t)
	seedMCPResidualUploadJournal(t, fixture)
	fixture.remote["a.bin"].ID = "changed-remote-identity"
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_state_changed" || output.RecoveryRequired {
		t.Fatalf("stale residual resume result=%#v output=%#v err=%v", result, output, err)
	}
	if fixture.uploadCalls["a.bin"] != 0 || fixture.uploadCalls["b.bin"] != 0 || fixture.remote["b.bin"].Visible {
		t.Fatalf("stale residual resume mutated state: calls=%#v remote_b=%v", fixture.uploadCalls, fixture.remote["b.bin"].Visible)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	attempts := map[string]int{}
	for index, item := range journal.Plan.Items {
		attempts[item.RelativePath] = journal.Items[index].Attempts
	}
	if journal.State != syncjournalpkg.StatusFailed || attempts["a.bin"] != 1 || attempts["b.bin"] != 1 {
		t.Fatalf("stale residual resume changed journal = %#v attempts=%#v", journal, attempts)
	}
}
