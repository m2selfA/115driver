package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxMCPFileBatchItems = 256

// DownloadFilesArgs defines one preflighted multi-file download request.
type DownloadFilesArgs struct {
	Files            []DownloadFileArgs `json:"files" jsonschema:"files to download in one batch; each item requires pick_code and local_path"`
	ExpectPlanID     string             `json:"expect_plan_id,omitempty" jsonschema:"optional MCPPlan v1 plan_id from a download-only plan_transfer call; execution re-plans from the same preflighted metadata and fails before transfer when it differs"`
	MaxChecksumBytes int64              `json:"max_checksum_bytes,omitempty" jsonschema:"maximum aggregate bytes hashed from existing local targets when expect_plan_id is used; default 4 GiB, maximum 64 GiB"`
}

// DownloadFilesItemResult reports one item without exposing its pick code,
// signed CDN URL, request headers, or other credential-like metadata.
type DownloadFilesItemResult struct {
	Index        int    `json:"index" jsonschema:"zero-based input item index"`
	FileName     string `json:"file_name,omitempty" jsonschema:"115 file name when metadata was available"`
	BytesWritten int64  `json:"bytes_written" jsonschema:"bytes written for this item"`
	Success      bool   `json:"success" jsonschema:"whether this item completed successfully"`
	Error        string `json:"error,omitempty" jsonschema:"sanitized item error when the transfer failed"`
}

// DownloadFilesResult summarizes one multi-file download run.
type DownloadFilesResult struct {
	Requested int                       `json:"requested" jsonschema:"number of requested files"`
	Succeeded int                       `json:"succeeded" jsonschema:"number of completed files"`
	Failed    int                       `json:"failed" jsonschema:"number of failed files"`
	Items     []DownloadFilesItemResult `json:"items" jsonschema:"per-file results in input order"`
}

type mcpDownloadBatchTransferItem struct {
	info            *driver.DownloadInfo
	localPath       string
	stableID        string
	refreshMetadata mcpDownloadInfoRefreshFunc
}

type mcpDownloadBatchTransferResult struct {
	result transfer.FileDownloadResult
	err    error
}

func normalizeMCPDownloadBatchPathKey(path string) string {
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		return strings.ToLower(path)
	}
	return path
}

func canonicalMCPDownloadBatchPathKey(path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if pathExists(absPath) {
		realPath, err := filepath.EvalSymlinks(absPath)
		if err != nil {
			return "", fmt.Errorf("resolve existing local target: %w", err)
		}
		return normalizeMCPDownloadBatchPathKey(realPath), nil
	}

	existingParent, err := nearestExistingPath(filepath.Dir(absPath))
	if err != nil {
		return "", err
	}
	realParent, err := filepath.EvalSymlinks(existingParent)
	if err != nil {
		return "", fmt.Errorf("resolve local target parent: %w", err)
	}
	remainder, err := filepath.Rel(existingParent, absPath)
	if err != nil {
		return "", err
	}
	if remainder == ".." || strings.HasPrefix(remainder, ".."+string(filepath.Separator)) {
		return "", errors.New("local target is not below its nearest existing parent")
	}
	return normalizeMCPDownloadBatchPathKey(filepath.Join(realParent, remainder)), nil
}

func validateMCPDownloadInfoForTransfer(info *driver.DownloadInfo, maxBytes int64, strategy string) (int64, error) {
	if info == nil || strings.TrimSpace(info.Url.Url) == "" {
		return 0, errors.New("download info has no CDN URL")
	}
	expectedSize := int64(info.FileSize)
	if expectedSize < 0 {
		expectedSize = transfer.UnknownFileSize
	}
	if maxBytes > 0 && expectedSize >= 0 && expectedSize > maxBytes {
		return 0, fmt.Errorf("%w: expected %d bytes, limit is %d bytes", transfer.ErrDownloadExceedsLimit, expectedSize, maxBytes)
	}
	if strategy == "chunk" && expectedSize < 0 {
		return 0, transfer.ErrChunkRequiresKnownSize
	}
	return expectedSize, nil
}

