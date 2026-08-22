package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func writeMCPUploadBatchFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mcpUploadTargetTestClient(t *testing.T, fileIDs map[string]bool, calls *int) *driver.Pan115Client {
	t.Helper()
	return driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		*calls = *calls + 1
		id := req.URL.Query().Get("file_id")
		isDir, ok := fileIDs[id]
		if !ok {
			return nil, fmt.Errorf("unexpected target lookup %q", id)
		}
		body := fmt.Sprintf(`{"state":true,"data":[{"cid":%q,"pid":"0","n":"dir","s":"0"}]}`, id)
		if !isDir {
			body = fmt.Sprintf(`{"state":true,"data":[{"fid":%q,"cid":"0","n":"file.bin","s":"1"}]}`, id)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))
}

func TestValidateMCPUploadTargetDirectoryChecksNonRootIdentityAndType(t *testing.T) {
	calls := 0
	client := mcpUploadTargetTestClient(t, map[string]bool{"dir-1": true, "file-1": false}, &calls)
	if err := validateMCPUploadTargetDirectory(client, "0"); err != nil || calls != 0 {
		t.Fatalf("root target validation = %v, calls=%d", err, calls)
	}
	if err := validateMCPUploadTargetDirectory(client, "dir-1"); err != nil {
		t.Fatalf("directory target was rejected: %v", err)
	}
	if err := validateMCPUploadTargetDirectory(client, "file-1"); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("file target validation error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("unexpected target lookup count: %d", calls)
	}
}

func TestPrepareMCPLocalUploadRejectsInvalidDirNameAndNonRegularSource(t *testing.T) {
	root := t.TempDir()
	file := writeMCPUploadBatchFile(t, root, "a.bin", "abc")
	if _, err := prepareMCPLocalUpload(root, UploadFromLocalArgs{LocalPath: file}); err == nil || !strings.Contains(err.Error(), "dir_id") {
		t.Fatalf("blank dir_id error = %v", err)
	}
	if _, err := prepareMCPLocalUpload(root, UploadFromLocalArgs{LocalPath: file, DirID: "0", FileName: "bad/name"}); err == nil {
		t.Fatal("remote file name containing a separator was accepted")
	}
	if _, err := prepareMCPLocalUpload(root, UploadFromLocalArgs{LocalPath: root, DirID: "0"}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory source error = %v", err)
	}
}

func TestUploadFromLocalFilesPreflightsEntireBatchBeforeUploader(t *testing.T) {
	root := t.TempDir()
	first := writeMCPUploadBatchFile(t, root, "first.bin", "first")
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	calls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		calls++
		return uploadpkg.Result{}, nil
	}
	result, _, err := ft.uploadFromLocalFiles(context.Background(), nil, UploadFromLocalFilesArgs{Files: []UploadFromLocalFileItem{
		{LocalPath: first, DirID: "1"},
		{LocalPath: root, DirID: "2"},
	}})
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	if result == nil || !result.IsError || calls != 0 {
		t.Fatalf("batch preflight did not fail before uploader: result=%#v calls=%d", result, calls)
	}
}

func TestUploadFromLocalDryRunPreflightsWithoutP10OrPathEcho(t *testing.T) {
	root := t.TempDir()
	path := writeMCPUploadBatchFile(t, root, "preview.bin", "payload")
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{}, errors.New("dry-run reached P10")
	}

	result, _, err := ft.uploadFromLocal(context.Background(), nil, UploadFromLocalArgs{LocalPath: path, DirID: "0", DryRun: true})
	if err != nil || result == nil || result.IsError || uploadCalls != 0 || len(result.Content) != 1 {
		t.Fatalf("local upload dry-run = %#v, %v uploads=%d", result, err, uploadCalls)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, path) || strings.Contains(text, "local_path") {
		t.Fatalf("local upload dry-run echoed source path: %s", text)
	}
	var plan MCPUploadPlan
	if err := json.Unmarshal([]byte(text), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.Operation != "upload_from_local" || plan.Requested != 1 || len(plan.Items) != 1 || plan.Items[0].FileName != "preview.bin" || plan.Items[0].FileSize == nil || *plan.Items[0].FileSize != 7 {
		t.Fatalf("unexpected local upload plan: %#v", plan)
	}
}

func TestUploadFromLocalDryRunPreservesKnownZeroByteSize(t *testing.T) {
	root := t.TempDir()
	path := writeMCPUploadBatchFile(t, root, "empty.bin", "")
	ft := NewFileTools(driver.New(), WithLocalRoot(root))

	result, _, err := ft.uploadFromLocal(context.Background(), nil, UploadFromLocalArgs{LocalPath: path, DirID: "0", DryRun: true})
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("zero-byte local upload dry-run = %#v, %v", result, err)
	}
	var plan MCPUploadPlan
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || plan.Items[0].FileSize == nil || *plan.Items[0].FileSize != 0 {
		t.Fatalf("zero-byte local upload size was not preserved: %#v", plan)
	}
}

