package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRevalidateMCPSyncPlanMatchesFreshReadyPlan(t *testing.T) {
	root := t.TempDir()
	writeMCPSyncTestFile(t, root, "same.bin", "same")
	client := &usageTestClient{
		dirIDs: map[string]string{"remote": "r"},
		treePages: map[string][]driver.File{
			"r": {{FileID: "same", ParentID: "r", Name: "same.bin", Size: 4, Sha1: mcpSyncTestSHA1("same")}},
		},
	}
	planArgs := PlanSyncArgs{LocalPath: root, RemotePath: "/remote", MaxNodes: 10, MaxChecksumBytes: 64}
	planned, err := planMCPSync(context.Background(), client, root, planArgs)
	if err != nil {
		t.Fatal(err)
	}
	result := revalidateMCPSyncPlan(context.Background(), client, root, RevalidateSyncPlanArgs{
		LocalPath: root, RemotePath: "/remote", MaxNodes: 10, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID,
	})
	if !result.Matches || !result.Ready || !result.GateSatisfied || result.PlanID != planned.Plan.PlanID || result.ErrorCode != "" || result.OperationCount != len(planned.Plan.Operations) || result.ChecksummedFiles != planned.Summary.ChecksummedFiles || result.ChecksummedBytes != planned.Summary.ChecksummedBytes {
		t.Fatalf("fresh sync revalidation = %#v", result)
	}
}

func TestRevalidateMCPSyncPlanDetectsSameSizeSameMtimeLocalRewriteWithoutReturningFreshPlan(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "same.bin")
	if err := os.WriteFile(localPath, []byte("AAAA"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	client := &usageTestClient{
		dirIDs: map[string]string{"remote": "r"},
		treePages: map[string][]driver.File{
			"r": {{FileID: "same", ParentID: "r", Name: "same.bin", Size: 4, Sha1: mcpSyncTestSHA1("AAAA")}},
		},
	}
	args := PlanSyncArgs{LocalPath: root, RemotePath: "/remote", MaxNodes: 10, MaxChecksumBytes: 64}
	planned, err := planMCPSync(context.Background(), client, root, args)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte("BBBB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localPath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}

	result := revalidateMCPSyncPlan(context.Background(), client, root, RevalidateSyncPlanArgs{
		LocalPath: root, RemotePath: "/remote", MaxNodes: 10, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID,
	})
	if result.Matches || result.Ready || result.GateSatisfied || result.PlanID != "" || result.ErrorCode != "plan_changed" || !strings.Contains(result.Error, "run plan_sync again") {
		t.Fatalf("stale sync revalidation = %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), planned.Summary.SnapshotID) || strings.Contains(string(encoded), filepath.ToSlash(root)) {
		t.Fatalf("stale sync revalidation leaked hidden/fresh state: %s", encoded)
	}
}

func TestRevalidateMCPSyncPlanDetectsLocalOnlyUploadSameSizeSameMtimeRewrite(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "local-only.bin")
	if err := os.WriteFile(localPath, []byte("AAAA"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	client := &usageTestClient{
		dirIDs:    map[string]string{"remote": "r"},
		treePages: map[string][]driver.File{"r": {}},
	}
	args := PlanSyncArgs{LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 10, MaxChecksumBytes: 64}
	planned, err := planMCPSync(context.Background(), client, root, args)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Summary.ChecksummedFiles != 1 || planned.Summary.ChecksummedBytes != 4 {
		t.Fatalf("local-only upload was not content snapshotted: %#v", planned.Summary)
	}
	if err := os.WriteFile(localPath, []byte("BBBB"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localPath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	result := revalidateMCPSyncPlan(context.Background(), client, root, RevalidateSyncPlanArgs{
		LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 10, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID,
	})
	if result.Matches || result.GateSatisfied || result.PlanID != "" || result.ErrorCode != "plan_changed" {
		t.Fatalf("local-only stale sync revalidation = %#v", result)
	}
}

func TestRevalidateMCPSyncPlanMatchingUnresolvedConflictDoesNotSatisfyGate(t *testing.T) {
	root := t.TempDir()
	writeMCPSyncTestFile(t, root, "conflict.bin", "AAAA")
	client := &usageTestClient{
		dirIDs: map[string]string{"remote": "r"},
		treePages: map[string][]driver.File{
			"r": {{FileID: "conflict", ParentID: "r", Name: "conflict.bin", Size: 4, Sha1: mcpSyncTestSHA1("BBBB")}},
		},
	}
	args := PlanSyncArgs{LocalPath: root, RemotePath: "/remote", MaxNodes: 10, MaxChecksumBytes: 64}
	planned, err := planMCPSync(context.Background(), client, root, args)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Summary.Ready {
		t.Fatalf("test fixture unexpectedly produced ready plan: %#v", planned.Summary)
	}
	result := revalidateMCPSyncPlan(context.Background(), client, root, RevalidateSyncPlanArgs{
		LocalPath: root, RemotePath: "/remote", MaxNodes: 10, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID,
	})
	if !result.Matches || result.Ready || result.GateSatisfied || result.PlanID != planned.Plan.PlanID || result.ErrorCode != "" {
		t.Fatalf("unresolved matching sync revalidation = %#v", result)
	}
}

func TestRevalidateMCPSyncPlanRejectsMissingOrMalformedExpectedIDBeforePlanning(t *testing.T) {
	for name, value := range map[string]string{
		"missing":   "",
		"malformed": "sha256:not-hex",
	} {
		t.Run(name, func(t *testing.T) {
			result := revalidateMCPSyncPlan(context.Background(), nil, "", RevalidateSyncPlanArgs{ExpectPlanID: value})
			if result.Matches || result.GateSatisfied || result.ErrorCode == "revalidation_failed" || result.ErrorCode == "" {
				t.Fatalf("invalid expected id revalidation = %#v", result)
			}
		})
	}
}

func TestRedactMCPSyncPlanErrorRemovesConfiguredLocalIdentities(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "private", "data")
	text := redactMCPSyncPlanError(errors.New("failed at "+localPath+" under "+root), root, PlanSyncArgs{LocalPath: localPath})
	if strings.Contains(text, root) || strings.Contains(text, localPath) || !strings.Contains(text, "[REDACTED_LOCAL_PATH]") {
		t.Fatalf("sync plan error was not redacted: %q", text)
	}
}

func prepareRevalidateSyncPlanWireFixture(t *testing.T) (*FileTools, RevalidateSyncPlanArgs, string, os.FileInfo) {
	t.Helper()
	root := t.TempDir()
	localPath := filepath.Join(root, "same.bin")
	if err := os.WriteFile(localPath, []byte("AAAA"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(localPath)
	if err != nil {
		t.Fatal(err)
	}
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			if strings.Trim(req.URL.Query().Get("path"), "/") != "remote" {
				t.Fatalf("unexpected revalidate_sync_plan getid path: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"id":"r"}`), nil
		case "/natsort/files.php", "/files":
			if req.URL.Query().Get("cid") != "r" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected revalidate_sync_plan listing: %s", req.URL)
			}
			body := `{"state":true,"cid":"r","count":1,"offset":0,"limit":500,"data":[{"fid":"same","cid":"r","n":"same.bin","s":"4","sha":"` + mcpSyncTestSHA1("AAAA") + `"}]}`
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		default:
			t.Fatalf("unexpected revalidate_sync_plan request: %s %s", req.Method, req.URL)
		}
		return nil, errors.New("unreachable")
	})})))
	ft := NewFileTools(client, WithLocalRoot(root))
	planArgs := PlanSyncArgs{LocalPath: root, RemotePath: "/remote", MaxNodes: 10, MaxChecksumBytes: 64}
	planned, err := planMCPSync(context.Background(), client, root, planArgs)
	if err != nil {
		t.Fatal(err)
	}
	return ft, RevalidateSyncPlanArgs{
		LocalPath: root, RemotePath: "/remote", MaxNodes: 10, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID,
	}, localPath, info
}

func callRevalidateSyncPlanWire(t *testing.T, ft *FileTools, args RevalidateSyncPlanArgs) *mcp.CallToolResult {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "revalidate-sync-plan-test", Version: "1"}, nil)
	ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "revalidate-sync-plan-client", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "revalidate_sync_plan",
		Arguments: map[string]any{
			"local_path": args.LocalPath, "remote_path": args.RemotePath, "max_nodes": args.MaxNodes,
			"max_checksum_bytes": args.MaxChecksumBytes, "expect_plan_id": args.ExpectPlanID,
		},
	})
	if err != nil {
		t.Fatalf("call revalidate_sync_plan: %v", err)
	}
	return result
}

