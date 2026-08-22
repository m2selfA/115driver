package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// UploadFromURLFileItem defines one external source in a multi-URL upload.
// Dry-run is intentionally batch-level so one call cannot mix preview and execution.
type UploadFromURLFileItem struct {
	URL      string `json:"url" jsonschema:"URL of the file to download and upload"`
	DirID    string `json:"dir_id" jsonschema:"target directory ID in 115 cloud for saving the file"`
	FileName string `json:"file_name,omitempty" jsonschema:"optional filename for the uploaded file, defaults to original filename"`
}

// UploadFromURLFilesArgs defines one preflighted multi-URL upload request.
type UploadFromURLFilesArgs struct {
	Files  []UploadFromURLFileItem `json:"files" jsonschema:"external URLs to fetch and upload; each item requires url and dir_id and may override file_name"`
	DryRun bool                    `json:"dry_run,omitempty" jsonschema:"validate the entire batch without fetching external URLs or uploading data"`
}

// UploadFromURLFilesItemResult reports one item without echoing its source URL
// or temporary-file/OSS/network details.
type UploadFromURLFilesItemResult struct {
	Index         int    `json:"index" jsonschema:"zero-based input item index"`
	FileName      string `json:"file_name" jsonschema:"remote file name"`
	BytesUploaded int64  `json:"bytes_uploaded" jsonschema:"bytes uploaded for this item"`
	Rapid         bool   `json:"rapid" jsonschema:"whether 115 rapid upload completed the item"`
	Resumed       bool   `json:"resumed" jsonschema:"whether multipart upload resumed prior state"`
	Verified      bool   `json:"verified" jsonschema:"whether an existing remote target was verified"`
	Skipped       bool   `json:"skipped" jsonschema:"whether object transfer was skipped after verification"`
	Success       bool   `json:"success" jsonschema:"whether this item completed successfully"`
	Error         string `json:"error,omitempty" jsonschema:"sanitized item error when upload failed"`
}

// UploadFromURLFilesResult summarizes a multi-URL upload run.
type UploadFromURLFilesResult struct {
	Requested int                            `json:"requested" jsonschema:"number of requested URLs"`
	Succeeded int                            `json:"succeeded" jsonschema:"number of completed uploads"`
	Failed    int                            `json:"failed" jsonschema:"number of failed uploads"`
	Items     []UploadFromURLFilesItemResult `json:"items" jsonschema:"per-item results in input order"`
}

// UploadFromURLFilesOutput is the typed structured-output envelope. Source URLs
// are intentionally absent from both plan and execution result shapes.
type UploadFromURLFilesOutput struct {
	Mode   string                    `json:"mode" jsonschema:"dry_run or execution"`
	Plan   *MCPUploadPlan            `json:"plan,omitempty" jsonschema:"safe dry-run plan when mode is dry_run"`
	Result *UploadFromURLFilesResult `json:"result,omitempty" jsonschema:"safe execution result when mode is execution"`
}

type mcpPreparedURLUpload struct {
	source   *url.URL
	dirID    string
	fileName string
}

func prepareMCPURLUpload(args UploadFromURLArgs) (mcpPreparedURLUpload, error) {
	downloadURL, err := validateUploadURL(args.URL)
	if err != nil {
		return mcpPreparedURLUpload{}, fmt.Errorf("invalid external URL: %w", err)
	}
	dirID, err := normalizeMCPUploadDirID(args.DirID)
	if err != nil {
		return mcpPreparedURLUpload{}, err
	}
	fileName := args.FileName
	if strings.TrimSpace(fileName) == "" {
		fileName = filepath.Base(downloadURL.Path)
		if fileName == "" || fileName == "." || fileName == "/" {
			fileName = "downloaded_file"
		}
	}
	fileName, err = validateMCPUploadFileName(fileName)
	if err != nil {
		return mcpPreparedURLUpload{}, err
	}
	return mcpPreparedURLUpload{source: downloadURL, dirID: dirID, fileName: fileName}, nil
}

