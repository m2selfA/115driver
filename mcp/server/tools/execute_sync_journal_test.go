package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpPersistentUploadFixture struct {
	ft          *FileTools
	args        ExecuteSyncPlanArgs
	state       mcpSyncPlannedState
	store       syncjournalpkg.Store
	localPath   string
	uploadCalls *int
	uploaded    *bool
}

func newMCPPersistentUploadFixture(t *testing.T) mcpPersistentUploadFixture {
	t.Helper()
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	uploaded := false
	uploadCalls := 0
	sha1 := mcpSyncTestSHA1("payload")
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			remotePath := strings.Trim(req.URL.Query().Get("path"), "/")
			switch remotePath {
			case "remote":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"r"}`), nil
			case "remote/source.bin":
				// File resolution must fall back to the parent listing; non-zero
				// getid responses are directory identities in remoteresolver.
				return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
			default:
				t.Fatalf("unexpected persistent upload getid path %q", remotePath)
			}
		case "/natsort/files.php", "/files":
			if req.URL.Query().Get("cid") != "r" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected persistent upload listing: %s", req.URL)
			}
			if uploaded {
				return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":1,"offset":0,"limit":500,"data":[{"fid":"remote-object-secret-42","cid":"r","n":"source.bin","s":"7","sha":"`+sha1+`"}]}`), nil
			}
			return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":0,"offset":0,"limit":500,"data":[]}`), nil
		case "/files/get_info":
			if req.URL.Query().Get("file_id") != "remote-object-secret-42" || !uploaded {
				t.Fatalf("unexpected persistent upload metadata request: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"data":[{"fid":"remote-object-secret-42","cid":"r","n":"source.bin","s":"7","sha":"`+sha1+`"}]}`), nil
		default:
			t.Fatalf("unexpected persistent upload request: %s %s", req.Method, req.URL)
		}
		return nil, errors.New("unreachable")
	})})))
	store := syncjournalpkg.Store{Root: t.TempDir(), ProfileScope: strings.Repeat("a", 64), AccountID: 42}
	ft := NewFileTools(client, WithLocalRoot(root), WithDestructiveTools(true), WithSyncJournalStore(&store))
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, dirID, name string, size int64, _ *os.File, options uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		if dirID != "r" || name != "source.bin" || size != 7 || options.PreparedDigest == nil || options.PreparedDigest.SHA1 == "" {
			t.Fatalf("unexpected persistent prepared upload: dir=%q name=%q size=%d options=%#v", dirID, name, size, options)
		}
		uploaded = true
		return uploadpkg.Result{BytesUploaded: size}, nil
	}
	planArgs := PlanSyncArgs{LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 20, MaxChecksumBytes: 64}
	state, err := planMCPSyncState(context.Background(), client, root, planArgs)
	if err != nil {
		t.Fatal(err)
	}
	return mcpPersistentUploadFixture{
		ft: ft,
		args: ExecuteSyncPlanArgs{
			LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 20, MaxChecksumBytes: 64,
			ExpectPlanID: state.Output.Plan.PlanID,
		},
		state: state, store: store, localPath: localPath, uploadCalls: &uploadCalls, uploaded: &uploaded,
	}
}

func TestExecuteSyncPlanPersistsCompletedJournalAndPostcondition(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || output.Summary.Succeeded != 1 || !output.Summary.JournalPersisted || output.Summary.JournalResumed || output.Summary.JournalVersion != syncjournalpkg.Version || output.Summary.JournalState != syncjournalpkg.StatusCompleted || output.Summary.JournalStatus != syncjournalpkg.StatusCompleted {
		t.Fatalf("persistent upload result=%#v output=%#v err=%v", result, output, err)
	}
	if *fixture.uploadCalls != 1 || !*fixture.uploaded {
		t.Fatalf("persistent upload calls=%d uploaded=%v", *fixture.uploadCalls, *fixture.uploaded)
	}
	aliasedPlanID, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID)
	if err != nil || aliasedPlanID != fixture.state.Plan.PlanID {
		t.Fatalf("review alias resolved internal plan=%q err=%v, want %q", aliasedPlanID, err, fixture.state.Plan.PlanID)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != syncjournalpkg.StatusCompleted || journal.Status != syncjournalpkg.StatusCompleted || len(journal.Items) != 1 || journal.Items[0].State != "succeeded" || journal.Items[0].Phase != syncjournalpkg.PhaseDone || journal.Items[0].Attempts != 1 || journal.Items[0].Post == nil {
		t.Fatalf("persisted completed journal = %#v", journal)
	}
	post := journal.Items[0].Post
	if post.Side != "remote" || !post.Exists || post.Kind != "file" || post.RemoteID != "remote-object-secret-42" || post.Size != 7 || !strings.EqualFold(post.SHA1, mcpSyncTestSHA1("payload")) {
		t.Fatalf("persisted upload postcondition = %#v", post)
	}
}

