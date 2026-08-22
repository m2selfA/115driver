package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

var (
	shareDownloadReceiveCode         string
	shareDownloadTimeout             = defaultDownloadTimeout
	shareDownloadInterfaces          string
	shareDownloadStrategy            string
	shareDownloadChunkSize           string
	shareDownloadWorkersPerInterface int
	shareDownloadDryRun              bool
)

type shareDownloadClient interface {
	DownloadByShareCodeRequest(shareCode, receiveCode, fileID string) (*driver.SharedDownloadRequest, error)
}

type shareDownloadPlan struct {
	FileID      string
	Info        *driver.SharedDownloadRequest
	Destination string
	Err         error
}

type shareDownloadItemResult struct {
	FileID      string   `json:"file_id"`
	FileName    string   `json:"file_name"`
	LocalPath   string   `json:"local_path"`
	Size        int64    `json:"size"`
	Transferred int64    `json:"transferred,omitempty"`
	Strategy    string   `json:"strategy"`
	Interfaces  []string `json:"interfaces,omitempty"`
	DryRun      bool     `json:"dry_run"`
}

var shareDownloadCmd = &cobra.Command{
	Use:   "download <share_code> <file_id>... <local_path>",
	Short: "Download one or more files from a share link",
	Long:  "Download one or more file IDs from a 115 share link through the normal multi-interface transfer engine. --from-file FILE reads additional file IDs one per line. A multi-file local_path is a destination directory. --dry-run resolves signed URLs and local destination mappings without creating local paths or transferring file data. Signed URLs, receive codes, cookies, and request headers are never emitted in command results.",
	Args:  shareDownloadArgs,
	RunE:  runShareDownloadCommand,
}

func shareDownloadArgs(cmd *cobra.Command, args []string) error {
	if len(args) < 2 {
		return &exitError{code: output.ExitArgs, msg: "share download requires <share_code> and <local_path>, plus at least one file ID or --from-file"}
	}
	fromFile, err := batchFromFile(cmd)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if len(args) < 3 && fromFile == "" {
		return &exitError{code: output.ExitArgs, msg: "share download requires at least one file ID or --from-file"}
	}
	if strings.TrimSpace(args[0]) == "" {
		return &exitError{code: output.ExitArgs, msg: "share code must not be empty"}
	}
	if strings.TrimSpace(args[len(args)-1]) == "" {
		return &exitError{code: output.ExitArgs, msg: "local path must not be empty"}
	}
	if _, err := resolveShareReceiveCode(cmd, shareDownloadReceiveCode); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	jobs, err := batchJobs(cmd)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if err := validateStaticTransferBatchPolicy(cmd, args[1:], jobs, "share downloads"); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if err := validateDownloadTimeout(shareDownloadTimeout); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if commandFlagChanged(cmd, "workers-per-interface") && shareDownloadWorkersPerInterface <= 0 {
		return &exitError{code: output.ExitArgs, msg: "--workers-per-interface must be > 0"}
	}
	strategy := strings.ToLower(strings.TrimSpace(shareDownloadStrategy))
	if strategy != "" && strategy != "file" && strategy != "chunk" {
		return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("unsupported transfer strategy %q; use \"file\" or \"chunk\"", shareDownloadStrategy)}
	}
	if chunkSizeText := strings.TrimSpace(shareDownloadChunkSize); chunkSizeText != "" {
		chunkSize, err := transfer.ParseByteSize(chunkSizeText)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("invalid transfer chunk size %q: %v", chunkSizeText, err)}
		}
		if chunkSize <= 0 {
			return &exitError{code: output.ExitArgs, msg: "transfer chunk size must be > 0"}
		}
	}
	return nil
}

func runShareDownloadCommand(cmd *cobra.Command, args []string) error {
	return runShareDownloadCommandWithClient(client, cmd, args, defaultDownloadPipelineDeps())
}

