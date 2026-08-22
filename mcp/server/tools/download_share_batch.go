package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DownloadShareFilesItem identifies one file inside a share and its local target.
type DownloadShareFilesItem struct {
	FileID    string `json:"file_id" jsonschema:"file ID inside the share"`
	LocalPath string `json:"local_path" jsonschema:"local path where this shared file will be saved"`
}

// DownloadShareFilesArgs defines a multi-file download from one 115 share.
type DownloadShareFilesArgs struct {
	ShareCode   string                   `json:"share_code" jsonschema:"share code"`
	ReceiveCode string                   `json:"receive_code" jsonschema:"share receive code/password"`
	UserAgent   string                   `json:"user_agent,omitempty" jsonschema:"optional user agent for all share download requests"`
	Files       []DownloadShareFilesItem `json:"files" jsonschema:"shared files to download in one batch"`
}

func (ft *FileTools) downloadShareFiles(ctx context.Context, req *mcp.CallToolRequest, args DownloadShareFilesArgs) (*mcp.CallToolResult, DownloadFilesResult, error) {
	shareCode := strings.TrimSpace(args.ShareCode)
	if shareCode == "" {
		return toolError("Share download batch requires a non-empty share_code"), DownloadFilesResult{}, nil
	}
	if len(args.Files) == 0 {
		return toolError("Share download batch requires at least one file"), DownloadFilesResult{}, nil
	}
	if len(args.Files) > maxMCPFileBatchItems {
		return toolError(fmt.Sprintf("Share download batch has %d files; maximum is %d", len(args.Files), maxMCPFileBatchItems)), DownloadFilesResult{}, nil
	}
	if ft.downloadTransfer == nil {
		ft.downloadTransfer = newMCPDownloadTransferState()
	}
	config := normalizeDownloadTransferConfig(ft.downloadTransfer.config)
	if err := config.Validate(); err != nil {
		return toolError(fmt.Sprintf("Invalid transfer configuration: %v", err)), DownloadFilesResult{}, nil
	}

	prepared := make([]mcpDownloadBatchTransferItem, len(args.Files))
	seenPaths := make(map[string]int, len(args.Files))
	seenFileIDs := make(map[string]int, len(args.Files))
	fileIDs := make([]string, len(args.Files))
	for i, item := range args.Files {
		fileID := strings.TrimSpace(item.FileID)
		if fileID == "" {
			return toolError(fmt.Sprintf("Share download batch item %d has an empty file_id", i)), DownloadFilesResult{}, nil
		}
		if previous, exists := seenFileIDs[fileID]; exists {
			return toolError(fmt.Sprintf("Share download batch items %d and %d use the same file_id", previous, i)), DownloadFilesResult{}, nil
		}
		seenFileIDs[fileID] = i
		fileIDs[i] = fileID

		localPath, err := validateMCPDownloadLocalTarget(ft.localRoot, item.LocalPath)
		if err != nil {
			return toolError(fmt.Sprintf("Share download batch item %d local target denied: %v", i, err)), DownloadFilesResult{}, nil
		}
		pathKey, err := canonicalMCPDownloadBatchPathKey(localPath)
		if err != nil {
			return toolError(fmt.Sprintf("Share download batch item %d local target identity failed: %v", i, err)), DownloadFilesResult{}, nil
		}
		if previous, exists := seenPaths[pathKey]; exists {
			return toolError(fmt.Sprintf("Share download batch items %d and %d resolve to the same local target", previous, i)), DownloadFilesResult{}, nil
		}
		seenPaths[pathKey] = i
		prepared[i].localPath = localPath
		prepared[i].stableID = mcpShareDownloadStableID(shareCode, fileID)
	}

	if ft.client == nil {
		return toolError("115 client is unavailable"), DownloadFilesResult{}, nil
	}

	// Resolve and validate every share metadata object before starting the first
	// data transfer. Receive-code-bearing API errors are redacted at this boundary.
	for i, fileID := range fileIDs {
		sharedInfo, err := ft.client.DownloadByShareCodeRequestWithUA(args.UserAgent, shareCode, args.ReceiveCode, fileID)
		if err != nil {
			message := fmt.Sprintf("Failed to get share download info for batch item %d: %v", i, err)
			return toolError(redactShareReceiveCode(message, args.ReceiveCode)), DownloadFilesResult{}, nil
		}
		info, err := sharedDownloadInfoForRequest(sharedInfo, fileID, prepared[i].stableID)
		if err != nil {
			return toolError(redactShareReceiveCode(fmt.Sprintf("Share download batch item %d metadata is invalid: %v", i, err), args.ReceiveCode)), DownloadFilesResult{}, nil
		}
		if _, err := validateMCPDownloadInfoForTransfer(info, ft.downloadMaxBytes, config.Strategy); err != nil {
			return toolError(redactShareReceiveCode(fmt.Sprintf("Share download batch item %d is not transferable: %v", i, err), args.ReceiveCode)), DownloadFilesResult{}, nil
		}
		stableID := prepared[i].stableID
		requestedFileID := fileID
		prepared[i].info = info
		prepared[i].refreshMetadata = func(refreshCtx context.Context) (*driver.DownloadInfo, error) {
			fresh, err := ft.client.DownloadByShareCodeRequestWithUA(args.UserAgent, shareCode, args.ReceiveCode, requestedFileID)
			if err != nil {
				return nil, err
			}
			return sharedDownloadInfoForRequest(fresh, requestedFileID, stableID)
		}
	}

	transferResults := make([]mcpDownloadBatchTransferResult, len(prepared))
	var batchErr error
	if config.Strategy == "file" {
		transferResults, batchErr = ft.downloadFileBatchThroughTransferWithRefresh(ctx, prepared)
		if transferResults == nil && batchErr != nil {
			message := fmt.Sprintf("Failed to schedule share download batch: %v", batchErr)
			return toolError(redactShareReceiveCode(message, args.ReceiveCode)), DownloadFilesResult{}, nil
		}
	} else {
		for i, item := range prepared {
			result, err := ft.downloadThroughTransferWithRefresh(ctx, item.info, item.localPath, item.stableID, item.refreshMetadata)
			transferResults[i] = mcpDownloadBatchTransferResult{result: result, err: err}
		}
	}

	response := DownloadFilesResult{Requested: len(prepared), Items: make([]DownloadFilesItemResult, len(prepared))}
	for i, item := range prepared {
		entry := DownloadFilesItemResult{
			Index:        i,
			FileName:     redactShareReceiveCode(item.info.FileName, args.ReceiveCode),
			BytesWritten: transferResults[i].result.BytesWritten,
			Success:      transferResults[i].err == nil,
		}
		if transferResults[i].err != nil {
			entry.Error = redactShareReceiveCode(transferResults[i].err.Error(), args.ReceiveCode)
			response.Failed++
		} else {
			response.Succeeded++
		}
		response.Items[i] = entry
	}
	if batchErr != nil && response.Failed == 0 {
		return toolError(redactShareReceiveCode(fmt.Sprintf("Share download batch scheduler failed: %v", batchErr), args.ReceiveCode)), DownloadFilesResult{}, nil
	}
	return downloadFilesCallResult(response)
}