func TestExecuteSyncPlanPersistentJournalWireExposesOnlySafeSummary(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	result := callExecuteSyncPlanWire(t, fixture.ft, fixture.args)
	if result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("persistent journal wire result=%#v", result)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output MCPSyncExecutionOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.PlanID != fixture.args.ExpectPlanID || !output.Summary.JournalPersisted || output.Summary.JournalVersion != syncjournalpkg.Version || output.Summary.JournalState != syncjournalpkg.StatusCompleted || output.Summary.JournalStatus != syncjournalpkg.StatusCompleted {
		t.Fatalf("persistent journal wire structured output=%#v", output)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		for _, forbidden := range []string{fixture.localPath, fixture.store.Root, fixture.state.Plan.PlanID, "remote-object-secret-42"} {
			if strings.Contains(payload, forbidden) {
				t.Fatalf("persistent journal wire output leaked hidden identity %q: %s", forbidden, payload)
			}
		}
		if strings.Contains(strings.ToLower(payload), "sha1") {
			t.Fatalf("persistent journal wire output leaked hash field: %s", payload)
		}
	}
}

func TestExecuteSyncPlanSafelyResumesNonDestructiveFailedJournal(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		journal.State = syncjournalpkg.StatusFailed
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = syncjournalpkg.PhaseMutationStarted
		journal.Items[0].Attempts = 1
		journal.Items[0].LastError = "synthetic non-destructive upload failure"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || !output.Summary.JournalPersisted || !output.Summary.JournalResumed || output.Summary.Succeeded != 1 {
		t.Fatalf("resumed upload result=%#v output=%#v err=%v", result, output, err)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != syncjournalpkg.StatusCompleted || journal.RunStats.ResumeRuns != 1 || journal.RunStats.Runs != 1 || journal.Items[0].Attempts != 2 || journal.Items[0].State != "succeeded" {
		t.Fatalf("resumed persistent journal = %#v", journal)
	}
}

type mcpPersistentDeleteFixture struct {
	ft        *FileTools
	args      ExecuteSyncPlanArgs
	state     mcpSyncPlannedState
	store     syncjournalpkg.Store
	localPath string
}

func newMCPPersistentLocalDeleteFixture(t *testing.T) mcpPersistentDeleteFixture {
	t.Helper()
	root := t.TempDir()
	localPath := filepath.Join(root, "orphan.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			if strings.Trim(req.URL.Query().Get("path"), "/") != "remote" {
				t.Fatalf("unexpected persistent delete getid: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"id":"r"}`), nil
		case "/natsort/files.php", "/files":
			if req.URL.Query().Get("cid") != "r" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected persistent delete listing: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":0,"offset":0,"limit":500,"data":[]}`), nil
		default:
			t.Fatalf("unexpected persistent delete request: %s %s", req.Method, req.URL)
		}
		return nil, errors.New("unreachable")
	})})))
	store := syncjournalpkg.Store{Root: t.TempDir(), ProfileScope: strings.Repeat("b", 64), AccountID: 42}
	ft := NewFileTools(client, WithLocalRoot(root), WithDestructiveTools(true), WithSyncJournalStore(&store))
	planArgs := PlanSyncArgs{LocalPath: root, RemotePath: "/remote", Direction: "download", DeleteExtraneous: true, MaxNodes: 20, MaxChecksumBytes: 64}
	state, err := planMCPSyncState(context.Background(), client, root, planArgs)
	if err != nil {
		t.Fatal(err)
	}
	return mcpPersistentDeleteFixture{
		ft:    ft,
		args:  ExecuteSyncPlanArgs{LocalPath: root, RemotePath: "/remote", Direction: "download", DeleteExtraneous: true, MaxNodes: 20, MaxChecksumBytes: 64, ExpectPlanID: state.Output.Plan.PlanID},
		state: state, store: store, localPath: localPath,
	}
}

