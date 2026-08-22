package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// FileTools holds file-related MCP tools
type FileTools struct {
	client            *driver.Pan115Client
	localRoot         string
	downloadTimeout   time.Duration
	urlUploadMaxBytes int64
	downloadMaxBytes  int64
	allowDestructive  bool
	allowSensitive    bool
	downloadTransfer  *mcpDownloadTransferState
	uploadTransfer    *mcpUploadTransferState
	syncJournalStore  *syncjournalpkg.Store
}

type FileToolsOption func(*FileTools)

func WithLocalRoot(root string) FileToolsOption {
	return func(ft *FileTools) {
		ft.localRoot = root
	}
}

func WithSyncJournalStore(store *syncjournalpkg.Store) FileToolsOption {
	return func(ft *FileTools) {
		ft.syncJournalStore = store
	}
}

func WithDownloadTimeout(timeout time.Duration) FileToolsOption {
	return func(ft *FileTools) {
		ft.downloadTimeout = timeout
	}
}

func WithURLUploadMaxBytes(maxBytes int64) FileToolsOption {
	return func(ft *FileTools) {
		ft.urlUploadMaxBytes = maxBytes
	}
}

func WithDownloadMaxBytes(maxBytes int64) FileToolsOption {
	return func(ft *FileTools) {
		ft.downloadMaxBytes = maxBytes
	}
}

func WithDestructiveTools(allow bool) FileToolsOption {
	return func(ft *FileTools) {
		ft.allowDestructive = allow
	}
}

// WithSensitiveTools controls tools that intentionally return short-lived credentials or signed URLs.
func WithSensitiveTools(allow bool) FileToolsOption {
	return func(ft *FileTools) {
		ft.allowSensitive = allow
	}
}

// NewFileTools creates a new FileTools instance
func NewFileTools(client *driver.Pan115Client, opts ...FileToolsOption) *FileTools {
	ft := &FileTools{
		client:            client,
		downloadTimeout:   defaultMCPDownloadTimeout,
		urlUploadMaxBytes: defaultMCPURLUploadMaxBytes,
		downloadMaxBytes:  defaultMCPDownloadMaxBytes,
		downloadTransfer:  newMCPDownloadTransferState(),
		uploadTransfer:    newMCPUploadTransferState(),
	}
	for _, opt := range opts {
		opt(ft)
	}
	return ft
}

const (
	defaultMCPURLUploadMaxBytes int64 = 2 << 30 // 2 GiB
	defaultMCPDownloadMaxBytes  int64 = 0       // unlimited
	defaultMCPDownloadTimeout         = 2 * time.Hour
)

var (
	errUnexpectedHTTPStatus = errors.New("unexpected HTTP status")
	errResponseTooLarge     = errors.New("response too large")
	errInvalidSizeLimit     = errors.New("invalid size limit")
)

// MkdirArgs defines arguments for mkdir tool
type MkdirArgs struct {
	ParentID string `json:"parent_id" jsonschema:"parent directory ID; use 0 for root"`
	Name     string `json:"name" jsonschema:"name of the new directory"`
	DryRun   bool   `json:"dry_run,omitempty" jsonschema:"preview the operation after read-only preflight without creating the directory"`
}

// DeleteArgs defines arguments for delete tool
type DeleteArgs struct {
	FileIDs []string `json:"file_ids" jsonschema:"IDs of files or directories to delete"`
	DryRun  bool     `json:"dry_run,omitempty" jsonschema:"preview all resolved objects without moving anything to the recycle bin"`
}

// RenameArgs defines arguments for rename tool
type RenameArgs struct {
	FileID  string `json:"file_id" jsonschema:"ID of file or directory to rename"`
	NewName string `json:"new_name" jsonschema:"new name for the file or directory"`
	DryRun  bool   `json:"dry_run,omitempty" jsonschema:"preview the resolved object and target name without renaming it"`
}

// MoveArgs defines arguments for move tool
type MoveArgs struct {
	DirID   string   `json:"dir_id" jsonschema:"target directory ID"`
	FileIDs []string `json:"file_ids" jsonschema:"IDs of files or directories to move"`
	DryRun  bool     `json:"dry_run,omitempty" jsonschema:"preview target/source metadata without moving anything"`
}

// CopyArgs defines arguments for copy tool
type CopyArgs struct {
	DirID   string   `json:"dir_id" jsonschema:"target directory ID"`
	FileIDs []string `json:"file_ids" jsonschema:"IDs of files or directories to copy"`
	DryRun  bool     `json:"dry_run,omitempty" jsonschema:"preview target/source metadata without copying anything"`
}

// StatArgs defines arguments for stat tool
type StatArgs struct {
	FileID string `json:"file_id" jsonschema:"ID of file or directory to get info"`
}

// UploadFromURLArgs defines arguments for uploading from URL
type UploadFromURLArgs struct {
	URL      string `json:"url" jsonschema:"URL of the file to download and upload"`
	DirID    string `json:"dir_id" jsonschema:"target directory ID in 115 cloud for saving the file"`
	FileName string `json:"file_name,omitempty" jsonschema:"optional filename for the uploaded file, defaults to original filename"`
	DryRun   bool   `json:"dry_run,omitempty" jsonschema:"validate URL/name/115 target without fetching or uploading the source"`
}