func (ft *FileTools) prepareMCPFileBatchJob(
	ctx context.Context,
	item mcpDownloadBatchTransferItem,
	config DownloadTransferConfig,
	state *mcpDownloadTransferState,
	selection mcpDownloadPathSelection,
	cache *transfer.CDNProbeCache,
) (transfer.FileTransferJob, error) {
	expectedSize, err := validateMCPDownloadInfoForTransfer(item.info, ft.downloadMaxBytes, config.Strategy)
	if err != nil {
		return transfer.FileTransferJob{}, err
	}

	stableID := strings.TrimSpace(item.stableID)
	if stableID == "" {
		return transfer.FileTransferJob{}, errors.New("download batch item has empty stable identity")
	}

	var selectedPaths []transfer.NetworkPath
	refresh := transfer.DownloadSourceRefreshFunc(nil)
	if config.URLRefreshes > 0 && item.refreshMetadata != nil {
		refresh = func(refreshCtx context.Context) (transfer.DownloadSource, error) {
			if refreshCtx != nil {
				if err := refreshCtx.Err(); err != nil {
					return transfer.DownloadSource{}, err
				}
			}
			fresh, err := item.refreshMetadata(refreshCtx)
			if err != nil {
				return transfer.DownloadSource{}, err
			}
			if fresh == nil || strings.TrimSpace(fresh.Url.Url) == "" {
				return transfer.DownloadSource{}, errors.New("refreshed download URL is empty")
			}
			if expectedSize >= 0 && int64(fresh.FileSize) != expectedSize {
				return transfer.DownloadSource{}, fmt.Errorf("refreshed file size changed from %d to %d", expectedSize, int64(fresh.FileSize))
			}
			refreshedCDN, probeErr := state.deps.probeCDNPaths(refreshCtx, fresh.Url.Url, fresh.Header, selectedPaths, cache)
			if probeErr != nil {
				return transfer.DownloadSource{}, fmt.Errorf("probe refreshed 115 CDN: %w", probeErr)
			}
			if expectedSize > 0 && len(refreshedCDN.Paths) == 0 {
				return transfer.DownloadSource{}, fmt.Errorf("refreshed 115 CDN host %q is not usable through the current transfer paths", refreshedCDN.Host)
			}
			return transfer.DownloadSource{URL: fresh.Url.Url, Header: fresh.Header}, nil
		}
	}

	cdn, err := state.deps.probeCDNPaths(ctx, item.info.Url.Url, item.info.Header, selection.candidates, cache)
	if err != nil {
		return transfer.FileTransferJob{}, fmt.Errorf("probe 115 CDN: %w", err)
	}
	selectedPaths = cdn.Paths
	if expectedSize > 0 && len(selectedPaths) == 0 {
		return transfer.FileTransferJob{}, fmt.Errorf("no selected network interface can reach 115 CDN host %q", cdn.Host)
	}
	if expectedSize == 0 && len(selectedPaths) == 0 {
		selectedPaths = nil
	}

	resumeKey := ""
	if config.Resume {
		resumeKey = stableID
	}
	return transfer.FileTransferJob{
		ID:              stableID,
		URL:             item.info.Url.Url,
		Header:          item.info.Header,
		DestinationPath: item.localPath,
		NetworkPaths:    selectedPaths,
		ExpectedSize:    expectedSize,
		MaxBytes:        ft.downloadMaxBytes,
		Timeout:         ft.downloadTimeout,
		ResumeKey:       resumeKey,
		Refresh:         refresh,
		MaxRefreshes:    config.URLRefreshes,
	}, nil
}

