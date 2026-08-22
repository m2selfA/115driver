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

	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"

	syncexecpkg "github.com/SheltonZhu/115driver/internal/syncexec"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNormalizeMCPSyncExecutionJobsAndFailurePolicy(t *testing.T) {
	if jobs, err := normalizeMCPSyncExecutionJobs(0); err != nil || jobs != 1 {
		t.Fatalf("default sync execution jobs = %d, %v", jobs, err)
	}
	if jobs, err := normalizeMCPSyncExecutionJobs(maxMCPSyncExecutionJobs); err != nil || jobs != maxMCPSyncExecutionJobs {
		t.Fatalf("max sync execution jobs = %d, %v", jobs, err)
	}
	if _, err := normalizeMCPSyncExecutionJobs(-1); err == nil {
		t.Fatal("negative sync execution jobs was accepted")
	}
	if _, err := normalizeMCPSyncExecutionJobs(maxMCPSyncExecutionJobs + 1); err == nil {
		t.Fatal("oversized sync execution jobs was accepted")
	}
	if err := syncexecpkg.ValidateFailurePolicy(false, 1); err == nil {
		t.Fatal("max_errors without continue_on_error was accepted")
	}
}

func TestValidateMCPSyncExecutablePlanAcceptsSnapshotGuardedDestructiveDirectories(t *testing.T) {
	allowed := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "remote-dir", Action: "delete-remote", Kind: "directory", RemotePresent: true, RemotePath: "/remote-dir", RemoteID: "remote-dir-id", Destructive: true},
		{RelativePath: "local-dir", Action: "delete-local", Kind: "directory", LocalPresent: true, LocalPath: filepath.Join(t.TempDir(), "local-dir"), Destructive: true},
		{RelativePath: "replace-remote", Action: "replace-remote", Kind: "file", ReplacesKind: "directory", LocalPresent: true, LocalPath: filepath.Join(t.TempDir(), "replace-remote-winner.bin"), LocalSHA1: mcpSyncTestSHA1("winner"), RemotePresent: true, RemotePath: "/replace-remote", RemoteID: "old-remote-dir", Destructive: true},
		{RelativePath: "replace-local", Action: "replace-local", Kind: "file", ReplacesKind: "directory", LocalPresent: true, LocalPath: filepath.Join(t.TempDir(), "replace-local"), RemotePresent: true, RemotePath: "/replace-local", RemoteID: "winner", RemoteSHA1: mcpSyncTestSHA1("winner"), Destructive: true},
	}}
	if err := validateMCPSyncExecutablePlan(allowed); err != nil {
		t.Fatalf("snapshot-guarded destructive directory plan rejected: %v", err)
	}
	for name, plan := range map[string]syncplanpkg.Plan{
		"remote-missing-id":  {Items: []syncplanpkg.Item{{RelativePath: "dir", Action: "delete-remote", Kind: "directory", RemotePresent: true, Destructive: true}}},
		"local-missing-path": {Items: []syncplanpkg.Item{{RelativePath: "dir", Action: "delete-local", Kind: "directory", LocalPresent: true, Destructive: true}}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateMCPSyncExecutablePlan(plan); err == nil {
				t.Fatal("incomplete destructive plan identity was accepted")
			}
		})
	}
}

