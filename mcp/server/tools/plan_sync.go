package tools

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/internal/remoteresolver"
	"github.com/SheltonZhu/115driver/internal/remotetree"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultMCPSyncPlanMaxNodes      = 1000
	maxMCPSyncPlanMaxNodes          = 5000
	defaultMCPSyncPlanChecksumBytes = int64(4 << 30)
	maxMCPSyncPlanChecksumBytes     = int64(64 << 30)
)

// PlanSyncArgs describes a read-only local/remote sync planning request. The
// local source must stay inside the configured MCP local root; no operation is
// executed by this tool.
type PlanSyncArgs struct {
	LocalPath        string `json:"local_path" jsonschema:"existing local directory inside the configured MCP local root"`
	RemotePath       string `json:"remote_path" jsonschema:"existing remote 115 directory path"`
	Direction        string `json:"direction,omitempty" jsonschema:"sync direction: both, upload, or download; default both"`
	ConflictPolicy   string `json:"conflict_policy,omitempty" jsonschema:"conflict policy: error, prefer-local, or prefer-remote; default error"`
	DeleteExtraneous bool   `json:"delete,omitempty" jsonschema:"plan mirror deletions on the destination side; requires explicit upload or download direction"`
	MaxNodes         int    `json:"max_nodes,omitempty" jsonschema:"aggregate local plus remote descendant budget; default 1000, maximum 5000; planning fails instead of returning a partial plan"`
	MaxChecksumBytes int64  `json:"max_checksum_bytes,omitempty" jsonschema:"maximum local bytes that may be hashed while resolving same-size file equality; default 4 GiB, maximum 64 GiB"`
}

// MCPSyncPlanItem is the safe agent-facing projection of one shared sync-plan
// item. Absolute local paths, SHA1 values, pick codes, and signed URLs are
// intentionally omitted; the opaque snapshot_id binds the hidden full snapshot.
type MCPSyncPlanItem struct {
	RelativePath  string `json:"relative_path" jsonschema:"path relative to both sync roots"`
	Action        string `json:"action" jsonschema:"planned action or conflict/skip state"`
	Kind          string `json:"kind" jsonschema:"file, directory, or mixed for an unresolved type conflict"`
	Reason        string `json:"reason" jsonschema:"stable planning reason"`
	LocalPresent  bool   `json:"local_present"`
	RemotePresent bool   `json:"remote_present"`
	LocalSize     int64  `json:"local_size,omitempty"`
	RemoteSize    int64  `json:"remote_size,omitempty"`
	Destructive   bool   `json:"destructive,omitempty"`
	ReplacesKind  string `json:"replaces_kind,omitempty"`
}

// MCPSyncPlanSummary preserves the useful counters from the shared CLI/MCP sync
// classifier without exposing local filesystem identities or content digests.
type MCPSyncPlanSummary struct {
	Ready                    bool   `json:"ready" jsonschema:"whether the sync plan has no unresolved conflicts"`
	SnapshotID               string `json:"snapshot_id" jsonschema:"opaque fingerprint of the complete hidden local/remote planning snapshot"`
	Direction                string `json:"direction"`
	ConflictPolicy           string `json:"conflict_policy"`
	DeleteExtraneous         bool   `json:"delete"`
	MaxNodes                 int    `json:"max_nodes"`
	MaxChecksumBytes         int64  `json:"max_checksum_bytes"`
	LocalNodes               int    `json:"local_nodes"`
	RemoteNodes              int    `json:"remote_nodes"`
	ChangeActions            int    `json:"change_actions"`
	Conflicts                int    `json:"conflicts"`
	ResolvedConflicts        int    `json:"resolved_conflicts"`
	DestructiveActions       int    `json:"destructive_actions"`
	RequiresAllowDestructive bool   `json:"requires_allow_destructive"`
	UploadFiles              int    `json:"upload_files"`
	UploadDirectories        int    `json:"upload_directories"`
	UploadBytes              int64  `json:"upload_bytes"`
	DownloadFiles            int    `json:"download_files"`
	DownloadDirectories      int    `json:"download_directories"`
	DownloadBytes            int64  `json:"download_bytes"`
	DeleteRemoteRoots        int    `json:"delete_remote_roots"`
	DeleteRemoteFiles        int    `json:"delete_remote_files"`
	DeleteRemoteDirectories  int    `json:"delete_remote_directories"`
	DeleteRemoteBytes        int64  `json:"delete_remote_bytes"`
	DeleteLocalRoots         int    `json:"delete_local_roots"`
	DeleteLocalFiles         int    `json:"delete_local_files"`
	DeleteLocalDirectories   int    `json:"delete_local_directories"`
	DeleteLocalBytes         int64  `json:"delete_local_bytes"`
	CoveredByDelete          int    `json:"covered_by_delete"`
	ChecksummedFiles         int    `json:"checksummed_files"`
	ChecksummedBytes         int64  `json:"checksummed_bytes"`
}

