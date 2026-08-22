package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// UploadFromLocalFileItem defines one local source in a multi-file upload.
// Dry-run is intentionally batch-level so one call cannot mix preview and execution.
type UploadFromLocalFileItem struct {
	LocalPath string `json:"local_path" jsonschema:"absolute path to the local file to upload"`
	DirID     string `json:"dir_id" jsonschema:"target directory ID in 115 cloud"`
	FileName  string `json:"file_name,omitempty" jsonschema:"optional filename for the uploaded file, defaults to original filename"`
}

// UploadFromLocalFilesArgs defines one preflighted multi-file local upload.
// Each item can target a different 115 directory.
type UploadFromLocalFilesArgs struct {
	Files            []UploadFromLocalFileItem `json:"files" jsonschema:"local files to upload; each item requires local_path and dir_id and may override file_name"`
	DryRun           bool                      `json:"dry_run,omitempty" jsonschema:"validate the entire batch without uploading file data"`
	ExpectPlanID     string                    `json:"expect_plan_id,omitempty" jsonschema:"optional MCPPlan v1 plan_id from plan_transfer; execution re-plans from the same preflighted sources and fails before upload when it differs"`
	MaxChecksumBytes int64                     `json:"max_checksum_bytes,omitempty" jsonschema:"maximum aggregate local bytes hashed when expect_plan_id is used; default 4 GiB, maximum 64 GiB"`
}

// UploadFromLocalFilesItemResult reports one upload without exposing the local
// source path, SHA1 digest, OSS endpoint, or selected network interfaces.
type UploadFromLocalFilesItemResult struct {
	Index         int    `json:"index" jsonschema:"zero-based input item index"`
	FileName      string `json:"file_name" jsonschema:"remote file name"`
	BytesUploaded int64  `json:"bytes_uploaded" jsonschema:"bytes uploaded for this item"`
	Rapid         bool   `json:"rapid" jsonschema:"whether 115 rapid upload completed the item"`
	Resumed       bool   `json:"resumed" jsonschema:"whether multipart upload resumed prior state"`
	Verified      bool   `json:"verified" jsonschema:"whether an existing remote target was verified"`
	Skipped       bool   `json:"skipped" jsonschema:"whether object transfer was skipped after verification"`
	Success       bool   `json:"success" jsonschema:"whether this item completed successfully"`
	Error         string `json:"error,omitempty" jsonschema:"item error when upload failed"`
}

// UploadFromLocalFilesResult summarizes a local upload batch.
type UploadFromLocalFilesResult struct {
	Requested int                              `json:"requested" jsonschema:"number of requested files"`
	Succeeded int                              `json:"succeeded" jsonschema:"number of completed files"`
	Failed    int                              `json:"failed" jsonschema:"number of failed files"`
	Items     []UploadFromLocalFilesItemResult `json:"items" jsonschema:"per-file results in input order"`
}

// UploadFromLocalFilesOutput is the typed structured-output envelope. TextContent
// remains backward compatible: dry-run emits the plan JSON and execution emits
// the result JSON directly.
type UploadFromLocalFilesOutput struct {
	Mode   string                      `json:"mode" jsonschema:"dry_run or execution"`
	Plan   *MCPUploadPlan              `json:"plan,omitempty" jsonschema:"safe dry-run plan when mode is dry_run"`
	Result *UploadFromLocalFilesResult `json:"result,omitempty" jsonschema:"safe execution result when mode is execution"`
}

type mcpPreparedLocalUpload struct {
	file     *os.File
	fileName string
	fileSize int64
	dirID    string
}

func normalizeMCPUploadDirID(dirID string) (string, error) {
	dirID = strings.TrimSpace(dirID)
	if dirID == "" {
		return "", errors.New("dir_id must be non-empty; use 0 for the 115 root directory")
	}
	return dirID, nil
}

func validateMCPUploadTargetDirectory(client *driver.Pan115Client, dirID string) error {
	_, err := loadMCPRemoteDirectory(client, dirID)
	return err
}

