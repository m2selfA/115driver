package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func seedMCPDestructiveRecoveryJournal(t *testing.T, fixture mcpPersistentDeleteFixture, phase string, keepOpen bool) *syncjournalpkg.Handle {
	t.Helper()
	handle, err := fixture.store.CreateCurrent(fixture.state.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := handle.Mutate(func(journal *syncjournalpkg.Journal) error {
		journal.State = syncjournalpkg.StatusFailed
		journal.Items[0].State = "failed"
		journal.Items[0].Phase = phase
		journal.Items[0].Attempts = 1
		journal.Items[0].LastError = "synthetic destructive interruption"
		return nil
	}); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if _, err := fixture.store.WriteReviewAlias(fixture.args.ExpectPlanID, fixture.state.Plan.PlanID); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	if keepOpen {
		return handle
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	return nil
}

func TestDiagnoseSyncRecoveryClassifiesDeleteLocalEvidence(t *testing.T) {
	t.Run("retry-full", func(t *testing.T) {
		fixture := newMCPPersistentLocalDeleteFixture(t)
		seedMCPDestructiveRecoveryJournal(t, fixture, syncjournalpkg.PhaseDeleteStarted, false)
		result, output, err := fixture.ft.diagnoseSyncRecovery(context.Background(), nil, DiagnoseSyncRecoveryArgs{PlanID: fixture.args.ExpectPlanID})
		if err != nil || result == nil || result.IsError || !output.Found || !output.ReconcileRequired || !output.EvidenceComplete || !output.Resolvable || output.DiagnosisID == "" || output.RetryFull != 1 || output.Checked != 1 || len(output.Items) != 1 || output.Items[0].Decision != string(syncjournalpkg.DestructiveRetryFull) {
			t.Fatalf("retry-full diagnosis result=%#v output=%#v err=%v", result, output, err)
		}
	})

	t.Run("completed", func(t *testing.T) {
		fixture := newMCPPersistentLocalDeleteFixture(t)
		seedMCPDestructiveRecoveryJournal(t, fixture, syncjournalpkg.PhaseDeleteStarted, false)
		if err := os.Remove(fixture.localPath); err != nil {
			t.Fatal(err)
		}
		result, output, err := fixture.ft.diagnoseSyncRecovery(context.Background(), nil, DiagnoseSyncRecoveryArgs{PlanID: fixture.args.ExpectPlanID})
		if err != nil || result == nil || result.IsError || !output.Resolvable || output.DiagnosisID == "" || output.Completed != 1 || output.Items[0].Decision != string(syncjournalpkg.DestructiveCompleted) {
			t.Fatalf("completed diagnosis result=%#v output=%#v err=%v", result, output, err)
		}
	})

	t.Run("ambiguous", func(t *testing.T) {
		fixture := newMCPPersistentLocalDeleteFixture(t)
		seedMCPDestructiveRecoveryJournal(t, fixture, syncjournalpkg.PhaseDeleteStarted, false)
		if err := os.WriteFile(fixture.localPath, []byte("changed-payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		result, output, err := fixture.ft.diagnoseSyncRecovery(context.Background(), nil, DiagnoseSyncRecoveryArgs{PlanID: fixture.args.ExpectPlanID})
		if err != nil || result == nil || result.IsError || output.Resolvable || output.DiagnosisID == "" || output.Ambiguous != 1 || output.Items[0].Decision != string(syncjournalpkg.DestructiveAmbiguous) {
			t.Fatalf("ambiguous diagnosis result=%#v output=%#v err=%v", result, output, err)
		}
	})
}

func TestDiagnoseSyncRecoveryRefusesInUseJournal(t *testing.T) {
	fixture := newMCPPersistentLocalDeleteFixture(t)
	handle := seedMCPDestructiveRecoveryJournal(t, fixture, syncjournalpkg.PhaseDeleteStarted, true)
	defer handle.Close()
	result, output, err := fixture.ft.diagnoseSyncRecovery(context.Background(), nil, DiagnoseSyncRecoveryArgs{PlanID: fixture.args.ExpectPlanID})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "execution_in_use" || !output.InUse || output.EvidenceComplete || output.Checked != 0 {
		t.Fatalf("in-use diagnosis result=%#v output=%#v err=%v", result, output, err)
	}
}

func TestDiagnoseDestructiveRecoveryReplaceLocalWinnerOnly(t *testing.T) {
	root := t.TempDir()
	sha1 := mcpSyncTestSHA1("data")
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			remotePath := strings.Trim(req.URL.Query().Get("path"), "/")
			switch remotePath {
			case "remote":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"r"}`), nil
			case "remote/conflict.bin":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
			default:
				t.Fatalf("unexpected recovery replacement getid path %q", remotePath)
			}
		case "/natsort/files.php", "/files":
			if req.URL.Query().Get("cid") != "r" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected recovery replacement listing: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":1,"offset":0,"limit":100,"data":[{"fid":"remote-source","cid":"r","n":"conflict.bin","s":"4","sha":"`+sha1+`"}]}`), nil
		case "/files/get_info":
			if req.URL.Query().Get("file_id") != "remote-source" {
				t.Fatalf("unexpected recovery replacement metadata: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"data":[{"fid":"remote-source","cid":"r","n":"conflict.bin","s":"4","sha":"`+sha1+`"}]}`), nil
		default:
			t.Fatalf("unexpected recovery replacement request: %s %s", req.Method, req.URL)
		}
		return nil, errors.New("unreachable")
	})})))
	item := syncplanpkg.Item{
		RelativePath: "conflict.bin", Action: "replace-local", Kind: "file", ReplacesKind: "file", Destructive: true,
		LocalPresent: true, RemotePresent: true, LocalPath: root + string(os.PathSeparator) + "conflict.bin", RemotePath: "/remote/conflict.bin",
		LocalSize: 4, RemoteSize: 4, RemoteID: "remote-source", RemoteSHA1: sha1,
	}
	executor := &mcpSyncExecutor{ft: NewFileTools(client, WithLocalRoot(root)), plan: syncplanpkg.Plan{Items: []syncplanpkg.Item{item}}}
	decision, err := executor.diagnoseDestructiveRecovery(context.Background(), executor.plan, item)
	if err != nil || decision != syncjournalpkg.DestructiveWinnerOnly {
		t.Fatalf("replace-local winner-only diagnosis = %q err=%v", decision, err)
	}
}

