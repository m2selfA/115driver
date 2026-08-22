package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestUploadFromLocalFilesExpectPlanIDAllowsMatchingPlan(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, _ string, _ string, size int64, _ *os.File, _ uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{BytesUploaded: size}, nil
	}
	files := []UploadFromLocalFileItem{{LocalPath: localPath, DirID: "0", FileName: "remote.bin"}}
	planned, err := planMCPTransfer(context.Background(), ft, PlanTransferArgs{Uploads: files})
	if err != nil {
		t.Fatal(err)
	}

	result, output, err := ft.uploadFromLocalFiles(context.Background(), nil, UploadFromLocalFilesArgs{Files: files, ExpectPlanID: planned.Plan.PlanID})
	if err != nil || result == nil || result.IsError || output.Result == nil || output.Result.Succeeded != 1 || uploadCalls != 1 {
		t.Fatalf("matching upload plan gate result=%#v output=%#v err=%v upload_calls=%d", result, output, err, uploadCalls)
	}
}

func TestUploadFromLocalFilesExpectPlanIDRejectsSameSizeSameMtimeContentChangeBeforeUploader(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(localPath, []byte("AAAA"), 0600); err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, _ string, _ string, _ int64, _ *os.File, _ uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{}, nil
	}
	files := []UploadFromLocalFileItem{{LocalPath: localPath, DirID: "0", FileName: "remote.bin"}}
	planned, err := planMCPTransfer(context.Background(), ft, PlanTransferArgs{Uploads: files})
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(localPath, []byte("BBBB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localPath, originalInfo.ModTime(), originalInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	result, _, err := ft.uploadFromLocalFiles(context.Background(), nil, UploadFromLocalFilesArgs{Files: files, ExpectPlanID: planned.Plan.PlanID})
	if err != nil || result == nil || !result.IsError || uploadCalls != 0 {
		t.Fatalf("stale upload plan gate result=%#v err=%v upload_calls=%d", result, err, uploadCalls)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "run plan_transfer again") || strings.Contains(text, planned.Plan.PlanID) {
		t.Fatalf("stale upload plan gate returned unsafe/unhelpful error: %s", text)
	}
}

func TestDownloadPreparedPlanGateRejectsSameSizeSameMtimeTargetChangeBeforeDataPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "download.bin")
	if err := os.WriteFile(target, []byte("AAAA"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	prepared := []mcpDownloadBatchTransferItem{{
		info:      &driver.DownloadInfo{FileName: "download.bin", FileSize: 4, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/file"}},
		localPath: target,
		stableID:  "pick-code-hidden",
	}}
	planned, err := buildMCPTransferPlan(nil, prepared, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("BBBB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}

	ft := NewFileTools(nil, WithLocalRoot(root))
	dataPathCalls := 0
	ft.downloadTransfer.deps.discoverNetworkPaths = func(context.Context) (transfer.NetworkDiscoveryResult, error) {
		dataPathCalls++
		return transfer.NetworkDiscoveryResult{}, errors.New("data path should not run")
	}
	_, err = ft.executeMCPPreparedDownloadsWithPlanGate(context.Background(), prepared, DefaultDownloadTransferConfig(), planned.Plan.PlanID, 0)
	if err == nil || !strings.Contains(err.Error(), "run plan_transfer again") || dataPathCalls != 0 {
		t.Fatalf("stale target gate err=%v data_path_calls=%d", err, dataPathCalls)
	}
}

func TestDownloadPreparedPlanGateRejectsMismatchBeforeDataPath(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "download.bin")
	ft := NewFileTools(nil, WithLocalRoot(root))
	dataPathCalls := 0
	ft.downloadTransfer.deps.discoverNetworkPaths = func(context.Context) (transfer.NetworkDiscoveryResult, error) {
		dataPathCalls++
		return transfer.NetworkDiscoveryResult{}, errors.New("data path should not run")
	}
	prepared := []mcpDownloadBatchTransferItem{{
		info:      &driver.DownloadInfo{FileName: "download.bin", FileSize: 4, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/file"}},
		localPath: target,
		stableID:  "pick-code-hidden",
	}}
	wrongPlanID := "sha256:" + strings.Repeat("0", 64)
	_, err := ft.executeMCPPreparedDownloadsWithPlanGate(context.Background(), prepared, DefaultDownloadTransferConfig(), wrongPlanID, 0)
	if err == nil || !strings.Contains(err.Error(), "run plan_transfer again") || dataPathCalls != 0 {
		t.Fatalf("download plan mismatch err=%v data_path_calls=%d", err, dataPathCalls)
	}
	if strings.Contains(err.Error(), "pick-code-hidden") || strings.Contains(err.Error(), wrongPlanID) {
		t.Fatalf("download plan mismatch leaked hidden identity: %v", err)
	}
}
