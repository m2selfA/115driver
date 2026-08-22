package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// OfflineTools holds offline-related MCP tools
type OfflineTools struct {
	client           *driver.Pan115Client
	defaultSaveDir   string
	allowDestructive bool
}

type OfflineToolsOption func(*OfflineTools)

// WithOfflineDefaultSaveDir sets the default offline download directory name.
func WithOfflineDefaultSaveDir(dir string) OfflineToolsOption {
	return func(ot *OfflineTools) {
		ot.defaultSaveDir = dir
	}
}

// WithOfflineDestructiveTools controls whether destructive offline tools are registered.
func WithOfflineDestructiveTools(allow bool) OfflineToolsOption {
	return func(ot *OfflineTools) {
		ot.allowDestructive = allow
	}
}

// NewOfflineTools creates a new OfflineTools instance
func NewOfflineTools(client *driver.Pan115Client, opts ...OfflineToolsOption) *OfflineTools {
	ot := &OfflineTools{
		client: client,
	}
	for _, opt := range opts {
		opt(ot)
	}
	return ot
}

// ListOfflineTaskArgs defines arguments for listing offline tasks
type ListOfflineTaskArgs struct {
	Page int64 `json:"page" jsonschema:"page number for pagination, default is 1"`
}

// MCPOfflineTask deliberately omits the original task URL. Offline HTTP URLs
// may contain bearer/query credentials and must not be echoed to MCP clients.
type MCPOfflineTask struct {
	InfoHash     string  `json:"info_hash"`
	Name         string  `json:"name"`
	Size         int64   `json:"size"`
	AddTime      int64   `json:"add_time"`
	Peers        int64   `json:"peers"`
	RateDownload float64 `json:"rate_download"`
	Status       int     `json:"status"`
	StatusText   string  `json:"status_text"`
	Percent      float64 `json:"percent"`
	UpdateTime   int64   `json:"update_time"`
	LeftTime     int64   `json:"left_time"`
	FileID       string  `json:"file_id"`
	DeleteFileID string  `json:"delete_file_id"`
	DirID        string  `json:"dir_id"`
	Move         int     `json:"move"`
}

// MCPOfflineTaskListOutput is the safe typed and textual list response.
type MCPOfflineTaskListOutput struct {
	Total     int64            `json:"total"`
	Count     int64            `json:"count"`
	PageRow   int64            `json:"page_row"`
	PageCount int64            `json:"page_count"`
	Page      int64            `json:"page"`
	Quota     int64            `json:"quota"`
	Tasks     []MCPOfflineTask `json:"tasks"`
}

// AddOfflineTaskURIsArgs defines arguments for adding offline tasks
type AddOfflineTaskURIsArgs struct {
	URIs      []string `json:"uris" jsonschema:"download URIs, supports http, ed2k, magnet"`
	SaveDirID string   `json:"save_dir_id,omitempty" jsonschema:"directory ID to save downloaded files, leave empty to use config default"`
	DryRun    bool     `json:"dry_run,omitempty" jsonschema:"validate all URIs and the save directory without creating offline tasks"`
}

// DeleteOfflineTasksArgs defines arguments for deleting offline tasks
type DeleteOfflineTasksArgs struct {
	Hashes      []string `json:"hashes" jsonschema:"task hashes to delete"`
	DeleteFiles bool     `json:"delete_files" jsonschema:"whether to delete associated files, default is false"`
	DryRun      bool     `json:"dry_run,omitempty" jsonschema:"validate all task hashes without deleting tasks or files"`
}

// ClearOfflineTasksArgs defines arguments for clearing offline tasks.
// Scope is preferred; ClearFlag remains for compatibility with older MCP callers.
type ClearOfflineTasksArgs struct {
	Scope     string `json:"scope,omitempty" jsonschema:"task scope to clear: completed (default), failed, active, or all"`
	ClearFlag *int64 `json:"clear_flag,omitempty" jsonschema:"legacy numeric task-only clear mode: 0 completed, 1 all, 2 failed, 3 active"`
	DryRun    bool   `json:"dry_run,omitempty" jsonschema:"resolve the scope without clearing any offline tasks"`
}

type MCPOfflineMutationPlan struct {
	Operation   string `json:"operation"`
	DryRun      bool   `json:"dry_run"`
	Requested   int    `json:"requested,omitempty"`
	SaveDirID   string `json:"save_dir_id,omitempty"`
	DeleteFiles bool   `json:"delete_files,omitempty"`
	Scope       string `json:"scope,omitempty"`
	ClearFlag   *int64 `json:"clear_flag,omitempty"`
}

