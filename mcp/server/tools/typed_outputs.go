package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPMutationExecutionResult is the safe execution summary for single remote mutations.
type MCPMutationExecutionResult struct {
	Operation   string `json:"operation" jsonschema:"completed mutation operation"`
	Success     bool   `json:"success" jsonschema:"whether the mutation completed successfully"`
	Requested   int    `json:"requested,omitempty" jsonschema:"number of requested source objects"`
	TargetDirID string `json:"target_dir_id,omitempty" jsonschema:"target directory ID for move/copy"`
	DirectoryID string `json:"directory_id,omitempty" jsonschema:"created directory ID for mkdir"`
}

// MCPMutationOutput gives single mutation tools one stable structured-output shape
// while their legacy TextContent remains unchanged.
type MCPMutationOutput struct {
	Mode   string                      `json:"mode" jsonschema:"dry_run or execution"`
	Plan   *MCPMutationPlan            `json:"plan,omitempty" jsonschema:"read-only mutation plan when mode is dry_run"`
	Batch  *MCPMutationBatchResult     `json:"batch,omitempty" jsonschema:"read-only single-item batch preview when applicable"`
	Result *MCPMutationExecutionResult `json:"result,omitempty" jsonschema:"execution summary when mode is execution"`
}

// MCPUploadExecutionResult intentionally contains no local path, source URL,
// digest, network path, OSS endpoint, cookie, or header data.
type MCPUploadExecutionResult struct {
	Message       string `json:"message"`
	FileName      string `json:"file_name,omitempty"`
	BytesUploaded int64  `json:"bytes_uploaded,omitempty"`
	Rapid         bool   `json:"rapid,omitempty"`
	Resumed       bool   `json:"resumed,omitempty"`
	Verified      bool   `json:"verified,omitempty"`
	Skipped       bool   `json:"skipped,omitempty"`
}

// MCPUploadOutput is shared by the two single-file upload tools.
type MCPUploadOutput struct {
	Mode   string                    `json:"mode" jsonschema:"dry_run or execution"`
	Plan   *MCPUploadPlan            `json:"plan,omitempty" jsonschema:"safe destination-only plan when mode is dry_run"`
	Result *MCPUploadExecutionResult `json:"result,omitempty" jsonschema:"safe execution summary when mode is execution"`
}

// MCPOfflineMutationExecutionResult never includes source URIs.
type MCPOfflineMutationExecutionResult struct {
	Operation   string   `json:"operation"`
	Success     bool     `json:"success"`
	Requested   int      `json:"requested,omitempty"`
	Hashes      []string `json:"hashes,omitempty"`
	Scope       string   `json:"scope,omitempty"`
	DeleteFiles bool     `json:"delete_files,omitempty"`
}

type MCPOfflineMutationOutput struct {
	Mode   string                             `json:"mode" jsonschema:"dry_run or execution"`
	Plan   *MCPOfflineMutationPlan            `json:"plan,omitempty"`
	Result *MCPOfflineMutationExecutionResult `json:"result,omitempty"`
}

// MCPRecycleMutationOutput intentionally never contains the recycle password.
type MCPRecycleMutationExecutionResult struct {
	Operation string `json:"operation"`
	Success   bool   `json:"success"`
	Requested int    `json:"requested"`
}

type MCPRecycleMutationOutput struct {
	Mode   string                             `json:"mode" jsonschema:"dry_run or execution"`
	Plan   *MCPRecycleMutationPlan            `json:"plan,omitempty"`
	Result *MCPRecycleMutationExecutionResult `json:"result,omitempty"`
}

func typedAdapterFailure(label string, err error) *mcp.CallToolResult {
	return toolError(fmt.Sprintf("%s structured-output adapter failed: %v", label, err))
}

func (ft *FileTools) mkdirTyped(ctx context.Context, req *mcp.CallToolRequest, args MkdirArgs) (*mcp.CallToolResult, MCPMutationOutput, error) {
	result, value, err := ft.mkdir(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPMutationOutput{}, err
	}
	if args.DryRun {
		batch, ok := value.(MCPMutationBatchResult)
		if !ok {
			return typedAdapterFailure("mkdir", fmt.Errorf("unexpected dry-run value %T", value)), MCPMutationOutput{}, nil
		}
		return result, MCPMutationOutput{Mode: "dry_run", Batch: &batch}, nil
	}
	directoryID, ok := value.(string)
	if !ok {
		return typedAdapterFailure("mkdir", fmt.Errorf("unexpected execution value %T", value)), MCPMutationOutput{}, nil
	}
	return result, MCPMutationOutput{Mode: "execution", Result: &MCPMutationExecutionResult{Operation: "mkdir", Success: true, Requested: 1, DirectoryID: directoryID}}, nil
}