func (ft *FileTools) downloadFileBatchThroughTransferWithRefresh(ctx context.Context, items []mcpDownloadBatchTransferItem) ([]mcpDownloadBatchTransferResult, error) {
	if len(items) == 0 {
		return []mcpDownloadBatchTransferResult{}, nil
	}
	if ft.downloadTransfer == nil {
		ft.downloadTransfer = newMCPDownloadTransferState()
	}
	state := ft.downloadTransfer
	config := normalizeDownloadTransferConfig(state.config)
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if config.Strategy != "file" {
		return nil, fmt.Errorf("file batch scheduler requires file strategy, got %q", config.Strategy)
	}

	selection, err := resolveMCPDownloadPathSelection(ctx, config.Interfaces, state.deps)
	if err != nil {
		return nil, err
	}
	cache, err := state.probeCache()
	if err != nil {
		return nil, err
	}
	health, err := state.healthTracker()
	if err != nil {
		return nil, err
	}

	jobs := make([]transfer.FileTransferJob, len(items))
	for i, item := range items {
		job, prepareErr := ft.prepareMCPFileBatchJob(ctx, item, config, state, selection, cache)
		if prepareErr != nil {
			return nil, fmt.Errorf("prepare download batch item %d: %w", i, prepareErr)
		}
		jobs[i] = job
	}

	report, scheduleErr := state.deps.scheduleFiles(
		ctx,
		selection.workers,
		jobs,
		transfer.WithFileScheduleRetries(config.Retries),
		transfer.WithFileScheduleWorkersPerInterface(config.WorkersPerInterface),
		transfer.WithFileScheduleHealthTracker(health),
	)
	if len(report.Results) != len(items) {
		shapeErr := fmt.Errorf("download scheduler returned %d results, expected %d", len(report.Results), len(items))
		if scheduleErr != nil {
			return nil, errors.Join(scheduleErr, shapeErr)
		}
		return nil, shapeErr
	}

	results := make([]mcpDownloadBatchTransferResult, len(items))
	for i, scheduled := range report.Results {
		results[i] = mcpDownloadBatchTransferResult{result: scheduled.Result, err: scheduled.Err}
	}
	return results, scheduleErr
}

type mcpDownloadBatchPreflight struct {
	items  []mcpDownloadBatchTransferItem
	config DownloadTransferConfig
}

func (ft *FileTools) preflightMCPDownloadFiles(ctx context.Context, files []DownloadFileArgs) (mcpDownloadBatchPreflight, error) {
	if len(files) == 0 {
		return mcpDownloadBatchPreflight{}, errors.New("Download batch requires at least one file")
	}
	if len(files) > maxMCPFileBatchItems {
		return mcpDownloadBatchPreflight{}, fmt.Errorf("Download batch has %d files; maximum is %d", len(files), maxMCPFileBatchItems)
	}
	if ft.downloadTransfer == nil {
		ft.downloadTransfer = newMCPDownloadTransferState()
	}
	config := normalizeDownloadTransferConfig(ft.downloadTransfer.config)
	if err := config.Validate(); err != nil {
		return mcpDownloadBatchPreflight{}, fmt.Errorf("Invalid transfer configuration: %w", err)
	}

	prepared := make([]mcpDownloadBatchTransferItem, len(files))
	seenPaths := make(map[string]int, len(files))
	seenPickCodes := make(map[string]int, len(files))
	for i, item := range files {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return mcpDownloadBatchPreflight{}, err
			}
		}
		pickCode := strings.TrimSpace(item.PickCode)
		if pickCode == "" {
			return mcpDownloadBatchPreflight{}, fmt.Errorf("Download batch item %d has an empty pick_code", i)
		}
		if previous, exists := seenPickCodes[pickCode]; exists {
			return mcpDownloadBatchPreflight{}, fmt.Errorf("Download batch items %d and %d use the same pick_code", previous, i)
		}
		seenPickCodes[pickCode] = i

		localPath, err := validateMCPDownloadLocalTarget(ft.localRoot, item.LocalPath)
		if err != nil {
			return mcpDownloadBatchPreflight{}, fmt.Errorf("Download batch item %d local target denied: %w", i, err)
		}
		pathKey, err := canonicalMCPDownloadBatchPathKey(localPath)
		if err != nil {
			return mcpDownloadBatchPreflight{}, fmt.Errorf("Download batch item %d local target identity failed: %w", i, err)
		}
		if previous, exists := seenPaths[pathKey]; exists {
			return mcpDownloadBatchPreflight{}, fmt.Errorf("Download batch items %d and %d resolve to the same local target", previous, i)
		}
		seenPaths[pathKey] = i
		prepared[i] = mcpDownloadBatchTransferItem{localPath: localPath, stableID: pickCode}
	}

	if ft.client == nil {
		return mcpDownloadBatchPreflight{}, errors.New("115 client is unavailable")
	}
	for i, item := range files {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return mcpDownloadBatchPreflight{}, err
			}
		}
		pickCode := strings.TrimSpace(item.PickCode)
		info, err := ft.client.DownloadWithUA(pickCode, item.UserAgent)
		if err != nil {
			return mcpDownloadBatchPreflight{}, fmt.Errorf("Failed to get download info for batch item %d: %w", i, err)
		}
		if _, err := validateMCPDownloadInfoForTransfer(info, ft.downloadMaxBytes, config.Strategy); err != nil {
			return mcpDownloadBatchPreflight{}, fmt.Errorf("Download batch item %d is not transferable: %w", i, err)
		}
		userAgent := item.UserAgent
		prepared[i].info = info
		prepared[i].refreshMetadata = func(refreshCtx context.Context) (*driver.DownloadInfo, error) {
			return ft.client.DownloadWithUA(pickCode, userAgent)
		}
	}
	return mcpDownloadBatchPreflight{items: prepared, config: config}, nil
}