func TestValidateMCPSyncExecutablePlanRejectsIncompleteActionSnapshots(t *testing.T) {
	local := filepath.Join(t.TempDir(), "local.bin")
	completeLocal := func(action, kind string) syncplanpkg.Item {
		return syncplanpkg.Item{RelativePath: "node", Action: action, Kind: kind, LocalPresent: true, LocalPath: local, LocalSHA1: mcpSyncTestSHA1("local")}
	}
	completeRemote := func(action, kind string) syncplanpkg.Item {
		return syncplanpkg.Item{RelativePath: "node", Action: action, Kind: kind, RemotePresent: true, RemotePath: "/node", RemoteID: "remote-id", RemoteSHA1: mcpSyncTestSHA1("remote")}
	}

	cases := map[string]syncplanpkg.Item{}
	upload := completeLocal("upload", "file")
	upload.LocalSHA1 = ""
	cases["upload-missing-local-content"] = upload

	deleteRemote := completeRemote("delete-remote", "file")
	deleteRemote.RemoteSHA1 = ""
	cases["delete-remote-missing-content"] = deleteRemote

	deleteLocal := completeLocal("delete-local", "file")
	deleteLocal.LocalSHA1 = ""
	cases["delete-local-missing-content"] = deleteLocal

	replaceRemoteWinner := completeRemote("replace-remote", "file")
	replaceRemoteWinner.ReplacesKind = "file"
	replaceRemoteWinner.LocalPresent = true
	replaceRemoteWinner.LocalPath = local
	replaceRemoteWinner.LocalSHA1 = ""
	cases["replace-remote-missing-winner-content"] = replaceRemoteWinner

	replaceRemoteTarget := replaceRemoteWinner
	replaceRemoteTarget.LocalSHA1 = mcpSyncTestSHA1("local")
	replaceRemoteTarget.RemoteSHA1 = ""
	cases["replace-remote-missing-target-content"] = replaceRemoteTarget

	replaceLocalWinner := completeLocal("replace-local", "file")
	replaceLocalWinner.ReplacesKind = "file"
	replaceLocalWinner.RemotePresent = true
	replaceLocalWinner.RemotePath = "/node"
	replaceLocalWinner.RemoteID = "remote-id"
	replaceLocalWinner.RemoteSHA1 = ""
	cases["replace-local-missing-winner-content"] = replaceLocalWinner

	replaceLocalTarget := replaceLocalWinner
	replaceLocalTarget.RemoteSHA1 = mcpSyncTestSHA1("remote")
	replaceLocalTarget.LocalSHA1 = ""
	cases["replace-local-missing-target-content"] = replaceLocalTarget

	badKind := completeLocal("upload", "file")
	badKind.Kind = "symlink"
	cases["unsupported-kind"] = badKind

	for name, item := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateMCPSyncExecutablePlan(syncplanpkg.Plan{Items: []syncplanpkg.Item{item}}); err == nil {
				t.Fatalf("incomplete executable action was accepted: %#v", item)
			}
		})
	}
}

func TestMCPSyncExecutorDeleteLocalDirectoryRevalidatesWholeSubtree(t *testing.T) {
	build := func(t *testing.T) (string, syncplanpkg.Plan, syncplanpkg.Item) {
		t.Helper()
		root := t.TempDir()
		dir := filepath.Join(root, "orphan")
		if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		oldPath := filepath.Join(dir, "sub", "old.bin")
		if err := os.WriteFile(oldPath, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		rootInfo, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
			{RelativePath: "orphan", Action: "delete-local", Kind: "directory", LocalPresent: true, LocalPath: dir, LocalModTimeUnixNano: rootInfo.ModTime().UnixNano(), Destructive: true},
			{RelativePath: "orphan/sub", Action: "skip", Kind: "directory", LocalPresent: true, LocalPath: filepath.Join(dir, "sub")},
			{RelativePath: "orphan/sub/old.bin", Action: "skip", Kind: "file", LocalPresent: true, LocalPath: oldPath, LocalSize: 3, LocalSHA1: mcpSyncTestSHA1("old")},
		}}
		return root, plan, plan.Items[0]
	}

	t.Run("unchanged", func(t *testing.T) {
		root, plan, item := build(t)
		executor := &mcpSyncExecutor{ft: NewFileTools(nil, WithLocalRoot(root)), plan: plan}
		if err := executor.deleteLocal(context.Background(), item); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(item.LocalPath); !os.IsNotExist(err) {
			t.Fatalf("planned local directory still exists: %v", err)
		}
	})

	t.Run("deep-mutation", func(t *testing.T) {
		root, plan, item := build(t)
		newPath := filepath.Join(item.LocalPath, "sub", "new.bin")
		if err := os.WriteFile(newPath, []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		executor := &mcpSyncExecutor{ft: NewFileTools(nil, WithLocalRoot(root)), plan: plan}
		if err := executor.deleteLocal(context.Background(), item); err == nil || !strings.Contains(err.Error(), "subtree node count changed") {
			t.Fatalf("stale local subtree delete error = %v", err)
		}
		for _, path := range []string{filepath.Join(item.LocalPath, "sub", "old.bin"), newPath} {
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("stale local subtree removed %q: %v", path, err)
			}
		}
	})
}

