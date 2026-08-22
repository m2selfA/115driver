package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPSyncRecoveryLatchMarksReplacementSecondPhaseFailures(t *testing.T) {
	executor := &mcpSyncExecutor{}
	err := executor.markMutationFailure(
		syncplanpkg.Item{Action: "replace-remote"},
		syncjournalpkg.MutationStageWrite,
		errors.New("synthetic winner failure"),
	)
	if !errors.Is(err, errMCPSyncRecoveryRequired) || !executor.recoveryRequired.Load() {
		t.Fatalf("replacement failure did not latch recovery review: err=%v latched=%v", err, executor.recoveryRequired.Load())
	}
}

type mcpSyncRecoveryFixture struct {
	ft          *FileTools
	args        ExecuteSyncPlanArgs
	root        string
	remoteID    string
	deleteCalls *int
	deleted     *bool
}

func newMCPSyncRecoveryDeleteFixture(t *testing.T) mcpSyncRecoveryFixture {
	t.Helper()
	root := t.TempDir()
	deleted := false
	deleteCalls := 0
	const remoteID = "remote-delete-secret-id"
	sha1 := mcpSyncTestSHA1("payload")
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			remotePath := strings.Trim(req.URL.Query().Get("path"), "/")
			switch remotePath {
			case "remote":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"r"}`), nil
			case "remote/orphan.bin":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
			default:
				t.Fatalf("unexpected recovery getid path %q", remotePath)
			}
		case "/natsort/files.php", "/files":
			if req.URL.Query().Get("cid") != "r" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected recovery listing: %s", req.URL)
			}
			if deleted {
				return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":0,"offset":0,"limit":500,"data":[]}`), nil
			}
			return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":1,"offset":0,"limit":500,"data":[{"fid":"`+remoteID+`","cid":"r","n":"orphan.bin","s":"7","sha":"`+sha1+`"}]}`), nil
		case "/files/get_info":
			if req.URL.Query().Get("file_id") != remoteID {
				t.Fatalf("unexpected recovery metadata id: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"data":[{"fid":"`+remoteID+`","cid":"r","n":"orphan.bin","s":"7","sha":"`+sha1+`"}]}`), nil
		case "/rb/delete":
			deleteCalls++
			deleted = true
			return nil, errors.New("synthetic delete response loss")
		default:
			t.Fatalf("unexpected recovery request: %s %s", req.Method, req.URL)
		}
		return nil, errors.New("unreachable")
	})})))

	planArgs := PlanSyncArgs{
		LocalPath: root, RemotePath: "/remote", Direction: "upload", DeleteExtraneous: true,
		MaxNodes: 20, MaxChecksumBytes: 64,
	}
	planned, err := planMCPSync(context.Background(), client, root, planArgs)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Summary.DeleteRemoteRoots != 1 || planned.Summary.DeleteRemoteFiles != 1 || planned.Summary.DeleteRemoteBytes != 7 {
		t.Fatalf("unexpected reviewed remote delete impact: %#v", planned.Summary)
	}
	return mcpSyncRecoveryFixture{
		ft: NewFileTools(client, WithLocalRoot(root), WithDestructiveTools(true)),
		args: ExecuteSyncPlanArgs{
			LocalPath: root, RemotePath: "/remote", Direction: "upload", DeleteExtraneous: true,
			MaxNodes: 20, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID,
		},
		root: root, remoteID: remoteID, deleteCalls: &deleteCalls, deleted: &deleted,
	}
}

func TestExecuteSyncPlanRemoteDeleteResponseLossRequiresRecoveryReview(t *testing.T) {
	fixture := newMCPSyncRecoveryDeleteFixture(t)
	result, output, err := fixture.ft.executeSyncPlan(context.Background(), nil, fixture.args)
	if err != nil || result == nil || !result.IsError || !output.RecoveryRequired || output.ErrorCode != "recovery_required" || output.Summary.Failed != 1 {
		t.Fatalf("ambiguous remote delete result=%#v output=%#v err=%v", result, output, err)
	}
	if *fixture.deleteCalls != 1 || !*fixture.deleted {
		t.Fatalf("ambiguous remote delete did not reach exactly one mutation: calls=%d deleted=%v", *fixture.deleteCalls, *fixture.deleted)
	}
	for _, forbidden := range []string{fixture.root, "/remote", fixture.remoteID} {
		if strings.Contains(output.Error, forbidden) {
			t.Fatalf("recovery-required top-level error leaked %q: %q", forbidden, output.Error)
		}
	}
}

func TestExecuteSyncPlanRecoveryRequiredIsVisibleInStructuredWireOutput(t *testing.T) {
	fixture := newMCPSyncRecoveryDeleteFixture(t)
	result := callExecuteSyncPlanWire(t, fixture.ft, fixture.args)
	if result == nil || !result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("wire recovery-required result=%#v", result)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output MCPSyncExecutionOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if !output.RecoveryRequired || output.ErrorCode != "recovery_required" || output.Summary.Failed != 1 {
		t.Fatalf("wire recovery-required structured output=%#v", output)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		for _, forbidden := range []string{fixture.root, "/remote", fixture.remoteID} {
			if strings.Contains(payload, forbidden) {
				t.Fatalf("wire recovery-required leaked %q: %s", forbidden, payload)
			}
		}
	}
	if *fixture.deleteCalls != 1 || !*fixture.deleted {
		t.Fatalf("wire recovery-required mutation count=%d deleted=%v", *fixture.deleteCalls, *fixture.deleted)
	}
}
