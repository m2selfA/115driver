package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPUploadThroughTransferWiresMachineTransferConfig(t *testing.T) {
	config := DefaultDownloadTransferConfig()
	config.Interfaces = "Ethernet,2"
	config.ChunkSize = "4MiB"
	config.Retries = 2
	config.WorkersPerInterface = 3
	ft := NewFileTools(nil, WithDownloadTransferConfig(config))

	file, err := os.CreateTemp(t.TempDir(), "upload-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write([]byte("payload")); err != nil {
		t.Fatal(err)
	}

	var captured uploadpkg.Options
	var firstHealth *transfer.NetworkHealthTracker
	calls := 0
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, client *driver.Pan115Client, dirID, name string, size int64, gotFile *os.File, options uploadpkg.Options) (uploadpkg.Result, error) {
		calls++
		if client != nil || dirID != "42" || name != "file.bin" || size != 7 || gotFile != file {
			t.Fatalf("unexpected upload invocation: client=%v dir=%q name=%q size=%d file=%v", client, dirID, name, size, gotFile)
		}
		captured = options
		if firstHealth == nil {
			firstHealth = options.HealthTracker
		} else if firstHealth != options.HealthTracker {
			t.Fatal("MCP upload health tracker was not reused")
		}
		return uploadpkg.Result{BytesUploaded: size}, nil
	}
	for i := 0; i < 2; i++ {
		if _, err := ft.uploadThroughTransfer(context.Background(), "42", "file.bin", 7, file); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("unexpected upload call count: %d", calls)
	}
	if captured.Interfaces != "Ethernet,2" || captured.ChunkSize != 4<<20 || captured.Retries != 2 || captured.WorkersPerInterface != 3 || captured.Timeout != uploadpkg.DefaultTimeout {
		t.Fatalf("P10 config was not preserved: %#v", captured)
	}
	if captured.HealthTracker == nil {
		t.Fatal("P10 upload did not receive P8 health tracker")
	}
}

func TestMCPUploadThroughTransferPreparedForwardsReviewedDigest(t *testing.T) {
	ft := NewFileTools(nil)
	file, err := os.CreateTemp(t.TempDir(), "prepared-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	prepared := &uploadpkg.PreparedDigest{SHA1: "ABCDEF", Size: info.Size(), ModTimeUnixNano: info.ModTime().UnixNano()}
	var captured *uploadpkg.PreparedDigest
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, _ string, _ string, _ int64, _ *os.File, options uploadpkg.Options) (uploadpkg.Result, error) {
		captured = options.PreparedDigest
		return uploadpkg.Result{}, nil
	}
	if _, err := ft.uploadThroughTransferPrepared(context.Background(), "0", "data.bin", info.Size(), file, prepared); err != nil {
		t.Fatal(err)
	}
	if captured != prepared {
		t.Fatalf("prepared digest was not forwarded by identity: got=%p want=%p", captured, prepared)
	}
}

func TestUploadFromURLRejectsUnsafeTargetBeforeP10Uploader(t *testing.T) {
	ft := NewFileTools(nil)
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		t.Fatal("P10 uploader ran for an SSRF-rejected URL")
		return uploadpkg.Result{}, nil
	}
	result, _, err := ft.uploadFromURL(context.Background(), nil, UploadFromURLArgs{URL: "http://127.0.0.1/private", DirID: "1"})
	if err != nil {
		t.Fatalf("handler should return tool error, got Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("expected SSRF rejection, got %#v", result)
	}
}

func TestUploadFromURLRejectsInvalid115TargetBeforeExternalFetch(t *testing.T) {
	lookups := 0
	client := mcpUploadTargetTestClient(t, map[string]bool{"file-target": false}, &lookups)
	ft := NewFileTools(client)
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		t.Fatal("P10 uploader ran for invalid 115 target")
		return uploadpkg.Result{}, nil
	}
	result, _, err := ft.uploadFromURL(context.Background(), nil, UploadFromURLArgs{URL: "https://example.com/file.bin", DirID: "file-target"})
	if err != nil {
		t.Fatalf("handler should return tool error, got Go error: %v", err)
	}
	if result == nil || !result.IsError || lookups != 1 {
		t.Fatalf("invalid 115 target did not fail before external fetch: result=%#v lookups=%d", result, lookups)
	}
}

func TestUploadFromLocalRejectsOutsideRootBeforeP10Uploader(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.bin")
	if err := os.WriteFile(outside, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(nil, WithLocalRoot(root))
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		t.Fatal("P10 uploader ran before local-root validation")
		return uploadpkg.Result{}, nil
	}
	result, _, err := ft.uploadFromLocal(context.Background(), nil, UploadFromLocalArgs{LocalPath: outside, DirID: "1"})
	if err != nil {
		t.Fatalf("handler should return tool error, got Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("expected local-root rejection, got %#v", result)
	}
}

func TestUploadDryRunsRejectUnusableTransferConfigBeforeDataPath(t *testing.T) {
	config := DefaultDownloadTransferConfig()
	config.ChunkSize = "1B"
	root := t.TempDir()
	localPath := filepath.Join(root, "preview.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}

	newTools := func() *FileTools {
		return NewFileTools(nil, WithLocalRoot(root), WithDownloadTransferConfig(config))
	}
	calls := []struct {
		name string
		call func(*FileTools) (*mcp.CallToolResult, any, error)
	}{
		{name: "url", call: func(ft *FileTools) (*mcp.CallToolResult, any, error) {
			return ft.uploadFromURL(context.Background(), nil, UploadFromURLArgs{URL: "https://example.invalid/file.bin", DirID: "0", DryRun: true})
		}},
		{name: "url-batch", call: func(ft *FileTools) (*mcp.CallToolResult, any, error) {
			return ft.uploadFromURLs(context.Background(), nil, UploadFromURLFilesArgs{DryRun: true, Files: []UploadFromURLFileItem{{URL: "https://example.invalid/file.bin", DirID: "0"}}})
		}},
		{name: "local", call: func(ft *FileTools) (*mcp.CallToolResult, any, error) {
			return ft.uploadFromLocal(context.Background(), nil, UploadFromLocalArgs{LocalPath: localPath, DirID: "0", DryRun: true})
		}},
		{name: "local-batch", call: func(ft *FileTools) (*mcp.CallToolResult, any, error) {
			return ft.uploadFromLocalFiles(context.Background(), nil, UploadFromLocalFilesArgs{DryRun: true, Files: []UploadFromLocalFileItem{{LocalPath: localPath, DirID: "0"}}})
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			result, _, err := tc.call(newTools())
			if err != nil || result == nil || !result.IsError || len(result.Content) != 1 {
				t.Fatalf("invalid-config %s dry-run = %#v, %v", tc.name, result, err)
			}
			text, ok := result.Content[0].(*mcp.TextContent)
			if !ok || !strings.Contains(text.Text, "100KiB") {
				t.Fatalf("invalid-config %s lost upload readiness cause: %#v", tc.name, result.Content[0])
			}
		})
	}
}

func TestMCPUploadTransferDefaultsAreLongLived(t *testing.T) {
	ft := NewFileTools(nil)
	if ft.uploadTransfer == nil || ft.uploadTransfer.config.Interfaces != "auto" {
		t.Fatalf("unexpected default upload transfer state: %#v", ft.uploadTransfer)
	}
	if uploadpkg.DefaultTimeout < time.Hour {
		t.Fatalf("upload timeout is unexpectedly short: %s", uploadpkg.DefaultTimeout)
	}
}
