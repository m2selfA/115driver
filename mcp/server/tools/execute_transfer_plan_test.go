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

	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func preparedMixedTransferFixture(t *testing.T) (mcpPreparedLocalUpload, mcpDownloadBatchTransferItem) {
	t.Helper()
	root := t.TempDir()
	localPath := filepath.Join(root, "upload.bin")
	if err := os.WriteFile(localPath, []byte("upload"), 0600); err != nil {
		t.Fatal(err)
	}
	upload, err := prepareMCPLocalUpload(root, UploadFromLocalArgs{LocalPath: localPath, DirID: "0", FileName: "remote.bin"})
	if err != nil {
		t.Fatal(err)
	}
	download := mcpDownloadBatchTransferItem{
		info:      &driver.DownloadInfo{FileName: "download.bin", FileSize: 4, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/file"}, Header: http.Header{}},
		localPath: filepath.Join(root, "download.bin"),
		stableID:  "pick-code-hidden",
	}
	return upload, download
}

func callExecuteTransferPlanWire(t *testing.T, ft *FileTools, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "execute-transfer-plan-test", Version: "1"}, nil)
	ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "execute-transfer-plan-client", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "execute_transfer_plan", Arguments: arguments})
	if err != nil {
		t.Fatalf("call execute_transfer_plan: %v", err)
	}
	return result
}

func TestExecuteTransferPlanWirePopulatesSafeStructuredContent(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(driver.New(), WithLocalRoot(root), WithDestructiveTools(true))
	files := []UploadFromLocalFileItem{{LocalPath: localPath, DirID: "0", FileName: "remote.bin"}}
	planned, err := planMCPTransfer(context.Background(), ft, PlanTransferArgs{Uploads: files})
	if err != nil {
		t.Fatal(err)
	}
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, _ string, _ string, size int64, _ *os.File, _ uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{BytesUploaded: size}, nil
	}
	result := callExecuteTransferPlanWire(t, ft, map[string]any{
		"uploads":        []any{map[string]any{"local_path": localPath, "dir_id": "0", "file_name": "remote.bin"}},
		"expect_plan_id": planned.Plan.PlanID,
	})
	if result == nil || result.IsError || result.StructuredContent == nil || uploadCalls != 1 || len(result.Content) != 1 {
		t.Fatalf("wire execute_transfer_plan result=%#v uploads=%d", result, uploadCalls)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), localPath) || strings.Contains(string(encoded), "local_path") {
		t.Fatalf("wire execute_transfer_plan structured output leaked local path: %s", encoded)
	}
	var output MCPTransferExecutionOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.PlanID != planned.Plan.PlanID || output.Summary.Requested != 1 || output.Summary.Succeeded != 1 || output.UploadResult == nil || output.UploadResult.Succeeded != 1 {
		t.Fatalf("unexpected structured execution output: %#v", output)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, localPath) || strings.Contains(text, "local_path") {
		t.Fatalf("wire execute_transfer_plan TextContent leaked local path: %s", text)
	}
	var legacy MCPTransferExecutionOutput
	if err := json.Unmarshal([]byte(text), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.PlanID != output.PlanID || legacy.Summary.Succeeded != output.Summary.Succeeded {
		t.Fatalf("wire text/structured execution outputs diverged: text=%#v structured=%#v", legacy, output)
	}
}

func TestExecuteTransferPlanWireErrorKeepsStructuredContentAndRedactsRuntimePath(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "runtime-secret-source.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(driver.New(), WithLocalRoot(root), WithDestructiveTools(true))
	files := []UploadFromLocalFileItem{{LocalPath: localPath, DirID: "0", FileName: "remote.bin"}}
	planned, err := planMCPTransfer(context.Background(), ft, PlanTransferArgs{Uploads: files})
	if err != nil {
		t.Fatal(err)
	}
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		return uploadpkg.Result{}, errors.New("synthetic upload failure at " + localPath)
	}
	result := callExecuteTransferPlanWire(t, ft, map[string]any{
		"uploads":        []any{map[string]any{"local_path": localPath, "dir_id": "0", "file_name": "remote.bin"}},
		"expect_plan_id": planned.Plan.PlanID,
	})
	if result == nil || !result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("wire failed execute_transfer_plan result=%#v", result)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		if strings.Contains(payload, localPath) || strings.Contains(payload, "runtime-secret-source.bin") {
			t.Fatalf("wire failed execute_transfer_plan leaked runtime local path: %s", payload)
		}
	}
	var output MCPTransferExecutionOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.UploadResult == nil || output.UploadResult.Failed != 1 || len(output.UploadResult.Items) != 1 || !strings.Contains(output.UploadResult.Items[0].Error, "[REDACTED]") {
		t.Fatalf("wire failed execute_transfer_plan lost safe structured error: %#v", output)
	}
}