func runShareDownloadCommandWithClient(shareClient shareDownloadClient, cmd *cobra.Command, args []string, deps downloadPipelineDeps) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	shareCode, fileIDs, localTarget, err := expandShareDownloadInputs(cmd, args)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	receiveCode, err := resolveShareReceiveCode(cmd, shareDownloadReceiveCode)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	jobs, err := batchJobs(cmd)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if jobs > 1 && len(fileIDs) < 2 {
		return &exitError{code: output.ExitArgs, msg: "--jobs > 1 is only valid for multi-file share downloads"}
	}
	parallelism := jobs
	if parallelism > len(fileIDs) {
		parallelism = len(fileIDs)
	}
	continueOnError := batchContinueOnError(cmd)
	if parallelism > 1 && !continueOnError {
		return &exitError{code: output.ExitArgs, msg: "--jobs > 1 requires --continue-on-error so concurrent failure semantics remain explicit"}
	}

	workerLimit := 0
	if parallelism > 1 {
		workerLimit, err = resolveShareDownloadWorkerLimit(cmd, parallelism)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
	}
	options, err := resolveShareDownloadOptions(cmd, workerLimit)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	plans, err := prepareShareDownloadPlans(ctx, shareClient, shareCode, receiveCode, fileIDs, localTarget)
	if err != nil {
		return err
	}
	if !continueOnError {
		for _, plan := range plans {
			if plan.Err != nil {
				return plan.Err
			}
		}
	}

	if shareDownloadDryRun {
		return finishShareDownloadBatch(shareCode, plans, nil, len(plans), parallelism, workerLimit, true, options.Strategy)
	}
	if err := createShareDownloadParents(plans); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	selection, err := resolveDownloadPathSelection(ctx, options.Interfaces, deps)
	if err != nil {
		return &exitError{code: output.ExitError, msg: fmt.Sprintf("Resolve share download network paths failed: %v", err)}
	}

	results := make([]shareDownloadItemResult, len(plans))
	errorsByIndex := make([]error, len(plans))
	processed := 0
	runOne := func(i int) error {
		plan := plans[i]
		if plan.Err != nil {
			return plan.Err
		}
		result, err := executeShareDownloadPlan(ctx, shareClient, shareCode, receiveCode, plan, options, selection, deps)
		if err == nil {
			results[i] = result
		}
		return err
	}
	if parallelism > 1 {
		errorsByIndex = runParallelBatch(len(plans), parallelism, workerLimit, func(i int) error {
			if !jsonOutput {
				fmt.Fprintf(os.Stderr, "[%d/%d] Share download: %s\n", i+1, len(plans), plans[i].FileID)
			}
			return runOne(i)
		})
		processed = len(plans)
	} else {
		for i := range plans {
			if !jsonOutput {
				fmt.Fprintf(os.Stderr, "[%d/%d] Share download: %s\n", i+1, len(plans), plans[i].FileID)
			}
			errorsByIndex[i] = runOne(i)
			processed = i + 1
			if errorsByIndex[i] != nil && !continueOnError {
				break
			}
		}
	}
	return finishShareDownloadBatch(shareCode, plans, results, processed, parallelism, workerLimit, false, options.Strategy, errorsByIndex...)
}

func expandShareDownloadInputs(cmd *cobra.Command, args []string) (string, []string, string, error) {
	shareCode := strings.TrimSpace(args[0])
	localTarget := args[len(args)-1]
	inputs := append([]string(nil), args[1:len(args)-1]...)
	fromFile, err := batchFromFile(cmd)
	if err != nil {
		return "", nil, "", err
	}
	if fromFile != "" {
		extra, err := readBatchSources(cmd, fromFile)
		if err != nil {
			return "", nil, "", err
		}
		inputs = append(inputs, extra...)
	}
	ids, err := normalizeShareDownloadIDs(inputs)
	if err != nil {
		return "", nil, "", err
	}
	return shareCode, ids, localTarget, nil
}

