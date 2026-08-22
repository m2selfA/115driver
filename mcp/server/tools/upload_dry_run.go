package tools

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPUploadPlanItem reports only safe destination metadata. Local source paths,
// external source URLs, digests, credentials, and transport details are omitted.
type MCPUploadPlanItem struct {
	Index    int    `json:"index" jsonschema:"zero-based input item index"`
	DirID    string `json:"dir_id" jsonschema:"validated 115 target directory ID"`
	FileName string `json:"file_name" jsonschema:"validated remote file name"`
	FileSize *int64 `json:"file_size,omitempty" jsonschema:"known local source size in bytes, including zero; omitted when unknown"`
}

// MCPUploadPlan is returned by upload dry-runs after static/read-only preflight.
type MCPUploadPlan struct {
	Operation string              `json:"operation" jsonschema:"planned upload operation"`
	DryRun    bool                `json:"dry_run" jsonschema:"always true for an upload preview"`
	Requested int                 `json:"requested" jsonschema:"number of planned uploads"`
	Items     []MCPUploadPlanItem `json:"items" jsonschema:"safe destination metadata in input order"`
}

func uploadPlanCallResult(plan MCPUploadPlan) (*mcp.CallToolResult, any, error) {
	encoded, err := json.Marshal(plan)
	if err != nil {
		return toolError(fmt.Sprintf("Failed to serialize upload dry-run: %v", err)), nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}}}, plan, nil
}

func localUploadPlan(operation string, items []mcpPreparedLocalUpload) MCPUploadPlan {
	plan := MCPUploadPlan{Operation: operation, DryRun: true, Requested: len(items), Items: make([]MCPUploadPlanItem, len(items))}
	for i, item := range items {
		fileSize := item.fileSize
		plan.Items[i] = MCPUploadPlanItem{Index: i, DirID: item.dirID, FileName: item.fileName, FileSize: &fileSize}
	}
	return plan
}

func urlUploadPlan(operation string, items []mcpPreparedURLUpload) MCPUploadPlan {
	plan := MCPUploadPlan{Operation: operation, DryRun: true, Requested: len(items), Items: make([]MCPUploadPlanItem, len(items))}
	for i, item := range items {
		plan.Items[i] = MCPUploadPlanItem{Index: i, DirID: item.dirID, FileName: item.fileName}
	}
	return plan
}