func TestMCPSyncExecutorDeleteRemoteDirectoryRevalidatesWholeSubtree(t *testing.T) {
	build := func(t *testing.T, mutate bool) (*mcpSyncExecutor, syncplanpkg.Item, *int) {
		t.Helper()
		deleteCalls := 0
		deleted := false
		client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case "/files/getid":
				remotePath := strings.Trim(req.URL.Query().Get("path"), "/")
				switch remotePath {
				case "remote/orphan":
					if deleted {
						return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
					}
					return mcpResolveJSONResponse(req, `{"state":true,"id":"orphan"}`), nil
				case "remote":
					return mcpResolveJSONResponse(req, `{"state":true,"id":"parent"}`), nil
				default:
					t.Fatalf("unexpected remote directory getid path %q", remotePath)
				}
			case "/files/get_info":
				if req.URL.Query().Get("file_id") != "orphan" {
					t.Fatalf("unexpected remote directory metadata id: %s", req.URL)
				}
				return mcpResolveJSONResponse(req, `{"state":true,"data":[{"cid":"orphan","pid":"parent","n":"orphan","s":"0"}]}`), nil
			case "/natsort/files.php", "/files":
				if req.URL.Query().Get("record_open_time") != "0" {
					t.Fatalf("destructive subtree validation lost read-only listing: %s", req.URL)
				}
				switch req.URL.Query().Get("cid") {
				case "orphan":
					return mcpResolveJSONResponse(req, `{"state":true,"cid":"orphan","count":1,"offset":0,"limit":500,"data":[{"cid":"sub","pid":"orphan","n":"sub","s":"0"}]}`), nil
				case "sub":
					body := `{"state":true,"cid":"sub","count":1,"offset":0,"limit":500,"data":[{"fid":"old","cid":"sub","n":"old.bin","s":"3","sha":"OLD"}]}`
					if mutate {
						body = `{"state":true,"cid":"sub","count":2,"offset":0,"limit":500,"data":[{"fid":"old","cid":"sub","n":"old.bin","s":"3","sha":"OLD"},{"fid":"new","cid":"sub","n":"new.bin","s":"3","sha":"NEW"}]}`
					}
					return mcpResolveJSONResponse(req, body), nil
				case "parent":
					return mcpResolveJSONResponse(req, `{"state":true,"cid":"parent","count":0,"offset":0,"limit":100,"data":[]}`), nil
				default:
					t.Fatalf("unexpected destructive subtree listing: %s", req.URL)
				}
			case "/rb/delete":
				deleteCalls++
				deleted = true
				return mcpResolveJSONResponse(req, `{"state":true}`), nil
			default:
				t.Fatalf("unexpected remote directory delete request: %s %s", req.Method, req.URL)
			}
			return nil, errors.New("unreachable")
		})})))
		plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
			{RelativePath: "orphan", Action: "delete-remote", Kind: "directory", RemotePresent: true, RemotePath: "/remote/orphan", RemoteID: "orphan", Destructive: true},
			{RelativePath: "orphan/sub", Action: "skip", Kind: "directory", RemotePresent: true, RemotePath: "/remote/orphan/sub", RemoteID: "sub"},
			{RelativePath: "orphan/sub/old.bin", Action: "skip", Kind: "file", RemotePresent: true, RemotePath: "/remote/orphan/sub/old.bin", RemoteID: "old", RemoteSize: 3, RemoteSHA1: "OLD"},
		}}
		return &mcpSyncExecutor{ft: NewFileTools(client, WithLocalRoot(t.TempDir())), plan: plan}, plan.Items[0], &deleteCalls
	}

	t.Run("unchanged", func(t *testing.T) {
		executor, item, deleteCalls := build(t, false)
		if err := executor.deleteRemote(context.Background(), item); err != nil {
			t.Fatal(err)
		}
		if *deleteCalls != 1 {
			t.Fatalf("remote directory delete calls = %d, want 1", *deleteCalls)
		}
	})

	t.Run("deep-mutation", func(t *testing.T) {
		executor, item, deleteCalls := build(t, true)
		if err := executor.deleteRemote(context.Background(), item); err == nil || !strings.Contains(err.Error(), "subtree node count changed") {
			t.Fatalf("stale remote subtree delete error = %v", err)
		}
		if *deleteCalls != 0 {
			t.Fatalf("stale remote subtree reached Delete %d time(s)", *deleteCalls)
		}
	})
}