func normalizeShareDownloadIDs(inputs []string) ([]string, error) {
	ids := make([]string, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for _, raw := range inputs {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("share file ID must not be empty")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("at least one share file ID is required")
	}
	return ids, nil
}

func resolveShareDownloadWorkerLimit(cmd *cobra.Command, jobs int) (int, error) {
	config, err := auth.ResolveTransferConfig(configPath)
	if err != nil {
		return 0, err
	}
	workers := config.WorkersPerInterface
	if commandFlagChanged(cmd, "workers-per-interface") {
		workers = shareDownloadWorkersPerInterface
	}
	return batchWorkerLimit(workers, jobs)
}

func resolveShareDownloadOptions(cmd *cobra.Command, workerLimit int) (downloadCommandOptions, error) {
	config, err := auth.ResolveTransferConfig(configPath)
	if err != nil {
		return downloadCommandOptions{}, err
	}
	workers := config.WorkersPerInterface
	if workerLimit > 0 {
		workers = workerLimit
	} else if commandFlagChanged(cmd, "workers-per-interface") {
		workers = shareDownloadWorkersPerInterface
	}
	interfaces := config.Interfaces
	if strings.TrimSpace(shareDownloadInterfaces) != "" {
		interfaces = strings.TrimSpace(shareDownloadInterfaces)
	}
	strategy := config.Strategy
	if strings.TrimSpace(shareDownloadStrategy) != "" {
		strategy = strings.ToLower(strings.TrimSpace(shareDownloadStrategy))
	}
	chunkSizeText := config.ChunkSize
	if strings.TrimSpace(shareDownloadChunkSize) != "" {
		chunkSizeText = strings.TrimSpace(shareDownloadChunkSize)
	}
	chunkSize, err := transfer.ParseByteSize(chunkSizeText)
	if err != nil {
		return downloadCommandOptions{}, fmt.Errorf("invalid transfer chunk size %q: %v", chunkSizeText, err)
	}
	options := downloadCommandOptions{
		Timeout: shareDownloadTimeout, Interfaces: interfaces, Strategy: strategy,
		WorkersPerInterface: workers, ProbeCacheTTL: config.ProbeCacheTTL, Retries: config.Retries,
		ChunkSize: chunkSize, HealthCooldown: config.HealthCooldown, HealthCooldownMax: config.HealthCooldownMax,
		Resume: false, URLRefreshes: config.URLRefreshes,
	}
	if err := validateDownloadCommandOptions(options); err != nil {
		return downloadCommandOptions{}, err
	}
	return options, nil
}

func prepareShareDownloadPlans(ctx context.Context, shareClient shareDownloadClient, shareCode, receiveCode string, fileIDs []string, localTarget string) ([]shareDownloadPlan, error) {
	multi := len(fileIDs) > 1
	if multi {
		if info, err := os.Stat(localTarget); err == nil {
			if !info.IsDir() {
				return nil, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("multi-file share download destination %q is not a directory", localTarget)}
			}
		} else if os.IsNotExist(err) {
			if err := validateCreatableDownloadParent(filepath.Dir(localTarget)); err != nil {
				return nil, &exitError{code: output.ExitArgs, msg: err.Error()}
			}
		} else {
			return nil, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("cannot inspect share download destination %q: %v", localTarget, err)}
		}
	}
	plans := make([]shareDownloadPlan, len(fileIDs))
	seenDestinations := make(map[string]string, len(fileIDs))
	for i, fileID := range fileIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		plans[i].FileID = fileID
		info, err := shareClient.DownloadByShareCodeRequest(shareCode, receiveCode, fileID)
		if err != nil {
			plans[i].Err = &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("cannot resolve share file %s: %v", fileID, err)}
			continue
		}
		if err := validateShareDownloadInfo(fileID, info); err != nil {
			plans[i].Err = &exitError{code: output.ExitError, msg: err.Error()}
			continue
		}
		destination := localTarget
		if multi {
			destination = filepath.Join(localTarget, info.FileName)
		} else {
			destination = resolver.ResolveLocalDownloadPath(localTarget, info.FileName)
		}
		if err := validateDownloadFileDestination(destination); err != nil {
			plans[i].Err = &exitError{code: output.ExitArgs, msg: err.Error()}
			continue
		}
		key := localCollisionKey(destination)
		if previous, ok := seenDestinations[key]; ok {
			return nil, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("share files %s and %s map to the same local path %q", previous, fileID, destination)}
		}
		seenDestinations[key] = fileID
		plans[i].Info = info
		plans[i].Destination = destination
	}
	return plans, nil
}

func validateShareDownloadInfo(requestedID string, info *driver.SharedDownloadRequest) error {
	if info == nil {
		return fmt.Errorf("share download metadata for %s is empty", requestedID)
	}
	if info.FileID != "" && info.FileID != requestedID {
		return fmt.Errorf("share download metadata ID mismatch: requested %s, received %s", requestedID, info.FileID)
	}
	if err := validateRemoteDownloadName(info.FileName); err != nil {
		return fmt.Errorf("unsafe share file name %q: %w", info.FileName, err)
	}
	if strings.TrimSpace(info.URL.URL) == "" {
		return fmt.Errorf("share download URL for %s is empty", requestedID)
	}
	if int64(info.FileSize) < 0 {
		return fmt.Errorf("share download size for %s is invalid: %d", requestedID, int64(info.FileSize))
	}
	return nil
}

func createShareDownloadParents(plans []shareDownloadPlan) error {
	for _, plan := range plans {
		if plan.Err != nil || plan.Destination == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(plan.Destination), 0755); err != nil {
			return fmt.Errorf("cannot create share download parent for %q: %v", plan.Destination, err)
		}
	}
	return nil
}