func TestUploadFromLocalFilesDryRunPreflightsWholeBatchWithoutP10(t *testing.T) {
	root := t.TempDir()
	first := writeMCPUploadBatchFile(t, root, "a.bin", "aaa")
	second := writeMCPUploadBatchFile(t, root, "b.bin", "bbbb")
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{}, errors.New("dry-run reached P10")
	}

	result, _, err := ft.uploadFromLocalFiles(context.Background(), nil, UploadFromLocalFilesArgs{DryRun: true, Files: []UploadFromLocalFileItem{
		{LocalPath: first, DirID: "0"},
		{LocalPath: second, DirID: "0", FileName: "remote-b.bin"},
	}})
	if err != nil || result == nil || result.IsError || uploadCalls != 0 || len(result.Content) != 1 {
		t.Fatalf("local upload batch dry-run = %#v, %v uploads=%d", result, err, uploadCalls)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, first) || strings.Contains(text, second) || strings.Contains(text, "local_path") {
		t.Fatalf("local batch dry-run echoed source path: %s", text)
	}
	var plan MCPUploadPlan
	if err := json.Unmarshal([]byte(text), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Operation != "upload_from_local_files" || !plan.DryRun || plan.Requested != 2 || len(plan.Items) != 2 || plan.Items[1].FileName != "remote-b.bin" {
		t.Fatalf("unexpected local batch upload plan: %#v", plan)
	}
}

func TestUploadFromLocalFilesContinuesAfterRuntimeFailure(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		writeMCPUploadBatchFile(t, root, "a.bin", "aaa"),
		writeMCPUploadBatchFile(t, root, "b.bin", "bbbb"),
		writeMCPUploadBatchFile(t, root, "c.bin", "ccccc"),
	}
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	calls := 0
	var firstHealth any
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, _ string, name string, size int64, _ *os.File, options uploadpkg.Options) (uploadpkg.Result, error) {
		calls++
		if firstHealth == nil {
			firstHealth = options.HealthTracker
		} else if firstHealth != options.HealthTracker {
			t.Fatal("batch upload did not reuse health tracker")
		}
		if calls == 2 {
			return uploadpkg.Result{BytesUploaded: 1}, errors.New("synthetic upload failure")
		}
		return uploadpkg.Result{BytesUploaded: size, Rapid: name == "c.bin"}, nil
	}
	args := UploadFromLocalFilesArgs{Files: []UploadFromLocalFileItem{
		{LocalPath: paths[0], DirID: "0"},
		{LocalPath: paths[1], DirID: "0"},
		{LocalPath: paths[2], DirID: "0"},
	}}
	result, _, err := ft.uploadFromLocalFiles(context.Background(), nil, args)
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	if result == nil || !result.IsError || calls != 3 || len(result.Content) != 1 {
		t.Fatalf("unexpected batch result/calls: result=%#v calls=%d", result, calls)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content: %#v", result.Content[0])
	}
	var decoded UploadFromLocalFilesResult
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != 3 || decoded.Succeeded != 2 || decoded.Failed != 1 || len(decoded.Items) != 3 {
		t.Fatalf("unexpected decoded batch result: %#v", decoded)
	}
	if !decoded.Items[0].Success || decoded.Items[1].Success || !strings.Contains(decoded.Items[1].Error, "synthetic upload failure") || !decoded.Items[2].Success || !decoded.Items[2].Rapid {
		t.Fatalf("unexpected per-item upload statuses: %#v", decoded.Items)
	}
}

func TestUploadFromLocalFilesRejectsInvalidRemoteDirectoryBeforeUploader(t *testing.T) {
	root := t.TempDir()
	a := writeMCPUploadBatchFile(t, root, "a.bin", "a")
	b := writeMCPUploadBatchFile(t, root, "b.bin", "b")
	lookups := 0
	client := mcpUploadTargetTestClient(t, map[string]bool{"dir-1": true, "file-2": false}, &lookups)
	ft := NewFileTools(client, WithLocalRoot(root))
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{}, nil
	}
	result, _, err := ft.uploadFromLocalFiles(context.Background(), nil, UploadFromLocalFilesArgs{Files: []UploadFromLocalFileItem{
		{LocalPath: a, DirID: "dir-1"},
		{LocalPath: b, DirID: "file-2"},
	}})
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	if result == nil || !result.IsError || uploadCalls != 0 || lookups != 2 {
		t.Fatalf("remote target preflight did not fail before upload: result=%#v uploads=%d lookups=%d", result, uploadCalls, lookups)
	}
}

func TestUploadFromLocalFilesRejectsDuplicateRemoteTargetBeforeUploader(t *testing.T) {
	root := t.TempDir()
	a := writeMCPUploadBatchFile(t, root, "a.bin", "a")
	b := writeMCPUploadBatchFile(t, root, "b.bin", "b")
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	calls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		calls++
		return uploadpkg.Result{}, nil
	}
	result, _, err := ft.uploadFromLocalFiles(context.Background(), nil, UploadFromLocalFilesArgs{Files: []UploadFromLocalFileItem{
		{LocalPath: a, DirID: "42", FileName: "same.bin"},
		{LocalPath: b, DirID: "42", FileName: "same.bin"},
	}})
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	if result == nil || !result.IsError || calls != 0 {
		t.Fatalf("duplicate remote target did not fail before uploader: result=%#v calls=%d", result, calls)
	}
}