func TestExecutePreparedTransferPlanRejectsMismatchBeforeAnyDataPath(t *testing.T) {
	upload, download := preparedMixedTransferFixture(t)
	defer upload.file.Close()
	ft := NewFileTools(nil)
	uploadCalls := 0
	downloadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{}, nil
	}
	ft.downloadTransfer.deps.discoverNetworkPaths = func(context.Context) (transfer.NetworkDiscoveryResult, error) {
		downloadCalls++
		return transfer.NetworkDiscoveryResult{}, errors.New("data path should not run")
	}

	wrongPlanID := "sha256:" + strings.Repeat("0", 64)
	_, err := ft.executeMCPPreparedTransferPlan(context.Background(), []mcpPreparedLocalUpload{upload}, []mcpDownloadBatchTransferItem{download}, DefaultDownloadTransferConfig(), wrongPlanID, 0)
	if err == nil || !strings.Contains(err.Error(), "run plan_transfer again") || uploadCalls != 0 || downloadCalls != 0 {
		t.Fatalf("mixed mismatch err=%v upload_calls=%d download_calls=%d", err, uploadCalls, downloadCalls)
	}
}

func TestExecutePreparedTransferPlanSkipsDownloadsAfterUploadFailure(t *testing.T) {
	upload, download := preparedMixedTransferFixture(t)
	defer upload.file.Close()
	planned, err := buildMCPTransferPlan([]mcpPreparedLocalUpload{upload}, []mcpDownloadBatchTransferItem{download}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(nil)
	uploadCalls := 0
	downloadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{}, errors.New("synthetic upload failure")
	}
	ft.downloadTransfer.deps.discoverNetworkPaths = func(context.Context) (transfer.NetworkDiscoveryResult, error) {
		downloadCalls++
		return transfer.NetworkDiscoveryResult{}, errors.New("download phase should be skipped")
	}

	output, err := ft.executeMCPPreparedTransferPlan(context.Background(), []mcpPreparedLocalUpload{upload}, []mcpDownloadBatchTransferItem{download}, DefaultDownloadTransferConfig(), planned.Plan.PlanID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if uploadCalls != 1 || downloadCalls != 0 || output.UploadResult == nil || output.UploadResult.Failed != 1 || output.DownloadResult != nil || !output.Summary.DownloadsSkipped || output.Summary.Skipped != 1 || output.Summary.Failed != 1 {
		t.Fatalf("upload-failure mixed execution output=%#v upload_calls=%d download_calls=%d", output, uploadCalls, downloadCalls)
	}
}

func TestExecutePreparedTransferPlanRunsMatchingMixedPlanInDeterministicPhases(t *testing.T) {
	upload, download := preparedMixedTransferFixture(t)
	defer upload.file.Close()
	planned, err := buildMCPTransferPlan([]mcpPreparedLocalUpload{upload}, []mcpDownloadBatchTransferItem{download}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(nil)
	phases := make([]string, 0, 2)
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, _ string, _ string, size int64, _ *os.File, _ uploadpkg.Options) (uploadpkg.Result, error) {
		phases = append(phases, "upload")
		return uploadpkg.Result{BytesUploaded: size}, nil
	}
	worker := mcpTestNetworkPath("Ethernet 1", 1, "10.0.0.1")
	ft.downloadTransfer.deps.discoverNetworkPaths = func(context.Context) (transfer.NetworkDiscoveryResult, error) {
		return transfer.NetworkDiscoveryResult{Paths: []transfer.NetworkPath{worker}, Probes: []transfer.NetworkPathProbe{{Path: worker, Reachable: true}}}, nil
	}
	ft.downloadTransfer.deps.probeCDNPaths = func(context.Context, string, http.Header, []transfer.NetworkPath, *transfer.CDNProbeCache, ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
		return transfer.CDNDiscoveryResult{Host: "cdn.example.invalid", Paths: []transfer.NetworkPath{worker}}, nil
	}
	ft.downloadTransfer.deps.scheduleFiles = func(_ context.Context, _ []transfer.NetworkPath, jobs []transfer.FileTransferJob, _ ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
		phases = append(phases, "download")
		results := make([]transfer.FileScheduleResult, len(jobs))
		for i, job := range jobs {
			results[i] = transfer.FileScheduleResult{JobID: job.ID, Result: transfer.FileDownloadResult{DestinationPath: job.DestinationPath, BytesWritten: job.ExpectedSize}}
		}
		return transfer.FileScheduleReport{Results: results}, nil
	}

	output, err := ft.executeMCPPreparedTransferPlan(context.Background(), []mcpPreparedLocalUpload{upload}, []mcpDownloadBatchTransferItem{download}, DefaultDownloadTransferConfig(), planned.Plan.PlanID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(phases, ",") != "upload,download" || output.Summary.Succeeded != 2 || output.Summary.Failed != 0 || output.Summary.Skipped != 0 || output.UploadResult == nil || output.DownloadResult == nil || output.PlanID != planned.Plan.PlanID {
		t.Fatalf("matching mixed execution output=%#v phases=%v", output, phases)
	}
}
