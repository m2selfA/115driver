package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SearchTools holds search-related MCP tools
type SearchTools struct {
	client *driver.Pan115Client
}

// NewSearchTools creates a new SearchTools instance
func NewSearchTools(client *driver.Pan115Client) *SearchTools {
	return &SearchTools{
		client: client,
	}
}

// SearchArgs defines arguments for search tool
type SearchArgs struct {
	SearchValue string `json:"search_value" jsonschema:"search keyword"`
	Offset      int    `json:"offset" jsonschema:"offset for pagination, default is 0"`
	Limit       int    `json:"limit" jsonschema:"limit number of results, default is 30, maximum is 500"`
	Type        int    `json:"type" jsonschema:"file type filter, 0:all 1:folder 2:document 3:image 4:video 5:audio 6:archive"`
	Order       string `json:"order" jsonschema:"sort field, e.g. file_name, user_ptime"`
	Asc         int    `json:"asc" jsonschema:"ascending order, 0:descending 1:ascending"`
}

// MCPSearchResult is the stable typed view of the existing search JSON result.
type MCPSearchResult struct {
	Count    int                 `json:"count"`
	Files    []MCPDirectoryEntry `json:"files"`
	Offset   int                 `json:"offset"`
	PageSize int                 `json:"page_size"`
	Order    string              `json:"order"`
	IsAsc    int                 `json:"is_asc"`
}

// RegisterTools registers search-related tools with the MCP server
func (st *SearchTools) RegisterTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "search",
		Description: "Search for files and directories in the 115 cloud storage",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, st.search)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_many",
		Description: "Run multiple independently paginated 115 searches in one bounded read-only batch",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, st.searchMany)
}

func validateMCPSearchArgs(args SearchArgs) error {
	if args.Offset < 0 {
		return fmt.Errorf("offset must not be negative")
	}
	if args.Limit < 0 {
		return fmt.Errorf("limit must not be negative")
	}
	if args.Limit > maxMCPSearchBatchLimit {
		return fmt.Errorf("limit must not exceed %d", maxMCPSearchBatchLimit)
	}
	if args.Type < 0 || args.Type > 6 {
		return fmt.Errorf("type must be between 0 and 6")
	}
	if args.Asc != 0 && args.Asc != 1 {
		return fmt.Errorf("asc must be 0 or 1")
	}
	return nil
}

func (st *SearchTools) search(ctx context.Context, req *mcp.CallToolRequest, args SearchArgs) (*mcp.CallToolResult, MCPSearchResult, error) {
	if err := validateMCPSearchArgs(args); err != nil {
		return toolError(fmt.Sprintf("Invalid search parameters: %v", err)), MCPSearchResult{}, nil
	}
	opts := &driver.SearchOption{
		SearchValue: args.SearchValue,
		Offset:      args.Offset,
		Limit:       args.Limit,
		Type:        args.Type,
		Order:       args.Order,
		Asc:         args.Asc,
	}

	result, err := st.client.Search(opts)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to search files: %v", err),
				},
			},
			IsError: true,
		}, MCPSearchResult{}, nil
	}

	// Preserve the historical TextContent shape while reusing the same typed
	// conversion as search_many.
	files := make([]map[string]interface{}, len(result.Files))
	for i, file := range result.Files {
		files[i] = map[string]interface{}{
			"file_id":      file.FileID,
			"parent_id":    file.ParentID,
			"name":         file.Name,
			"size":         file.Size,
			"pick_code":    file.PickCode,
			"sha1":         file.Sha1,
			"is_directory": file.IsDirectory,
			"star":         file.Star,
			"create_time":  file.CreateTime,
			"update_time":  file.UpdateTime,
		}
	}

	response := map[string]interface{}{
		"count":     result.Count,
		"files":     files,
		"offset":    result.Offset,
		"page_size": result.PageSize,
		"order":     result.Order,
		"is_asc":    result.IsAsc,
	}
	typed := mcpSearchResultFromDriver(result)

	responseJSON, err := json.Marshal(response)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize search results: %v", err),
				},
			},
			IsError: true,
		}, MCPSearchResult{}, nil
	}

	return mcpTypedTextResult(string(responseJSON), typed, false)
}
