package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RecycleTools holds recycle bin-related MCP tools
type RecycleTools struct {
	client           *driver.Pan115Client
	allowDestructive bool
}

type RecycleToolsOption func(*RecycleTools)

func WithRecycleDestructiveTools(allow bool) RecycleToolsOption {
	return func(rt *RecycleTools) {
		rt.allowDestructive = allow
	}
}

// NewRecycleTools creates a new RecycleTools instance
func NewRecycleTools(client *driver.Pan115Client, opts ...RecycleToolsOption) *RecycleTools {
	rt := &RecycleTools{
		client: client,
	}
	for _, opt := range opts {
		opt(rt)
	}
	return rt
}

// ListRecycleArgs defines arguments for listing recycle bin items
type ListRecycleArgs struct {
	Offset int `json:"offset" jsonschema:"offset for pagination, default is 0"`
	Limit  int `json:"limit" jsonschema:"number of items to return, default is 40, maximum is 100"`
}

const (
	defaultRecycleLimit = 40
	maxRecycleLimit     = 100
)

// MCPRecycleBinItem is a stable typed view of one recycle-bin entry.
type MCPRecycleBinItem struct {
	ID         string `json:"id"`
	FileName   string `json:"file_name"`
	FileSize   int64  `json:"file_size"`
	ParentID   string `json:"cid"`
	ParentName string `json:"parent_name"`
	DeleteTime int64  `json:"dtime"`
}

// MCPRecycleListOutput wraps the legacy bare-array TextContent for structured output.
type MCPRecycleListOutput struct {
	Items []MCPRecycleBinItem `json:"items"`
}

// RevertRecycleArgs defines arguments for reverting recycle bin items
type RevertRecycleArgs struct {
	ItemIDs []string `json:"item_ids" jsonschema:"IDs of items to revert"`
	DryRun  bool     `json:"dry_run,omitempty" jsonschema:"validate and preview item IDs without restoring anything"`
}

// CleanRecycleArgs defines arguments for cleaning recycle bin items
type CleanRecycleArgs struct {
	Password string   `json:"password" jsonschema:"password for cleaning recycle bin"`
	ItemIDs  []string `json:"item_ids" jsonschema:"IDs of items to clean"`
	DryRun   bool     `json:"dry_run,omitempty" jsonschema:"validate and preview item IDs without permanently deleting anything"`
}

type MCPRecycleMutationPlan struct {
	Operation string   `json:"operation"`
	DryRun    bool     `json:"dry_run"`
	Requested int      `json:"requested"`
	ItemIDs   []string `json:"item_ids"`
}

// RegisterTools registers recycle bin-related tools with the MCP server
func (rt *RecycleTools) RegisterTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "listRecycleBin",
		Description: "List items in the recycle bin",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, rt.listRecycleBin)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_recycle_pages",
		Description: "List multiple independently paginated recycle-bin pages in one bounded read-only batch",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, rt.listRecyclePages)

	if rt.allowDestructive {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "revertRecycleBin",
			Description: "Restore recycle-bin items after full ID preflight; dry_run validates without changing state",
			Annotations: mcpMutationToolAnnotations(false),
		}, rt.revertRecycleBinTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "cleanRecycleBin",
			Description: "Permanently clean selected recycle-bin items after full ID preflight; dry_run validates without deleting",
			Annotations: mcpDestructiveToolAnnotations(),
		}, rt.cleanRecycleBinTyped)
	}
}

func (rt *RecycleTools) listRecycleBin(ctx context.Context, req *mcp.CallToolRequest, args ListRecycleArgs) (*mcp.CallToolResult, MCPRecycleListOutput, error) {
	offset, limit, paginationErr := validateRecyclePagination(args.Offset, args.Limit)
	if paginationErr != nil {
		return toolError(fmt.Sprintf("Invalid recycle pagination: %v", paginationErr)), MCPRecycleListOutput{}, nil
	}

	items, err := rt.client.ListRecycleBin(offset, limit)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to list recycle bin: %v", err),
				},
			},
			IsError: true,
		}, MCPRecycleListOutput{}, nil
	}

	resultJSON, err := json.Marshal(items)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize result: %v", err),
				},
			},
			IsError: true,
		}, MCPRecycleListOutput{}, nil
	}
	typedItems := mcpRecycleBinItems(items)

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: string(resultJSON),
			},
		},
	}, MCPRecycleListOutput{Items: typedItems}, nil
}

func validateRecyclePagination(offset, limit int) (int, int, error) {
	if offset < 0 {
		return 0, 0, fmt.Errorf("offset must not be negative")
	}
	if limit < 0 {
		return 0, 0, fmt.Errorf("limit must not be negative")
	}
	if limit == 0 {
		limit = defaultRecycleLimit
	}
	if limit > maxRecycleLimit {
		return 0, 0, fmt.Errorf("limit must not exceed %d", maxRecycleLimit)
	}
	return offset, limit, nil
}

func (rt *RecycleTools) revertRecycleBin(ctx context.Context, req *mcp.CallToolRequest, args RevertRecycleArgs) (*mcp.CallToolResult, any, error) {
	itemIDs, err := normalizeMCPUniqueStrings(args.ItemIDs, "recycle item ID", maxMCPMutationBatchItems)
	if err != nil {
		return toolError(fmt.Sprintf("Recycle restore preflight failed: %v", err)), nil, nil
	}
	if args.DryRun {
		return mcpJSONResult(MCPRecycleMutationPlan{Operation: "revert_recycle", DryRun: true, Requested: len(itemIDs), ItemIDs: itemIDs}, "Failed to serialize recycle restore dry-run")
	}
	if rt.client == nil {
		return toolError("115 client is unavailable"), nil, nil
	}

	err = rt.client.RevertRecycleBin(itemIDs...)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to revert recycle bin items: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: "Items reverted successfully",
			},
		},
	}, nil, nil
}

func (rt *RecycleTools) cleanRecycleBin(ctx context.Context, req *mcp.CallToolRequest, args CleanRecycleArgs) (*mcp.CallToolResult, any, error) {
	itemIDs, err := normalizeMCPUniqueStrings(args.ItemIDs, "recycle item ID", maxMCPMutationBatchItems)
	if err != nil {
		return toolError(fmt.Sprintf("Recycle clean preflight failed: %v", err)), nil, nil
	}
	if args.DryRun {
		return mcpJSONResult(MCPRecycleMutationPlan{Operation: "clean_recycle", DryRun: true, Requested: len(itemIDs), ItemIDs: itemIDs}, "Failed to serialize recycle clean dry-run")
	}
	if rt.client == nil {
		return toolError("115 client is unavailable"), nil, nil
	}

	err = rt.client.CleanRecycleBin(args.Password, itemIDs...)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to clean recycle bin items: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: "Items cleaned successfully",
			},
		},
	}, nil, nil
}