func (ft *FileTools) deleteTyped(ctx context.Context, req *mcp.CallToolRequest, args DeleteArgs) (*mcp.CallToolResult, MCPMutationOutput, error) {
	result, value, err := ft.delete(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPMutationOutput{}, err
	}
	if args.DryRun {
		plan, ok := value.(MCPMutationPlan)
		if !ok {
			return typedAdapterFailure("delete", fmt.Errorf("unexpected dry-run value %T", value)), MCPMutationOutput{}, nil
		}
		return result, MCPMutationOutput{Mode: "dry_run", Plan: &plan}, nil
	}
	return result, MCPMutationOutput{Mode: "execution", Result: &MCPMutationExecutionResult{Operation: "delete", Success: true, Requested: len(args.FileIDs)}}, nil
}

func (ft *FileTools) renameTyped(ctx context.Context, req *mcp.CallToolRequest, args RenameArgs) (*mcp.CallToolResult, MCPMutationOutput, error) {
	result, value, err := ft.rename(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPMutationOutput{}, err
	}
	if args.DryRun {
		batch, ok := value.(MCPMutationBatchResult)
		if !ok {
			return typedAdapterFailure("rename", fmt.Errorf("unexpected dry-run value %T", value)), MCPMutationOutput{}, nil
		}
		return result, MCPMutationOutput{Mode: "dry_run", Batch: &batch}, nil
	}
	return result, MCPMutationOutput{Mode: "execution", Result: &MCPMutationExecutionResult{Operation: "rename", Success: true, Requested: 1}}, nil
}

func (ft *FileTools) moveTyped(ctx context.Context, req *mcp.CallToolRequest, args MoveArgs) (*mcp.CallToolResult, MCPMutationOutput, error) {
	result, value, err := ft.move(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPMutationOutput{}, err
	}
	if args.DryRun {
		plan, ok := value.(MCPMutationPlan)
		if !ok {
			return typedAdapterFailure("move", fmt.Errorf("unexpected dry-run value %T", value)), MCPMutationOutput{}, nil
		}
		return result, MCPMutationOutput{Mode: "dry_run", Plan: &plan}, nil
	}
	return result, MCPMutationOutput{Mode: "execution", Result: &MCPMutationExecutionResult{Operation: "move", Success: true, Requested: len(args.FileIDs), TargetDirID: strings.TrimSpace(args.DirID)}}, nil
}

func (ft *FileTools) copyTyped(ctx context.Context, req *mcp.CallToolRequest, args CopyArgs) (*mcp.CallToolResult, MCPMutationOutput, error) {
	result, value, err := ft.copy(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPMutationOutput{}, err
	}
	if args.DryRun {
		plan, ok := value.(MCPMutationPlan)
		if !ok {
			return typedAdapterFailure("copy", fmt.Errorf("unexpected dry-run value %T", value)), MCPMutationOutput{}, nil
		}
		return result, MCPMutationOutput{Mode: "dry_run", Plan: &plan}, nil
	}
	return result, MCPMutationOutput{Mode: "execution", Result: &MCPMutationExecutionResult{Operation: "copy", Success: true, Requested: len(args.FileIDs), TargetDirID: strings.TrimSpace(args.DirID)}}, nil
}

func (ft *FileTools) mkdirManyTyped(ctx context.Context, req *mcp.CallToolRequest, args MkdirManyArgs) (*mcp.CallToolResult, MCPMutationBatchResult, error) {
	result, value, err := ft.mkdirMany(ctx, req, args)
	if err != nil || result == nil {
		return result, MCPMutationBatchResult{}, err
	}
	output, ok := value.(MCPMutationBatchResult)
	if !ok {
		if result.IsError {
			return result, MCPMutationBatchResult{}, nil
		}
		return typedAdapterFailure("mkdir_many", fmt.Errorf("unexpected result value %T", value)), MCPMutationBatchResult{}, nil
	}
	return result, output, nil
}

func (ft *FileTools) renameManyTyped(ctx context.Context, req *mcp.CallToolRequest, args RenameManyArgs) (*mcp.CallToolResult, MCPMutationBatchResult, error) {
	result, value, err := ft.renameMany(ctx, req, args)
	if err != nil || result == nil {
		return result, MCPMutationBatchResult{}, err
	}
	output, ok := value.(MCPMutationBatchResult)
	if !ok {
		if result.IsError {
			return result, MCPMutationBatchResult{}, nil
		}
		return typedAdapterFailure("rename_many", fmt.Errorf("unexpected result value %T", value)), MCPMutationBatchResult{}, nil
	}
	return result, output, nil
}

