package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestUploadFromLocalSingleExpectPlanIDMatchesAndExecutes(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	planArgs := PlanTransferArgs{Uploads: []UploadFromLocalFileItem{{LocalPath: localPath, DirID: "0", FileName: "remote.bin"}}, MaxChecksumBytes: 64}
	planned, err := planMCPTransfer(context.Background(), ft, planArgs)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, _ string, _ string, size int64, _ *os.File, _ uploadpkg.Options) (uploadpkg.Result, error) {
		calls++
		return uploadpkg.Result{BytesUploaded: size}, nil
	}
	result, _, err := ft.uploadFromLocal(context.Background(), nil, UploadFromLocalArgs{
		LocalPath: localPath, DirID: "0", FileName: "remote.bin", ExpectPlanID: planned.Plan.PlanID, MaxChecksumBytes: 64,
	})
	if err != nil || result == nil || result.IsError || calls != 1 {
		t.Fatalf("matching single upload gate result=%#v err=%v calls=%d", result, err, calls)
	}
}

func TestUploadFromLocalSingleExpectPlanIDBlocksSameSizeSameMtimeRewrite(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source-secret.bin")
	if err := os.WriteFile(localPath, []byte("AAAA"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	planArgs := PlanTransferArgs{Uploads: []UploadFromLocalFileItem{{LocalPath: localPath, DirID: "0", FileName: "remote.bin"}}, MaxChecksumBytes: 64}
	planned, err := planMCPTransfer(context.Background(), ft, planArgs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte("BBBB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localPath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		calls++
		return uploadpkg.Result{}, nil
	}
	result, _, err := ft.uploadFromLocal(context.Background(), nil, UploadFromLocalArgs{
		LocalPath: localPath, DirID: "0", FileName: "remote.bin", ExpectPlanID: planned.Plan.PlanID, MaxChecksumBytes: 64,
	})
	if err != nil || result == nil || !result.IsError || calls != 0 || len(result.Content) != 1 {
		t.Fatalf("stale single upload gate result=%#v err=%v calls=%d", result, err, calls)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "run plan_transfer again") || strings.Contains(text, localPath) {
		t.Fatalf("stale single upload gate leaked/lost diagnostic: %s", text)
	}
}

func TestSingleTransferPlanGateArgumentsFailBeforeClientUse(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(localPath, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(nil, WithLocalRoot(root))

	if result, _, err := ft.uploadFromLocal(context.Background(), nil, UploadFromLocalArgs{LocalPath: localPath, DirID: "0", DryRun: true, ExpectPlanID: "sha256:" + strings.Repeat("a", 64)}); err != nil || result == nil || !result.IsError {
		t.Fatalf("single upload dry-run accepted expect_plan_id: result=%#v err=%v", result, err)
	}
	if result, _, err := ft.uploadFromLocal(context.Background(), nil, UploadFromLocalArgs{LocalPath: localPath, DirID: "0", MaxChecksumBytes: 1}); err != nil || result == nil || !result.IsError {
		t.Fatalf("single upload accepted checksum budget without gate: result=%#v err=%v", result, err)
	}
	if result, _, err := ft.downloadFile(context.Background(), nil, DownloadSingleFileArgs{PickCode: "pick", LocalPath: filepath.Join(root, "out.bin"), ExpectPlanID: "sha256:not-hex"}); err != nil || result == nil || !result.IsError {
		t.Fatalf("single download malformed gate reached client: result=%#v err=%v", result, err)
	}
	if result, _, err := ft.downloadFile(context.Background(), nil, DownloadSingleFileArgs{PickCode: "pick", LocalPath: filepath.Join(root, "out.bin"), MaxChecksumBytes: 1}); err != nil || result == nil || !result.IsError {
		t.Fatalf("single download checksum budget without gate reached client: result=%#v err=%v", result, err)
	}
}