// UploadFromLocalArgs defines arguments for uploading from local file
type UploadFromLocalArgs struct {
	LocalPath        string `json:"local_path" jsonschema:"absolute path to the local file to upload"`
	DirID            string `json:"dir_id" jsonschema:"target directory ID in 115 cloud"`
	FileName         string `json:"file_name,omitempty" jsonschema:"optional filename for the uploaded file, defaults to original filename"`
	DryRun           bool   `json:"dry_run,omitempty" jsonschema:"validate local source/name/115 target without uploading file data"`
	ExpectPlanID     string `json:"expect_plan_id,omitempty" jsonschema:"optional MCPPlan v1 plan_id from an upload-only plan_transfer call; execution fails before upload when current state differs"`
	MaxChecksumBytes int64  `json:"max_checksum_bytes,omitempty" jsonschema:"maximum local bytes hashed when expect_plan_id is used; default 4 GiB, maximum 64 GiB"`
}

// DownloadFileArgs defines one ordinary 115 download source/target item. It is
// intentionally execution-gate-free because plan_transfer and download_files
// reuse this exact item shape.
type DownloadFileArgs struct {
	PickCode  string `json:"pick_code" jsonschema:"pick code of the file to download"`
	LocalPath string `json:"local_path" jsonschema:"local path where the downloaded file will be saved"`
	UserAgent string `json:"user_agent,omitempty" jsonschema:"optional user agent for the download request, uses 115 browser UA if not specified"`
}

// DownloadSingleFileArgs extends one download item with an optional reviewed
// plan gate without changing the shared batch/planner item wire contract.
type DownloadSingleFileArgs struct {
	PickCode         string `json:"pick_code" jsonschema:"pick code of the file to download"`
	LocalPath        string `json:"local_path" jsonschema:"local path where the downloaded file will be saved"`
	UserAgent        string `json:"user_agent,omitempty" jsonschema:"optional user agent for the download request, uses 115 browser UA if not specified"`
	ExpectPlanID     string `json:"expect_plan_id,omitempty" jsonschema:"optional MCPPlan v1 plan_id from a download-only plan_transfer call; execution fails before download when current state differs"`
	MaxChecksumBytes int64  `json:"max_checksum_bytes,omitempty" jsonschema:"maximum existing local target bytes hashed when expect_plan_id is used; default 4 GiB, maximum 64 GiB"`
}

// DownloadShareFileArgs defines arguments for downloading a file from a share link.
type DownloadShareFileArgs struct {
	ShareCode   string `json:"share_code" jsonschema:"share code"`
	ReceiveCode string `json:"receive_code" jsonschema:"share receive code/password"`
	FileID      string `json:"file_id" jsonschema:"file ID inside the share"`
	LocalPath   string `json:"local_path" jsonschema:"local path where the downloaded file will be saved"`
	UserAgent   string `json:"user_agent,omitempty" jsonschema:"optional user agent for the share download request"`
}

// GetDownloadInfoArgs defines arguments for getting download information
type GetDownloadInfoArgs struct {
	PickCode  string `json:"pick_code" jsonschema:"pick code of the file to get info for"`
	UserAgent string `json:"user_agent,omitempty" jsonschema:"optional user agent for the download request, uses 115 browser UA if not specified"`
}

// GetDownloadInfoResult defines the result for getting download information
type GetDownloadInfoResult struct {
	URL      string `json:"url" jsonschema:"download URL"`
	FileName string `json:"file_name" jsonschema:"file name"`
	Size     int64  `json:"size" jsonschema:"file size in bytes"`
}

// DownloadFileResult defines the result for downloading a file
type DownloadFileResult struct {
	Message string `json:"message" jsonschema:"result message"`
}

