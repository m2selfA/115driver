package tools

import (
	"context"
	"fmt"
	"math"

	"github.com/SheltonZhu/115driver/internal/remoteresolver"
	"github.com/SheltonZhu/115driver/internal/remotetree"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxMCPUsagePaths        = 64
	defaultMCPUsageMaxNodes = 10000
	maxMCPUsageMaxNodes     = 50000
)

// SummarizeUsageArgs defines a bounded read-only remote usage request.
type SummarizeUsageArgs struct {
	Paths    []string `json:"paths" jsonschema:"remote 115 paths to summarize; duplicate logical paths are rejected"`
	MaxDepth int      `json:"max_depth,omitempty" jsonschema:"maximum descendant depth for directories; 0 means unlimited"`
	MaxNodes int      `json:"max_nodes,omitempty" jsonschema:"aggregate descendant/file node budget across all paths; default 10000, maximum 50000"`
}

// MCPUsageSummary mirrors CLI du semantics while exposing why a result may be partial.
type MCPUsageSummary struct {
	Path         string `json:"path" jsonschema:"requested remote path"`
	Size         int64  `json:"size" jsonschema:"summed descendant file bytes, or file size for a file target"`
	Files        int64  `json:"files" jsonschema:"number of files counted"`
	Directories  int64  `json:"directories" jsonschema:"number of descendant directories counted; directory root itself is excluded"`
	NodesVisited int    `json:"nodes_visited" jsonschema:"number of file/directory nodes counted against the budget"`
	MaxDepth     int    `json:"max_depth" jsonschema:"requested maximum traversal depth; 0 means unlimited"`
	Complete     bool   `json:"complete" jsonschema:"whether the requested path was fully summarized within depth and node limits"`
	DepthLimited bool   `json:"depth_limited" jsonschema:"whether traversal intentionally stopped descending at max_depth"`
	NodeLimited  bool   `json:"node_limited" jsonschema:"whether traversal stopped because the aggregate node budget was exhausted"`
}

// SummarizeUsageItemResult reports one requested path while preserving input order.
type SummarizeUsageItemResult struct {
	Index   int              `json:"index" jsonschema:"zero-based input path index"`
	Path    string           `json:"path" jsonschema:"requested remote path"`
	Success bool             `json:"success" jsonschema:"whether a usage summary was produced"`
	Error   string           `json:"error,omitempty" jsonschema:"item error when the path could not be summarized"`
	Data    *MCPUsageSummary `json:"data,omitempty" jsonschema:"usage summary when successful, including partial bounded summaries"`
}

// SummarizeUsageResult summarizes a bounded multi-path usage request.
type SummarizeUsageResult struct {
	Requested       int                        `json:"requested" jsonschema:"number of requested paths"`
	Succeeded       int                        `json:"succeeded" jsonschema:"number of paths with a produced summary"`
	Failed          int                        `json:"failed" jsonschema:"number of paths that could not be summarized"`
	MaxDepth        int                        `json:"max_depth" jsonschema:"requested maximum traversal depth"`
	MaxNodes        int                        `json:"max_nodes" jsonschema:"aggregate node budget applied to the request"`
	NodesVisited    int                        `json:"nodes_visited" jsonschema:"aggregate nodes counted across successful and partially processed paths"`
	BudgetExhausted bool                       `json:"budget_exhausted" jsonschema:"whether max_nodes prevented further counting or later paths from running"`
	Items           []SummarizeUsageItemResult `json:"items" jsonschema:"per-path results in input order"`
}

type mcpUsageClient interface {
	remoteresolver.Client
	remotetree.PagedClient
	GetFile(fileID string) (*driver.File, error)
}

func normalizeMCPUsageArgs(args SummarizeUsageArgs) ([]string, int, error) {
	if len(args.Paths) > maxMCPUsagePaths {
		return nil, 0, fmt.Errorf("usage request has %d paths; maximum is %d", len(args.Paths), maxMCPUsagePaths)
	}
	paths, err := normalizeMCPResolvePaths(args.Paths)
	if err != nil {
		return nil, 0, err
	}
	if args.MaxDepth < 0 {
		return nil, 0, fmt.Errorf("max_depth must be >= 0")
	}
	if args.MaxNodes < 0 {
		return nil, 0, fmt.Errorf("max_nodes must be >= 0")
	}
	maxNodes := args.MaxNodes
	if maxNodes == 0 {
		maxNodes = defaultMCPUsageMaxNodes
	}
	if maxNodes > maxMCPUsageMaxNodes {
		return nil, 0, fmt.Errorf("max_nodes must not exceed %d", maxMCPUsageMaxNodes)
	}
	return paths, maxNodes, nil
}

