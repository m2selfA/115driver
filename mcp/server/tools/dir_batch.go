package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxMCPDirectoryBatchItems     = 256
	defaultMCPDirectoryBatchLimit = int64(100)
	maxMCPDirectoryBatchEntries   = int64(5000)
)

// ListDirectoriesItem defines one independently paginated directory request.
type ListDirectoriesItem struct {
	DirID  string `json:"dir_id,omitempty" jsonschema:"directory ID to list; empty means root directory 0"`
	Offset int64  `json:"offset,omitempty" jsonschema:"zero-based page offset, default 0"`
	Limit  int64  `json:"limit,omitempty" jsonschema:"page size, default 100, maximum 500"`
}

// ListDirectoriesArgs defines a bounded read-only multi-directory listing.
type ListDirectoriesArgs struct {
	Directories []ListDirectoriesItem `json:"directories" jsonschema:"independently paginated directory requests; exact duplicate requests are rejected"`
}

// MCPDirectoryEntry is the stable, typed subset returned by list_directories.
type MCPDirectoryEntry struct {
	FileID      string `json:"file_id" jsonschema:"115 object ID"`
	ParentID    string `json:"parent_id" jsonschema:"parent directory ID"`
	Name        string `json:"name" jsonschema:"base name"`
	Size        int64  `json:"size" jsonschema:"file size in bytes; directories normally report zero"`
	PickCode    string `json:"pick_code,omitempty" jsonschema:"115 pick code for files when available"`
	SHA1        string `json:"sha1,omitempty" jsonschema:"SHA1 digest for files when available"`
	IsDirectory bool   `json:"is_directory" jsonschema:"whether this entry is a directory"`
	Star        bool   `json:"star" jsonschema:"whether this entry is starred"`
	CreateTime  string `json:"create_time,omitempty" jsonschema:"creation time in RFC3339 when available"`
	UpdateTime  string `json:"update_time,omitempty" jsonschema:"update time in RFC3339 when available"`
}

// ListDirectoriesItemResult reports one page while preserving input order.
type ListDirectoriesItemResult struct {
	Index      int                 `json:"index" jsonschema:"zero-based input request index"`
	DirID      string              `json:"dir_id" jsonschema:"normalized directory ID"`
	Offset     int64               `json:"offset" jsonschema:"requested page offset"`
	Limit      int64               `json:"limit" jsonschema:"effective page size"`
	Returned   int                 `json:"returned" jsonschema:"number of returned entries"`
	NextOffset *int64              `json:"next_offset,omitempty" jsonschema:"candidate continuation offset when this page filled its limit"`
	Success    bool                `json:"success" jsonschema:"whether this directory page was listed successfully"`
	Error      string              `json:"error,omitempty" jsonschema:"sanitized item error when listing failed"`
	Entries    []MCPDirectoryEntry `json:"entries,omitempty" jsonschema:"directory entries in server order"`
}

// ListDirectoriesResult summarizes a bounded read-only listing batch.
type ListDirectoriesResult struct {
	Requested int                         `json:"requested" jsonschema:"number of requested directory pages"`
	Succeeded int                         `json:"succeeded" jsonschema:"number of successful directory pages"`
	Failed    int                         `json:"failed" jsonschema:"number of failed directory pages"`
	Items     []ListDirectoriesItemResult `json:"items" jsonschema:"per-directory results in input order"`
}

type mcpPreparedDirectoryList struct {
	dirID  string
	offset int64
	limit  int64
}

func prepareMCPDirectoryBatch(args ListDirectoriesArgs) ([]mcpPreparedDirectoryList, error) {
	if len(args.Directories) == 0 {
		return nil, fmt.Errorf("at least one directory request is required")
	}
	if len(args.Directories) > maxMCPDirectoryBatchItems {
		return nil, fmt.Errorf("received %d directory requests; maximum is %d", len(args.Directories), maxMCPDirectoryBatchItems)
	}

	prepared := make([]mcpPreparedDirectoryList, len(args.Directories))
	seen := make(map[string]int, len(args.Directories))
	var entryBudget int64
	for i, item := range args.Directories {
		dirID := strings.TrimSpace(item.DirID)
		if dirID == "" {
			dirID = "0"
		}
		offset, limit, err := validateDirectoryListPagination(item.Offset, item.Limit)
		if err != nil {
			return nil, fmt.Errorf("directory request %d pagination: %w", i, err)
		}
		if limit == 0 {
			limit = defaultMCPDirectoryBatchLimit
		}
		entryBudget += limit
		if entryBudget > maxMCPDirectoryBatchEntries {
			return nil, fmt.Errorf("directory batch effective page budget %d exceeds maximum %d", entryBudget, maxMCPDirectoryBatchEntries)
		}
		key := fmt.Sprintf("%s\x00%d\x00%d", dirID, offset, limit)
		if previous, ok := seen[key]; ok {
			return nil, fmt.Errorf("directory requests %d and %d are exact duplicates", previous, i)
		}
		seen[key] = i
		prepared[i] = mcpPreparedDirectoryList{dirID: dirID, offset: offset, limit: limit}
	}
	return prepared, nil
}

func mcpDirectoryEntryFromFile(file driver.File) MCPDirectoryEntry {
	entry := MCPDirectoryEntry{
		FileID: file.FileID, ParentID: file.ParentID, Name: file.Name, Size: file.Size,
		PickCode: file.PickCode, SHA1: file.Sha1, IsDirectory: file.IsDirectory, Star: file.Star,
	}
	if !file.CreateTime.IsZero() {
		entry.CreateTime = file.CreateTime.Format(time.RFC3339)
	}
	if !file.UpdateTime.IsZero() {
		entry.UpdateTime = file.UpdateTime.Format(time.RFC3339)
	}
	return entry
}

func listDirectoriesCallResult(response ListDirectoriesResult) (*mcp.CallToolResult, ListDirectoriesResult, error) {
	return mcpTypedJSONResult("list_directories", response, response, response.Failed > 0)
}

func (dt *DirTools) listDirectories(ctx context.Context, req *mcp.CallToolRequest, args ListDirectoriesArgs) (*mcp.CallToolResult, ListDirectoriesResult, error) {
	prepared, err := prepareMCPDirectoryBatch(args)
	if err != nil {
		return toolError(fmt.Sprintf("list_directories preflight failed: %v", err)), ListDirectoriesResult{}, nil
	}
	if dt.client == nil {
		return toolError("115 client is unavailable"), ListDirectoriesResult{}, nil
	}

	response := ListDirectoriesResult{Requested: len(prepared), Items: make([]ListDirectoriesItemResult, len(prepared))}
	for i, item := range prepared {
		entry := ListDirectoriesItemResult{Index: i, DirID: item.dirID, Offset: item.offset, Limit: item.limit}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				entry.Error = err.Error()
				response.Failed++
				response.Items[i] = entry
				continue
			}
		}
		files, err := dt.client.ListPage(item.dirID, item.offset, item.limit, driver.WithRecordOpenTime(false))
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}
		if files == nil {
			entry.Error = "directory listing returned no response"
			response.Failed++
			response.Items[i] = entry
			continue
		}
		entry.Entries = make([]MCPDirectoryEntry, len(*files))
		for j, file := range *files {
			entry.Entries[j] = mcpDirectoryEntryFromFile(file)
		}
		entry.Returned = len(entry.Entries)
		if int64(entry.Returned) == item.limit {
			next := item.offset + int64(entry.Returned)
			entry.NextOffset = &next
		}
		entry.Success = true
		response.Succeeded++
		response.Items[i] = entry
	}
	return listDirectoriesCallResult(response)
}
