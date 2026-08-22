package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/SheltonZhu/115driver/internal/remoteresolver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxMCPResolvePaths = 256

// ResolvePathsArgs defines a bounded read-only remote path resolution batch.
type ResolvePathsArgs struct {
	Paths []string `json:"paths" jsonschema:"remote 115 paths to resolve to stable IDs; exact duplicate logical paths are rejected"`
}

// ResolvePathItemResult reports one path lookup while preserving input order.
type ResolvePathItemResult struct {
	Index       int    `json:"index" jsonschema:"zero-based input path index"`
	Path        string `json:"path" jsonschema:"requested remote path"`
	FileID      string `json:"file_id,omitempty" jsonschema:"resolved 115 file or directory ID"`
	IsDirectory bool   `json:"is_directory" jsonschema:"whether the resolved object is a directory"`
	Success     bool   `json:"success" jsonschema:"whether the path resolved successfully"`
	Error       string `json:"error,omitempty" jsonschema:"item error when resolution failed"`
}

// ResolvePathsResult summarizes one remote path resolution batch.
type ResolvePathsResult struct {
	Requested int                     `json:"requested" jsonschema:"number of requested paths"`
	Succeeded int                     `json:"succeeded" jsonschema:"number of successfully resolved paths"`
	Failed    int                     `json:"failed" jsonschema:"number of failed paths"`
	Items     []ResolvePathItemResult `json:"items" jsonschema:"per-path results in input order"`
}

func normalizeMCPResolvePaths(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("at least one remote path is required")
	}
	if len(paths) > maxMCPResolvePaths {
		return nil, fmt.Errorf("path batch has %d items; maximum is %d", len(paths), maxMCPResolvePaths)
	}

	normalized := make([]string, len(paths))
	seen := make(map[string]int, len(paths))
	for i, remotePath := range paths {
		if strings.TrimSpace(remotePath) == "" {
			return nil, fmt.Errorf("remote path at index %d is blank", i)
		}
		key := strings.Trim(remotePath, "/")
		if key == "" {
			key = "/"
		}
		if previous, exists := seen[key]; exists {
			return nil, fmt.Errorf("remote paths at indexes %d and %d resolve to the same logical path", previous, i)
		}
		seen[key] = i
		normalized[i] = remotePath
	}
	return normalized, nil
}

func resolvePathsCallResult(response ResolvePathsResult) (*mcp.CallToolResult, ResolvePathsResult, error) {
	return mcpTypedJSONResult("resolve_paths", response, response, response.Failed > 0)
}

func (dt *DirTools) resolvePaths(ctx context.Context, req *mcp.CallToolRequest, args ResolvePathsArgs) (*mcp.CallToolResult, ResolvePathsResult, error) {
	paths, err := normalizeMCPResolvePaths(args.Paths)
	if err != nil {
		return toolError(fmt.Sprintf("resolve_paths preflight failed: %v", err)), ResolvePathsResult{}, nil
	}
	if dt.client == nil {
		return toolError("115 client is unavailable"), ResolvePathsResult{}, nil
	}

	snapshotClient := newMCPResolveSnapshotClient(dt.client)
	resolver := remoteresolver.New(snapshotClient)
	response := ResolvePathsResult{Requested: len(paths), Items: make([]ResolvePathItemResult, len(paths))}
	for i, remotePath := range paths {
		entry := ResolvePathItemResult{Index: i, Path: remotePath}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				entry.Error = err.Error()
				response.Failed++
				response.Items[i] = entry
				continue
			}
		}

		fileID, isDirectory, err := resolver.ResolvePath(remotePath)
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}
		entry.FileID = fileID
		entry.IsDirectory = isDirectory
		entry.Success = true
		response.Succeeded++
		response.Items[i] = entry
	}
	return resolvePathsCallResult(response)
}