func TestExecuteSyncPlanRefusesDestructiveCrashJournalReplay(t *testing.T) {
	fixture := newMCPPersistentLocalDeleteFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		journal.State = syncjournalpkg.StatusFailed
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = syncjournalpkg.PhaseDeleteStarted
		journal.Items[0].Attempts = 1
		journal.Items[0].LastError = "synthetic crash after delete started"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := handle.Snapshot().Status; got != syncjournalpkg.StatusReconcileRequired {
		t.Fatalf("destructive crash journal status = %q", got)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}

	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || !result.IsError || !output.RecoveryRequired || output.ErrorCode != "recovery_required" || output.Summary.Processed != 0 {
		t.Fatalf("destructive crash replay result=%#v output=%#v err=%v", result, output, err)
	}
	if _, err := os.Stat(fixture.localPath); err != nil {
		t.Fatalf("destructive crash replay touched local target: %v", err)
	}
}

func TestExecuteSyncPlanRefusesConcurrentSamePlanJournal(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_in_use" || output.Summary.Processed != 0 || *fixture.uploadCalls != 0 {
		t.Fatalf("concurrent journal result=%#v output=%#v err=%v upload_calls=%d", result, output, err, *fixture.uploadCalls)
	}
}

func TestExecuteSyncPlanBackfillsAliasForPreAliasCurrentJournal(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("pre-alias fixture unexpectedly has reviewed binding: %v", err)
	}

	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || !output.Summary.JournalResumed || output.Summary.Succeeded != 1 || *fixture.uploadCalls != 1 || !*fixture.uploaded {
		t.Fatalf("pre-alias resume result=%#v output=%#v err=%v calls=%d uploaded=%v", result, output, err, *fixture.uploadCalls, *fixture.uploaded)
	}
	resolved, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID)
	if err != nil || resolved != fixture.state.Plan.PlanID {
		t.Fatalf("pre-alias resume did not backfill reviewed binding: resolved=%q err=%v", resolved, err)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil || journal.State != syncjournalpkg.StatusCompleted || journal.RunStats.ResumeRuns != 1 || len(journal.Items) != 1 || journal.Items[0].State != "succeeded" {
		t.Fatalf("pre-alias resumed journal=%#v err=%v", journal, err)
	}
}

func TestExecuteSyncPlanRebindsStaleAliasToCompatibleCurrentJournal(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	staleRawPlanID := strings.Repeat("7", 64)
	if staleRawPlanID == fixture.state.Plan.PlanID {
		staleRawPlanID = strings.Repeat("8", 64)
	}
	if _, err := fixture.store.WriteReviewAlias(fixture.args.ExpectPlanID, staleRawPlanID); err != nil {
		t.Fatal(err)
	}

	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || !output.Summary.JournalResumed || output.Summary.Succeeded != 1 || *fixture.uploadCalls != 1 {
		t.Fatalf("stale-alias rebound result=%#v output=%#v err=%v calls=%d", result, output, err, *fixture.uploadCalls)
	}
	resolved, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID)
	if err != nil || resolved != fixture.state.Plan.PlanID {
		t.Fatalf("stale alias was not rebound to compatible current journal: resolved=%q err=%v", resolved, err)
	}
}

func TestPrepareSyncResidualResumeRejectsAliasToDifferentReviewedPlanIdentity(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	wrongReviewID := "sha256:" + strings.Repeat("f", 64)
	if wrongReviewID == fixture.args.ExpectPlanID {
		wrongReviewID = "sha256:" + strings.Repeat("e", 64)
	}
	if _, err := fixture.store.WriteReviewAlias(wrongReviewID, fixture.state.Plan.PlanID); err != nil {
		t.Fatal(err)
	}
	planArgs := PlanSyncArgs{
		LocalPath: fixture.args.LocalPath, RemotePath: fixture.args.RemotePath,
		Direction: fixture.args.Direction, ConflictPolicy: fixture.args.ConflictPolicy,
		DeleteExtraneous: fixture.args.DeleteExtraneous, MaxNodes: fixture.args.MaxNodes, MaxChecksumBytes: fixture.args.MaxChecksumBytes,
	}
	run, _, handled, code, _, err := fixture.ft.prepareMCPSyncResidualResume(context.Background(), wrongReviewID, planArgs, mcpSyncDeleteBudget{})
	if err != nil || run != nil || !handled || code != "journal_alias_conflict" {
		t.Fatalf("mismatched alias resume run=%#v handled=%v code=%q err=%v", run, handled, code, err)
	}
	resolved, err := fixture.store.ResolveReviewAlias(wrongReviewID)
	if err != nil || resolved != fixture.state.Plan.PlanID {
		t.Fatalf("mismatched alias was unexpectedly mutated: resolved=%q err=%v", resolved, err)
	}
}