// RegisterTools registers file-related tools with the MCP server
func (ft *FileTools) RegisterTools(server *mcp.Server) {
	if ft.allowDestructive {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "mkdir",
			Description: "Create a new directory after read-only parent preflight; dry_run previews without creating it",
			Annotations: mcpMutationToolAnnotations(false),
		}, ft.mkdirTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "mkdir_many",
			Description: "Create multiple directories only after full-batch parent/name preflight; supports dry_run and explicit continue_on_error",
			Annotations: mcpMutationToolAnnotations(false),
		}, ft.mkdirManyTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "delete",
			Description: "Delete files or directories after full source preflight; dry_run previews without moving anything to the recycle bin",
			Annotations: mcpDestructiveToolAnnotations(),
		}, ft.deleteTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "rename",
			Description: "Rename a file or directory after read-only source/name preflight; dry_run previews without renaming",
			Annotations: mcpDestructiveToolAnnotations(),
		}, ft.renameTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "rename_many",
			Description: "Rename multiple objects only after full-batch source/name preflight; supports dry_run and explicit continue_on_error",
			Annotations: mcpDestructiveToolAnnotations(),
		}, ft.renameManyTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "move",
			Description: "Move files or directories after full source/target/ancestry preflight; dry_run previews without moving",
			Annotations: mcpDestructiveToolAnnotations(),
		}, ft.moveTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "copy",
			Description: "Copy files or directories after full source/target/ancestry preflight; dry_run previews without copying",
			Annotations: mcpMutationToolAnnotations(false),
		}, ft.copyTyped)
	}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "stat",
		Description: "Get detailed information about a file or directory",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, ft.stat)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "stat_many",
		Description: "Get detailed metadata for multiple file or directory IDs in one bounded read-only batch",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, ft.statMany)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "validate_plan",
		Description: "Validate MCPPlan v1 content identity, canonical structure, dependency DAG, estimates, and safety classification without executing or checking external state",
		Annotations: mcpReadOnlyToolAnnotations(),
	}, ft.validatePlan)

	if ft.allowDestructive {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "upload_from_url",
			Description: "Upload a file to 115 cloud storage from a URL",
			Annotations: mcpMutationToolAnnotations(false),
		}, ft.uploadFromURLTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "upload_from_urls",
			Description: "Fetch and upload multiple external URLs after full-batch source and 115 target preflight",
			Annotations: mcpMutationToolAnnotations(false),
		}, ft.uploadFromURLs)

		if ft.localRoot != "" {
			mcp.AddTool(server, &mcp.Tool{
				Name:        "upload_from_local",
				Description: "Upload a local file to 115 cloud storage, optionally requiring an upload-only plan_transfer expect_plan_id before the data path starts",
				Annotations: mcpMutationToolAnnotations(false),
			}, ft.uploadFromLocalTyped)

			mcp.AddTool(server, &mcp.Tool{
				Name:        "upload_from_local_files",
				Description: "Upload multiple local files after full-batch local preflight; each file may target a different 115 directory",
				Annotations: mcpMutationToolAnnotations(false),
			}, ft.uploadFromLocalFiles)

			mcp.AddTool(server, &mcp.Tool{
				Name:        "execute_transfer_plan",
				Description: "Execute a reviewed plan_transfer batch only after rerunning full upload/download preflight and matching the required MCPPlan v1 expect_plan_id; mixed execution runs uploads before downloads and skips downloads after any upload failure",
				Annotations: mcpDestructiveToolAnnotations(),
			}, ft.executeTransferPlan)

			mcp.AddTool(server, &mcp.Tool{
				Name:        "execute_sync_plan",
				Description: "Execute or safely resume a reviewed plan_sync through the shared syncexec engine using persistent journal locking, whole-tree/postcondition resume gates, residual planning, and per-item snapshot/subtree validation",
				Annotations: mcpDestructiveToolAnnotations(),
			}, ft.executeSyncPlan)

			mcp.AddTool(server, &mcp.Tool{
				Name:        "execute_sync_journal_cleanup",
				Description: "Move an exactly reviewed plan_sync_journal_cleanup candidate set into shared Session Store trash after reacquiring GC/migration/journal/alias locks and matching expect_cleanup_id",
				Annotations: mcpDestructiveToolAnnotations(),
			}, ft.executeSyncJournalCleanup)

			mcp.AddTool(server, &mcp.Tool{
				Name:        "restore_sync_journal",
				Description: "Restore an exactly reviewed sync journal from shared Session Store trash only after matching its content-addressed restore_id under GC/migration/raw-plan/alias guards",
				Annotations: mcpDestructiveToolAnnotations(),
			}, ft.restoreSyncJournal)

			mcp.AddTool(server, &mcp.Tool{
				Name:        "reconcile_sync_recovery",
				Description: "Apply an explicitly reviewed diagnose_sync_recovery decision only after reacquiring the shared journal lock and matching the exact diagnosis_id; changes journal control state only and never mutates local or 115 content",
				Annotations: mcpDestructiveToolAnnotations(),
			}, ft.reconcileSyncRecovery)

			mcp.AddTool(server, &mcp.Tool{
				Name:        "reconcile_sync_journal_alias",
				Description: "Remove one exactly reviewed orphan sync-journal alias only after rerunning lifecycle diagnosis, matching its opaque repair_id, and re-proving current/trash absence under raw-journal and alias locks",
				Annotations: mcpDestructiveToolAnnotations(),
			}, ft.reconcileSyncJournalAlias)

			mcp.AddTool(server, &mcp.Tool{
				Name:        "execute_sync_journal_alias_repair",
				Description: "Remove an exactly reviewed bounded orphan-alias batch only after rebuilding the complete authenticated-account orphan set and revalidating it under all raw-plan then alias locks; stale/crash-changed state requires a fresh plan, ordinary failure rolls back under lock, and abrupt process death is crash-convergent rather than power-loss atomic",
				Annotations: mcpDestructiveToolAnnotations(),
			}, ft.executeSyncJournalAliasRepair)
		}
	}

	if ft.localRoot != "" {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "inspect_sync_journal",
			Description: "Inspect the safe state of a persistent sync execution journal by reviewed plan_id without exposing stored paths, object IDs, digests, postconditions, or raw errors",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.inspectSyncJournal)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "diagnose_sync_recovery",
			Description: "Classify stable evidence for reconciliation-gated sync actions, including interrupted destructive mutations and non-destructive mutation-done postconditions, without mutating journal/content state or exposing hidden identities/digests",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.diagnoseSyncRecovery)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "diagnose_sync_journal_aliases",
			Description: "Audit exact-account reviewed-plan alias lifecycle as live, orphan, soft-deleted-shadow, identity-mismatch, or invalid-target; foreign-account alias bindings fail the authenticated scan closed rather than being skipped",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.diagnoseSyncJournalAliases)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "plan_sync_journal_alias_repair",
			Description: "Preview a bounded exact-account orphan-only reviewed-plan alias repair batch and return a repair_set_id that binds the complete currently diagnosed orphan set; changed or crash-converged state always requires a fresh plan",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.planSyncJournalAliasRepair)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "list_sync_executions",
			Description: "List recent persistent sync executions for the current profile/account using reviewed MCP plan IDs and safe status/count metadata only",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.listSyncExecutions)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "list_sync_journal_trash",
			Description: "List restorable current-v2 sync journals in shared Session Store trash using reviewed MCP plan IDs and opaque content-addressed restore_id tokens only",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.listSyncJournalTrash)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "plan_sync_journal_cleanup",
			Description: "Preview old completed/failed sync journals eligible for retention GC and return a content-addressed cleanup_id; bulk migration markers fail closed",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.planSyncJournalCleanup)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "plan_sync",
			Description: "Build a bounded read-only local/remote sync plan using the same classifier as the CLI; returns MCPPlan v1 without exposing local absolute paths or content digests",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.planSync)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "revalidate_sync_plan",
			Description: "Rebuild a reviewed plan_sync request from current local/remote state and report whether its expect_plan_id still matches and is conflict-free; never returns a replacement plan on mismatch",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.revalidateSyncPlan)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "plan_transfer",
			Description: "Build a read-only MCPPlan v1 for local uploads and 115 downloads using the same batch preflight as the transfer tools without exposing local paths, pick codes, signed URLs, or headers",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.planTransfer)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "revalidate_transfer_plan",
			Description: "Rebuild a reviewed plan_transfer request from current local/remote state and report whether its expect_plan_id still matches; never returns a replacement plan on mismatch",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.revalidateTransferPlan)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "download_file",
			Description: "Download a file from 115 cloud storage to local path, optionally requiring a download-only plan_transfer expect_plan_id; may replace an existing regular target file",
			Annotations: mcpDestructiveToolAnnotations(),
		}, ft.downloadFileTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "download_files",
			Description: "Download multiple 115 files in one preflighted batch and use cross-file scheduling when transfer strategy is file; may replace existing regular target files",
			Annotations: mcpDestructiveToolAnnotations(),
		}, ft.downloadFiles)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "download_share_file",
			Description: "Download a file from a 115 share link to a local path without exposing signed URL or share credentials; may replace an existing regular target file",
			Annotations: mcpDestructiveToolAnnotations(),
		}, ft.downloadShareFileTyped)

		mcp.AddTool(server, &mcp.Tool{
			Name:        "download_share_files",
			Description: "Download multiple files from one 115 share in a preflighted batch without exposing signed URLs or share credentials; may replace existing regular target files",
			Annotations: mcpDestructiveToolAnnotations(),
		}, ft.downloadShareFiles)
	}

	if ft.allowSensitive {
		mcp.AddTool(server, &mcp.Tool{
			Name:        "get_download_info",
			Description: "Get file metadata plus a short-lived signed download URL; sensitive because the URL is returned in MCP content",
			Annotations: mcpReadOnlyToolAnnotations(),
		}, ft.getDownloadInfoTyped)
	}
}