func TestMCPSyncExecutorReplaceRemoteValidatesLocalWinnerBeforeOldSubtree(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "winner.bin")
	if err := os.WriteFile(localPath, []byte("AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	item := syncplanpkg.Item{
		RelativePath:         "winner.bin",
		Action:               "replace-remote",
		Kind:                 "file",
		ReplacesKind:         "directory",
		LocalPresent:         true,
		LocalPath:            localPath,
		LocalSize:            4,
		LocalModTimeUnixNano: info.ModTime().UnixNano(),
		LocalSHA1:            mcpSyncTestSHA1("AAAA"),
		RemotePresent:        true,
		RemotePath:           "/remote/winner.bin",
		RemoteID:             "old-dir",
		Destructive:          true,
	}
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{item}}
	if err := os.WriteFile(localPath, []byte("BBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	// A nil 115 client is intentional: stale winner validation must return before
	// touching the old remote subtree or issuing Delete.
	executor := &mcpSyncExecutor{ft: NewFileTools(nil, WithLocalRoot(root)), plan: plan}
	if err := executor.removeRemote(context.Background(), item); err == nil || !strings.Contains(err.Error(), "replacement winner") {
		t.Fatalf("stale replacement winner error = %v", err)
	}
}

func TestMCPSyncExecutorLocalSnapshotDetectsSameSizeSameMtimeRewrite(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(localPath, []byte("AAAA"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	item := syncplanpkg.Item{
		RelativePath:         "source.bin",
		Action:               "upload",
		Kind:                 "file",
		LocalPath:            localPath,
		LocalSize:            4,
		LocalModTimeUnixNano: info.ModTime().UnixNano(),
		LocalSHA1:            mcpSyncTestSHA1("AAAA"),
	}
	executor := &mcpSyncExecutor{ft: NewFileTools(nil, WithLocalRoot(root))}
	file, digest, err := executor.openLocalFileSnapshot(item)
	if err != nil || file == nil || digest == nil || !strings.EqualFold(digest.SHA1, item.LocalSHA1) {
		t.Fatalf("fresh local snapshot = file=%v digest=%#v err=%v", file, digest, err)
	}
	_ = file.Close()

	if err := os.WriteFile(localPath, []byte("BBBB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if file, _, err := executor.openLocalFileSnapshot(item); err == nil || file != nil || !strings.Contains(err.Error(), "changed content") {
		if file != nil {
			_ = file.Close()
		}
		t.Fatalf("same-size/same-mtime rewrite snapshot = file=%v err=%v", file, err)
	}
}

func TestMCPSyncExecutionPreflightBarrierStopsBeforeCallbacks(t *testing.T) {
	root := t.TempDir()
	executor := &mcpSyncExecutor{ft: NewFileTools(nil, WithLocalRoot(root))}
	plan := syncplanpkg.Plan{
		Ready: true,
		Items: []syncplanpkg.Item{{RelativePath: "dir", Action: "upload", Kind: "directory"}},
	}
	deps := executor.deps(func(context.Context) error { return errMCPSyncPlanChanged }, false, false)
	summary, err := syncexecpkg.ExecuteWithJobsFailurePolicy(context.Background(), plan, true, 1, false, 0, deps)
	if !errors.Is(err, errMCPSyncPlanChanged) || summary.Processed != 0 || summary.PreflightPassed {
		t.Fatalf("sync execution barrier summary=%#v err=%v", summary, err)
	}
}

func TestMCPSyncExecutionOutputRedactsHiddenIdentities(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "private.bin")
	const (
		remoteID   = "remote-id-secret"
		localSHA1  = "LOCALSHA1SECRET"
		remoteSHA1 = "REMOTESHA1SECRET"
		remoteRoot = "/private/remote"
	)
	plan := syncplanpkg.Plan{
		RemoteRoot:   remoteRoot,
		RemoteRootID: "root-id-secret",
		Items: []syncplanpkg.Item{{
			RelativePath: "private.bin", LocalPath: localPath, RemotePath: remoteRoot + "/private.bin",
			RemoteID: remoteID, LocalSHA1: localSHA1, RemoteSHA1: remoteSHA1,
		}},
	}
	summary := syncexecpkg.Summary{
		PlannedItems: 1,
		Items:        []syncexecpkg.ItemResult{{RelativePath: "private.bin", Action: "upload", Status: "failed", Error: "failed " + localPath + " " + remoteID + " " + localSHA1 + " " + remoteRoot}},
	}
	ft := NewFileTools(nil, WithLocalRoot(root))
	output := mcpSyncExecutionOutput("sha256:reviewed", summary, ft, PlanSyncArgs{LocalPath: root, RemotePath: remoteRoot}, plan, errors.New("top "+localPath+" "+remoteSHA1+" "+remoteRoot))
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{root, localPath, remoteRoot, remoteID, localSHA1, remoteSHA1, "root-id-secret"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("sync execution output leaked %q: %s", forbidden, text)
		}
	}
	if !strings.Contains(text, "[REDACTED") {
		t.Fatalf("sync execution output did not show sanitized marker: %s", text)
	}
}

func TestExecuteSyncPlanUploadOnlyReplansTwiceBeforeOneP10Call(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	getIDCalls := 0
	listCalls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			getIDCalls++
			path := strings.Trim(req.URL.Query().Get("path"), "/")
			if path == "remote" {
				return mcpResolveJSONResponse(req, `{"state":true,"id":"r"}`), nil
			}
			if path == "remote/source.bin" {
				return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
			}
			t.Fatalf("unexpected getid path: %q", path)
		case "/natsort/files.php", "/files":
			listCalls++
			if req.URL.Query().Get("cid") != "r" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected read-only sync listing: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":0,"offset":0,"limit":500,"data":[]}`), nil
		default:
			t.Fatalf("unexpected sync execution request: %s %s", req.Method, req.URL)
		}
		return nil, errors.New("unreachable")
	})})))
	ft := NewFileTools(client, WithLocalRoot(root))
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, dirID, name string, size int64, _ *os.File, options uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		if dirID != "r" || name != "source.bin" || size != 7 || options.PreparedDigest == nil || options.PreparedDigest.SHA1 == "" {
			t.Fatalf("unexpected prepared sync upload: dir=%q name=%q size=%d options=%#v", dirID, name, size, options)
		}
		return uploadpkg.Result{BytesUploaded: size}, nil
	}
	planArgs := PlanSyncArgs{LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 20, MaxChecksumBytes: 64}
	planned, err := planMCPSync(context.Background(), client, root, planArgs)
	if err != nil {
		t.Fatal(err)
	}
	readsAfterPlanning := getIDCalls + listCalls
	result, output, err := ft.executeSyncPlan(context.Background(), nil, ExecuteSyncPlanArgs{
		LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 20, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID,
	})
	if err != nil || result == nil || result.IsError || output.ErrorCode != "" || output.Summary.Succeeded != 1 || output.Summary.UploadedFiles != 1 || output.Summary.PreflightChecked != 1 || !output.Summary.PreflightPassed || uploadCalls != 1 {
		t.Fatalf("upload-only execute_sync_plan result=%#v output=%#v err=%v upload_calls=%d", result, output, err, uploadCalls)
	}
	if getIDCalls+listCalls <= readsAfterPlanning {
		t.Fatalf("execute_sync_plan did not perform fresh read gates: before=%d after=%d", readsAfterPlanning, getIDCalls+listCalls)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), root) || strings.Contains(string(encoded), localPath) || strings.Contains(strings.ToLower(string(encoded)), "sha1") {
		t.Fatalf("execute_sync_plan success leaked hidden local identity: %s", encoded)
	}
}

