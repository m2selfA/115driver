package tools

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
)

func TestNormalizeMCPSyncDeleteBudgetAndSharedTotals(t *testing.T) {
	for name, args := range map[string]ExecuteSyncPlanArgs{
		"negative-roots": {DeleteExtraneous: true, MaxDeleteRoots: -1},
		"negative-items": {DeleteExtraneous: true, MaxDeleteItems: -1},
		"negative-bytes": {DeleteExtraneous: true, MaxDeleteBytes: -1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeMCPSyncDeleteBudget(args); err == nil {
				t.Fatalf("invalid delete budget was accepted: %#v", args)
			}
		})
	}

	plan := syncplanpkg.Plan{
		DeleteRemoteRoots: 1, DeleteRemoteFiles: 1, DeleteRemoteDirs: 1, DeleteRemoteBytes: 5,
		DeleteLocalRoots: 1, DeleteLocalFiles: 1, DeleteLocalDirs: 1, DeleteLocalBytes: 4,
	}
	exact, err := normalizeMCPSyncDeleteBudget(ExecuteSyncPlanArgs{DeleteExtraneous: true, MaxDeleteRoots: 2, MaxDeleteItems: 4, MaxDeleteBytes: 9})
	if err != nil || exact.validate(plan) != nil {
		t.Fatalf("exact delete budget rejected: budget=%#v err=%v", exact, err)
	}
	for name, budget := range map[string]mcpSyncDeleteBudget{
		"roots": {maxRoots: 1},
		"items": {maxItems: 3},
		"bytes": {maxBytes: 8},
	} {
		t.Run(name, func(t *testing.T) {
			if err := budget.validate(plan); !errors.Is(err, errMCPSyncDeleteBudgetExceeded) {
				t.Fatalf("delete budget error = %v, want budget sentinel", err)
			}
		})
	}
	standalone, err := normalizeMCPSyncDeleteBudget(ExecuteSyncPlanArgs{MaxDeleteRoots: 1})
	if err != nil || standalone.maxRoots != 1 {
		t.Fatalf("replacement-capable budget without mirror delete = %#v, %v", standalone, err)
	}
}

func TestMCPSyncDeleteBudgetCountsReplacementSubtreeWithoutMirrorDelete(t *testing.T) {
	root := t.TempDir()
	local := map[string]syncplanpkg.Entry{
		"node": {RelativePath: "node", Kind: "file", LocalPath: filepath.Join(root, "node"), Size: 4},
	}
	remote := map[string]syncplanpkg.Entry{
		"node":             {RelativePath: "node", Kind: "directory", RemotePath: "/remote/node", RemoteID: "node"},
		"node/sub":         {RelativePath: "node/sub", Kind: "directory", RemotePath: "/remote/node/sub", RemoteID: "sub"},
		"node/sub/old.bin": {RelativePath: "node/sub/old.bin", Kind: "file", RemotePath: "/remote/node/sub/old.bin", RemoteID: "old", Size: 7, SHA1: mcpSyncTestSHA1("old")},
	}
	plan, err := syncplanpkg.Build(local, remote, root, "/remote", "remote-root", syncplanpkg.Options{Direction: syncplanpkg.DirectionUpload, ConflictPolicy: syncplanpkg.ConflictLocal}, syncplanpkg.Resolvers{
		LocalDigest: func(entry syncplanpkg.Entry) (*uploadpkg.PreparedDigest, error) {
			return &uploadpkg.PreparedDigest{SHA1: mcpSyncTestSHA1("winner"), Size: entry.Size}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	roots, items, bytes := plan.DeleteTotals()
	if roots != 1 || items != 3 || bytes != 7 {
		t.Fatalf("replacement delete totals = %d/%d/%d", roots, items, bytes)
	}
	for name, budget := range map[string]mcpSyncDeleteBudget{
		"items": {maxRoots: 1, maxItems: 2, maxBytes: 7},
		"bytes": {maxRoots: 1, maxItems: 3, maxBytes: 6},
	} {
		t.Run(name, func(t *testing.T) {
			if err := budget.validate(plan); !errors.Is(err, errMCPSyncDeleteBudgetExceeded) {
				t.Fatalf("replacement budget error = %v", err)
			}
		})
	}
	if err := (mcpSyncDeleteBudget{maxRoots: 1, maxItems: 3, maxBytes: 7}).validate(plan); err != nil {
		t.Fatalf("exact replacement budget rejected: %v", err)
	}
}

func TestExecuteSyncPlanDeleteBudgetStopsBeforeLocalMirrorDelete(t *testing.T) {
	root := t.TempDir()
	localPath := filepath.Join(root, "orphan.bin")
	if err := os.WriteFile(localPath, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/files/getid":
			if strings.Trim(req.URL.Query().Get("path"), "/") != "remote" {
				t.Fatalf("unexpected sync budget getid path: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"id":"r"}`), nil
		case "/natsort/files.php", "/files":
			if req.URL.Query().Get("cid") != "r" || req.URL.Query().Get("record_open_time") != "0" {
				t.Fatalf("unexpected sync budget listing: %s", req.URL)
			}
			return mcpResolveJSONResponse(req, `{"state":true,"cid":"r","count":0,"offset":0,"limit":500,"data":[]}`), nil
		default:
			t.Fatalf("unexpected sync budget request: %s %s", req.Method, req.URL)
		}
		return nil, errors.New("unreachable")
	})})))
	planArgs := PlanSyncArgs{
		LocalPath: root, RemotePath: "/remote", Direction: "download", DeleteExtraneous: true,
		MaxNodes: 20, MaxChecksumBytes: 64,
	}
	planned, err := planMCPSync(context.Background(), client, root, planArgs)
	if err != nil {
		t.Fatal(err)
	}
	if planned.Summary.DeleteLocalRoots != 1 || planned.Summary.DeleteLocalFiles != 1 || planned.Summary.DeleteLocalBytes != 7 {
		t.Fatalf("unexpected reviewed delete impact: %#v", planned.Summary)
	}

	ft := NewFileTools(client, WithLocalRoot(root))
	result, output, err := ft.executeSyncPlan(context.Background(), nil, ExecuteSyncPlanArgs{
		LocalPath: root, RemotePath: "/remote", Direction: "download", DeleteExtraneous: true,
		MaxNodes: 20, MaxChecksumBytes: 64, ExpectPlanID: planned.Plan.PlanID, MaxDeleteBytes: 1,
	})
	if err != nil || result == nil || !result.IsError || output.ErrorCode != "delete_budget_exceeded" || output.Summary.Processed != 0 {
		t.Fatalf("budgeted execute_sync_plan result=%#v output=%#v err=%v", result, output, err)
	}
	if _, err := os.Stat(localPath); err != nil {
		t.Fatalf("mirror-delete budget failure removed local file: %v", err)
	}
}