func (ft *FileTools) mkdir(ctx context.Context, req *mcp.CallToolRequest, args MkdirArgs) (*mcp.CallToolResult, any, error) {
	parentID, err := normalizeMCPUploadDirID(args.ParentID)
	if err != nil {
		return toolError(fmt.Sprintf("Mkdir parent preflight failed: %v", err)), nil, nil
	}
	name, err := validateMCPRemoteObjectName(args.Name)
	if err != nil {
		return toolError(fmt.Sprintf("Mkdir name preflight failed: %v", err)), nil, nil
	}
	if err := preflightMCPRemoteTargetNames(ft.client, []mcpRemoteNameTarget{{Index: 0, ParentID: parentID, Name: name}}); err != nil {
		return toolError(fmt.Sprintf("Mkdir remote name preflight failed: %v", err)), nil, nil
	}
	if args.DryRun {
		return mutationBatchCallResult(MCPMutationBatchResult{
			Operation: "mkdir", DryRun: true, Requested: 1, Planned: 1,
			Items: []MCPMutationBatchItemResult{{Index: 0, ParentID: parentID, Name: name, IsDirectory: true, Status: "planned"}},
		})
	}

	dirID, err := ft.client.Mkdir(parentID, name)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to create directory: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	result := map[string]string{
		"directory_id": dirID,
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize result: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: string(resultJSON),
			},
		},
	}, dirID, nil
}