func normalizeOfflinePage(page int64) (int64, error) {
	if page < 0 {
		return 0, fmt.Errorf("page must not be negative")
	}
	if page == 0 {
		return 1, nil
	}
	return page, nil
}

func normalizeMCPOfflineURIs(rawURIs []string) ([]string, error) {
	uris, err := normalizeMCPUniqueStrings(rawURIs, "offline URI", maxMCPMutationBatchItems)
	if err != nil {
		return nil, err
	}
	for i, raw := range uris {
		colon := strings.IndexByte(raw, ':')
		if colon <= 0 {
			return nil, fmt.Errorf("offline URI at index %d has no supported scheme", i)
		}
		scheme := strings.ToLower(raw[:colon])
		switch scheme {
		case "http", "https":
			parsed, err := url.Parse(raw)
			if err != nil || parsed.Host == "" {
				return nil, fmt.Errorf("offline URI at index %d has invalid %s URL syntax", i, scheme)
			}
		case "magnet":
			if !strings.HasPrefix(strings.ToLower(raw), "magnet:?") || len(raw) <= len("magnet:?") {
				return nil, fmt.Errorf("offline URI at index %d has invalid magnet syntax", i)
			}
		case "ed2k":
			if !strings.HasPrefix(strings.ToLower(raw), "ed2k://") || len(raw) <= len("ed2k://") {
				return nil, fmt.Errorf("offline URI at index %d has invalid ed2k syntax", i)
			}
		default:
			return nil, fmt.Errorf("offline URI at index %d uses unsupported scheme %q", i, scheme)
		}
	}
	return uris, nil
}

func mcpOfflineClearScopeForFlag(clearFlag int64) (string, error) {
	switch clearFlag {
	case 0:
		return "completed", nil
	case 1:
		return "all", nil
	case 2:
		return "failed", nil
	case 3:
		return "active", nil
	default:
		return "", fmt.Errorf("clear_flag must be 0 (completed), 1 (all), 2 (failed), or 3 (active)")
	}
}

func resolveMCPOfflineClearScope(scope string, clearFlag *int64) (string, int64, error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if scope == "" {
		if clearFlag == nil {
			return "completed", 0, nil
		}
		legacyScope, err := mcpOfflineClearScopeForFlag(*clearFlag)
		if err != nil {
			return "", 0, err
		}
		return legacyScope, *clearFlag, nil
	}

	var resolvedFlag int64
	switch scope {
	case "completed":
		resolvedFlag = 0
	case "all":
		resolvedFlag = 1
	case "failed":
		resolvedFlag = 2
	case "active":
		resolvedFlag = 3
	default:
		return "", 0, fmt.Errorf("scope must be completed, failed, active, or all")
	}
	if clearFlag != nil {
		legacyScope, err := mcpOfflineClearScopeForFlag(*clearFlag)
		if err != nil {
			return "", 0, err
		}
		if *clearFlag != resolvedFlag {
			return "", 0, fmt.Errorf("scope %q conflicts with clear_flag %d (%s)", scope, *clearFlag, legacyScope)
		}
	}
	return scope, resolvedFlag, nil
}

func (ot *OfflineTools) resolveOfflineSaveDirID(rawSaveDirID string) (string, error) {
	if ot.client == nil {
		return "", fmt.Errorf("115 client is unavailable")
	}
	saveDirID := strings.TrimSpace(rawSaveDirID)
	if saveDirID == "" && ot.defaultSaveDir != "" {
		resp, err := ot.client.DirName2CID(ot.defaultSaveDir)
		if err != nil {
			return "", fmt.Errorf("resolve default offline save directory %q: %w", ot.defaultSaveDir, err)
		}
		if resp == nil || string(resp.CategoryID) == "0" {
			return "", fmt.Errorf("default save directory not found (from config default_offline_save_dir): %s", ot.defaultSaveDir)
		}
		saveDirID = string(resp.CategoryID)
	}
	if saveDirID == "" {
		saveDirID = "0"
	}
	dir, err := loadMCPRemoteDirectory(ot.client, saveDirID)
	if err != nil {
		return "", fmt.Errorf("validate offline save directory: %w", err)
	}
	return dir.FileID, nil
}

// RegisterTools registers offline-related tools with the MCP server
func (ot *OfflineTools) RegisterTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "listOfflineTasks",
		Description: "List offline download tasks",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, ot.listOfflineTasks)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_offline_pages",
		Description: "List multiple offline-task pages in one bounded read-only batch without reflecting source URLs",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, ot.listOfflinePages)

	if ot.allowDestructive {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "addOfflineTaskURIs",
			Description: "Add offline tasks after full URI/save-directory preflight; dry_run validates without submitting tasks",
			Annotations: mcpMutationToolAnnotations(false),
		}, ot.addOfflineTaskURIsTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "deleteOfflineTasks",
			Description: "Delete offline tasks after full hash preflight; dry_run validates without deleting tasks or files",
			Annotations: mcpDestructiveToolAnnotations(),
		}, ot.deleteOfflineTasksTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "clearOfflineTasks",
			Description: "Clear offline task records by completed, failed, active, or all scope; dry_run resolves scope without clearing",
			Annotations: mcpDestructiveToolAnnotations(),
		}, ot.clearOfflineTasksTyped)
	}
}