func summarizeMCPUsageTarget(ctx context.Context, client mcpUsageClient, pathResolver *remoteresolver.PathResolver, remotePath string, maxDepth, remainingNodes int) (MCPUsageSummary, bool, error) {
	summary := MCPUsageSummary{Path: remotePath, MaxDepth: maxDepth, Complete: true}
	fileID, isDirectory, err := pathResolver.ResolvePath(remotePath)
	if err != nil {
		return summary, false, err
	}
	if !isDirectory {
		if remainingNodes <= 0 {
			summary.Complete = false
			summary.NodeLimited = true
			return summary, true, nil
		}
		file, err := client.GetFile(fileID)
		if err != nil {
			return summary, false, err
		}
		if file == nil {
			return summary, false, fmt.Errorf("file metadata for %q is empty: %w", remotePath, driver.ErrUnexpected)
		}
		if file.IsDirectory {
			return summary, false, fmt.Errorf("resolved file %q unexpectedly became a directory: %w", remotePath, driver.ErrUnexpected)
		}
		if file.Size < 0 {
			return summary, false, fmt.Errorf("file %q has negative size: %w", remotePath, driver.ErrUnexpected)
		}
		summary.Size = file.Size
		summary.Files = 1
		summary.NodesVisited = 1
		return summary, false, nil
	}

	nodeLimited := false
	walkResult, err := remotetree.WalkPaged(client, fileID, remotePath, maxDepth, func(entry remotetree.Entry) (bool, error) {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		if summary.NodesVisited >= remainingNodes {
			nodeLimited = true
			return true, nil
		}
		if entry.File.Size < 0 {
			return false, fmt.Errorf("remote entry %q has negative size: %w", entry.RemotePath, driver.ErrUnexpected)
		}
		summary.NodesVisited++
		if entry.File.IsDirectory {
			summary.Directories++
			return false, nil
		}
		if entry.File.Size > 0 && summary.Size > math.MaxInt64-entry.File.Size {
			return false, fmt.Errorf("directory size exceeds int64 while counting %q", entry.RemotePath)
		}
		summary.Files++
		summary.Size += entry.File.Size
		return false, nil
	})
	if err != nil {
		return summary, nodeLimited, err
	}
	summary.DepthLimited = walkResult.DepthLimited
	summary.NodeLimited = nodeLimited
	summary.Complete = !summary.DepthLimited && !summary.NodeLimited
	return summary, nodeLimited, nil
}

func summarizeMCPUsage(ctx context.Context, client mcpUsageClient, args SummarizeUsageArgs) (SummarizeUsageResult, error) {
	paths, maxNodes, err := normalizeMCPUsageArgs(args)
	if err != nil {
		return SummarizeUsageResult{}, err
	}
	if client == nil {
		return SummarizeUsageResult{}, fmt.Errorf("115 client is unavailable")
	}
	response := SummarizeUsageResult{
		Requested: len(paths),
		MaxDepth:  args.MaxDepth,
		MaxNodes:  maxNodes,
		Items:     make([]SummarizeUsageItemResult, len(paths)),
	}
	snapshotClient := newMCPUsageSnapshotClient(client)
	pathResolver := remoteresolver.New(snapshotClient)

	for i, remotePath := range paths {
		entry := SummarizeUsageItemResult{Index: i, Path: remotePath}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				entry.Error = err.Error()
				response.Failed++
				response.Items[i] = entry
				continue
			}
		}
		if response.NodesVisited >= maxNodes {
			response.BudgetExhausted = true
			entry.Error = "node budget exhausted before this path could be summarized"
			response.Failed++
			response.Items[i] = entry
			continue
		}

		summary, nodeLimited, err := summarizeMCPUsageTarget(ctx, snapshotClient, pathResolver, remotePath, args.MaxDepth, maxNodes-response.NodesVisited)
		response.NodesVisited += summary.NodesVisited
		if nodeLimited {
			response.BudgetExhausted = true
		}
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}
		entry.Success = true
		entry.Data = &summary
		response.Succeeded++
		response.Items[i] = entry
	}
	return response, nil
}

func summarizeUsageCallResult(response SummarizeUsageResult) (*mcp.CallToolResult, SummarizeUsageResult, error) {
	return mcpTypedJSONResult("summarize_usage", response, response, response.Failed > 0)
}

func (dt *DirTools) summarizeUsage(ctx context.Context, req *mcp.CallToolRequest, args SummarizeUsageArgs) (*mcp.CallToolResult, SummarizeUsageResult, error) {
	response, err := summarizeMCPUsage(ctx, dt.client, args)
	if err != nil {
		return toolError(fmt.Sprintf("summarize_usage preflight failed: %v", err)), SummarizeUsageResult{}, nil
	}
	return summarizeUsageCallResult(response)
}