// MCPSyncPlanOutput combines the safe sync-specific projection with the generic
// content-addressed MCPPlan v1 envelope that later execution tools can consume.
type MCPSyncPlanOutput struct {
	Summary MCPSyncPlanSummary `json:"summary"`
	Plan    MCPPlan            `json:"plan"`
	Items   []MCPSyncPlanItem  `json:"items" jsonschema:"safe per-path planning decisions in deterministic order"`
}

type mcpSyncPlanClient interface {
	mcpListTreeClient
	GetFile(fileID string) (*driver.File, error)
}

type mcpSyncPlanNormalizedArgs struct {
	localPath        string
	remotePath       string
	options          syncplanpkg.Options
	maxNodes         int
	maxChecksumBytes int64
}

func normalizeMCPSyncPlanArgs(localRoot string, args PlanSyncArgs) (mcpSyncPlanNormalizedArgs, error) {
	localPath := strings.TrimSpace(args.LocalPath)
	if localPath == "" {
		return mcpSyncPlanNormalizedArgs{}, fmt.Errorf("local_path is required")
	}
	remotePath := strings.TrimSpace(args.RemotePath)
	if remotePath == "" {
		return mcpSyncPlanNormalizedArgs{}, fmt.Errorf("remote_path is required")
	}
	validatedLocal, err := validateLocalPath(localRoot, localPath, true)
	if err != nil {
		return mcpSyncPlanNormalizedArgs{}, fmt.Errorf("local sync root denied: %w", err)
	}
	options, err := syncplanpkg.ResolveOptionsWithDelete(args.Direction, args.ConflictPolicy, args.DeleteExtraneous)
	if err != nil {
		return mcpSyncPlanNormalizedArgs{}, err
	}
	maxNodes := args.MaxNodes
	if maxNodes < 0 {
		return mcpSyncPlanNormalizedArgs{}, fmt.Errorf("max_nodes must be >= 0")
	}
	if maxNodes == 0 {
		maxNodes = defaultMCPSyncPlanMaxNodes
	}
	if maxNodes > maxMCPSyncPlanMaxNodes {
		return mcpSyncPlanNormalizedArgs{}, fmt.Errorf("max_nodes must not exceed %d", maxMCPSyncPlanMaxNodes)
	}
	maxChecksumBytes := args.MaxChecksumBytes
	if maxChecksumBytes < 0 {
		return mcpSyncPlanNormalizedArgs{}, fmt.Errorf("max_checksum_bytes must be >= 0")
	}
	if maxChecksumBytes == 0 {
		maxChecksumBytes = defaultMCPSyncPlanChecksumBytes
	}
	if maxChecksumBytes > maxMCPSyncPlanChecksumBytes {
		return mcpSyncPlanNormalizedArgs{}, fmt.Errorf("max_checksum_bytes must not exceed %d", maxMCPSyncPlanChecksumBytes)
	}
	return mcpSyncPlanNormalizedArgs{
		localPath:        validatedLocal,
		remotePath:       syncplanpkg.CanonicalRemoteRoot(remotePath),
		options:          options,
		maxNodes:         maxNodes,
		maxChecksumBytes: maxChecksumBytes,
	}, nil
}