func executeShareDownloadPlan(ctx context.Context, shareClient shareDownloadClient, shareCode, receiveCode string, plan shareDownloadPlan, options downloadCommandOptions, selection downloadPathSelection, deps downloadPipelineDeps) (shareDownloadItemResult, error) {
	result := shareDownloadItemResult{FileID: plan.FileID, FileName: plan.Info.FileName, LocalPath: plan.Destination, Size: int64(plan.Info.FileSize), Strategy: options.Strategy}
	for _, path := range selection.Workers {
		result.Interfaces = append(result.Interfaces, path.String())
	}
	cache, err := transfer.NewCDNProbeCache(options.ProbeCacheTTL)
	if err != nil {
		return result, err
	}
	health, err := transfer.NewNetworkHealthTracker(transfer.NetworkHealthOptions{Cooldown: options.HealthCooldown, CooldownMax: options.HealthCooldownMax})
	if err != nil {
		return result, err
	}
	expectedSize := int64(plan.Info.FileSize)
	probeOptions := []transfer.CDNProbeOption(nil)
	if strings.EqualFold(options.Strategy, "chunk") && expectedSize > 0 {
		probeOptions = append(probeOptions, transfer.WithCDNProbeRequireRangeValidation(true))
	}
	cdn, err := deps.probeCDNPaths(ctx, plan.Info.URL.URL, plan.Info.Header, selection.Candidates, cache, probeOptions...)
	if err != nil {
		return result, fmt.Errorf("probe share CDN for %s: %w", plan.FileID, err)
	}
	selectedPaths := cdn.Paths
	if strings.EqualFold(options.Strategy, "chunk") && expectedSize > 0 {
		selectedPaths = cdn.RangePaths
		if len(selectedPaths) == 0 {
			return result, fmt.Errorf("share CDN host %q does not support byte ranges on selected interfaces", cdn.Host)
		}
	}
	if len(selectedPaths) == 0 && expectedSize > 0 {
		return result, fmt.Errorf("no selected network interface can reach share CDN host %q", cdn.Host)
	}

	refresh := transfer.DownloadSourceRefreshFunc(nil)
	if options.URLRefreshes > 0 {
		refresh = func(refreshCtx context.Context) (transfer.DownloadSource, error) {
			fresh, err := shareClient.DownloadByShareCodeRequest(shareCode, receiveCode, plan.FileID)
			if err != nil {
				return transfer.DownloadSource{}, err
			}
			if err := validateShareDownloadInfo(plan.FileID, fresh); err != nil {
				return transfer.DownloadSource{}, err
			}
			if int64(fresh.FileSize) != expectedSize {
				return transfer.DownloadSource{}, fmt.Errorf("refreshed share file size changed from %d to %d", expectedSize, int64(fresh.FileSize))
			}
			refreshedCDN, err := deps.probeCDNPaths(refreshCtx, fresh.URL.URL, fresh.Header, selectedPaths, cache, probeOptions...)
			if err != nil {
				return transfer.DownloadSource{}, fmt.Errorf("probe refreshed share CDN: %w", err)
			}
			usable := refreshedCDN.Paths
			if strings.EqualFold(options.Strategy, "chunk") && expectedSize > 0 {
				usable = refreshedCDN.RangePaths
			}
			if expectedSize > 0 && len(usable) == 0 {
				return transfer.DownloadSource{}, fmt.Errorf("refreshed share CDN host %q is not usable through current transfer paths", refreshedCDN.Host)
			}
			return transfer.DownloadSource{URL: fresh.URL.URL, Header: fresh.Header}, nil
		}
	}

	if strings.EqualFold(options.Strategy, "chunk") {
		chunkResult, err := deps.downloadChunks(ctx, transfer.ChunkDownloadRequest{
			URL: plan.Info.URL.URL, Header: plan.Info.Header, DestinationPath: plan.Destination,
			NetworkPaths: selectedPaths, ExpectedSize: expectedSize, ChunkSize: options.ChunkSize,
			Timeout: options.Timeout, Retries: options.Retries, RecoveryRetries: options.Retries,
			WorkersPerInterface: options.WorkersPerInterface, HealthTracker: health,
			Refresh: refresh, MaxRefreshes: options.URLRefreshes,
		})
		if err != nil {
			return result, err
		}
		result.Transferred = chunkResult.BytesWritten
		return result, nil
	}

	report, err := deps.scheduleFiles(ctx, selection.Workers, []transfer.FileTransferJob{{
		ID: plan.FileID, URL: plan.Info.URL.URL, Header: plan.Info.Header, DestinationPath: plan.Destination,
		NetworkPaths: selectedPaths, ExpectedSize: expectedSize, Timeout: options.Timeout,
		Refresh: refresh, MaxRefreshes: options.URLRefreshes,
	}}, transfer.WithFileScheduleRetries(options.Retries), transfer.WithFileScheduleWorkersPerInterface(options.WorkersPerInterface), transfer.WithFileScheduleHealthTracker(health))
	if err != nil {
		return result, err
	}
	if len(report.Results) != 1 {
		return result, fmt.Errorf("share download scheduler returned %d result(s), want 1", len(report.Results))
	}
	if report.Results[0].Err != nil {
		return result, report.Results[0].Err
	}
	result.Transferred = report.Results[0].Result.BytesWritten
	return result, nil
}