func (ft *FileTools) uploadFromURLTyped(ctx context.Context, req *mcp.CallToolRequest, args UploadFromURLArgs) (*mcp.CallToolResult, MCPUploadOutput, error) {
	result, value, err := ft.uploadFromURL(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPUploadOutput{}, err
	}
	if args.DryRun {
		plan, ok := value.(MCPUploadPlan)
		if !ok {
			return typedAdapterFailure("upload_from_url", fmt.Errorf("unexpected dry-run value %T", value)), MCPUploadOutput{}, nil
		}
		return result, MCPUploadOutput{Mode: "dry_run", Plan: &plan}, nil
	}
	execution, ok := value.(MCPUploadExecutionResult)
	if !ok {
		return typedAdapterFailure("upload_from_url", fmt.Errorf("unexpected execution value %T", value)), MCPUploadOutput{}, nil
	}
	return result, MCPUploadOutput{Mode: "execution", Result: &execution}, nil
}

func (ft *FileTools) uploadFromLocalTyped(ctx context.Context, req *mcp.CallToolRequest, args UploadFromLocalArgs) (*mcp.CallToolResult, MCPUploadOutput, error) {
	result, value, err := ft.uploadFromLocal(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPUploadOutput{}, err
	}
	if args.DryRun {
		plan, ok := value.(MCPUploadPlan)
		if !ok {
			return typedAdapterFailure("upload_from_local", fmt.Errorf("unexpected dry-run value %T", value)), MCPUploadOutput{}, nil
		}
		return result, MCPUploadOutput{Mode: "dry_run", Plan: &plan}, nil
	}
	execution, ok := value.(MCPUploadExecutionResult)
	if !ok {
		return typedAdapterFailure("upload_from_local", fmt.Errorf("unexpected execution value %T", value)), MCPUploadOutput{}, nil
	}
	return result, MCPUploadOutput{Mode: "execution", Result: &execution}, nil
}

func (ft *FileTools) downloadFileTyped(ctx context.Context, req *mcp.CallToolRequest, args DownloadSingleFileArgs) (*mcp.CallToolResult, DownloadFileResult, error) {
	result, value, err := ft.downloadFile(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, DownloadFileResult{}, err
	}
	output, ok := value.(DownloadFileResult)
	if !ok {
		return typedAdapterFailure("download_file", fmt.Errorf("unexpected result value %T", value)), DownloadFileResult{}, nil
	}
	return result, output, nil
}

func (ft *FileTools) downloadShareFileTyped(ctx context.Context, req *mcp.CallToolRequest, args DownloadShareFileArgs) (*mcp.CallToolResult, DownloadFileResult, error) {
	result, value, err := ft.downloadShareFile(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, DownloadFileResult{}, err
	}
	output, ok := value.(DownloadFileResult)
	if !ok {
		return typedAdapterFailure("download_share_file", fmt.Errorf("unexpected result value %T", value)), DownloadFileResult{}, nil
	}
	return result, output, nil
}

func (ft *FileTools) getDownloadInfoTyped(ctx context.Context, req *mcp.CallToolRequest, args GetDownloadInfoArgs) (*mcp.CallToolResult, GetDownloadInfoResult, error) {
	result, value, err := ft.getDownloadInfo(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, GetDownloadInfoResult{}, err
	}
	output, ok := value.(GetDownloadInfoResult)
	if !ok {
		return typedAdapterFailure("get_download_info", fmt.Errorf("unexpected result value %T", value)), GetDownloadInfoResult{}, nil
	}
	return result, output, nil
}

func (ot *OfflineTools) addOfflineTaskURIsTyped(ctx context.Context, req *mcp.CallToolRequest, args AddOfflineTaskURIsArgs) (*mcp.CallToolResult, MCPOfflineMutationOutput, error) {
	result, value, err := ot.addOfflineTaskURIs(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPOfflineMutationOutput{}, err
	}
	if args.DryRun {
		plan, ok := value.(MCPOfflineMutationPlan)
		if !ok {
			return typedAdapterFailure("addOfflineTaskURIs", fmt.Errorf("unexpected dry-run value %T", value)), MCPOfflineMutationOutput{}, nil
		}
		return result, MCPOfflineMutationOutput{Mode: "dry_run", Plan: &plan}, nil
	}
	hashes, ok := value.([]string)
	if !ok {
		return typedAdapterFailure("addOfflineTaskURIs", fmt.Errorf("unexpected execution value %T", value)), MCPOfflineMutationOutput{}, nil
	}
	return result, MCPOfflineMutationOutput{Mode: "execution", Result: &MCPOfflineMutationExecutionResult{Operation: "add_offline", Success: true, Requested: len(args.URIs), Hashes: hashes}}, nil
}