func TestExecuteSyncPlanPlanMismatchDoesNotReachP10(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			path := strings.Trim(req.URL.Query().Get("path"), "/")
			if path == "remote" {
				return mcpResolveJSONResponse(req, `{"state":true,"id":"r"}`), nil
			}
			return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
		case "/natsort/files.php", "/files":
			return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":0,"offset":0,"limit":500,"data":[]}`), nil
		default:
			t.Fatalf("unexpected mismatch request: %s", req.URL)
		}
		return nil, errors.New("unreachable")
	})})))
	ft := NewFileTools(client, WithLocalRoot(root))
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{}, nil
	}
	wrongPlanID := "sha256:" + strings.Repeat("0", 64)
	result, output, err := ft.executeSyncPlan(context.Background(), nil, ExecuteSyncPlanArgs{
		LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 20, MaxChecksumBytes: 64, ExpectPlanID: wrongPlanID,
	})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "plan_changed" || output.PlanID != "" || uploadCalls != 0 {
		t.Fatalf("stale execute_sync_plan result=%#v output=%#v err=%v upload_calls=%d", result, output, err, uploadCalls)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, wrongPlanID) || strings.Contains(text, localPath) {
		t.Fatalf("stale execute_sync_plan leaked reviewed/local identity: %s", text)
	}
}