func (ft *FileTools) delete(ctx context.Context, req *mcp.CallToolRequest, args DeleteArgs) (*mcp.CallToolResult, any, error) {
	objects, normalizedIDs, err := preflightMCPMutationObjects(ft.client, args.FileIDs)
	if err != nil {
		return toolError(fmt.Sprintf("Delete source preflight failed: %v", err)), nil, nil
	}
	if args.DryRun {
		return mutationPlanCallResult(MCPMutationPlan{Operation: "delete", DryRun: true, Requested: len(objects), Items: objects})
	}

	err = ft.client.Delete(normalizedIDs...)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to delete files: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: "Files deleted successfully",
			},
		},
	}, nil, nil
}

func (ft *FileTools) rename(ctx context.Context, req *mcp.CallToolRequest, args RenameArgs) (*mcp.CallToolResult, any, error) {
	newName, err := validateMCPRemoteObjectName(args.NewName)
	if err != nil {
		return toolError(fmt.Sprintf("Rename name preflight failed: %v", err)), nil, nil
	}
	objects, normalizedIDs, err := preflightMCPMutationObjects(ft.client, []string{args.FileID})
	if err != nil {
		return toolError(fmt.Sprintf("Rename source preflight failed: %v", err)), nil, nil
	}
	object := objects[0]
	if err := preflightMCPRemoteTargetNames(ft.client, []mcpRemoteNameTarget{{Index: 0, ParentID: object.ParentID, Name: newName, AllowFileID: object.FileID}}); err != nil {
		return toolError(fmt.Sprintf("Rename remote name preflight failed: %v", err)), nil, nil
	}
	if args.DryRun {
		return mutationBatchCallResult(MCPMutationBatchResult{
			Operation: "rename", DryRun: true, Requested: 1, Planned: 1,
			Items: []MCPMutationBatchItemResult{{Index: 0, FileID: object.FileID, ParentID: object.ParentID, Name: object.Name, NewName: newName, IsDirectory: object.IsDirectory, Status: "planned"}},
		})
	}

	err = ft.client.Rename(normalizedIDs[0], newName)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to rename file: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: "File renamed successfully",
			},
		},
	}, nil, nil
}

func (ft *FileTools) move(ctx context.Context, req *mcp.CallToolRequest, args MoveArgs) (*mcp.CallToolResult, any, error) {
	target, err := loadMCPRemoteDirectory(ft.client, args.DirID)
	if err != nil {
		return toolError(fmt.Sprintf("Move target preflight failed: %v", err)), nil, nil
	}
	objects, normalizedIDs, err := preflightMCPMutationObjects(ft.client, args.FileIDs)
	if err != nil {
		return toolError(fmt.Sprintf("Move source preflight failed: %v", err)), nil, nil
	}
	if err := validateMCPMoveCopyTargetAncestry(ft.client, target, objects); err != nil {
		return toolError(fmt.Sprintf("Move ancestry preflight failed: %v", err)), nil, nil
	}
	if args.DryRun {
		return mutationPlanCallResult(MCPMutationPlan{Operation: "move", DryRun: true, Requested: len(objects), TargetDirID: target.FileID, Items: objects})
	}

	err = ft.client.Move(target.FileID, normalizedIDs...)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to move files: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: "Files moved successfully",
			},
		},
	}, nil, nil
}

func (ft *FileTools) copy(ctx context.Context, req *mcp.CallToolRequest, args CopyArgs) (*mcp.CallToolResult, any, error) {
	target, err := loadMCPRemoteDirectory(ft.client, args.DirID)
	if err != nil {
		return toolError(fmt.Sprintf("Copy target preflight failed: %v", err)), nil, nil
	}
	objects, normalizedIDs, err := preflightMCPMutationObjects(ft.client, args.FileIDs)
	if err != nil {
		return toolError(fmt.Sprintf("Copy source preflight failed: %v", err)), nil, nil
	}
	if err := validateMCPMoveCopyTargetAncestry(ft.client, target, objects); err != nil {
		return toolError(fmt.Sprintf("Copy ancestry preflight failed: %v", err)), nil, nil
	}
	if args.DryRun {
		return mutationPlanCallResult(MCPMutationPlan{Operation: "copy", DryRun: true, Requested: len(objects), TargetDirID: target.FileID, Items: objects})
	}

	err = ft.client.Copy(target.FileID, normalizedIDs...)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to copy files: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: "Files copied successfully",
			},
		},
	}, nil, nil
}