func scanMCPSyncRemoteEntries(ctx context.Context, client mcpSyncPlanClient, remoteRoot, localRoot string, maxNodes int) (map[string]syncplanpkg.Entry, string, int, error) {
	if client == nil {
		return nil, "", 0, fmt.Errorf("115 client is unavailable")
	}
	if maxNodes < 0 {
		return nil, "", 0, fmt.Errorf("remote node budget must be >= 0")
	}
	snapshotClient := newMCPListTreeSnapshotClient(client)
	resolver := remoteresolver.New(snapshotClient)
	remoteRootID, err := resolver.ResolveDir(remoteRoot)
	if err != nil {
		return nil, "", 0, fmt.Errorf("resolve remote sync root %q: %w", remoteRoot, err)
	}
	entries := make(map[string]syncplanpkg.Entry)
	nodes := 0
	nodeLimited := false
	_, err = remotetree.WalkPaged(snapshotClient, remoteRootID, remoteRoot, 0, func(walkEntry remotetree.Entry) (bool, error) {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		if nodes >= maxNodes {
			nodeLimited = true
			return true, nil
		}
		if err := syncplanpkg.ValidateRelativePath(walkEntry.RelativePath); err != nil {
			return false, fmt.Errorf("invalid remote sync path %q: %w", walkEntry.RelativePath, err)
		}
		kind := "file"
		size := walkEntry.File.Size
		if walkEntry.File.IsDirectory {
			kind = "directory"
			size = 0
		}
		modTime := int64(0)
		if !walkEntry.File.UpdateTime.IsZero() {
			modTime = walkEntry.File.UpdateTime.UnixNano()
		}
		entry := syncplanpkg.Entry{
			RelativePath:    walkEntry.RelativePath,
			Kind:            kind,
			LocalPath:       filepath.Join(localRoot, filepath.FromSlash(walkEntry.RelativePath)),
			RemotePath:      walkEntry.RemotePath,
			RemoteID:        walkEntry.File.FileID,
			Size:            size,
			SHA1:            strings.ToUpper(strings.TrimSpace(walkEntry.File.Sha1)),
			ModTimeUnixNano: modTime,
		}
		if err := syncplanpkg.AddEntry(entries, entry, "remote"); err != nil {
			return false, err
		}
		nodes++
		return false, nil
	})
	if err != nil {
		return nil, "", nodes, err
	}
	if nodeLimited {
		return nil, "", nodes, fmt.Errorf("remote sync tree exceeds remaining max_nodes budget %d", maxNodes)
	}
	return entries, remoteRootID, nodes, nil
}

func resolveMCPSyncRemoteSHA1(client mcpSyncPlanClient, remote syncplanpkg.Entry) (string, error) {
	if value := strings.ToUpper(strings.TrimSpace(remote.SHA1)); value != "" {
		return value, nil
	}
	if client == nil || strings.TrimSpace(remote.RemoteID) == "" {
		return "", fmt.Errorf("remote file %q has no stable identity for SHA1 lookup", remote.RelativePath)
	}
	info, err := client.GetFile(remote.RemoteID)
	if err != nil {
		return "", fmt.Errorf("read remote SHA1 for %q: %w", remote.RelativePath, err)
	}
	if info == nil || info.IsDirectory || info.Size != remote.Size {
		return "", fmt.Errorf("remote file %q changed while resolving SHA1", remote.RelativePath)
	}
	return strings.ToUpper(strings.TrimSpace(info.Sha1)), nil
}

