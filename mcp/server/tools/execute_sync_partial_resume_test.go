package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
)

type mcpPartialResumeFixture struct {
	ft           *FileTools
	args         ExecuteSyncPlanArgs
	planArgs     PlanSyncArgs
	initial      mcpSyncPlannedState
	store        syncjournalpkg.Store
	uploadedA    bool
	uploadedB    bool
	uploadCallsA int
	uploadCallsB int
	bFailures    int
}

func newMCPPartialResumeFixture(t *testing.T) *mcpPartialResumeFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.bin"), []byte("AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.bin"), []byte("BBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture := &mcpPartialResumeFixture{
		store:     syncjournalpkg.Store{Root: t.TempDir(), ProfileScope: strings.Repeat("7", 64), AccountID: 42},
		bFailures: 1,
	}
	shaA := mcpSyncTestSHA1("AAAA")
	shaB := mcpSyncTestSHA1("BBBB")
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			remotePath := strings.Trim(req.URL.Query().Get("path"), "/")
			switch remotePath {
			case "remote":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"r"}`), nil
			case "remote/a.bin", "remote/b.bin":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
			default:
				t.Fatalf("unexpected partial-resume getid path %q", remotePath)
			}
		case "/natsort/files.php", "/files":
			if req.URL.Query().Get("cid") != "r" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected partial-resume listing: %s", req.URL)
			}
			entries := make([]string, 0, 2)
			if fixture.uploadedA {
				entries = append(entries, fmt.Sprintf(`{"fid":"remote-a","cid":"r","n":"a.bin","s":"4","sha":%q}`, shaA))
			}
			if fixture.uploadedB {
				entries = append(entries, fmt.Sprintf(`{"fid":"remote-b","cid":"r","n":"b.bin","s":"4","sha":%q}`, shaB))
			}
			body := fmt.Sprintf(`{"state":true,"cid":"r","count":%d,"offset":0,"limit":500,"data":[%s]}`, len(entries), strings.Join(entries, ","))
			return mcpResolveJSONResponse(req, body), nil
		case "/files/get_info":
			switch req.URL.Query().Get("file_id") {
			case "remote-a":
				if !fixture.uploadedA {
					t.Fatal("metadata requested for a.bin before upload")
				}
				return mcpResolveJSONResponse(req, fmt.Sprintf(`{"state":true,"data":[{"fid":"remote-a","cid":"r","n":"a.bin","s":"4","sha":%q}]}`, shaA)), nil
			case "remote-b":
				if !fixture.uploadedB {
					t.Fatal("metadata requested for b.bin before upload")
				}
				return mcpResolveJSONResponse(req, fmt.Sprintf(`{"state":true,"data":[{"fid":"remote-b","cid":"r","n":"b.bin","s":"4","sha":%q}]}`, shaB)), nil
			default:
				t.Fatalf("unexpected partial-resume metadata request: %s", req.URL)
			}
		default:
			t.Fatalf("unexpected partial-resume request: %s %s", req.Method, req.URL)
		}
		return nil, errors.New("unreachable")
	})})))
	fixture.ft = NewFileTools(client, WithLocalRoot(root), WithDestructiveTools(true), WithSyncJournalStore(&fixture.store))
	fixture.ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, dirID, name string, size int64, _ *os.File, options uploadpkg.Options) (uploadpkg.Result, error) {
		if dirID != "r" || size != 4 || options.PreparedDigest == nil || options.PreparedDigest.SHA1 == "" {
			t.Fatalf("unexpected partial prepared upload: dir=%q name=%q size=%d options=%#v", dirID, name, size, options)
		}
		switch name {
		case "a.bin":
			fixture.uploadCallsA++
			fixture.uploadedA = true
		case "b.bin":
			fixture.uploadCallsB++
			if fixture.bFailures > 0 {
				fixture.bFailures--
				return uploadpkg.Result{}, errors.New("synthetic first-run b.bin failure")
			}
			fixture.uploadedB = true
		default:
			t.Fatalf("unexpected partial-resume upload name %q", name)
		}
		return uploadpkg.Result{BytesUploaded: size}, nil
	}
	fixture.planArgs = PlanSyncArgs{LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 20, MaxChecksumBytes: 128}
	initial, err := planMCPSyncState(context.Background(), client, root, fixture.planArgs)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Output.Summary.UploadFiles != 2 || len(initial.Plan.Items) != 2 {
		t.Fatalf("unexpected initial partial plan: %#v", initial.Output.Summary)
	}
	fixture.initial = initial
	fixture.args = ExecuteSyncPlanArgs{
		LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 20, MaxChecksumBytes: 128,
		ExpectPlanID: initial.Output.Plan.PlanID,
	}
	return fixture
}

func TestExecuteSyncPlanPartialJournalResumeExecutesOnlyRemainingUpload(t *testing.T) {
	fixture := newMCPPartialResumeFixture(t)
	firstResult, firstOutput, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || firstResult == nil || !firstResult.IsError || firstOutput.ErrorCode != "execution_failed" || firstOutput.Summary.Succeeded != 1 || firstOutput.Summary.Failed != 1 || !firstOutput.Summary.JournalPersisted || firstOutput.Summary.JournalResumed {
		t.Fatalf("first partial run result=%#v output=%#v err=%v", firstResult, firstOutput, err)
	}
	if fixture.uploadCallsA != 1 || fixture.uploadCallsB != 1 || !fixture.uploadedA || fixture.uploadedB {
		t.Fatalf("first partial calls a=%d b=%d uploadedA=%v uploadedB=%v", fixture.uploadCallsA, fixture.uploadCallsB, fixture.uploadedA, fixture.uploadedB)
	}
	fresh, err := planMCPSync(context.Background(), fixture.ft.client, fixture.ft.localRoot, fixture.planArgs)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Plan.PlanID == fixture.args.ExpectPlanID || fresh.Summary.UploadFiles != 1 {
		t.Fatalf("partial success did not change fresh public plan: fresh=%#v reviewed=%q", fresh, fixture.args.ExpectPlanID)
	}

	secondResult, secondOutput, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || secondResult == nil || secondResult.IsError || secondOutput.ErrorCode != "" || !secondOutput.Summary.JournalPersisted || !secondOutput.Summary.JournalResumed || secondOutput.Summary.JournalCompletedBefore != 1 || secondOutput.Summary.Succeeded != 1 || secondOutput.Summary.Skipped != 1 || secondOutput.Summary.UploadedFiles != 1 {
		t.Fatalf("resumed partial run result=%#v output=%#v err=%v", secondResult, secondOutput, err)
	}
	if fixture.uploadCallsA != 1 || fixture.uploadCallsB != 2 || !fixture.uploadedA || !fixture.uploadedB {
		t.Fatalf("resume repeated completed work or missed pending work: a=%d b=%d uploadedA=%v uploadedB=%v", fixture.uploadCallsA, fixture.uploadCallsB, fixture.uploadedA, fixture.uploadedB)
	}
	journal, err := fixture.store.InspectCurrent(fixture.initial.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != syncjournalpkg.StatusCompleted || journal.Status != syncjournalpkg.StatusCompleted || journal.RunStats.Runs != 2 || journal.RunStats.ResumeRuns != 1 || journal.RunStats.InterruptedRuns != 0 || journal.Items[0].Attempts != 1 || journal.Items[1].Attempts != 2 || journal.Items[0].Post == nil || journal.Items[1].Post == nil {
		t.Fatalf("final partial-resume journal=%#v", journal)
	}
}

func TestExecuteSyncPlanPartialResumeRejectsChangedCompletedItemBeforeP10(t *testing.T) {
	fixture := newMCPPartialResumeFixture(t)
	firstResult, firstOutput, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || firstResult == nil || !firstResult.IsError || firstOutput.Summary.Succeeded != 1 || firstOutput.Summary.Failed != 1 {
		t.Fatalf("first changed-completed run result=%#v output=%#v err=%v", firstResult, firstOutput, err)
	}
	fixture.uploadedA = false // external removal after the recorded successful postcondition
	callsA, callsB := fixture.uploadCallsA, fixture.uploadCallsB
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_state_changed" {
		t.Fatalf("changed completed item resume result=%#v output=%#v err=%v", result, output, err)
	}
	if fixture.uploadCallsA != callsA || fixture.uploadCallsB != callsB {
		t.Fatalf("changed completed item reached P10: before a=%d b=%d after a=%d b=%d", callsA, callsB, fixture.uploadCallsA, fixture.uploadCallsB)
	}
}

func TestExecuteSyncPlanPartialResumeRejectsExternallyCreatedPendingTargetBeforeP10(t *testing.T) {
	fixture := newMCPPartialResumeFixture(t)
	firstResult, firstOutput, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || firstResult == nil || !firstResult.IsError || firstOutput.Summary.Succeeded != 1 || firstOutput.Summary.Failed != 1 {
		t.Fatalf("first external-target run result=%#v output=%#v err=%v", firstResult, firstOutput, err)
	}
	fixture.uploadedB = true // target appeared outside this journal after b.bin failed
	callsA, callsB := fixture.uploadCallsA, fixture.uploadCallsB
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_state_changed" {
		t.Fatalf("external pending target resume result=%#v output=%#v err=%v", result, output, err)
	}
	if fixture.uploadCallsA != callsA || fixture.uploadCallsB != callsB {
		t.Fatalf("external pending target reached P10: before a=%d b=%d after a=%d b=%d", callsA, callsB, fixture.uploadCallsA, fixture.uploadCallsB)
	}
}

func prepareInterruptedPersistentUploadJournal(t *testing.T, fixture mcpPersistentUploadFixture) {
	t.Helper()
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().Add(-time.Minute).UTC()
	if err := handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		journal.State = syncjournalpkg.StatusActive
		journal.Status = syncjournalpkg.StatusActive
		journal.Items[0].State = "running"
		journal.Items[0].Phase = syncjournalpkg.PhaseMutationStarted
		journal.Items[0].Attempts = 1
		journal.RunStats.Runs = 1
		journal.RunStats.LastStartedAt = &started
		journal.RunStats.LastFinishedAt = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.WriteReviewAlias(fixture.args.ExpectPlanID, fixture.state.Plan.PlanID); err != nil {
		t.Fatal(err)
	}
}

func TestExecuteSyncPlanResumesInterruptedNonDestructiveMutationAndCountsCrash(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	prepareInterruptedPersistentUploadJournal(t, fixture)
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || !output.Summary.JournalResumed || output.Summary.JournalCompletedBefore != 0 || output.Summary.UploadedFiles != 1 || *fixture.uploadCalls != 1 || !*fixture.uploaded {
		t.Fatalf("interrupted resume result=%#v output=%#v err=%v calls=%d uploaded=%v", result, output, err, *fixture.uploadCalls, *fixture.uploaded)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != syncjournalpkg.StatusCompleted || journal.RunStats.Runs != 2 || journal.RunStats.ResumeRuns != 1 || journal.RunStats.InterruptedRuns != 1 || journal.Items[0].Attempts != 2 || journal.Items[0].Post == nil {
		t.Fatalf("interrupted resume journal=%#v", journal)
	}
}

func TestExecuteSyncPlanInterruptedMutationRejectsAppearedTargetBeforeP10(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	prepareInterruptedPersistentUploadJournal(t, fixture)
	*fixture.uploaded = true // possible external/ambiguous completion after the crashed mutation-started phase
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_state_changed" || *fixture.uploadCalls != 0 {
		t.Fatalf("appeared interrupted target result=%#v output=%#v err=%v calls=%d", result, output, err, *fixture.uploadCalls)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != syncjournalpkg.StatusActive || journal.Items[0].Phase != syncjournalpkg.PhaseMutationStarted || journal.Items[0].Attempts != 1 || journal.RunStats.Runs != 1 || journal.RunStats.InterruptedRuns != 0 {
		t.Fatalf("refused interrupted journal was mutated: %#v", journal)
	}
}

func TestExecuteSyncPlanRefusesUnverifiedMutationDoneReplay(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		journal.State = syncjournalpkg.StatusFailed
		journal.Status = syncjournalpkg.StatusFailed
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = syncjournalpkg.PhaseMutationDone
		journal.Items[0].Attempts = 1
		journal.Items[0].LastError = "postcondition was not observable"
		now := time.Now().UTC()
		journal.RunStats.Runs = 1
		journal.RunStats.LastStartedAt = &now
		journal.RunStats.LastFinishedAt = &now
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.WriteReviewAlias(fixture.args.ExpectPlanID, fixture.state.Plan.PlanID); err != nil {
		t.Fatal(err)
	}
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_reconcile_required" || *fixture.uploadCalls != 0 || *fixture.uploaded {
		t.Fatalf("mutation-done replay result=%#v output=%#v err=%v calls=%d uploaded=%v", result, output, err, *fixture.uploadCalls, *fixture.uploaded)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Items[0].Phase != syncjournalpkg.PhaseMutationDone || journal.Items[0].Attempts != 1 {
		t.Fatalf("mutation-done journal was altered by refused replay: %#v", journal.Items[0])
	}
}