func (ft *FileTools) stat(ctx context.Context, req *mcp.CallToolRequest, args StatArgs) (*mcp.CallToolResult, MCPStatData, error) {
	info, err := ft.client.Stat(args.FileID)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to get file info: %v", err),
				},
			},
			IsError: true,
		}, MCPStatData{}, nil
	}
	data, err := mcpStatDataFromInfo(info)
	if err != nil {
		return toolError(fmt.Sprintf("Failed to normalize file info: %v", err)), MCPStatData{}, nil
	}

	result := map[string]interface{}{
		"name":         info.Name,
		"pick_code":    info.PickCode,
		"sha1":         info.Sha1,
		"is_directory": info.IsDirectory,
		"file_count":   info.FileCount,
		"dir_count":    info.DirCount,
		"create_time":  info.CreateTime,
		"update_time":  info.UpdateTime,
		"parents":      info.Parents,
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize result: %v", err),
				},
			},
			IsError: true,
		}, MCPStatData{}, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: string(resultJSON),
			},
		},
	}, *data, nil
}

func (ft *FileTools) uploadFromURL(ctx context.Context, req *mcp.CallToolRequest, args UploadFromURLArgs) (*mcp.CallToolResult, any, error) {
	prepared, err := prepareMCPURLUpload(args)
	if err != nil {
		return toolError(fmt.Sprintf("URL upload preflight failed: %v", err)), nil, nil
	}
	if err := ft.validateUploadTransferReadiness(); err != nil {
		return toolError(fmt.Sprintf("Invalid upload transfer configuration: %v", err)), nil, nil
	}
	if ft.client == nil {
		return toolError("115 client is unavailable"), nil, nil
	}
	if err := validateMCPUploadTargetDirectory(ft.client, prepared.dirID); err != nil {
		return toolError(fmt.Sprintf("URL upload target preflight failed: %v", err)), nil, nil
	}
	if args.DryRun {
		return uploadPlanCallResult(urlUploadPlan("upload_from_url", []mcpPreparedURLUpload{prepared}))
	}
	uploadResult, err := ft.uploadPreparedURL(ctx, prepared)
	if err != nil {
		return toolError(fmt.Sprintf("Failed to upload external URL: %v", err)), nil, nil
	}
	execution := MCPUploadExecutionResult{
		Message:       "File uploaded successfully from URL",
		FileName:      prepared.fileName,
		BytesUploaded: uploadResult.BytesUploaded,
		Rapid:         uploadResult.Rapid,
		Resumed:       uploadResult.Resumed,
		Verified:      uploadResult.Verified,
		Skipped:       uploadResult.Skipped,
	}
	resultJSON, err := json.Marshal(map[string]interface{}{
		"message":        "File uploaded successfully from URL",
		"file_name":      prepared.fileName,
		"bytes_uploaded": uploadResult.BytesUploaded,
		"rapid":          uploadResult.Rapid,
		"resumed":        uploadResult.Resumed,
		"verified":       uploadResult.Verified,
		"skipped":        uploadResult.Skipped,
	})
	if err != nil {
		return toolError(fmt.Sprintf("Failed to serialize URL upload result: %v", err)), nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}}}, execution, nil
}

func (ft *FileTools) uploadFromLocal(ctx context.Context, req *mcp.CallToolRequest, args UploadFromLocalArgs) (*mcp.CallToolResult, any, error) {
	expectedPlanID, err := normalizeMCPExpectedPlanID(args.ExpectPlanID)
	if err != nil {
		return toolError(err.Error()), nil, nil
	}
	if args.DryRun && expectedPlanID != "" {
		return toolError("expect_plan_id is only valid when executing upload_from_local"), nil, nil
	}
	if args.MaxChecksumBytes != 0 && expectedPlanID == "" {
		return toolError("max_checksum_bytes requires expect_plan_id"), nil, nil
	}
	prepared, err := prepareMCPLocalUpload(ft.localRoot, args)
	if err != nil {
		return toolError(fmt.Sprintf("Local upload preflight failed: %v", err)), nil, nil
	}
	defer prepared.file.Close()
	if err := ft.validateUploadTransferReadiness(); err != nil {
		return toolError(fmt.Sprintf("Invalid upload transfer configuration: %v", err)), nil, nil
	}
	if ft.client == nil {
		return toolError("115 client is unavailable"), nil, nil
	}
	if err := validateMCPUploadTargetDirectory(ft.client, prepared.dirID); err != nil {
		return toolError(fmt.Sprintf("Local upload target preflight failed: %v", err)), nil, nil
	}
	if args.DryRun {
		return uploadPlanCallResult(localUploadPlan("upload_from_local", []mcpPreparedLocalUpload{prepared}))
	}
	if expectedPlanID != "" {
		planArgs := PlanTransferArgs{
			Uploads:          []UploadFromLocalFileItem{{LocalPath: args.LocalPath, DirID: args.DirID, FileName: args.FileName}},
			MaxChecksumBytes: args.MaxChecksumBytes,
		}
		if _, planErr := verifyMCPPreparedTransferPlan([]mcpPreparedLocalUpload{prepared}, nil, expectedPlanID, args.MaxChecksumBytes); planErr != nil {
			return toolError("upload plan gate failed: " + redactPlanTransferError(planErr, planArgs)), nil, nil
		}
	}

	// Local file data may use the P10 multi-interface OSS path.
	_, err = ft.uploadThroughTransfer(ctx, prepared.dirID, prepared.fileName, prepared.fileSize, prepared.file)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to upload file to 115: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	execution := MCPUploadExecutionResult{Message: "Local file uploaded successfully"}

	result := map[string]string{
		"message": "Local file uploaded successfully",
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize result: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: string(resultJSON),
			},
		},
	}, execution, nil
}