func mcpSyncPlanOperationRefs(item syncplanpkg.Item) (sourceRef, targetRef, targetSide string) {
	localRef := "local:" + item.RelativePath
	remoteRef := "remote:" + item.RelativePath
	switch item.Action {
	case "upload", "replace-remote":
		return localRef, remoteRef, "remote"
	case "download", "replace-local":
		return remoteRef, localRef, "local"
	case "delete-remote":
		return "", remoteRef, "remote"
	case "delete-local":
		return "", localRef, "local"
	default:
		return "", "", ""
	}
}

func mcpSyncPlanOperationBytes(item syncplanpkg.Item) int64 {
	if item.Kind != "file" {
		return 0
	}
	switch item.Action {
	case "upload", "replace-remote":
		return item.LocalSize
	case "download", "replace-local":
		return item.RemoteSize
	default:
		return 0
	}
}

func nearestMCPSyncParentOperation(relativePath, side string, directoryOps map[string]string) string {
	parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relativePath)))
	for parent != "." && parent != "/" && parent != "" {
		if operationID := directoryOps[side+"\x00"+parent]; operationID != "" {
			return operationID
		}
		next := filepath.ToSlash(filepath.Dir(filepath.FromSlash(parent)))
		if next == parent {
			break
		}
		parent = next
	}
	return ""
}

func buildMCPSyncPlanEnvelope(plan syncplanpkg.Plan) (MCPPlan, error) {
	generic := MCPPlan{
		Kind:        "sync",
		CreatedFrom: "plan_sync",
		Operations:  make([]MCPPlanOperation, 0, plan.ChangeActions),
		Preconditions: []MCPPlanPrecondition{
			{
				Kind:     "sync_snapshot",
				Ref:      "sync",
				Expected: plan.PlanID,
			},
			{
				Kind:     "sync_ready",
				Ref:      "sync",
				Expected: "true",
			},
		},
	}
	directoryOps := make(map[string]string)
	for _, item := range plan.Items {
		sourceRef, targetRef, targetSide := mcpSyncPlanOperationRefs(item)
		if targetSide == "" {
			continue
		}
		operationID := fmt.Sprintf("op-%06d", len(generic.Operations))
		estimatedBytes := mcpSyncPlanOperationBytes(item)
		safety := MCPPlanSafetyAdditive
		if item.Destructive || strings.HasPrefix(item.Action, "replace-") || strings.HasPrefix(item.Action, "delete-") {
			safety = MCPPlanSafetyDestructive
		}
		generic.Operations = append(generic.Operations, MCPPlanOperation{
			ID:             operationID,
			Operation:      item.Action,
			SafetyClass:    safety,
			SourceRef:      sourceRef,
			TargetRef:      targetRef,
			EstimatedBytes: &estimatedBytes,
		})
		if dependency := nearestMCPSyncParentOperation(item.RelativePath, targetSide, directoryOps); dependency != "" {
			generic.Dependencies = append(generic.Dependencies, MCPPlanDependency{OperationID: operationID, DependsOn: dependency})
		}
		if item.Kind == "directory" && (item.Action == "upload" || item.Action == "download" || item.Action == "replace-remote" || item.Action == "replace-local") {
			directoryOps[targetSide+"\x00"+item.RelativePath] = operationID
		}
	}
	return finalizeMCPPlan(generic)
}

func mcpSyncPlanSafeItems(plan syncplanpkg.Plan) []MCPSyncPlanItem {
	items := make([]MCPSyncPlanItem, len(plan.Items))
	for i, item := range plan.Items {
		items[i] = MCPSyncPlanItem{
			RelativePath:  item.RelativePath,
			Action:        item.Action,
			Kind:          item.Kind,
			Reason:        item.Reason,
			LocalPresent:  item.LocalPresent,
			RemotePresent: item.RemotePresent,
			LocalSize:     item.LocalSize,
			RemoteSize:    item.RemoteSize,
			Destructive:   item.Destructive,
			ReplacesKind:  item.ReplacesKind,
		}
	}
	return items
}

