package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ExecuteTransferPlanArgs executes a previously reviewed plan_transfer request.
// The original inputs are supplied again so the server can remain stateless: it
// reruns the same full preflight, rebuilds MCPPlan v1, and requires an exact
// content-addressed plan match before the first transfer side effect.
type ExecuteTransferPlanArgs struct {
	Uploads          []UploadFromLocalFileItem `json:"uploads,omitempty" jsonschema:"same upload items supplied to plan_transfer"`
	Downloads        []DownloadFileArgs        `json:"downloads,omitempty" jsonschema:"same download items supplied to plan_transfer"`
	ExpectPlanID     string                    `json:"expect_plan_id" jsonschema:"required MCPPlan v1 plan_id returned by plan_transfer"`
	MaxChecksumBytes int64                     `json:"max_checksum_bytes,omitempty" jsonschema:"same local content checksum budget used by plan_transfer for upload sources and existing download targets; default 4 GiB, maximum 64 GiB"`
}

type MCPTransferExecutionSummary struct {
	Requested         int  `json:"requested"`
	UploadRequested   int  `json:"upload_requested"`
	DownloadRequested int  `json:"download_requested"`
	Succeeded         int  `json:"succeeded"`
	Failed            int  `json:"failed"`
	Skipped           int  `json:"skipped"`
	DownloadsSkipped  bool `json:"downloads_skipped,omitempty" jsonschema:"downloads were not started because an earlier upload failed"`
}

// MCPTransferExecutionOutput never exposes the original local paths, pick codes,
// signed URLs, request headers, or content digests. It preserves safe per-item
// results so partial execution is explicit rather than presented as atomic.
type MCPTransferExecutionOutput struct {
	PlanID         string                      `json:"plan_id"`
	Summary        MCPTransferExecutionSummary `json:"summary"`
	UploadResult   *UploadFromLocalFilesResult `json:"upload_result,omitempty"`
	DownloadResult *DownloadFilesResult        `json:"download_result,omitempty"`
	Error          string                      `json:"error,omitempty" jsonschema:"sanitized top-level execution error"`
}

func redactMCPTransferExecutionItemErrors(output *MCPTransferExecutionOutput, args ExecuteTransferPlanArgs) {
	if output == nil {
		return
	}
	planArgs := PlanTransferArgs{Uploads: args.Uploads, Downloads: args.Downloads, MaxChecksumBytes: args.MaxChecksumBytes}
	if output.UploadResult != nil {
		for i := range output.UploadResult.Items {
			if text := strings.TrimSpace(output.UploadResult.Items[i].Error); text != "" {
				output.UploadResult.Items[i].Error = redactPlanTransferError(errors.New(text), planArgs)
			}
		}
	}
	if output.DownloadResult != nil {
		for i := range output.DownloadResult.Items {
			if text := strings.TrimSpace(output.DownloadResult.Items[i].Error); text != "" {
				output.DownloadResult.Items[i].Error = redactPlanTransferError(errors.New(text), planArgs)
			}
		}
	}
	if text := strings.TrimSpace(output.Error); text != "" {
		output.Error = redactPlanTransferError(errors.New(text), planArgs)
	}
}

