package tools

import (
	"context"
	"fmt"

	"github.com/SheltonZhu/115driver/internal/remoteresolver"
	"github.com/SheltonZhu/115driver/internal/remotetree"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxMCPListTreePaths        = 32
	defaultMCPListTreeMaxNodes = 1000
	maxMCPListTreeMaxNodes     = 5000
)

// ListTreeArgs defines a bounded recursive listing for one or more remote directories.
type ListTreeArgs struct {
	Paths    []string `json:"paths" jsonschema:"remote 115 directory paths to traverse; duplicate logical paths are rejected"`
	MaxDepth int      `json:"max_depth,omitempty" jsonschema:"maximum descendant depth; 0 means unlimited"`
	MaxNodes int      `json:"max_nodes,omitempty" jsonschema:"aggregate returned-entry budget across all roots; default 1000, maximum 5000"`
}

// MCPRemoteTreeEntry is the stable recursive-listing representation of one descendant.
type MCPRemoteTreeEntry struct {
	FileID       string `json:"file_id" jsonschema:"115 object ID"`
	ParentID     string `json:"parent_id" jsonschema:"parent directory ID"`
	Name         string `json:"name" jsonschema:"base name"`
	Size         int64  `json:"size" jsonschema:"file size in bytes; directories normally report zero"`
	PickCode     string `json:"pick_code,omitempty" jsonschema:"115 pick code for files when available"`
	SHA1         string `json:"sha1,omitempty" jsonschema:"SHA1 digest for files when available"`
	IsDirectory  bool   `json:"is_directory" jsonschema:"whether this entry is a directory"`
	Star         bool   `json:"star" jsonschema:"whether this entry is starred"`
	CreateTime   string `json:"create_time,omitempty" jsonschema:"creation time in RFC3339 when available"`
	UpdateTime   string `json:"update_time,omitempty" jsonschema:"update time in RFC3339 when available"`
	RelativePath string `json:"relative_path" jsonschema:"path relative to the requested root"`
	Path         string `json:"path" jsonschema:"full display path below the requested root"`
	Depth        int    `json:"depth" jsonschema:"descendant depth where direct children are depth 1"`
}

// ListTreeItemResult reports one recursive directory request.
type ListTreeItemResult struct {
	Index        int                  `json:"index" jsonschema:"zero-based input root index"`
	Path         string               `json:"path" jsonschema:"requested remote directory path"`
	Success      bool                 `json:"success" jsonschema:"whether a bounded recursive listing was produced"`
	Error        string               `json:"error,omitempty" jsonschema:"item error when the directory could not be traversed"`
	Entries      []MCPRemoteTreeEntry `json:"entries,omitempty" jsonschema:"descendants in breadth-first server order"`
	NodesVisited int                  `json:"nodes_visited" jsonschema:"number of returned descendants counted against max_nodes"`
	MaxDepth     int                  `json:"max_depth" jsonschema:"requested maximum traversal depth"`
	Complete     bool                 `json:"complete" jsonschema:"whether the subtree was fully listed within depth and node limits"`
	DepthLimited bool                 `json:"depth_limited" jsonschema:"whether max_depth prevented descending further"`
	NodeLimited  bool                 `json:"node_limited" jsonschema:"whether max_nodes stopped this subtree early"`
}

// ListTreeResult summarizes one bounded recursive listing batch.
type ListTreeResult struct {
	Requested       int                  `json:"requested" jsonschema:"number of requested directory roots"`
	Succeeded       int                  `json:"succeeded" jsonschema:"number of roots with a produced listing"`
	Failed          int                  `json:"failed" jsonschema:"number of roots that could not be listed"`
	MaxDepth        int                  `json:"max_depth" jsonschema:"requested maximum traversal depth"`
	MaxNodes        int                  `json:"max_nodes" jsonschema:"aggregate returned-entry budget"`
	NodesVisited    int                  `json:"nodes_visited" jsonschema:"aggregate returned descendants across all roots"`
	BudgetExhausted bool                 `json:"budget_exhausted" jsonschema:"whether max_nodes stopped traversal or prevented later roots"`
	Items           []ListTreeItemResult `json:"items" jsonschema:"per-root results in input order"`
}