func preflightMCPUploadTargetDirectories(client *driver.Pan115Client, items []mcpPreparedLocalUpload) error {
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, ok := seen[item.dirID]; ok {
			continue
		}
		seen[item.dirID] = struct{}{}
		if err := validateMCPUploadTargetDirectory(client, item.dirID); err != nil {
			return err
		}
	}
	return nil
}

func validateMCPUploadFileName(name string) (string, error) {
	if strings.TrimSpace(name) == "" || name == "." || name == ".." {
		return "", errors.New("file_name must be a non-empty single path component")
	}
	if strings.ContainsAny(name, `/\`) || strings.ContainsRune(name, '\x00') {
		return "", errors.New("file_name must not contain path separators or NUL")
	}
	return name, nil
}

func prepareMCPLocalUpload(root string, args UploadFromLocalArgs) (mcpPreparedLocalUpload, error) {
	dirID, err := normalizeMCPUploadDirID(args.DirID)
	if err != nil {
		return mcpPreparedLocalUpload{}, err
	}
	localPath, err := validateLocalPath(root, args.LocalPath, true)
	if err != nil {
		return mcpPreparedLocalUpload{}, fmt.Errorf("local file access denied: %w", err)
	}
	file, err := os.Open(localPath)
	if err != nil {
		return mcpPreparedLocalUpload{}, fmt.Errorf("open local file: %w", err)
	}
	closeOnError := true
	defer func() {
		if closeOnError {
			_ = file.Close()
		}
	}()

	info, err := file.Stat()
	if err != nil {
		return mcpPreparedLocalUpload{}, fmt.Errorf("stat local file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return mcpPreparedLocalUpload{}, errors.New("local_path must refer to a regular file")
	}

	fileName := args.FileName
	if strings.TrimSpace(fileName) == "" {
		fileName = info.Name()
	}
	fileName, err = validateMCPUploadFileName(fileName)
	if err != nil {
		return mcpPreparedLocalUpload{}, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return mcpPreparedLocalUpload{}, fmt.Errorf("seek local file: %w", err)
	}

	closeOnError = false
	return mcpPreparedLocalUpload{file: file, fileName: fileName, fileSize: info.Size(), dirID: dirID}, nil
}

func closeMCPPreparedLocalUploads(items []mcpPreparedLocalUpload) {
	for i := range items {
		if items[i].file != nil {
			_ = items[i].file.Close()
			items[i].file = nil
		}
	}
}

func (ft *FileTools) preflightMCPLocalUploadFiles(files []UploadFromLocalFileItem) ([]mcpPreparedLocalUpload, error) {
	if len(files) == 0 {
		return nil, errors.New("Local upload batch requires at least one file")
	}
	if len(files) > maxMCPFileBatchItems {
		return nil, fmt.Errorf("Local upload batch has %d files; maximum is %d", len(files), maxMCPFileBatchItems)
	}
	if err := ft.validateUploadTransferReadiness(); err != nil {
		return nil, fmt.Errorf("Invalid upload transfer configuration: %w", err)
	}

	prepared := make([]mcpPreparedLocalUpload, 0, len(files))
	seenTargets := make(map[string]int, len(files))
	for i, item := range files {
		entry, err := prepareMCPLocalUpload(ft.localRoot, UploadFromLocalArgs{LocalPath: item.LocalPath, DirID: item.DirID, FileName: item.FileName})
		if err != nil {
			closeMCPPreparedLocalUploads(prepared)
			return nil, fmt.Errorf("Local upload batch item %d failed preflight: %w", i, err)
		}
		targetKey := entry.dirID + "\x00" + entry.fileName
		if previous, exists := seenTargets[targetKey]; exists {
			_ = entry.file.Close()
			closeMCPPreparedLocalUploads(prepared)
			return nil, fmt.Errorf("Local upload batch items %d and %d target the same 115 directory/name", previous, i)
		}
		seenTargets[targetKey] = i
		prepared = append(prepared, entry)
	}

	if ft.client == nil {
		closeMCPPreparedLocalUploads(prepared)
		return nil, errors.New("115 client is unavailable")
	}
	if err := preflightMCPUploadTargetDirectories(ft.client, prepared); err != nil {
		closeMCPPreparedLocalUploads(prepared)
		return nil, fmt.Errorf("Local upload batch target preflight failed: %w", err)
	}
	return prepared, nil
}

func localUploadBatchPlanCallResult(plan MCPUploadPlan) (*mcp.CallToolResult, UploadFromLocalFilesOutput, error) {
	result, _, err := uploadPlanCallResult(plan)
	if err != nil {
		return result, UploadFromLocalFilesOutput{}, err
	}
	return result, UploadFromLocalFilesOutput{Mode: "dry_run", Plan: &plan}, nil
}

func uploadFromLocalFilesCallResult(response UploadFromLocalFilesResult) (*mcp.CallToolResult, UploadFromLocalFilesOutput, error) {
	output := UploadFromLocalFilesOutput{Mode: "execution", Result: &response}
	return mcpTypedJSONResult("local upload batch", response, output, response.Failed > 0)
}

func (ft *FileTools) executeMCPPreparedLocalUploads(ctx context.Context, prepared []mcpPreparedLocalUpload) UploadFromLocalFilesResult {
	response := UploadFromLocalFilesResult{
		Requested: len(prepared),
		Items:     make([]UploadFromLocalFilesItemResult, len(prepared)),
	}
	for i, item := range prepared {
		entry := UploadFromLocalFilesItemResult{Index: i, FileName: item.fileName}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				entry.Error = err.Error()
				response.Failed++
				response.Items[i] = entry
				for j := i + 1; j < len(prepared); j++ {
					response.Items[j] = UploadFromLocalFilesItemResult{Index: j, FileName: prepared[j].fileName, Error: err.Error()}
					response.Failed++
				}
				break
			}
		}
		if _, err := item.file.Seek(0, io.SeekStart); err != nil {
			entry.Error = fmt.Sprintf("seek local file: %v", err)
			response.Failed++
			response.Items[i] = entry
			continue
		}
		result, err := ft.uploadThroughTransfer(ctx, item.dirID, item.fileName, item.fileSize, item.file)
		entry.BytesUploaded = result.BytesUploaded
		entry.Rapid = result.Rapid
		entry.Resumed = result.Resumed
		entry.Verified = result.Verified
		entry.Skipped = result.Skipped
		if err != nil {
			entry.Error = err.Error()
			response.Failed++
		} else {
			entry.Success = true
			response.Succeeded++
		}
		response.Items[i] = entry
	}
	return response
}

func (ft *FileTools) uploadFromLocalFiles(ctx context.Context, req *mcp.CallToolRequest, args UploadFromLocalFilesArgs) (*mcp.CallToolResult, UploadFromLocalFilesOutput, error) {
	expectedPlanID, err := normalizeMCPExpectedPlanID(args.ExpectPlanID)
	if err != nil {
		return toolError(err.Error()), UploadFromLocalFilesOutput{}, nil
	}
	if args.DryRun && expectedPlanID != "" {
		return toolError("expect_plan_id is only valid when executing upload_from_local_files"), UploadFromLocalFilesOutput{}, nil
	}
	if args.MaxChecksumBytes != 0 && expectedPlanID == "" {
		return toolError("max_checksum_bytes requires expect_plan_id"), UploadFromLocalFilesOutput{}, nil
	}

	prepared, err := ft.preflightMCPLocalUploadFiles(args.Files)
	if err != nil {
		return toolError(err.Error()), UploadFromLocalFilesOutput{}, nil
	}
	defer closeMCPPreparedLocalUploads(prepared)
	if args.DryRun {
		return localUploadBatchPlanCallResult(localUploadPlan("upload_from_local_files", prepared))
	}
	if expectedPlanID != "" {
		if _, planErr := verifyMCPPreparedTransferPlan(prepared, nil, expectedPlanID, args.MaxChecksumBytes); planErr != nil {
			planArgs := PlanTransferArgs{Uploads: args.Files, MaxChecksumBytes: args.MaxChecksumBytes}
			return toolError("upload plan gate failed: " + redactPlanTransferError(planErr, planArgs)), UploadFromLocalFilesOutput{}, nil
		}
	}

	return uploadFromLocalFilesCallResult(ft.executeMCPPreparedLocalUploads(ctx, prepared))
}
