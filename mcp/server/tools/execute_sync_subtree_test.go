package tools

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
)

func TestMCPSyncDestructiveDirectoryRequiresVerifiableReviewedSubtree(t *testing.T) {
	localRoot := filepath.Join(t.TempDir(), "local-dir")
	valid := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "local-dir", Action: "delete-local", Kind: "directory", Destructive: true, LocalPresent: true, LocalPath: localRoot},
		{RelativePath: "local-dir/child.bin", Action: "skip", Kind: "file", LocalPresent: true, LocalPath: filepath.Join(localRoot, "child.bin"), LocalSize: 4, LocalSHA1: "LOCALCHILD"},
		{RelativePath: "remote-dir", Action: "delete-remote", Kind: "directory", Destructive: true, RemotePresent: true, RemotePath: "/remote-dir", RemoteID: "remote-dir-id"},
		{RelativePath: "remote-dir/child.bin", Action: "skip", Kind: "file", RemotePresent: true, RemotePath: "/remote-dir/child.bin", RemoteID: "remote-child-id", RemoteSize: 4, RemoteSHA1: "REMOTECHILD"},
	}}
	if err := validateMCPSyncExecutablePlan(valid); err != nil {
		t.Fatalf("verifiable destructive directory plan rejected: %v", err)
	}

	invalid := []syncplanpkg.Plan{
		{Items: []syncplanpkg.Item{{RelativePath: "dir", Action: "delete-remote", Kind: "directory", Destructive: true, RemotePresent: true, RemotePath: "/dir"}}},
		{Items: []syncplanpkg.Item{
			{RelativePath: "dir", Action: "delete-local", Kind: "directory", Destructive: true, LocalPresent: true, LocalPath: filepath.Join(t.TempDir(), "dir")},
			{RelativePath: "dir/a.bin", Action: "skip", Kind: "file", LocalPresent: true, LocalPath: filepath.Join(t.TempDir(), "dir", "a.bin"), LocalSize: 1},
		}},
		{Items: []syncplanpkg.Item{
			{RelativePath: "dir", Action: "delete-remote", Kind: "directory", Destructive: true, RemotePresent: true, RemotePath: "/dir", RemoteID: "dir-id"},
			{RelativePath: "dir/a.bin", Action: "skip", Kind: "file", RemotePresent: true, RemotePath: "/dir/a.bin", RemoteID: "file-id", RemoteSize: 1},
		}},
	}
	for i, plan := range invalid {
		if err := validateMCPSyncExecutablePlan(plan); err == nil {
			t.Fatalf("invalid destructive subtree %d was accepted", i)
		}
	}
}

func TestMCPSyncDestructiveLocalSubtreeGateRejectsSameSizeSameMtimeChildRewrite(t *testing.T) {
	root := t.TempDir()
	dirPath := filepath.Join(root, "dir")
	if err := os.Mkdir(dirPath, 0o755); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(dirPath, "child.bin")
	if err := os.WriteFile(childPath, []byte("AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dirPath)
	if err != nil {
		t.Fatal(err)
	}
	childInfo, err := os.Stat(childPath)
	if err != nil {
		t.Fatal(err)
	}
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "dir", Action: "delete-local", Kind: "directory", Destructive: true, LocalPresent: true, LocalPath: dirPath, LocalModTimeUnixNano: dirInfo.ModTime().UnixNano()},
		{RelativePath: "dir/child.bin", Action: "skip", Kind: "file", Reason: "covered-by-delete-local:dir", LocalPresent: true, LocalPath: childPath, LocalSize: 4, LocalModTimeUnixNano: childInfo.ModTime().UnixNano(), LocalSHA1: mcpSyncTestSHA1("AAAA")},
	}}
	executor := &mcpSyncExecutor{ft: NewFileTools(nil, WithLocalRoot(root)), plan: plan}
	if err := executor.validateLocalSubtree(plan.Items[0]); err != nil {
		t.Fatalf("fresh local destructive subtree rejected: %v", err)
	}

	if err := os.WriteFile(childPath, []byte("BBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(childPath, childInfo.ModTime(), childInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dirPath, dirInfo.ModTime(), dirInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := executor.validateLocalSubtree(plan.Items[0]); err == nil || !strings.Contains(err.Error(), "changed content") {
		t.Fatalf("same-size/same-mtime local subtree rewrite was accepted: %v", err)
	}
}

func TestMCPSyncDestructiveRemoteSubtreeGateRejectsChildContentRewrite(t *testing.T) {
	sha1 := "AAA"
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			if strings.Trim(req.URL.Query().Get("path"), "/") != "remote/dir" {
				t.Fatalf("unexpected remote subtree getid: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"id":"d"}`), nil
		case "/files/get_info":
			if req.URL.Query().Get("file_id") != "d" {
				t.Fatalf("unexpected remote subtree get_info: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"data":[{"cid":"d","pid":"r","n":"dir","s":"0"}]}`), nil
		case "/natsort/files.php", "/files":
			if req.URL.Query().Get("cid") != "d" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected remote subtree listing: %s", req.URL)
			}
			body := `{"state":true,"cid":"d","count":1,"offset":0,"limit":1,"data":[{"fid":"f","cid":"d","n":"child.bin","s":"4","sha":"` + sha1 + `"}]}`
			return mcpResolveJSONResponse(req, body), nil
		default:
			t.Fatalf("unexpected remote subtree request: %s", req.URL)
		}
		return nil, errors.New("unreachable")
	})})))
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "dir", Action: "delete-remote", Kind: "directory", Destructive: true, RemotePresent: true, RemotePath: "/remote/dir", RemoteID: "d"},
		{RelativePath: "dir/child.bin", Action: "skip", Kind: "file", Reason: "covered-by-delete-remote:dir", RemotePresent: true, RemotePath: "/remote/dir/child.bin", RemoteID: "f", RemoteSize: 4, RemoteSHA1: "AAA"},
	}}
	executor := &mcpSyncExecutor{ft: NewFileTools(client, WithLocalRoot(t.TempDir())), plan: plan}
	if err := executor.validateRemoteSubtree(context.Background(), plan.Items[0]); err != nil {
		t.Fatalf("fresh remote destructive subtree rejected: %v", err)
	}
	sha1 = "BBB"
	if err := executor.validateRemoteSubtree(context.Background(), plan.Items[0]); err == nil || !strings.Contains(err.Error(), "changed content") {
		t.Fatalf("remote subtree content rewrite was accepted: %v", err)
	}
}
