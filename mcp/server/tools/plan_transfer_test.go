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

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPlanMCPTransferRejectsEmptyAndAggregateLimit(t *testing.T) {
	root := t.TempDir()
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	if _, err := planMCPTransfer(context.Background(), ft, PlanTransferArgs{}); err == nil {
		t.Fatal("empty transfer plan was accepted")
	}
	tooMany := make([]UploadFromLocalFileItem, maxMCPFileBatchItems+1)
	if _, err := planMCPTransfer(context.Background(), ft, PlanTransferArgs{Uploads: tooMany}); err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("oversized transfer plan error = %v", err)
	}
}

func TestPlanMCPTransferUploadOnlyUsesSharedPreflightAndOmitsLocalPath(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	output, err := planMCPTransfer(context.Background(), ft, PlanTransferArgs{Uploads: []UploadFromLocalFileItem{{LocalPath: localPath, DirID: "0", FileName: "remote.bin"}}})
	if err != nil {
		t.Fatal(err)
	}
	if output.Summary.Requested != 1 || output.Summary.Uploads != 1 || output.Summary.Downloads != 0 || output.Summary.KnownTransferBytes != 7 || output.Summary.ChecksummedFiles != 1 || output.Summary.ChecksummedBytes != 7 {
		t.Fatalf("upload-only transfer summary = %#v", output.Summary)
	}
	if len(output.Plan.Operations) != 1 || output.Plan.SafetyClass != MCPPlanSafetyAdditive || output.Plan.Operations[0].Operation != "upload" {
		t.Fatalf("upload-only plan = %#v", output.Plan)
	}
	if len(output.Plan.Preconditions) != 2 || !strings.HasPrefix(output.Plan.PlanID, "sha256:") {
		t.Fatalf("upload-only preconditions/identity = %#v", output.Plan)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, localPath) || strings.Contains(text, `"local_path"`) || strings.Contains(strings.ToLower(text), `"sha1"`) {
		t.Fatalf("plan_transfer leaked local source identity: %s", text)
	}
}