func TestRevalidateSyncPlanWirePopulatesSafeStructuredMatch(t *testing.T) {
	ft, args, localPath, _ := prepareRevalidateSyncPlanWireFixture(t)
	result := callRevalidateSyncPlanWire(t, ft, args)
	if result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("wire matching revalidate_sync_plan result=%#v", result)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		if strings.Contains(payload, args.LocalPath) || strings.Contains(payload, localPath) || strings.Contains(strings.ToLower(payload), "snapshot_id") || strings.Contains(strings.ToLower(payload), "sha1") {
			t.Fatalf("wire matching revalidate_sync_plan leaked hidden state: %s", payload)
		}
	}
	var output MCPSyncPlanRevalidationOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Matches || !output.Ready || !output.GateSatisfied || output.PlanID != args.ExpectPlanID || output.ErrorCode != "" {
		t.Fatalf("unexpected wire matching revalidate_sync_plan output: %#v", output)
	}
}

func TestRevalidateSyncPlanWireMismatchWithholdsReviewedAndFreshState(t *testing.T) {
	ft, args, localPath, before := prepareRevalidateSyncPlanWireFixture(t)
	if err := os.WriteFile(localPath, []byte("BBBB"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(localPath, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	result := callRevalidateSyncPlanWire(t, ft, args)
	if result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("wire stale revalidate_sync_plan result=%#v", result)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		if strings.Contains(payload, args.ExpectPlanID) || strings.Contains(payload, args.LocalPath) || strings.Contains(payload, localPath) || strings.Contains(strings.ToLower(payload), "snapshot_id") || strings.Contains(strings.ToLower(payload), "sha1") {
			t.Fatalf("wire stale revalidate_sync_plan leaked reviewed/fresh state: %s", payload)
		}
	}
	var output MCPSyncPlanRevalidationOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.Matches || output.Ready || output.GateSatisfied || output.PlanID != "" || output.ErrorCode != "plan_changed" {
		t.Fatalf("unexpected wire stale revalidate_sync_plan output: %#v", output)
	}
}

func TestRevalidateSyncPlanCallResultKeepsTextAndTypedOutputEquivalent(t *testing.T) {
	response := MCPSyncPlanRevalidationOutput{Matches: true, Ready: true, GateSatisfied: true, PlanID: "sha256:test", SafetyClass: MCPPlanSafetyAdditive, OperationCount: 2}
	result, output, err := revalidateSyncPlanCallResult(response)
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("revalidate_sync_plan result=%#v output=%#v err=%v", result, output, err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var decoded MCPSyncPlanRevalidationOutput
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != output {
		t.Fatalf("revalidate_sync_plan text/typed output diverged: text=%#v typed=%#v", decoded, output)
	}
}
