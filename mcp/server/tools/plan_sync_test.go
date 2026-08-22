package tools

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func mcpSyncTestSHA1(value string) string {
	digest := sha1.Sum([]byte(value))
	return strings.ToUpper(hex.EncodeToString(digest[:]))
}

func writeMCPSyncTestFile(t *testing.T, root, relative, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeMCPSyncPlanArgsBoundsAndPolicies(t *testing.T) {
	root := t.TempDir()
	normalized, err := normalizeMCPSyncPlanArgs(root, PlanSyncArgs{LocalPath: root, RemotePath: "/remote"})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.maxNodes != defaultMCPSyncPlanMaxNodes || normalized.maxChecksumBytes != defaultMCPSyncPlanChecksumBytes || normalized.options.Direction != "both" || normalized.options.ConflictPolicy != "error" {
		t.Fatalf("plan_sync defaults = %#v", normalized)
	}
	for name, args := range map[string]PlanSyncArgs{
		"blank-local":       {RemotePath: "/remote"},
		"blank-remote":      {LocalPath: root},
		"negative-nodes":    {LocalPath: root, RemotePath: "/remote", MaxNodes: -1},
		"too-many-nodes":    {LocalPath: root, RemotePath: "/remote", MaxNodes: maxMCPSyncPlanMaxNodes + 1},
		"negative-checksum": {LocalPath: root, RemotePath: "/remote", MaxChecksumBytes: -1},
		"too-much-checksum": {LocalPath: root, RemotePath: "/remote", MaxChecksumBytes: maxMCPSyncPlanChecksumBytes + 1},
		"two-way-delete":    {LocalPath: root, RemotePath: "/remote", DeleteExtraneous: true},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeMCPSyncPlanArgs(root, args); err == nil {
				t.Fatal("expected plan_sync preflight failure")
			}
		})
	}
}

func TestBuildMCPSyncPlanEnvelopeAddsDirectoryDependencies(t *testing.T) {
	shared := syncPlanFixtureForEnvelope()
	generic, err := buildMCPSyncPlanEnvelope(shared)
	if err != nil {
		t.Fatal(err)
	}
	if len(generic.Operations) != 2 || len(generic.Dependencies) != 1 {
		t.Fatalf("generic sync plan = %#v", generic)
	}
	if generic.Dependencies[0].OperationID != generic.Operations[1].ID || generic.Dependencies[0].DependsOn != generic.Operations[0].ID {
		t.Fatalf("child operation dependency = %#v", generic.Dependencies)
	}
	if generic.SafetyClass != MCPPlanSafetyAdditive || generic.EstimatedBytes == nil || *generic.EstimatedBytes != 4 {
		t.Fatalf("generic sync plan safety/bytes = %#v", generic)
	}
	if len(generic.Preconditions) != 2 {
		t.Fatalf("generic sync preconditions = %#v", generic.Preconditions)
	}
	preconditions := make(map[string]MCPPlanPrecondition, len(generic.Preconditions))
	for _, precondition := range generic.Preconditions {
		preconditions[precondition.Kind] = precondition
	}
	if preconditions["sync_snapshot"].Expected != shared.PlanID || preconditions["sync_ready"].Expected != "true" {
		t.Fatalf("generic sync preconditions = %#v", generic.Preconditions)
	}
}

func syncPlanFixtureForEnvelope() syncplanpkg.Plan {
	return syncplanpkg.Plan{
		PlanID:        strings.Repeat("a", 64),
		Ready:         true,
		ChangeActions: 2,
		Items: []syncplanpkg.Item{
			{RelativePath: "dir", Action: "upload", Kind: "directory", Reason: "local-only", LocalPresent: true},
			{RelativePath: "dir/file.bin", Action: "upload", Kind: "file", Reason: "local-only", LocalPresent: true, LocalSize: 4},
		},
	}
}