func TestExecuteSyncPlanRefusesAliasWhoseJournalIsSoftDeletedInCrashWindow(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.BindReviewAlias(fixture.args.ExpectPlanID); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	location, err := fixture.store.Location(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	trashPath, err := syncjournalpkg.MoveDirectoryToSessionTrash(fixture.store.Root, location.Dir, fixture.state.Plan.PlanID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := syncjournalpkg.ReadTrashReviewAliases(trashPath); err != nil || found {
		t.Fatalf("crash-window trash unexpectedly has alias sidecar: found=%v err=%v", found, err)
	}

	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_trashed" || output.Summary.Processed != 0 || *fixture.uploadCalls != 0 || *fixture.uploaded {
		t.Fatalf("soft-deleted crash replay result=%#v output=%#v err=%v calls=%d uploaded=%v", result, output, err, *fixture.uploadCalls, *fixture.uploaded)
	}
	resolved, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID)
	if err != nil || resolved != fixture.state.Plan.PlanID {
		t.Fatalf("soft-deleted crash alias was mutated: resolved=%q err=%v", resolved, err)
	}
}

func TestExecuteSyncPlanHealsOrphanReviewAliasBeforeFreshExecution(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	if _, err := fixture.store.WriteReviewAlias(fixture.args.ExpectPlanID, fixture.state.Plan.PlanID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("orphan fixture unexpectedly has a current journal: %v", err)
	}

	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || output.Summary.Succeeded != 1 || output.Summary.JournalResumed || *fixture.uploadCalls != 1 || !*fixture.uploaded {
		t.Fatalf("orphan alias self-heal result=%#v output=%#v err=%v calls=%d uploaded=%v", result, output, err, *fixture.uploadCalls, *fixture.uploaded)
	}
	resolved, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID)
	if err != nil || resolved != fixture.state.Plan.PlanID {
		t.Fatalf("fresh execution did not rebuild reviewed alias: resolved=%q err=%v", resolved, err)
	}
	journal, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID)
	if err != nil || journal.State != syncjournalpkg.StatusCompleted || len(journal.Items) != 1 || journal.Items[0].State != "succeeded" {
		t.Fatalf("fresh execution did not rebuild completed journal: journal=%#v err=%v", journal, err)
	}
}

func TestExecuteSyncPlanPreservesLockedOrphanReviewAlias(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	if _, err := fixture.store.WriteReviewAlias(fixture.args.ExpectPlanID, fixture.state.Plan.PlanID); err != nil {
		t.Fatal(err)
	}
	location, err := fixture.store.Location(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := transfer.AcquireSessionLock(location.LockPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()

	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_in_use" || output.Summary.Processed != 0 || *fixture.uploadCalls != 0 {
		t.Fatalf("locked orphan alias result=%#v output=%#v err=%v calls=%d", result, output, err, *fixture.uploadCalls)
	}
	resolved, err := fixture.store.ResolveReviewAlias(fixture.args.ExpectPlanID)
	if err != nil || resolved != fixture.state.Plan.PlanID {
		t.Fatalf("locked orphan alias was mutated: resolved=%q err=%v", resolved, err)
	}
	if _, err := fixture.store.InspectCurrent(fixture.state.Plan.PlanID); !errors.Is(err, syncjournalpkg.ErrNotFound) {
		t.Fatalf("locked orphan path unexpectedly gained a current journal: %v", err)
	}
}

func TestExecuteSyncPlanDelegatesLegacyJournalMigrationToCLI(t *testing.T) {
	fixture := newMCPPersistentUploadFixture(t)
	location, err := fixture.store.Location(fixture.state.Plan.PlanID)
	if err != nil {
		t.Fatal(err)
	}
	if err := transfer.WritePrivateFileAtomic(location.JournalPath, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "journal_migration_required" || output.Summary.Processed != 0 || *fixture.uploadCalls != 0 {
		t.Fatalf("legacy journal result=%#v output=%#v err=%v upload_calls=%d", result, output, err, *fixture.uploadCalls)
	}
}