func (ft *FileTools) executeMCPPreparedTransferPlan(
	ctx context.Context,
	uploads []mcpPreparedLocalUpload,
	downloads []mcpDownloadBatchTransferItem,
	downloadConfig DownloadTransferConfig,
	expectedPlanID string,
	maxChecksumBytes int64,
) (MCPTransferExecutionOutput, error) {
	planned, err := verifyMCPPreparedTransferPlan(uploads, downloads, expectedPlanID, maxChecksumBytes)
	if err != nil {
		return MCPTransferExecutionOutput{}, fmt.Errorf("rebuild/verify transfer plan: %w", err)
	}

	output := MCPTransferExecutionOutput{
		PlanID: planned.Plan.PlanID,
		Summary: MCPTransferExecutionSummary{
			Requested:         len(uploads) + len(downloads),
			UploadRequested:   len(uploads),
			DownloadRequested: len(downloads),
		},
	}
	if len(uploads) > 0 {
		uploadResult := ft.executeMCPPreparedLocalUploads(ctx, uploads)
		output.UploadResult = &uploadResult
		output.Summary.Succeeded += uploadResult.Succeeded
		output.Summary.Failed += uploadResult.Failed
		if uploadResult.Failed > 0 {
			output.Summary.Skipped = len(downloads)
			output.Summary.DownloadsSkipped = len(downloads) > 0
			return output, nil
		}
	}

	if len(downloads) > 0 {
		downloadResult, err := ft.executeMCPPreparedDownloads(ctx, downloads, downloadConfig)
		if err != nil {
			return output, fmt.Errorf("execute download phase: %w", err)
		}
		output.DownloadResult = &downloadResult
		output.Summary.Succeeded += downloadResult.Succeeded
		output.Summary.Failed += downloadResult.Failed
	}
	return output, nil
}

func executeTransferPlanCallResult(response MCPTransferExecutionOutput) (*mcp.CallToolResult, MCPTransferExecutionOutput, error) {
	isError := response.Error != "" || response.Summary.Failed > 0 || response.Summary.Skipped > 0
	return mcpTypedJSONResult("execute_transfer_plan", response, response, isError)
}

func (ft *FileTools) executeTransferPlan(ctx context.Context, req *mcp.CallToolRequest, args ExecuteTransferPlanArgs) (*mcp.CallToolResult, MCPTransferExecutionOutput, error) {
	expectedPlanID, err := normalizeMCPExpectedPlanID(args.ExpectPlanID)
	if err != nil {
		return toolError(err.Error()), MCPTransferExecutionOutput{}, nil
	}
	if expectedPlanID == "" {
		return toolError("expect_plan_id is required"), MCPTransferExecutionOutput{}, nil
	}
	total := len(args.Uploads) + len(args.Downloads)
	if total == 0 {
		return toolError("at least one upload or download is required"), MCPTransferExecutionOutput{}, nil
	}
	if total > maxMCPFileBatchItems {
		return toolError(fmt.Sprintf("transfer execution has %d items; maximum is %d", total, maxMCPFileBatchItems)), MCPTransferExecutionOutput{}, nil
	}

	var uploads []mcpPreparedLocalUpload
	if len(args.Uploads) > 0 {
		uploads, err = ft.preflightMCPLocalUploadFiles(args.Uploads)
		if err != nil {
			planArgs := PlanTransferArgs{Uploads: args.Uploads, Downloads: args.Downloads, MaxChecksumBytes: args.MaxChecksumBytes}
			return toolError("execute_transfer_plan upload preflight failed: " + redactPlanTransferError(err, planArgs)), MCPTransferExecutionOutput{}, nil
		}
		defer closeMCPPreparedLocalUploads(uploads)
	}

	var downloads []mcpDownloadBatchTransferItem
	downloadConfig := DefaultDownloadTransferConfig()
	if len(args.Downloads) > 0 {
		preflight, preflightErr := ft.preflightMCPDownloadFiles(ctx, args.Downloads)
		if preflightErr != nil {
			planArgs := PlanTransferArgs{Uploads: args.Uploads, Downloads: args.Downloads, MaxChecksumBytes: args.MaxChecksumBytes}
			return toolError("execute_transfer_plan download preflight failed: " + redactPlanTransferError(preflightErr, planArgs)), MCPTransferExecutionOutput{}, nil
		}
		downloads = preflight.items
		downloadConfig = preflight.config
	}

	response, executionErr := ft.executeMCPPreparedTransferPlan(ctx, uploads, downloads, downloadConfig, expectedPlanID, args.MaxChecksumBytes)
	if executionErr != nil {
		response.Error = executionErr.Error()
	}
	redactMCPTransferExecutionItemErrors(&response, args)
	return executeTransferPlanCallResult(response)
}