func (ot *OfflineTools) listOfflineTasks(ctx context.Context, req *mcp.CallToolRequest, args ListOfflineTaskArgs) (*mcp.CallToolResult, MCPOfflineTaskListOutput, error) {
	page, pageErr := normalizeOfflinePage(args.Page)
	if pageErr != nil {
		return toolError(fmt.Sprintf("Invalid offline page: %v", pageErr)), MCPOfflineTaskListOutput{}, nil
	}

	result, err := ot.client.ListOfflineTask(page)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to list offline tasks: %v", err),
				},
			},
			IsError: true,
		}, MCPOfflineTaskListOutput{}, nil
	}

	// Deliberately do not expose task.Url: it can contain signed/query credentials.
	response := mcpOfflineTaskListOutput(result)

	responseJSON, err := json.Marshal(response)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize offline tasks: %v", err),
				},
			},
			IsError: true,
		}, MCPOfflineTaskListOutput{}, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: string(responseJSON),
			},
		},
	}, response, nil
}

func (ot *OfflineTools) addOfflineTaskURIs(ctx context.Context, req *mcp.CallToolRequest, args AddOfflineTaskURIsArgs) (*mcp.CallToolResult, any, error) {
	uris, err := normalizeMCPOfflineURIs(args.URIs)
	if err != nil {
		return toolError(fmt.Sprintf("Offline add preflight failed: %v", err)), nil, nil
	}
	saveDirID, err := ot.resolveOfflineSaveDirID(args.SaveDirID)
	if err != nil {
		return toolError(fmt.Sprintf("Offline add save-directory preflight failed: %v", err)), nil, nil
	}
	if args.DryRun {
		return mcpJSONResult(MCPOfflineMutationPlan{Operation: "add_offline", DryRun: true, Requested: len(uris), SaveDirID: saveDirID}, "Failed to serialize offline add dry-run")
	}

	hashes, err := ot.client.AddOfflineTaskURIs(uris, saveDirID)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to add offline tasks: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	response := map[string]interface{}{
		"hashes": hashes,
	}

	responseJSON, err := json.Marshal(response)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize response: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: string(responseJSON),
			},
		},
	}, hashes, nil
}

func (ot *OfflineTools) deleteOfflineTasks(ctx context.Context, req *mcp.CallToolRequest, args DeleteOfflineTasksArgs) (*mcp.CallToolResult, any, error) {
	hashes, err := normalizeMCPUniqueStrings(args.Hashes, "offline task hash", maxMCPMutationBatchItems)
	if err != nil {
		return toolError(fmt.Sprintf("Offline delete preflight failed: %v", err)), nil, nil
	}
	if args.DryRun {
		return mcpJSONResult(MCPOfflineMutationPlan{Operation: "delete_offline", DryRun: true, Requested: len(hashes), DeleteFiles: args.DeleteFiles}, "Failed to serialize offline delete dry-run")
	}
	if ot.client == nil {
		return toolError("115 client is unavailable"), nil, nil
	}

	err = ot.client.DeleteOfflineTasks(hashes, args.DeleteFiles)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to delete offline tasks: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: "Successfully deleted offline tasks",
			},
		},
	}, nil, nil
}

func (ot *OfflineTools) clearOfflineTasks(ctx context.Context, req *mcp.CallToolRequest, args ClearOfflineTasksArgs) (*mcp.CallToolResult, any, error) {
	scope, clearFlag, resolveErr := resolveMCPOfflineClearScope(args.Scope, args.ClearFlag)
	if resolveErr != nil {
		return toolError(fmt.Sprintf("Invalid offline clear request: %v", resolveErr)), nil, nil
	}
	if args.DryRun {
		flag := clearFlag
		return mcpJSONResult(MCPOfflineMutationPlan{Operation: "clear_offline", DryRun: true, Scope: scope, ClearFlag: &flag}, "Failed to serialize offline clear dry-run")
	}
	if ot.client == nil {
		return toolError("115 client is unavailable"), nil, nil
	}
	err := ot.client.ClearOfflineTasks(clearFlag)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to clear offline tasks: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: fmt.Sprintf("Successfully cleared offline tasks with scope %s", scope),
			},
		},
	}, nil, nil
}