func prepareExecuteSyncPlanWireUploadFixture(t *testing.T) (*FileTools, ExecuteSyncPlanArgs, string) {
	t.Helper()
	root := t.TempDir()
	localPath := filepath.Join(root, "wire-source-secret.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			remotePath := strings.Trim(req.URL.Query().Get("path"), "/")
			switch remotePath {
			case "remote":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"r"}`), nil
			case "remote/wire-source-secret.bin":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
			default:
				t.Fatalf("unexpected wire sync getid path %q", remotePath)
			}
		case "/natsort/files.php", "/files":
			if req.URL.Query().Get("cid") != "r" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected wire sync read-only listing: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":0,"offset":0,"limit":500,"data":[]}`), nil
		default:
			t.Fatalf("unexpected wire sync request: %s %s", req.Method, req.URL)
		}
		return nil, errors.New("unreachable")
	})})))
	ft := NewFileTools(client, WithLocalRoot(root), WithDestructiveTools(true))
	planArgs := PlanSyncArgs{LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 20, MaxChecksumBytes: 64}
	planned, err := planMCPSync(context.Background(), client, root, planArgs)
	if err != nil {
		t.Fatal(err)
	}
	return ft, ExecuteSyncPlanArgs{
		LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 20, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID,
	}, localPath
}

