package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// StatManyArgs defines a bounded read-only metadata batch.
type StatManyArgs struct {
	FileIDs []string `json:"file_ids" jsonschema:"IDs of files or directories to inspect; duplicates are rejected"`
}

// MCPStatData mirrors the existing stat JSON shape for one object.
type MCPStatData struct {
	Name        string            `json:"name"`
	PickCode    string            `json:"pick_code"`
	SHA1        string            `json:"sha1"`
	IsDirectory bool              `json:"is_directory"`
	FileCount   int               `json:"file_count"`
	DirCount    int               `json:"dir_count"`
	CreateTime  time.Time         `json:"create_time"`
	UpdateTime  time.Time         `json:"update_time"`
	Parents     []*driver.DirInfo `json:"parents"`
}

// StatManyItemResult reports one metadata lookup while preserving input order.
type StatManyItemResult struct {
	Index   int          `json:"index"`
	FileID  string       `json:"file_id"`
	Success bool         `json:"success"`
	Error   string       `json:"error,omitempty"`
	Data    *MCPStatData `json:"data,omitempty"`
}

// StatManyResult summarizes a read-only metadata batch.
type StatManyResult struct {
	Requested int                  `json:"requested"`
	Succeeded int                  `json:"succeeded"`
	Failed    int                  `json:"failed"`
	Items     []StatManyItemResult `json:"items"`
}

func mcpStatDataFromInfo(info *driver.FileStatInfo) (*MCPStatData, error) {
	if info == nil {
		return nil, fmt.Errorf("stat returned no metadata")
	}
	parents := append([]*driver.DirInfo(nil), info.Parents...)
	return &MCPStatData{
		Name:        info.Name,
		PickCode:    info.PickCode,
		SHA1:        info.Sha1,
		IsDirectory: info.IsDirectory,
		FileCount:   info.FileCount,
		DirCount:    info.DirCount,
		CreateTime:  info.CreateTime,
		UpdateTime:  info.UpdateTime,
		Parents:     parents,
	}, nil
}

func statManyCallResult(response StatManyResult) (*mcp.CallToolResult, StatManyResult, error) {
	return mcpTypedJSONResult("stat_many", response, response, response.Failed > 0)
}

func (ft *FileTools) statMany(ctx context.Context, req *mcp.CallToolRequest, args StatManyArgs) (*mcp.CallToolResult, StatManyResult, error) {
	fileIDs, err := normalizeMCPUniqueStrings(args.FileIDs, "file ID", maxMCPFileBatchItems)
	if err != nil {
		return toolError(fmt.Sprintf("stat_many preflight failed: %v", err)), StatManyResult{}, nil
	}
	if ft.client == nil {
		return toolError("115 client is unavailable"), StatManyResult{}, nil
	}

	response := StatManyResult{Requested: len(fileIDs), Items: make([]StatManyItemResult, len(fileIDs))}
	for i, fileID := range fileIDs {
		entry := StatManyItemResult{Index: i, FileID: fileID}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				entry.Error = err.Error()
				response.Failed++
				response.Items[i] = entry
				continue
			}
		}
		info, err := ft.client.Stat(fileID)
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}
		data, err := mcpStatDataFromInfo(info)
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
			response.Items[i] = entry
			continue
		}
		entry.Success = true
		entry.Data = data
		response.Succeeded++
		response.Items[i] = entry
	}
	return statManyCallResult(response)
}