type mcpListTreeClient interface {
	remoteresolver.Client
	remotetree.PagedClient
}

func normalizeMCPListTreeArgs(args ListTreeArgs) ([]string, int, error) {
	if len(args.Paths) > maxMCPListTreePaths {
		return nil, 0, fmt.Errorf("recursive listing has %d paths; maximum is %d", len(args.Paths), maxMCPListTreePaths)
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
		maxNodes = defaultMCPListTreeMaxNodes
	}
	if maxNodes > maxMCPListTreeMaxNodes {
		return nil, 0, fmt.Errorf("max_nodes must not exceed %d", maxMCPListTreeMaxNodes)
	}
	return paths, maxNodes, nil
}

func mcpRemoteTreeEntryFromWalk(entry remotetree.Entry) MCPRemoteTreeEntry {
	base := mcpDirectoryEntryFromFile(entry.File)
	return MCPRemoteTreeEntry{
		FileID:       base.FileID,
		ParentID:     base.ParentID,
		Name:         base.Name,
		Size:         base.Size,
		PickCode:     base.PickCode,
		SHA1:         base.SHA1,
		IsDirectory:  base.IsDirectory,
		Star:         base.Star,
		CreateTime:   base.CreateTime,
		UpdateTime:   base.UpdateTime,
		RelativePath: entry.RelativePath,
		Path:         entry.RemotePath,
		Depth:        entry.Depth,
	}
}

func listMCPRemoteTree(ctx context.Context, client mcpListTreeClient, args ListTreeArgs) (ListTreeResult, error) {
	paths, maxNodes, err := normalizeMCPListTreeArgs(args)
	if err != nil {
		return ListTreeResult{}, err
	}
	if client == nil {
		return ListTreeResult{}, fmt.Errorf("115 client is unavailable")
	}
	response := ListTreeResult{
		Requested: len(paths),
		MaxDepth:  args.MaxDepth,
		MaxNodes:  maxNodes,
		Items:     make([]ListTreeItemResult, len(paths)),
	}
	snapshotClient := newMCPListTreeSnapshotClient(client)
	pathResolver := remoteresolver.New(snapshotClient)

	for i, remotePath := range paths {
		entry := ListTreeItemResult{Index: i, Path: remotePath, MaxDepth: args.MaxDepth}
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
			entry.Error = "node budget exhausted before this directory could be traversed"
			response.Failed++
			response.Items[i] = entry
			continue
		}

		dirID, err := pathResolver.ResolveDir(remotePath)
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}

		remainingNodes := maxNodes - response.NodesVisited
		collected := make([]MCPRemoteTreeEntry, 0, minInt(remainingNodes, 128))
		nodeLimited := false
		walkResult, err := remotetree.WalkPaged(snapshotClient, dirID, remotePath, args.MaxDepth, func(walkEntry remotetree.Entry) (bool, error) {
			if ctx != nil {
				if err := ctx.Err(); err != nil {
					return false, err
				}
			}
			if len(collected) >= remainingNodes {
				nodeLimited = true
				return true, nil
			}
			collected = append(collected, mcpRemoteTreeEntryFromWalk(walkEntry))
			return false, nil
		})
		entry.NodesVisited = len(collected)
		response.NodesVisited += entry.NodesVisited
		if nodeLimited {
			response.BudgetExhausted = true
		}
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}
		entry.Entries = collected
		entry.DepthLimited = walkResult.DepthLimited
		entry.NodeLimited = nodeLimited
		entry.Complete = !entry.DepthLimited && !entry.NodeLimited
		entry.Success = true
		response.Succeeded++
		response.Items[i] = entry
	}
	return response, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func listTreeCallResult(response ListTreeResult) (*mcp.CallToolResult, ListTreeResult, error) {
	return mcpTypedJSONResult("list_tree", response, response, response.Failed > 0)
}

func (dt *DirTools) listTree(ctx context.Context, req *mcp.CallToolRequest, args ListTreeArgs) (*mcp.CallToolResult, ListTreeResult, error) {
	response, err := listMCPRemoteTree(ctx, dt.client, args)
	if err != nil {
		return toolError(fmt.Sprintf("list_tree preflight failed: %v", err)), ListTreeResult{}, nil
	}
	return listTreeCallResult(response)
}