func callExecuteSyncPlanWire(t *testing.T, ft *FileTools, args ExecuteSyncPlanArgs) *mcp.CallToolResult {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "execute-sync-plan-test", Version: "1"}, nil)
	ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "execute-sync-plan-client", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	arguments := map[string]any{
		"local_path": args.LocalPath, "remote_path": args.RemotePath, "direction": args.Direction,
		"max_nodes": args.MaxNodes, "max_checksum_bytes": args.MaxChecksumBytes, "expect_plan_id": args.ExpectPlanID,
	}
	if args.ConflictPolicy != "" {
		arguments["conflict_policy"] = args.ConflictPolicy
	}
	if args.DeleteExtraneous {
		arguments["delete"] = true
	}
	if args.MaxDeleteRoots != 0 {
		arguments["max_delete_roots"] = args.MaxDeleteRoots
	}
	if args.MaxDeleteItems != 0 {
		arguments["max_delete_items"] = args.MaxDeleteItems
	}
	if args.MaxDeleteBytes != 0 {
		arguments["max_delete_bytes"] = args.MaxDeleteBytes
	}
	if args.Jobs != 0 {
		arguments["jobs"] = args.Jobs
	}
	if args.ContinueOnError {
		arguments["continue_on_error"] = true
	}
	if args.MaxErrors != 0 {
		arguments["max_errors"] = args.MaxErrors
	}
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "execute_sync_plan", Arguments: arguments})
	if err != nil {
		t.Fatalf("call execute_sync_plan: %v", err)
	}
	return result
}

func TestExecuteSyncPlanWirePopulatesSafeStructuredContent(t *testing.T) {
	ft, args, localPath := prepareExecuteSyncPlanWireUploadFixture(t)
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, _ string, _ string, size int64, _ *os.File, _ uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{BytesUploaded: size}, nil
	}
	result := callExecuteSyncPlanWire(t, ft, args)
	if result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 || uploadCalls != 1 {
		t.Fatalf("wire execute_sync_plan result=%#v upload_calls=%d", result, uploadCalls)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		if strings.Contains(payload, args.LocalPath) || strings.Contains(payload, localPath) || strings.Contains(strings.ToLower(payload), "sha1") {
			t.Fatalf("wire execute_sync_plan leaked hidden identity: %s", payload)
		}
	}
	var output MCPSyncExecutionOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.PlanID != args.ExpectPlanID || output.ErrorCode != "" || output.Summary.Succeeded != 1 || output.Summary.UploadedFiles != 1 || output.Summary.PreflightChecked != 1 || !output.Summary.PreflightPassed {
		t.Fatalf("unexpected wire execute_sync_plan structured output: %#v", output)
	}
}

func TestExecuteSyncPlanWireRuntimeErrorKeepsStructuredContentAndRedactsPath(t *testing.T) {
	ft, args, localPath := prepareExecuteSyncPlanWireUploadFixture(t)
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		return uploadpkg.Result{}, errors.New("synthetic upload failure at " + localPath)
	}
	result := callExecuteSyncPlanWire(t, ft, args)
	if result == nil || !result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("wire failed execute_sync_plan result=%#v", result)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		if strings.Contains(payload, args.LocalPath) || strings.Contains(payload, localPath) {
			t.Fatalf("wire failed execute_sync_plan leaked runtime local identity: %s", payload)
		}
	}
	var output MCPSyncExecutionOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.ErrorCode != "execution_failed" || output.Summary.Failed != 1 || len(output.Items) != 1 || !strings.Contains(output.Items[0].Error, "[REDACTED") {
		t.Fatalf("wire failed execute_sync_plan lost safe structured error: %#v", output)
	}
}

func TestExecuteSyncPlanCallResultKeepsTextAndTypedOutputEquivalent(t *testing.T) {
	response := MCPSyncExecutionOutput{
		PlanID:  "sha256:test",
		Summary: MCPSyncExecutionSummary{PlannedItems: 1, Processed: 1, Succeeded: 1, Jobs: 1, PreflightChecked: 1, PreflightPassed: true},
		Items:   []syncexecpkg.ItemResult{{RelativePath: "file.bin", Action: "upload", Status: "succeeded"}},
	}
	result, output, err := executeSyncPlanCallResult(response)
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("execute_sync_plan result=%#v output=%#v err=%v", result, output, err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var decoded MCPSyncExecutionOutput
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.PlanID != output.PlanID || decoded.Summary.Succeeded != 1 || len(decoded.Items) != 1 || decoded.Items[0].RelativePath != "file.bin" {
		t.Fatalf("execute_sync_plan text/typed output diverged: text=%#v typed=%#v", decoded, output)
	}
}
