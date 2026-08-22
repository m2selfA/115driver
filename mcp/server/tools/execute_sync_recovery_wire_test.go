package tools

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func prepareExecuteSyncRecoveryWireFixture(t *testing.T) (*FileTools, ExecuteSyncPlanArgs, *int, *bool) {
	t.Helper()
	root := t.TempDir()
	deleted := false
	deleteCalls := 0
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
				t.Fatalf("unexpected recovery wire getid path %q", remotePath)
			}
		case "/natsort/files.php", "/files":
			if req.URL.Query().Get("cid") != "r" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected recovery wire listing: %s", req.URL)
			}
			if deleted {
				return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":0,"offset":0,"limit":500,"data":[]}`), nil
			}
			return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":1,"offset":0,"limit":500,"data":[{"fid":"old","cid":"r","n":"orphan.bin","s":"7","sha":"`+sha1+`"}]}`), nil
		case "/files/get_info":
			return mcpResolveJSONResponse(req, `{"state":true,"data":[{"fid":"old","cid":"r","n":"orphan.bin","s":"7","sha":"`+sha1+`"}]}`), nil
		case "/rb/delete":
			deleteCalls++
			deleted = true
			return nil, errors.New("synthetic delete response loss")
		default:
			t.Fatalf("unexpected recovery wire request: %s %s", req.Method, req.URL)
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
	ft := NewFileTools(client, WithLocalRoot(root), WithDestructiveTools(true))
	return ft, ExecuteSyncPlanArgs{
		LocalPath: root, RemotePath: "/remote", Direction: "upload", DeleteExtraneous: true,
		MaxNodes: 20, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID,
	}, &deleteCalls, &deleted
}

func TestExecuteSyncPlanWireRecoveryRequiredIsStructured(t *testing.T) {
	ft, args, deleteCalls, deleted := prepareExecuteSyncRecoveryWireFixture(t)
	result := callExecuteSyncPlanWire(t, ft, args)
	if result == nil || !result.IsError || result.StructuredContent == nil || len(result.Content) != 1 {
		t.Fatalf("wire recovery execute_sync_plan result=%#v", result)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output MCPSyncExecutionOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if !output.RecoveryRequired || output.ErrorCode != "recovery_required" || output.Summary.Failed != 1 || *deleteCalls != 1 || !*deleted {
		t.Fatalf("wire recovery structured output=%#v delete_calls=%d deleted=%v", output, *deleteCalls, *deleted)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, payload := range []string{string(encoded), text} {
		if strings.Contains(payload, args.LocalPath) || strings.Contains(payload, args.RemotePath) || strings.Contains(payload, "old") || strings.Contains(strings.ToLower(payload), "sha1") {
			t.Fatalf("wire recovery output leaked hidden identity: %s", payload)
		}
	}
}