func preflightMCPURLUploadTargetDirectories(client *driver.Pan115Client, items []mcpPreparedURLUpload) error {
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

func (ft *FileTools) uploadPreparedURLWithClient(ctx context.Context, item mcpPreparedURLUpload, httpClient *http.Client) (uploadpkg.Result, error) {
	if item.source == nil {
		return uploadpkg.Result{}, errors.New("prepared external URL is empty")
	}
	if httpClient == nil {
		return uploadpkg.Result{}, errors.New("external URL HTTP client is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, item.source.String(), nil)
	if err != nil {
		return uploadpkg.Result{}, fmt.Errorf("create external download request: %w", err)
	}
	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return uploadpkg.Result{}, fmt.Errorf("download external URL: %w", sanitizeMCPExternalURLError(err))
	}
	if resp == nil || resp.Body == nil {
		return uploadpkg.Result{}, errors.New("external URL returned an empty HTTP response")
	}
	defer resp.Body.Close()

	tempFile, err := os.CreateTemp("", "115_mcp_upload_*")
	if err != nil {
		return uploadpkg.Result{}, fmt.Errorf("create upload temporary file: %w", err)
	}
	tempPath := tempFile.Name()
	defer os.Remove(tempPath)
	defer tempFile.Close()

	if err := copyHTTPResponse(tempFile, resp, ft.urlUploadMaxBytes); err != nil {
		return uploadpkg.Result{}, fmt.Errorf("save external content to temporary file: %w", err)
	}
	fileInfo, err := tempFile.Stat()
	if err != nil {
		return uploadpkg.Result{}, fmt.Errorf("stat upload temporary file: %w", err)
	}
	if _, err := tempFile.Seek(0, io.SeekStart); err != nil {
		return uploadpkg.Result{}, fmt.Errorf("seek upload temporary file: %w", err)
	}

	// The external URL was fetched through the SSRF-restricted client. Only this
	// local tempfile -> trusted 115 OSS stage uses P10.
	result, err := ft.uploadThroughTransfer(ctx, item.dirID, item.fileName, fileInfo.Size(), tempFile)
	if err != nil {
		return result, fmt.Errorf("upload external content to 115: %w", err)
	}
	return result, nil
}

func (ft *FileTools) uploadPreparedURL(ctx context.Context, item mcpPreparedURLUpload) (uploadpkg.Result, error) {
	httpClient := newMCPURLUploadHTTPClient(ft.downloadTimeout)
	defer httpClient.CloseIdleConnections()
	return ft.uploadPreparedURLWithClient(ctx, item, httpClient)
}

func urlUploadBatchPlanCallResult(plan MCPUploadPlan) (*mcp.CallToolResult, UploadFromURLFilesOutput, error) {
	result, _, err := uploadPlanCallResult(plan)
	if err != nil {
		return result, UploadFromURLFilesOutput{}, err
	}
	return result, UploadFromURLFilesOutput{Mode: "dry_run", Plan: &plan}, nil
}

func uploadFromURLFilesCallResult(response UploadFromURLFilesResult) (*mcp.CallToolResult, UploadFromURLFilesOutput, error) {
	output := UploadFromURLFilesOutput{Mode: "execution", Result: &response}
	return mcpTypedJSONResult("URL upload batch", response, output, response.Failed > 0)
}

func (ft *FileTools) uploadFromURLs(ctx context.Context, req *mcp.CallToolRequest, args UploadFromURLFilesArgs) (*mcp.CallToolResult, UploadFromURLFilesOutput, error) {
	if len(args.Files) == 0 {
		return toolError("URL upload batch requires at least one file"), UploadFromURLFilesOutput{}, nil
	}
	if len(args.Files) > maxMCPFileBatchItems {
		return toolError(fmt.Sprintf("URL upload batch has %d files; maximum is %d", len(args.Files), maxMCPFileBatchItems)), UploadFromURLFilesOutput{}, nil
	}
	if err := ft.validateUploadTransferReadiness(); err != nil {
		return toolError(fmt.Sprintf("Invalid upload transfer configuration: %v", err)), UploadFromURLFilesOutput{}, nil
	}

	prepared := make([]mcpPreparedURLUpload, len(args.Files))
	seenTargets := make(map[string]int, len(args.Files))
	for i, item := range args.Files {
		entry, err := prepareMCPURLUpload(UploadFromURLArgs{URL: item.URL, DirID: item.DirID, FileName: item.FileName})
		if err != nil {
			return toolError(fmt.Sprintf("URL upload batch item %d failed preflight: %v", i, err)), UploadFromURLFilesOutput{}, nil
		}
		targetKey := entry.dirID + "\x00" + entry.fileName
		if previous, exists := seenTargets[targetKey]; exists {
			return toolError(fmt.Sprintf("URL upload batch items %d and %d target the same 115 directory/name", previous, i)), UploadFromURLFilesOutput{}, nil
		}
		seenTargets[targetKey] = i
		prepared[i] = entry
	}
	if ft.client == nil {
		return toolError("115 client is unavailable"), UploadFromURLFilesOutput{}, nil
	}
	if err := preflightMCPURLUploadTargetDirectories(ft.client, prepared); err != nil {
		return toolError(fmt.Sprintf("URL upload batch target preflight failed: %v", err)), UploadFromURLFilesOutput{}, nil
	}
	if args.DryRun {
		return urlUploadBatchPlanCallResult(urlUploadPlan("upload_from_urls", prepared))
	}

	httpClient := newMCPURLUploadHTTPClient(ft.downloadTimeout)
	defer httpClient.CloseIdleConnections()

	response := UploadFromURLFilesResult{
		Requested: len(prepared),
		Items:     make([]UploadFromURLFilesItemResult, len(prepared)),
	}
	for i, item := range prepared {
		entry := UploadFromURLFilesItemResult{Index: i, FileName: item.fileName}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				entry.Error = err.Error()
				response.Failed++
				response.Items[i] = entry
				for j := i + 1; j < len(prepared); j++ {
					response.Items[j] = UploadFromURLFilesItemResult{Index: j, FileName: prepared[j].fileName, Error: err.Error()}
					response.Failed++
				}
				break
			}
		}
		result, err := ft.uploadPreparedURLWithClient(ctx, item, httpClient)
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
	return uploadFromURLFilesCallResult(response)
}