type mcpSyncTreeState struct {
	normalized    mcpSyncPlanNormalizedArgs
	localSnapshot syncplanpkg.LocalSnapshot
	remoteEntries map[string]syncplanpkg.Entry
	remoteRootID  string
	remoteNodes   int
}

// scanMCPSyncTrees performs only the bounded local/remote inventory phase. It
// deliberately does not run the classifier or hash local content, allowing
// journal resume preflight to spend max_checksum_bytes exactly once on the
// completed local postconditions that actually require digest verification.
func scanMCPSyncTrees(ctx context.Context, client mcpSyncPlanClient, localRoot string, args PlanSyncArgs) (mcpSyncTreeState, error) {
	normalized, err := normalizeMCPSyncPlanArgs(localRoot, args)
	if err != nil {
		return mcpSyncTreeState{}, err
	}
	localSnapshot, err := syncplanpkg.ScanLocal(ctx, normalized.localPath, normalized.remotePath, normalized.maxNodes)
	if err != nil {
		return mcpSyncTreeState{}, fmt.Errorf("scan local sync root: %w", err)
	}
	remainingNodes := normalized.maxNodes - localSnapshot.Nodes
	remoteEntries, remoteRootID, remoteNodes, err := scanMCPSyncRemoteEntries(ctx, client, normalized.remotePath, localSnapshot.Root, remainingNodes)
	if err != nil {
		return mcpSyncTreeState{}, err
	}
	return mcpSyncTreeState{
		normalized: normalized, localSnapshot: localSnapshot,
		remoteEntries: remoteEntries, remoteRootID: remoteRootID, remoteNodes: remoteNodes,
	}, nil
}

type mcpSyncPlannedState struct {
	Output        MCPSyncPlanOutput
	Plan          syncplanpkg.Plan
	LocalEntries  map[string]syncplanpkg.Entry
	RemoteEntries map[string]syncplanpkg.Entry
}

