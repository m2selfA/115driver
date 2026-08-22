package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DirTools holds directory-related MCP tools
type DirTools struct {
	client *driver.Pan115Client
}

const (
	maxDirectoryListLimit int64 = 500
)

// NewDirTools creates a new DirTools instance
func NewDirTools(client *driver.Pan115Client) *DirTools {
	return &DirTools{
		client: client,
	}
}

// ListDirectoryArgs defines arguments for list directory tool
type ListDirectoryArgs struct {
	DirID  string `json:"dir_id" jsonschema:"directory ID to list, default is root directory: 0"`
	Offset int64  `json:"offset,omitempty" jsonschema:"offset for pagination, default is 0"`
	Limit  int64  `json:"limit,omitempty" jsonschema:"number of items to return, default is all items"`
}

// ListDirectoryOutput is the typed MCP view of one directory listing. The
// legacy TextContent remains the original bare driver-file JSON array.
type ListDirectoryOutput struct {
	Entries []MCPDirectoryEntry `json:"entries" jsonschema:"directory entries in server order"`
}

// RegisterTools registers directory-related tools with the MCP server
func (dt *DirTools) RegisterTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "listDirectory",
		Description: "List files and directories in a specific directory without recording an open-time side effect",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, dt.listDirectory)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_directories",
		Description: "List multiple independently paginated directories in one bounded read-only batch without recording open-time side effects",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, dt.listDirectories)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "resolve_paths",
		Description: "Resolve multiple remote 115 paths to stable file/directory IDs in one bounded read-only batch",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, dt.resolvePaths)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "inspect_paths",
		Description: "Resolve multiple remote paths and return safe object metadata in one bounded read-only call",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, dt.inspectPaths)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "summarize_usage",
		Description: "Summarize file bytes and descendant counts for remote paths with explicit depth and aggregate node budgets",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, dt.summarizeUsage)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "compare_directories",
		Description: "Compare two remote directory trees with bounded read-only traversal and fail-closed absence classification",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, dt.compareDirectories)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_tree",
		Description: "Recursively list descendants of remote directories with explicit depth and aggregate node budgets",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, dt.listTree)
}

func (dt *DirTools) listDirectory(ctx context.Context, req *mcp.CallToolRequest, args ListDirectoryArgs) (*mcp.CallToolResult, ListDirectoryOutput, error) {
	var (
		files *[]driver.File
		err   error
	)

	offset, limit, paginationErr := validateDirectoryListPagination(args.Offset, args.Limit)
	if paginationErr != nil {
		return toolError(fmt.Sprintf("Invalid directory pagination: %v", paginationErr)), ListDirectoryOutput{}, nil
	}
	if limit > 0 {
		files, err = dt.client.ListPage(args.DirID, offset, limit, driver.WithRecordOpenTime(false))
	} else {
		files, err = dt.client.List(args.DirID, driver.WithRecordOpenTime(false))
		if err == nil && files != nil && offset > 0 {
			remaining := directoryFilesFromOffset(*files, offset)
			files = &remaining
		}
	}

	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to list directory: %v", err),
				},
			},
			IsError: true,
		}, ListDirectoryOutput{}, nil
	}

	// Preserve the historical bare-array TextContent while suppressing the
	// remote thumbnail URL. Thumbnail URLs may carry short-lived query tokens;
	// the typed output already intentionally omits them.
	resultJSON, err := json.Marshal(sanitizeMCPDirectoryTextFiles(files))
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize result: %v", err),
				},
			},
			IsError: true,
		}, ListDirectoryOutput{}, nil
	}
	output := ListDirectoryOutput{}
	if files != nil {
		output.Entries = make([]MCPDirectoryEntry, len(*files))
		for i, file := range *files {
			output.Entries[i] = mcpDirectoryEntryFromFile(file)
		}
	}
	return mcpTypedTextResult(string(resultJSON), output, false)
}

func sanitizeMCPDirectoryTextFiles(files *[]driver.File) *[]driver.File {
	if files == nil {
		return nil
	}
	safe := append([]driver.File(nil), (*files)...)
	for i := range safe {
		safe[i].ThumbURL = ""
	}
	return &safe
}

func validateDirectoryListPagination(offset, limit int64) (int64, int64, error) {
	if offset < 0 {
		return 0, 0, fmt.Errorf("offset must not be negative")
	}
	if limit < 0 {
		return 0, 0, fmt.Errorf("limit must not be negative")
	}
	if limit > maxDirectoryListLimit {
		return 0, 0, fmt.Errorf("limit must not exceed %d", maxDirectoryListLimit)
	}
	return offset, limit, nil
}

func directoryFilesFromOffset(files []driver.File, offset int64) []driver.File {
	if offset <= 0 {
		return files
	}
	if offset >= int64(len(files)) {
		return files[:0]
	}
	return files[int(offset):]
}