func TestDiagnoseSyncRecoveryChecksumBudgetFailsBeforeEvidenceIO(t *testing.T) {
	journal := syncjournalpkg.Journal{
		State: syncjournalpkg.StatusFailed,
		Plan: syncplanpkg.Plan{Items: []syncplanpkg.Item{
			{RelativePath: "a.bin", Action: "download", Kind: "file", RemoteSize: 40 << 30},
			{RelativePath: "b.bin", Action: "download", Kind: "file", RemoteSize: 40 << 30},
		}},
		Items: []syncjournalpkg.Item{
			{Index: 0, State: "failed", Phase: syncjournalpkg.PhaseMutationDone},
			{Index: 1, State: "failed", Phase: syncjournalpkg.PhaseMutationDone},
		},
	}
	output, evidence := (&FileTools{}).diagnoseSyncRecoveryJournal(context.Background(), "sha256:"+strings.Repeat("a", 64), journal)
	if output.ErrorCode != "checksum_budget_exceeded" || output.EvidenceComplete || output.Checked != 0 || output.ChecksummedBytes != 0 || output.ChecksumBudgetBytes != maxMCPSyncRecoveryChecksumBytes || len(evidence) != 0 {
		t.Fatalf("checksum preflight output=%#v evidence=%#v", output, evidence)
	}
}

func TestCaptureRecoveryLocalChecksRuntimeBudgetBeforeHash(t *testing.T) {
	root := t.TempDir()
	path := root + string(os.PathSeparator) + "small.bin"
	if err := os.WriteFile(path, []byte("ab"), 0o600); err != nil {
		t.Fatal(err)
	}
	budget := &mcpSyncRecoveryChecksumBudget{limit: 1}
	executor := &mcpSyncExecutor{ft: NewFileTools(nil, WithLocalRoot(root)), recoveryChecksumBudget: budget}
	post, exists, err := executor.captureRecoveryLocal(path)
	if post != nil || exists || !errors.Is(err, errMCPSyncRecoveryChecksumBudgetExceeded) || budget.used != 0 {
		t.Fatalf("runtime checksum budget result post=%#v exists=%v err=%v used=%d", post, exists, err, budget.used)
	}
}

func TestDiagnoseSyncRecoveryWireUsesStructuredContentWithoutSecrets(t *testing.T) {
	fixture := newMCPPersistentLocalDeleteFixture(t)
	seedMCPDestructiveRecoveryJournal(t, fixture, syncjournalpkg.PhaseDeleteStarted, false)
	server := mcp.NewServer(&mcp.Implementation{Name: "diagnose-sync-recovery-test", Version: "1"}, nil)
	fixture.ft.RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	clientSession, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "diagnose_sync_recovery", Arguments: map[string]any{"plan_id": fixture.args.ExpectPlanID}})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("wire recovery diagnosis result=%#v err=%v", result, err)
	}
	structured, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output DiagnoseSyncRecoveryOutput
	if err := json.Unmarshal(structured, &output); err != nil {
		t.Fatal(err)
	}
	if !output.Resolvable || output.DiagnosisID == "" || output.RetryFull != 1 || output.Items[0].RelativePath != "orphan.bin" {
		t.Fatalf("wire recovery diagnosis output=%#v", output)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(structured), text} {
		for _, forbidden := range []string{fixture.state.Plan.PlanID, fixture.store.Root, fixture.localPath, "synthetic destructive interruption"} {
			if strings.Contains(payload, forbidden) {
				t.Fatalf("wire recovery diagnosis leaked %q: %s", forbidden, payload)
			}
		}
		if strings.Contains(strings.ToLower(payload), "sha1") || strings.Contains(strings.ToLower(payload), "postcondition") {
			t.Fatalf("wire recovery diagnosis leaked hidden evidence: %s", payload)
		}
	}
}