func downloadFilesCallResult(response DownloadFilesResult) (*mcp.CallToolResult, DownloadFilesResult, error) {
	return mcpTypedJSONResult("download batch", response, response, response.Failed > 0)
}

func (ft *FileTools) executeMCPPreparedDownloads(ctx context.Context, prepared []mcpDownloadBatchTransferItem, config DownloadTransferConfig) (DownloadFilesResult, error) {
	transferResults := make([]mcpDownloadBatchTransferResult, len(prepared))
	var batchErr error
	if config.Strategy == "file" {
		transferResults, batchErr = ft.downloadFileBatchThroughTransferWithRefresh(ctx, prepared)
		if transferResults == nil && batchErr != nil {
			return DownloadFilesResult{}, fmt.Errorf("Failed to schedule download batch: %w", batchErr)
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
			FileName:     item.info.FileName,
			BytesWritten: transferResults[i].result.BytesWritten,
			Success:      transferResults[i].err == nil,
		}
		if transferResults[i].err != nil {
			entry.Error = transferResults[i].err.Error()
			response.Failed++
		} else {
			response.Succeeded++
		}
		response.Items[i] = entry
	}
	if batchErr != nil && response.Failed == 0 {
		return DownloadFilesResult{}, fmt.Errorf("Download batch scheduler failed: %w", batchErr)
	}
	return response, nil
}

func (ft *FileTools) executeMCPPreparedDownloadsWithPlanGate(ctx context.Context, prepared []mcpDownloadBatchTransferItem, config DownloadTransferConfig, expectedPlanID string, maxChecksumBytes int64) (DownloadFilesResult, error) {
	if expectedPlanID != "" {
		if _, err := verifyMCPPreparedTransferPlan(nil, prepared, expectedPlanID, maxChecksumBytes); err != nil {
			return DownloadFilesResult{}, fmt.Errorf("build/verify transfer plan gate: %w", err)
		}
	}
	return ft.executeMCPPreparedDownloads(ctx, prepared, config)
}

func (ft *FileTools) downloadFiles(ctx context.Context, req *mcp.CallToolRequest, args DownloadFilesArgs) (*mcp.CallToolResult, DownloadFilesResult, error) {
	expectedPlanID, err := normalizeMCPExpectedPlanID(args.ExpectPlanID)
	if err != nil {
		return toolError(err.Error()), DownloadFilesResult{}, nil
	}
	if args.MaxChecksumBytes != 0 && expectedPlanID == "" {
		return toolError("max_checksum_bytes requires expect_plan_id"), DownloadFilesResult{}, nil
	}
	preflight, err := ft.preflightMCPDownloadFiles(ctx, args.Files)
	if err != nil {
		return toolError(err.Error()), DownloadFilesResult{}, nil
	}
	prepared := preflight.items
	config := preflight.config
	response, err := ft.executeMCPPreparedDownloadsWithPlanGate(ctx, prepared, config, expectedPlanID, args.MaxChecksumBytes)
	if err != nil {
		planArgs := PlanTransferArgs{Downloads: args.Files, MaxChecksumBytes: args.MaxChecksumBytes}
		return toolError("download plan gate/execution failed: " + redactPlanTransferError(err, planArgs)), DownloadFilesResult{}, nil
	}
	return downloadFilesCallResult(response)
}