func finishShareDownloadBatch(shareCode string, plans []shareDownloadPlan, results []shareDownloadItemResult, processed, jobs, workerLimit int, dryRun bool, strategy string, executionErrors ...error) error {
	if processed < 0 {
		processed = 0
	}
	if processed > len(plans) {
		processed = len(plans)
	}
	items := make([]batchItemResult, 0, processed)
	successes := make([]shareDownloadItemResult, 0, processed)
	for i, plan := range plans[:processed] {
		err := plan.Err
		if i < len(executionErrors) && executionErrors[i] != nil {
			err = executionErrors[i]
		}
		itemResult := shareDownloadItemResult{FileID: plan.FileID, DryRun: dryRun, Strategy: strategy}
		if plan.Info != nil {
			itemResult.FileName = plan.Info.FileName
			itemResult.LocalPath = plan.Destination
			itemResult.Size = int64(plan.Info.FileSize)
		}
		if i < len(results) && results[i].FileID != "" {
			itemResult = results[i]
			itemResult.DryRun = dryRun
		}
		if err != nil {
			items = append(items, failedBatchItem(plan.FileID, itemResult, err))
			printBatchItemFailure(i, len(plans), "share download "+plan.FileID, err)
			continue
		}
		successes = append(successes, itemResult)
		items = append(items, successfulBatchItem(plan.FileID, itemResult))
	}
	base := map[string]interface{}{"share_code": shareCode, "downloads": successes, "jobs": jobs, "dry_run": dryRun}
	if workerLimit > 0 {
		base["workers_per_interface_per_job"] = workerLimit
	}
	data := batchResultData(len(plans), items, base)
	if batchFailedCount(items) > 0 {
		return batchIncompleteError("share download batch", len(plans), items, data)
	}
	printer.PrintSuccess(data)
	if !jsonOutput {
		if dryRun {
			fmt.Printf("DRY-RUN share download plan complete: %d file(s); no local data was written.\n", len(successes))
		} else {
			fmt.Printf("Share download complete: %d file(s).\n", len(successes))
		}
	}
	return nil
}

func init() {
	shareDownloadCmd.Flags().StringVar(&shareDownloadReceiveCode, "receive-code", "", "Share receive code/password")
	shareDownloadCmd.Flags().DurationVar(&shareDownloadTimeout, "timeout", defaultDownloadTimeout, "Download timeout per file, use 0 to disable")
	shareDownloadCmd.Flags().StringVar(&shareDownloadInterfaces, "interfaces", "", "Override transfer interfaces (auto, or comma-separated interface names/indexes/IPs)")
	shareDownloadCmd.Flags().StringVar(&shareDownloadStrategy, "strategy", "", "Override transfer strategy (file or chunk)")
	shareDownloadCmd.Flags().StringVar(&shareDownloadChunkSize, "chunk-size", "", "Override chunk strategy range size (for example 32MiB)")
	shareDownloadCmd.Flags().IntVar(&shareDownloadWorkersPerInterface, "workers-per-interface", 0, "Override independent connections per physical interface")
	shareDownloadCmd.Flags().BoolVar(&shareDownloadDryRun, "dry-run", false, "Resolve signed URLs and local mappings without creating paths or transferring data")
	addContinueOnErrorFlag(shareDownloadCmd)
	addBatchJobsFlag(shareDownloadCmd)
	addBatchFromFileFlag(shareDownloadCmd)
	shareCmd.AddCommand(shareDownloadCmd)
}
