package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

func TestMCPUploadThroughTransferWiresMachineTransferConfig(t *testing.T) {
	config := DefaultDownloadTransferConfig()
	config.Interfaces = "Ethernet,2"
	config.ChunkSize = "4MiB"
	config.Retries = 2
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
	if captured.Interfaces != "Ethernet,2" || captured.ChunkSize != 4<<20 || captured.Retries != 2 || captured.Timeout != uploadpkg.DefaultTimeout {
		t.Fatalf("P10 config was not preserved: %#v", captured)
	}
	if captured.HealthTracker == nil {
		t.Fatal("P10 upload did not receive P8 health tracker")
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

func TestMCPUploadTransferDefaultsAreLongLived(t *testing.T) {
	ft := NewFileTools(nil)
	if ft.uploadTransfer == nil || ft.uploadTransfer.config.Interfaces != "auto" {
		t.Fatalf("unexpected default upload transfer state: %#v", ft.uploadTransfer)
	}
	if uploadpkg.DefaultTimeout < time.Hour {
		t.Fatalf("upload timeout is unexpectedly short: %s", uploadpkg.DefaultTimeout)
	}
}