func planMCPSyncState(ctx context.Context, client mcpSyncPlanClient, localRoot string, args PlanSyncArgs) (mcpSyncPlannedState, error) {
	trees, err := scanMCPSyncTrees(ctx, client, localRoot, args)
	if err != nil {
		return mcpSyncPlannedState{}, err
	}
	normalized := trees.normalized
	localSnapshot := trees.localSnapshot
	remoteEntries := trees.remoteEntries
	remoteRootID := trees.remoteRootID
	remoteNodes := trees.remoteNodes

	checksummedBytes := int64(0)
	sharedPlan, err := syncplanpkg.Build(localSnapshot.Entries, remoteEntries, localSnapshot.Root, normalized.remotePath, remoteRootID, normalized.options, syncplanpkg.Resolvers{
		RemoteSHA1: func(entry syncplanpkg.Entry) (string, error) {
			return resolveMCPSyncRemoteSHA1(client, entry)
		},
		LocalDigest: func(entry syncplanpkg.Entry) (*uploadpkg.PreparedDigest, error) {
			if entry.Size < 0 || checksummedBytes > normalized.maxChecksumBytes-entry.Size {
				return nil, fmt.Errorf("sync checksum budget exceeded before %q: used=%d next=%d max=%d", entry.RelativePath, checksummedBytes, entry.Size, normalized.maxChecksumBytes)
			}
			digest, err := syncplanpkg.PrepareLocalDigest(entry)
			if err != nil {
				return nil, err
			}
			checksummedBytes += entry.Size
			return digest, nil
		},
	})
	if err != nil {
		return mcpSyncPlannedState{}, err
	}
	if sharedPlan.ChecksummedBytes != checksummedBytes {
		return mcpSyncPlannedState{}, fmt.Errorf("sync checksum accounting mismatch: planner=%d callback=%d", sharedPlan.ChecksummedBytes, checksummedBytes)
	}
	genericPlan, err := buildMCPSyncPlanEnvelope(sharedPlan)
	if err != nil {
		return mcpSyncPlannedState{}, fmt.Errorf("build MCP sync plan envelope: %w", err)
	}
	output := MCPSyncPlanOutput{
		Summary: MCPSyncPlanSummary{
			Ready:                    sharedPlan.Ready,
			SnapshotID:               sharedPlan.PlanID,
			Direction:                sharedPlan.Direction,
			ConflictPolicy:           sharedPlan.ConflictPolicy,
			DeleteExtraneous:         sharedPlan.DeleteExtraneous,
			MaxNodes:                 normalized.maxNodes,
			MaxChecksumBytes:         normalized.maxChecksumBytes,
			LocalNodes:               localSnapshot.Nodes,
			RemoteNodes:              remoteNodes,
			ChangeActions:            sharedPlan.ChangeActions,
			Conflicts:                sharedPlan.Conflicts,
			ResolvedConflicts:        sharedPlan.ResolvedConflicts,
			DestructiveActions:       sharedPlan.DestructiveActions,
			RequiresAllowDestructive: sharedPlan.RequiresAllowDestructive,
			UploadFiles:              sharedPlan.UploadFiles,
			UploadDirectories:        sharedPlan.UploadDirs,
			UploadBytes:              sharedPlan.UploadBytes,
			DownloadFiles:            sharedPlan.DownloadFiles,
			DownloadDirectories:      sharedPlan.DownloadDirs,
			DownloadBytes:            sharedPlan.DownloadBytes,
			DeleteRemoteRoots:        sharedPlan.DeleteRemoteRoots,
			DeleteRemoteFiles:        sharedPlan.DeleteRemoteFiles,
			DeleteRemoteDirectories:  sharedPlan.DeleteRemoteDirs,
			DeleteRemoteBytes:        sharedPlan.DeleteRemoteBytes,
			DeleteLocalRoots:         sharedPlan.DeleteLocalRoots,
			DeleteLocalFiles:         sharedPlan.DeleteLocalFiles,
			DeleteLocalDirectories:   sharedPlan.DeleteLocalDirs,
			DeleteLocalBytes:         sharedPlan.DeleteLocalBytes,
			CoveredByDelete:          sharedPlan.CoveredByDelete,
			ChecksummedFiles:         sharedPlan.ChecksummedFiles,
			ChecksummedBytes:         sharedPlan.ChecksummedBytes,
		},
		Plan:  genericPlan,
		Items: mcpSyncPlanSafeItems(sharedPlan),
	}
	return mcpSyncPlannedState{
		Output: output, Plan: sharedPlan,
		LocalEntries: localSnapshot.Entries, RemoteEntries: remoteEntries,
	}, nil
}

func planMCPSync(ctx context.Context, client mcpSyncPlanClient, localRoot string, args PlanSyncArgs) (MCPSyncPlanOutput, error) {
	state, err := planMCPSyncState(ctx, client, localRoot, args)
	if err != nil {
		return MCPSyncPlanOutput{}, err
	}
	return state.Output, nil
}

func planSyncCallResult(response MCPSyncPlanOutput) (*mcp.CallToolResult, MCPSyncPlanOutput, error) {
	return mcpTypedJSONResult("plan_sync", response, response, false)
}

func (ft *FileTools) planSync(ctx context.Context, req *mcp.CallToolRequest, args PlanSyncArgs) (*mcp.CallToolResult, MCPSyncPlanOutput, error) {
	response, err := planMCPSync(ctx, ft.client, ft.localRoot, args)
	if err != nil {
		return toolError("plan_sync failed: " + redactMCPSyncPlanError(err, ft.localRoot, args)), MCPSyncPlanOutput{}, nil
	}
	return planSyncCallResult(response)
}