func TestPlanMCPSyncBuildsSafeReadOnlyPlan(t *testing.T) {
	root := t.TempDir()
	writeMCPSyncTestFile(t, root, "same.bin", "same")
	writeMCPSyncTestFile(t, root, "local.bin", "local")
	client := &usageTestClient{
		dirIDs: map[string]string{"remote": "r"},
		treePages: map[string][]driver.File{
			"r": {
				{FileID: "same", ParentID: "r", Name: "same.bin", Size: 4, Sha1: mcpSyncTestSHA1("same")},
				{FileID: "remote", ParentID: "r", Name: "remote.bin", Size: 6, Sha1: mcpSyncTestSHA1("remote")},
			},
		},
	}
	output, err := planMCPSync(context.Background(), client, root, PlanSyncArgs{
		LocalPath: root, RemotePath: "/remote", MaxNodes: 10, MaxChecksumBytes: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The sync planner snapshots both same-size comparison candidates and local
	// files that a later upload action would consume, so the retained execution
	// digest set covers same.bin (4 bytes) plus local.bin (5 bytes).
	if !output.Summary.Ready || output.Summary.ChangeActions != 2 || output.Summary.LocalNodes != 2 || output.Summary.RemoteNodes != 2 || output.Summary.ChecksummedFiles != 2 || output.Summary.ChecksummedBytes != 9 {
		t.Fatalf("unexpected sync summary: %#v", output.Summary)
	}
	if len(output.Plan.Operations) != 2 || output.Plan.SafetyClass != MCPPlanSafetyAdditive || output.Plan.EstimatedBytes == nil || *output.Plan.EstimatedBytes != 11 {
		t.Fatalf("unexpected generic plan: %#v", output.Plan)
	}
	if len(output.Plan.Preconditions) != 2 || !strings.HasPrefix(output.Plan.PlanID, "sha256:") {
		t.Fatalf("sync snapshot was not bound into MCPPlan: %#v", output.Plan)
	}
	preconditions := make(map[string]MCPPlanPrecondition, len(output.Plan.Preconditions))
	for _, precondition := range output.Plan.Preconditions {
		preconditions[precondition.Kind] = precondition
	}
	if preconditions["sync_snapshot"].Expected != output.Summary.SnapshotID || preconditions["sync_ready"].Expected != "true" {
		t.Fatalf("sync execution preconditions = %#v", output.Plan.Preconditions)
	}
	byPath := make(map[string]MCPSyncPlanItem, len(output.Items))
	for _, item := range output.Items {
		byPath[item.RelativePath] = item
	}
	if byPath["same.bin"].Action != "skip" || byPath["local.bin"].Action != "upload" || byPath["remote.bin"].Action != "download" {
		t.Fatalf("unexpected sync item projection: %#v", byPath)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	if strings.Contains(text, filepath.ToSlash(root)) || strings.Contains(strings.ToLower(text), `"sha1"`) || strings.Contains(strings.ToLower(text), `"local_path"`) {
		t.Fatalf("safe sync output leaked local identity or digest: %s", text)
	}
}

func TestPlanMCPSyncAddsParentDirectoryDependency(t *testing.T) {
	root := t.TempDir()
	writeMCPSyncTestFile(t, root, "dir/file.bin", "data")
	client := &usageTestClient{dirIDs: map[string]string{"remote": "r"}, treePages: map[string][]driver.File{"r": {}}}
	output, err := planMCPSync(context.Background(), client, root, PlanSyncArgs{LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Plan.Operations) != 2 || len(output.Plan.Dependencies) != 1 {
		t.Fatalf("nested upload plan = %#v", output.Plan)
	}
	if output.Plan.Dependencies[0].DependsOn != output.Plan.Operations[0].ID || output.Plan.Dependencies[0].OperationID != output.Plan.Operations[1].ID {
		t.Fatalf("nested upload dependency = %#v", output.Plan.Dependencies)
	}
}

func TestPlanMCPSyncFailsClosedOnNodeAndChecksumBudgets(t *testing.T) {
	t.Run("node-budget", func(t *testing.T) {
		root := t.TempDir()
		writeMCPSyncTestFile(t, root, "a.bin", "a")
		writeMCPSyncTestFile(t, root, "b.bin", "b")
		client := &usageTestClient{dirIDs: map[string]string{"remote": "r"}, treePages: map[string][]driver.File{"r": {}}}
		if _, err := planMCPSync(context.Background(), client, root, PlanSyncArgs{LocalPath: root, RemotePath: "/remote", MaxNodes: 1}); err == nil || !strings.Contains(err.Error(), "max_nodes") {
			t.Fatalf("node budget did not fail closed: %v", err)
		}
	})

	t.Run("checksum-budget", func(t *testing.T) {
		root := t.TempDir()
		writeMCPSyncTestFile(t, root, "same-size.bin", "12345")
		client := &usageTestClient{
			dirIDs:    map[string]string{"remote": "r"},
			treePages: map[string][]driver.File{"r": {{FileID: "same", Name: "same-size.bin", Size: 5, Sha1: mcpSyncTestSHA1("other")}}},
		}
		if _, err := planMCPSync(context.Background(), client, root, PlanSyncArgs{LocalPath: root, RemotePath: "/remote", MaxNodes: 10, MaxChecksumBytes: 4}); err == nil || !strings.Contains(err.Error(), "checksum budget") {
			t.Fatalf("checksum budget did not fail closed: %v", err)
		}
	})
}

func TestPlanMCPSyncReportsMirrorDeleteImpact(t *testing.T) {
	root := t.TempDir()
	client := &usageTestClient{
		dirIDs: map[string]string{"remote": "r"},
		treePages: map[string][]driver.File{
			"r": {
				{FileID: "orphan", ParentID: "r", Name: "orphan", IsDirectory: true},
			},
			"orphan": {
				{FileID: "file", ParentID: "orphan", Name: "file.bin", Size: 7, Sha1: mcpSyncTestSHA1("content")},
			},
		},
	}
	output, err := planMCPSync(context.Background(), client, root, PlanSyncArgs{
		LocalPath: root, RemotePath: "/remote", Direction: "upload", DeleteExtraneous: true, MaxNodes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if output.Summary.DeleteRemoteRoots != 1 || output.Summary.DeleteRemoteDirectories != 1 || output.Summary.DeleteRemoteFiles != 1 || output.Summary.DeleteRemoteBytes != 7 || output.Summary.CoveredByDelete != 1 {
		t.Fatalf("mirror-delete impact = %#v", output.Summary)
	}
	if output.Plan.SafetyClass != MCPPlanSafetyDestructive || output.Summary.DestructiveActions != 1 || !output.Summary.RequiresAllowDestructive {
		t.Fatalf("mirror-delete safety = summary=%#v plan=%#v", output.Summary, output.Plan)
	}
}

func TestPlanMCPSyncIgnores115DriverTransferState(t *testing.T) {
	root := t.TempDir()
	writeMCPSyncTestFile(t, root, ".20260821.115driver-upload-test.session.json", "state")
	client := &usageTestClient{dirIDs: map[string]string{"remote": "r"}, treePages: map[string][]driver.File{"r": {}}}
	output, err := planMCPSync(context.Background(), client, root, PlanSyncArgs{LocalPath: root, RemotePath: "/remote", Direction: "upload", MaxNodes: 10})
	if err != nil {
		t.Fatal(err)
	}
	if output.Summary.LocalNodes != 0 || output.Summary.ChangeActions != 0 || len(output.Items) != 0 {
		t.Fatalf("transfer state leaked into sync plan: %#v", output)
	}
}

func TestPlanSyncCallResultKeepsTextAndTypedOutputEquivalent(t *testing.T) {
	response := MCPSyncPlanOutput{Summary: MCPSyncPlanSummary{Ready: true, SnapshotID: strings.Repeat("a", 64)}, Plan: MCPPlan{PlanVersion: 1, PlanID: "sha256:x", Kind: "sync", CreatedFrom: "plan_sync", SafetyClass: MCPPlanSafetyReadOnly}}
	result, output, err := planSyncCallResult(response)
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("plan_sync result=%#v output=%#v err=%v", result, output, err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var decoded MCPSyncPlanOutput
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Summary.SnapshotID != output.Summary.SnapshotID || decoded.Plan.Kind != output.Plan.Kind {
		t.Fatalf("plan_sync text/typed output diverged: text=%#v typed=%#v", decoded, output)
	}
}