func TestBuildMCPTransferPlanMarksExistingDownloadTargetDestructiveAndHidesPickCode(t *testing.T) {
	root := t.TempDir()
	uploadPath := filepath.Join(root, "upload.bin")
	if err := os.WriteFile(uploadPath, []byte("up!!"), 0600); err != nil {
		t.Fatal(err)
	}
	uploadFile, err := os.Open(uploadPath)
	if err != nil {
		t.Fatal(err)
	}
	defer uploadFile.Close()
	uploadInfo, err := uploadFile.Stat()
	if err != nil {
		t.Fatal(err)
	}

	downloadTarget := filepath.Join(root, "download.bin")
	if err := os.WriteFile(downloadTarget, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	const secretPickCode = "secret-pick-code"
	output, err := buildMCPTransferPlan(
		[]mcpPreparedLocalUpload{{file: uploadFile, fileName: "remote.bin", fileSize: uploadInfo.Size(), dirID: "0"}},
		[]mcpDownloadBatchTransferItem{{
			info:      &driver.DownloadInfo{FileName: "download.bin", FileSize: 7, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/secret"}},
			localPath: downloadTarget,
			stableID:  secretPickCode,
		}},
		0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if output.Summary.Requested != 2 || output.Summary.ExistingLocalTargets != 1 || output.Summary.KnownTransferBytes != 11 || output.Summary.ChecksummedFiles != 2 || output.Summary.ChecksummedBytes != 7 {
		t.Fatalf("mixed transfer summary = %#v", output.Summary)
	}
	if len(output.Plan.Operations) != 2 || output.Plan.SafetyClass != MCPPlanSafetyDestructive || output.Plan.EstimatedBytes == nil || *output.Plan.EstimatedBytes != 11 {
		t.Fatalf("mixed transfer plan = %#v", output.Plan)
	}
	if output.Items[0].Direction != "upload" || output.Items[1].Direction != "download" || !output.Items[1].TargetExists || output.Items[1].SafetyClass != MCPPlanSafetyDestructive {
		t.Fatalf("mixed transfer items = %#v", output.Items)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{uploadPath, downloadTarget, secretPickCode, "cdn.example.invalid", `"pick_code"`, `"local_path"`, `"url"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("plan_transfer output leaked %q: %s", forbidden, text)
		}
	}
}

func TestBuildMCPTransferPlanPreservesUnknownDownloadSize(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "unknown.bin")
	output, err := buildMCPTransferPlan(nil, []mcpDownloadBatchTransferItem{{
		info:      &driver.DownloadInfo{FileName: "unknown.bin", FileSize: -1, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/unknown"}},
		localPath: target,
		stableID:  "pick-unknown",
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if output.Summary.UnknownSizeTransfers != 1 || output.Summary.KnownTransferBytes != 0 || output.Plan.EstimatedBytes != nil || output.Items[0].FileSize != nil {
		t.Fatalf("unknown-size transfer plan = %#v / %#v", output.Summary, output.Plan)
	}
	if output.Plan.SafetyClass != MCPPlanSafetyAdditive {
		t.Fatalf("absent unknown-size target safety = %q", output.Plan.SafetyClass)
	}
}

func TestBuildMCPTransferPlanContentHashDetectsSameSizeSameMtimeRewrite(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	original := []byte("AAAA")
	if err := os.WriteFile(localPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}

	prepare := func() mcpPreparedLocalUpload {
		t.Helper()
		item, err := prepareMCPLocalUpload(root, UploadFromLocalArgs{LocalPath: localPath, DirID: "0", FileName: "remote.bin"})
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	first := prepare()
	firstPlan, err := buildMCPTransferPlan([]mcpPreparedLocalUpload{first}, nil, 0)
	_ = first.file.Close()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(localPath, []byte("BBBB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localPath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("test setup did not preserve size/mtime: before=%#v after=%#v", before, after)
	}

	second := prepare()
	secondPlan, err := buildMCPTransferPlan([]mcpPreparedLocalUpload{second}, nil, 0)
	_ = second.file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if firstPlan.Plan.PlanID == secondPlan.Plan.PlanID {
		t.Fatalf("same-size/same-mtime content rewrite preserved plan_id %s", firstPlan.Plan.PlanID)
	}
}

func TestBuildMCPTransferPlanDownloadTargetContentHashDetectsSameSizeSameMtimeRewrite(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.bin")
	if err := os.WriteFile(target, []byte("AAAA"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	item := mcpDownloadBatchTransferItem{
		info:      &driver.DownloadInfo{FileName: "remote.bin", FileSize: 4, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/file"}},
		localPath: target,
		stableID:  "pick-hidden",
	}
	first, err := buildMCPTransferPlan(nil, []mcpDownloadBatchTransferItem{item}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Summary.ChecksummedFiles != 1 || first.Summary.ChecksummedBytes != 4 || first.Summary.ExistingLocalTargets != 1 {
		t.Fatalf("existing target checksum summary = %#v", first.Summary)
	}

	if err := os.WriteFile(target, []byte("BBBB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("test setup did not preserve target size/mtime: before=%#v after=%#v", before, after)
	}
	second, err := buildMCPTransferPlan(nil, []mcpDownloadBatchTransferItem{item}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Plan.PlanID == second.Plan.PlanID {
		t.Fatalf("same-size/same-mtime destructive target rewrite preserved plan_id %s", first.Plan.PlanID)
	}
}

func TestSnapshotMCPTransferDownloadTargetRejectsTargetAppearingAfterPreflight(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "late.bin")
	identity, err := inspectMCPTransferDownloadTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	if identity.exists {
		t.Fatal("absent target was inspected as existing")
	}
	if err := os.WriteFile(target, []byte("late"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotMCPTransferDownloadTarget(identity); err == nil || !strings.Contains(err.Error(), "appeared after checksum preflight") {
		t.Fatalf("late target snapshot error = %v", err)
	}
}

func TestBuildMCPTransferPlanTimestampOnlyChangesKeepContentIdentityStable(t *testing.T) {
	root := t.TempDir()
	uploadPath := filepath.Join(root, "upload.bin")
	targetPath := filepath.Join(root, "target.bin")
	if err := os.WriteFile(uploadPath, []byte("UPLOAD"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(targetPath, []byte("TARGET"), 0600); err != nil {
		t.Fatal(err)
	}
	prepareUpload := func() mcpPreparedLocalUpload {
		t.Helper()
		item, err := prepareMCPLocalUpload(root, UploadFromLocalArgs{LocalPath: uploadPath, DirID: "0", FileName: "remote.bin"})
		if err != nil {
			t.Fatal(err)
		}
		return item
	}
	download := []mcpDownloadBatchTransferItem{{
		info:      &driver.DownloadInfo{FileName: "download.bin", FileSize: 6, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/file"}},
		localPath: targetPath,
		stableID:  "pick-hidden",
	}}
	firstUpload := prepareUpload()
	first, err := buildMCPTransferPlan([]mcpPreparedLocalUpload{firstUpload}, download, 0)
	_ = firstUpload.file.Close()
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(uploadPath, future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(targetPath, future, future); err != nil {
		t.Fatal(err)
	}
	secondUpload := prepareUpload()
	second, err := buildMCPTransferPlan([]mcpPreparedLocalUpload{secondUpload}, download, 0)
	_ = secondUpload.file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if first.Plan.PlanID != second.Plan.PlanID {
		t.Fatalf("timestamp-only changes altered content-addressed plan identity: %s != %s", first.Plan.PlanID, second.Plan.PlanID)
	}
}

func TestBuildMCPTransferPlanPreflightsChecksumBudgetBeforeHashing(t *testing.T) {
	uploads := []mcpPreparedLocalUpload{{fileSize: 4}, {fileSize: 4}}
	_, err := buildMCPTransferPlan(uploads, nil, 7)
	if err == nil || !strings.Contains(err.Error(), "checksum budget exceeded") {
		t.Fatalf("checksum budget preflight error = %v", err)
	}
	if _, err := normalizeMCPTransferChecksumBudget(-1); err == nil {
		t.Fatal("negative transfer checksum budget was accepted")
	}
	if _, err := normalizeMCPTransferChecksumBudget(maxMCPTransferPlanChecksumBytes + 1); err == nil {
		t.Fatal("oversized transfer checksum budget was accepted")
	}
}

func TestNormalizeMCPExpectedPlanIDAndMismatchAreSafe(t *testing.T) {
	upper := "SHA256:" + strings.Repeat("A", 64)
	normalized, err := normalizeMCPExpectedPlanID(upper)
	if err != nil || normalized != "sha256:"+strings.Repeat("a", 64) {
		t.Fatalf("normalized plan id = %q, %v", normalized, err)
	}
	if _, err := normalizeMCPExpectedPlanID("sha256:not-hex"); err == nil {
		t.Fatal("malformed expected plan id was accepted")
	}
	output := MCPTransferPlanOutput{Plan: MCPPlan{PlanID: "sha256:" + strings.Repeat("b", 64)}}
	if err := verifyMCPExpectedTransferPlanID(output.Plan.PlanID, output); err != nil {
		t.Fatalf("matching expected plan id failed: %v", err)
	}
	err = verifyMCPExpectedTransferPlanID("sha256:"+strings.Repeat("c", 64), output)
	if err == nil || strings.Contains(err.Error(), strings.Repeat("c", 64)) || strings.Contains(err.Error(), strings.Repeat("b", 64)) {
		t.Fatalf("unsafe/missing expected-plan mismatch error: %v", err)
	}
}

func TestPlanTransferErrorsRedactLocalPathsAndPickCodes(t *testing.T) {
	t.Run("local-path", func(t *testing.T) {
		root := t.TempDir()
		secretPath := filepath.Join(root, "missing-secret-source.bin")
		ft := NewFileTools(driver.New(), WithLocalRoot(root))
		result, _, err := ft.planTransfer(context.Background(), nil, PlanTransferArgs{Uploads: []UploadFromLocalFileItem{{LocalPath: secretPath, DirID: "0"}}})
		if err != nil || result == nil || !result.IsError || len(result.Content) != 1 {
			t.Fatalf("local-path failure result=%#v err=%v", result, err)
		}
		text := result.Content[0].(*mcp.TextContent).Text
		if strings.Contains(text, secretPath) || !strings.Contains(text, "[REDACTED]") {
			t.Fatalf("plan_transfer local path leaked in error: %s", text)
		}
	})

	t.Run("pick-code", func(t *testing.T) {
		const secretPickCode = "secret-pick-code-reflected-by-upstream"
		client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("upstream rejected " + secretPickCode)
		})})))
		root := t.TempDir()
		ft := NewFileTools(client, WithLocalRoot(root))
		result, _, err := ft.planTransfer(context.Background(), nil, PlanTransferArgs{Downloads: []DownloadFileArgs{{PickCode: secretPickCode, LocalPath: filepath.Join(root, "target.bin")}}})
		if err != nil || result == nil || !result.IsError || len(result.Content) != 1 {
			t.Fatalf("pick-code failure result=%#v err=%v", result, err)
		}
		text := result.Content[0].(*mcp.TextContent).Text
		if strings.Contains(text, secretPickCode) || !strings.Contains(text, "[REDACTED]") {
			t.Fatalf("plan_transfer pick code leaked in error: %s", text)
		}
	})
}

func TestPlanTransferCallResultKeepsTextAndTypedOutputEquivalent(t *testing.T) {
	response := MCPTransferPlanOutput{
		Summary: MCPTransferPlanSummary{Requested: 1, Uploads: 1, KnownTransferBytes: 3},
		Plan:    MCPPlan{PlanVersion: 1, PlanID: "sha256:test", Kind: "transfer", CreatedFrom: "plan_transfer", SafetyClass: MCPPlanSafetyAdditive},
		Items:   []MCPTransferPlanItem{{OperationID: "upload-000000", Direction: "upload", FileName: "a.bin", SafetyClass: MCPPlanSafetyAdditive}},
	}
	result, output, err := planTransferCallResult(response)
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("plan_transfer result=%#v output=%#v err=%v", result, output, err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var decoded MCPTransferPlanOutput
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Summary.Requested != output.Summary.Requested || decoded.Plan.Kind != output.Plan.Kind || len(decoded.Items) != 1 {
		t.Fatalf("plan_transfer text/typed output diverged: text=%#v typed=%#v", decoded, output)
	}
}