func (ot *OfflineTools) deleteOfflineTasksTyped(ctx context.Context, req *mcp.CallToolRequest, args DeleteOfflineTasksArgs) (*mcp.CallToolResult, MCPOfflineMutationOutput, error) {
	result, value, err := ot.deleteOfflineTasks(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPOfflineMutationOutput{}, err
	}
	if args.DryRun {
		plan, ok := value.(MCPOfflineMutationPlan)
		if !ok {
			return typedAdapterFailure("deleteOfflineTasks", fmt.Errorf("unexpected dry-run value %T", value)), MCPOfflineMutationOutput{}, nil
		}
		return result, MCPOfflineMutationOutput{Mode: "dry_run", Plan: &plan}, nil
	}
	return result, MCPOfflineMutationOutput{Mode: "execution", Result: &MCPOfflineMutationExecutionResult{Operation: "delete_offline", Success: true, Requested: len(args.Hashes), DeleteFiles: args.DeleteFiles}}, nil
}

func (ot *OfflineTools) clearOfflineTasksTyped(ctx context.Context, req *mcp.CallToolRequest, args ClearOfflineTasksArgs) (*mcp.CallToolResult, MCPOfflineMutationOutput, error) {
	result, value, err := ot.clearOfflineTasks(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPOfflineMutationOutput{}, err
	}
	if args.DryRun {
		plan, ok := value.(MCPOfflineMutationPlan)
		if !ok {
			return typedAdapterFailure("clearOfflineTasks", fmt.Errorf("unexpected dry-run value %T", value)), MCPOfflineMutationOutput{}, nil
		}
		return result, MCPOfflineMutationOutput{Mode: "dry_run", Plan: &plan}, nil
	}
	scope, _, err := resolveMCPOfflineClearScope(args.Scope, args.ClearFlag)
	if err != nil {
		return typedAdapterFailure("clearOfflineTasks", err), MCPOfflineMutationOutput{}, nil
	}
	return result, MCPOfflineMutationOutput{Mode: "execution", Result: &MCPOfflineMutationExecutionResult{Operation: "clear_offline", Success: true, Scope: scope}}, nil
}

func (rt *RecycleTools) revertRecycleBinTyped(ctx context.Context, req *mcp.CallToolRequest, args RevertRecycleArgs) (*mcp.CallToolResult, MCPRecycleMutationOutput, error) {
	result, value, err := rt.revertRecycleBin(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPRecycleMutationOutput{}, err
	}
	if args.DryRun {
		plan, ok := value.(MCPRecycleMutationPlan)
		if !ok {
			return typedAdapterFailure("revertRecycleBin", fmt.Errorf("unexpected dry-run value %T", value)), MCPRecycleMutationOutput{}, nil
		}
		return result, MCPRecycleMutationOutput{Mode: "dry_run", Plan: &plan}, nil
	}
	return result, MCPRecycleMutationOutput{Mode: "execution", Result: &MCPRecycleMutationExecutionResult{Operation: "revert_recycle", Success: true, Requested: len(args.ItemIDs)}}, nil
}

func (rt *RecycleTools) cleanRecycleBinTyped(ctx context.Context, req *mcp.CallToolRequest, args CleanRecycleArgs) (*mcp.CallToolResult, MCPRecycleMutationOutput, error) {
	result, value, err := rt.cleanRecycleBin(ctx, req, args)
	if err != nil || result == nil || result.IsError {
		return result, MCPRecycleMutationOutput{}, err
	}
	if args.DryRun {
		plan, ok := value.(MCPRecycleMutationPlan)
		if !ok {
			return typedAdapterFailure("cleanRecycleBin", fmt.Errorf("unexpected dry-run value %T", value)), MCPRecycleMutationOutput{}, nil
		}
		return result, MCPRecycleMutationOutput{Mode: "dry_run", Plan: &plan}, nil
	}
	return result, MCPRecycleMutationOutput{Mode: "execution", Result: &MCPRecycleMutationExecutionResult{Operation: "clean_recycle", Success: true, Requested: len(args.ItemIDs)}}, nil
}