func (ft *FileTools) downloadFile(ctx context.Context, req *mcp.CallToolRequest, args DownloadSingleFileArgs) (*mcp.CallToolResult, any, error) {
	expectedPlanID, err := normalizeMCPExpectedPlanID(args.ExpectPlanID)
	if err != nil {
		return toolError(err.Error()), nil, nil
	}
	if args.MaxChecksumBytes != 0 && expectedPlanID == "" {
		return toolError("max_checksum_bytes requires expect_plan_id"), nil, nil
	}
	localPath, err := validateMCPDownloadLocalTarget(ft.localRoot, args.LocalPath)
	if err != nil {
		return toolError(fmt.Sprintf("Local download target denied: %v", err)), nil, nil
	}
	if ft.downloadTransfer == nil {
		ft.downloadTransfer = newMCPDownloadTransferState()
	}
	if err := ft.downloadTransfer.config.Validate(); err != nil {
		return toolError(fmt.Sprintf("Invalid transfer configuration: %v", err)), nil, nil
	}

	// The signed CDN URL and headers come only from the authenticated 115 API.
	downloadInfo, err := ft.client.DownloadWithUA(args.PickCode, args.UserAgent)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to get download info: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}
	if expectedPlanID != "" {
		prepared := []mcpDownloadBatchTransferItem{{info: downloadInfo, localPath: localPath, stableID: strings.TrimSpace(args.PickCode)}}
		planArgs := PlanTransferArgs{
			Downloads:        []DownloadFileArgs{{PickCode: args.PickCode, LocalPath: args.LocalPath, UserAgent: args.UserAgent}},
			MaxChecksumBytes: args.MaxChecksumBytes,
		}
		if _, planErr := verifyMCPPreparedTransferPlan(nil, prepared, expectedPlanID, args.MaxChecksumBytes); planErr != nil {
			return toolError("download plan gate failed: " + redactPlanTransferError(planErr, planArgs)), nil, nil
		}
	}

	if _, err := ft.downloadThroughTransfer(ctx, downloadInfo, localPath, args.PickCode, args.UserAgent); err != nil {
		return toolError(fmt.Sprintf("Failed to download file: %v", err)), nil, nil
	}

	result := DownloadFileResult{
		Message: "File downloaded successfully",
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize result: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: string(resultJSON),
			},
		},
	}, result, nil
}

func (ft *FileTools) downloadShareFile(ctx context.Context, req *mcp.CallToolRequest, args DownloadShareFileArgs) (*mcp.CallToolResult, any, error) {
	localPath, err := validateMCPDownloadLocalTarget(ft.localRoot, args.LocalPath)
	if err != nil {
		return toolError(fmt.Sprintf("Local download target denied: %v", err)), nil, nil
	}
	if ft.downloadTransfer == nil {
		ft.downloadTransfer = newMCPDownloadTransferState()
	}
	if err := ft.downloadTransfer.config.Validate(); err != nil {
		return toolError(fmt.Sprintf("Invalid transfer configuration: %v", err)), nil, nil
	}
	if ft.client == nil {
		return toolError("115 client is unavailable"), nil, nil
	}

	info, err := ft.client.DownloadByShareCodeRequestWithUA(args.UserAgent, args.ShareCode, args.ReceiveCode, args.FileID)
	if err != nil {
		return toolError(redactShareReceiveCode(fmt.Sprintf("Failed to get share download info: %v", err), args.ReceiveCode)), nil, nil
	}
	if _, err := ft.downloadShareThroughTransfer(ctx, info, localPath, args.ShareCode, args.ReceiveCode, args.FileID, args.UserAgent); err != nil {
		return toolError(redactShareReceiveCode(fmt.Sprintf("Failed to download shared file: %v", err), args.ReceiveCode)), nil, nil
	}

	result := DownloadFileResult{Message: "Shared file downloaded successfully"}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return toolError(fmt.Sprintf("Failed to serialize result: %v", err)), nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(resultJSON)}}}, result, nil
}

func (ft *FileTools) getDownloadInfo(ctx context.Context, req *mcp.CallToolRequest, args GetDownloadInfoArgs) (*mcp.CallToolResult, any, error) {
	// Get download info with the specified User-Agent
	downloadInfo, err := ft.client.DownloadWithUA(args.PickCode, args.UserAgent)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to get download info: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	result := GetDownloadInfoResult{
		URL:      downloadInfo.Url.Url,
		FileName: downloadInfo.FileName,
		Size:     int64(downloadInfo.FileSize),
	}

	resultJSON, err := json.Marshal(result)
	if err != nil {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{
					Text: fmt.Sprintf("Failed to serialize result: %v", err),
				},
			},
			IsError: true,
		}, nil, nil
	}

	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: string(resultJSON),
			},
		},
	}, result, nil
}

func validateUploadURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, errors.New("malformed URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("unsupported scheme %q", parsed.Scheme)
	}
	host := parsed.Hostname()
	if host == "" {
		return nil, errors.New("missing host")
	}
	if isUnsafeHost(host) {
		return nil, fmt.Errorf("host %q is not allowed", host)
	}
	return parsed, nil
}

func sanitizeMCPExternalURLString(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" {
		return "[REDACTED_URL]"
	}
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
}

func sanitizeMCPExternalURLError(err error) error {
	if err == nil {
		return nil
	}
	urlErr, ok := err.(*url.Error)
	if !ok {
		return err
	}
	clone := *urlErr
	clone.URL = sanitizeMCPExternalURLString(urlErr.URL)
	clone.Err = sanitizeMCPExternalURLError(urlErr.Err)
	return &clone
}

func isUnsafeHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isUnsafeIP(ip)
	}
	return false
}

func validateResolvedIPs(host string, ips []net.IP) error {
	if len(ips) == 0 {
		return fmt.Errorf("host %q did not resolve to any IPs", host)
	}
	for _, ip := range ips {
		if ip == nil || isUnsafeIP(ip) {
			return fmt.Errorf("host %q resolved to unsafe IP %s", host, ip)
		}
	}
	return nil
}

func dialResolvedIPs(ctx context.Context, network, host, port string, ips []net.IP, dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	if err := validateResolvedIPs(host, ips); err != nil {
		return nil, err
	}

	var errs []error
	for _, ip := range ips {
		address := net.JoinHostPort(ip.String(), port)
		conn, err := dial(ctx, network, address)
		if err == nil {
			return conn, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", address, err))
	}
	return nil, fmt.Errorf("dial %q: %w", host, errors.Join(errs...))
}

func isUnsafeIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

// NormalizeLocalRoot validates an optional MCP local filesystem boundary and
// returns its canonical absolute path. Empty disables local file tools.
func NormalizeLocalRoot(root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve local root path: %w", err)
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve local root: %w", err)
	}
	info, err := os.Stat(realRoot)
	if err != nil {
		return "", fmt.Errorf("stat local root: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("local root is not a directory: %s", realRoot)
	}
	return filepath.Clean(realRoot), nil
}

func validateMCPDownloadLocalTarget(root, target string) (string, error) {
	localPath, err := validateLocalPath(root, target, false)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(localPath)
	if err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("existing download target must be a regular file")
		}
		return localPath, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat download target: %w", err)
	}
	return localPath, nil
}

func validateLocalPath(root, target string, mustExist bool) (string, error) {
	if root == "" {
		return "", errors.New("local root is not configured")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", fmt.Errorf("resolve local root: %w", err)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}

	pathToCheck := absTarget
	if !mustExist && !pathExists(absTarget) {
		pathToCheck, err = nearestExistingPath(filepath.Dir(absTarget))
		if err != nil {
			return "", err
		}
	}
	realCheck, err := filepath.EvalSymlinks(pathToCheck)
	if err != nil {
		if mustExist || !os.IsNotExist(err) {
			return "", err
		}
		realCheck = pathToCheck
	}

	rel, err := filepath.Rel(realRoot, realCheck)
	if err != nil {
		return "", err
	}
	if rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", errors.New("path escapes local root")
	}
	return absTarget, nil
}

func nearestExistingPath(path string) (string, error) {
	for {
		if pathExists(path) {
			return path, nil
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", fmt.Errorf("no existing parent for %q", path)
		}
		path = parent
	}
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func copyHTTPResponse(dst io.Writer, resp *http.Response, maxBytes int64) error {
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: %d", errUnexpectedHTTPStatus, resp.StatusCode)
	}
	if maxBytes < 0 {
		return fmt.Errorf("%w: %d", errInvalidSizeLimit, maxBytes)
	}
	if maxBytes == 0 {
		_, err := io.Copy(dst, resp.Body)
		return err
	}
	limited := &io.LimitedReader{R: resp.Body, N: maxBytes + 1}
	written, err := io.Copy(dst, limited)
	if err != nil {
		return err
	}
	if written > maxBytes || limited.N == 0 {
		return fmt.Errorf("%w: limit is %d bytes", errResponseTooLarge, maxBytes)
	}
	return nil
}

// newMCPURLUploadHTTPClient is only for untrusted upload_from_url sources. It
// validates redirects and resolved IPs to preserve the MCP SSRF boundary.
func newMCPURLUploadHTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if _, err := validateUploadURL(req.URL.String()); err != nil {
				return err
			}
			return nil
		},
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				host, port, err := net.SplitHostPort(address)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
				if err != nil {
					return nil, err
				}
				return dialResolvedIPs(ctx, network, host, port, ips, dialer.DialContext)
			},
		},
	}
}

func toolError(text string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: text},
		},
		IsError: true,
	}
}
