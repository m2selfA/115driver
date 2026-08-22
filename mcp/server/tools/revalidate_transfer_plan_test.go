package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRevalidateMCPTransferPlanMatchesFreshPlanWithoutDataPath(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "source.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{}, nil
	}
	uploads := []UploadFromLocalFileItem{{LocalPath: localPath, DirID: "0", FileName: "remote.bin"}}
	planned, err := planMCPTransfer(context.Background(), ft, PlanTransferArgs{Uploads: uploads, MaxChecksumBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	result := revalidateMCPTransferPlan(context.Background(), ft, RevalidateTransferPlanArgs{
		Uploads: uploads, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID,
	})
	if !result.Matches || !result.GateSatisfied || result.PlanID != planned.Plan.PlanID || result.ErrorCode != "" || result.SafetyClass != planned.Plan.SafetyClass || result.OperationCount != 1 || result.KnownTransferBytes != 7 || result.ChecksummedFiles != 1 || result.ChecksummedBytes != 7 || uploadCalls != 0 {
		t.Fatalf("fresh transfer revalidation=%#v upload_calls=%d", result, uploadCalls)
	}
}

func TestRevalidateMCPTransferPlanDetectsSameSizeSameMtimeRewriteWithoutReturningFreshPlan(t *testing.T) {
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
	uploads := []UploadFromLocalFileItem{{LocalPath: localPath, DirID: "0", FileName: "remote.bin"}}
	planned, err := planMCPTransfer(context.Background(), ft, PlanTransferArgs{Uploads: uploads, MaxChecksumBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte("BBBB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localPath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}

	result := revalidateMCPTransferPlan(context.Background(), ft, RevalidateTransferPlanArgs{
		Uploads: uploads, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID,
	})
	if result.Matches || result.GateSatisfied || result.PlanID != "" || result.ErrorCode != "plan_changed" || !strings.Contains(result.Error, "run plan_transfer again") {
		t.Fatalf("stale transfer revalidation = %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), localPath) || strings.Contains(string(encoded), planned.Plan.PlanID) {
		t.Fatalf("stale transfer revalidation leaked reviewed/fresh state: %s", encoded)
	}
}

func TestRevalidateMCPTransferPlanRejectsMissingOrMalformedExpectedIDBeforePlanning(t *testing.T) {
	for name, value := range map[string]string{
		"missing":   "",
		"malformed": "sha256:not-hex",
	} {
		t.Run(name, func(t *testing.T) {
			result := revalidateMCPTransferPlan(context.Background(), nil, RevalidateTransferPlanArgs{ExpectPlanID: value})
			if result.Matches || result.GateSatisfied || result.ErrorCode == "revalidation_failed" || result.ErrorCode == "" {
				t.Fatalf("invalid expected id revalidation = %#v", result)
			}
		})
	}
}

func TestRevalidateTransferPlanCallResultKeepsTextAndTypedOutputEquivalent(t *testing.T) {
	response := MCPTransferPlanRevalidationOutput{Matches: true, GateSatisfied: true, PlanID: "sha256:test", SafetyClass: MCPPlanSafetyAdditive, OperationCount: 2, ChecksummedFiles: 1, ChecksummedBytes: 3}
	result, output, err := revalidateTransferPlanCallResult(response)
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("revalidate_transfer_plan result=%#v output=%#v err=%v", result, output, err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var decoded MCPTransferPlanRevalidationOutput
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != output {
		t.Fatalf("revalidate_transfer_plan text/typed output diverged: text=%#v typed=%#v", decoded, output)
	}
}

func TestRevalidateTransferPlanWireReturnsCredentialFreeStructuredStatus(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "wire-secret-source.bin")
	if err := os.WriteFile(localPath, []byte("AAAA"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(driver.New(), WithLocalRoot(root))
	uploads := []UploadFromLocalFileItem{{LocalPath: localPath, DirID: "0", FileName: "remote.bin"}}
	planned, err := planMCPTransfer(context.Background(), ft, PlanTransferArgs{Uploads: uploads, MaxChecksumBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte("BBBB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localPath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "revalidate-transfer-test", Version: "1"}, nil)
	ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "revalidate-transfer-client", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "revalidate_transfer_plan",
		Arguments: map[string]any{
			"uploads":            []any{map[string]any{"local_path": localPath, "dir_id": "0", "file_name": "remote.bin"}},
			"max_checksum_bytes": 64,
			"expect_plan_id":     planned.Plan.PlanID,
		},
	})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("wire revalidate_transfer_plan result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		if strings.Contains(payload, localPath) || strings.Contains(payload, "wire-secret-source.bin") || strings.Contains(payload, planned.Plan.PlanID) {
			t.Fatalf("wire stale transfer revalidation leaked reviewed/local state: %s", payload)
		}
	}
	var output MCPTransferPlanRevalidationOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.Matches || output.GateSatisfied || output.PlanID != "" || output.ErrorCode != "plan_changed" {
		t.Fatalf("unexpected wire stale transfer revalidation: %#v", output)
	}
}
