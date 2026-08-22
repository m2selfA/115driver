package cmd

import (
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/spf13/cobra"
)

type batchDownloadSource struct {
	RemotePath string
	LocalPath  string
}

type batchDownloadPlan struct {
	Source batchDownloadSource
	Err    error
}

var (
	uploadSingleRunE   func(*cobra.Command, []string) error
	downloadSingleRunE func(*cobra.Command, []string) error
)

func batchWorkerLimit(workersPerInterface, jobs int) (int, error) {
	if workersPerInterface <= 0 {
		return 0, fmt.Errorf("workers-per-interface must be > 0")
	}
	if jobs <= 0 {
		return 0, fmt.Errorf("--jobs must be > 0")
	}
	if jobs > workersPerInterface {
		return 0, fmt.Errorf("--jobs %d exceeds the workers-per-interface budget %d; increase --workers-per-interface or reduce --jobs", jobs, workersPerInterface)
	}
	return workersPerInterface / jobs, nil
}

func resolveBatchTransferWorkerLimit(cmd *cobra.Command, jobs, override int) (int, error) {
	config, err := auth.ResolveTransferConfig(configPath)
	if err != nil {
		return 0, err
	}
	workers := config.WorkersPerInterface
	if cmd != nil && cmd.Flags().Changed("workers-per-interface") {
		workers = override
	}
	return batchWorkerLimit(workers, jobs)
}

func runBatchUploadCommand(cmd *cobra.Command, args []string) error {
	sources := args[:len(args)-1]
	remoteDir := args[len(args)-1]
	continueOnError := batchContinueOnError(cmd)
	jobs, err := batchJobs(cmd)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	parallelism := jobs
	if parallelism > len(sources) {
		parallelism = len(sources)
	}
	if parallelism > 1 && !continueOnError {
		return &exitError{code: output.ExitArgs, msg: "--jobs > 1 requires --continue-on-error so concurrent failure semantics remain explicit"}
	}
	var validationErr error
	if continueOnError {
		validationErr = validateBatchUploadGlobalSources(sources)
	} else {
		validationErr = validateBatchUploadSources(sources)
	}
	if validationErr != nil {
		return &exitError{code: output.ExitArgs, msg: validationErr.Error()}
	}
	if uploadSingleRunE == nil {
		return &exitError{code: output.ExitError, msg: "upload command is not initialized"}
	}

	workerLimit := 0
	if parallelism > 1 {
		workerLimit, err = resolveBatchTransferWorkerLimit(cmd, parallelism, uploadWorkersPerInterface)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
	}
	successes := make([]map[string]interface{}, 0, len(sources))
	items := make([]batchItemResult, 0, len(sources))
	if parallelism > 1 {
		errorsByIndex := runParallelBatch(len(sources), parallelism, workerLimit, func(i int) error {
			source := sources[i]
			if !jsonOutput {
				fmt.Fprintf(os.Stderr, "[%d/%d] Upload source: %s\n", i+1, len(sources), source)
			}
			return uploadSingleRunE(cmd, []string{source, remoteDir})
		})
		for i, source := range sources {
			itemData := map[string]interface{}{"local_path": source, "remote_dir": remoteDir}
			if errorsByIndex[i] != nil {
				items = append(items, failedBatchItem(source, itemData, errorsByIndex[i]))
				printBatchItemFailure(i, len(sources), "upload "+source, errorsByIndex[i])
				continue
			}
			successes = append(successes, itemData)
			items = append(items, successfulBatchItem(source, itemData))
		}
	} else {
		for i, source := range sources {
			if !jsonOutput {
				fmt.Fprintf(os.Stderr, "[%d/%d] Upload source: %s\n", i+1, len(sources), source)
			}
			itemData := map[string]interface{}{"local_path": source, "remote_dir": remoteDir}
			err := runBatchItem(func() error {
				return uploadSingleRunE(cmd, []string{source, remoteDir})
			})
			if err != nil {
				items = append(items, failedBatchItem(source, itemData, err))
				printBatchItemFailure(i, len(sources), "upload "+source, err)
				if !continueOnError {
					break
				}
				continue
			}
			successes = append(successes, itemData)
			items = append(items, successfulBatchItem(source, itemData))
		}
	}
	base := map[string]interface{}{"remote_dir": remoteDir, "sources": successes, "jobs": parallelism}
	if workerLimit > 0 {
		base["workers_per_interface_per_job"] = workerLimit
	}
	data := batchResultData(len(sources), items, base)
	if batchFailedCount(items) > 0 {
		return batchIncompleteError("batch upload", len(sources), items, data)
	}
	printer.PrintSuccess(data)
	if !jsonOutput {
		fmt.Printf("Batch upload complete: %d source(s) -> %s\n", len(successes), remoteDir)
	}
	return nil
}

func validateBatchUploadGlobalSources(sources []string) error {
	if len(sources) < 2 {
		return nil
	}
	if strings.TrimSpace(uploadSession) != "" {
		return fmt.Errorf("--session cannot be used with multiple upload sources; managed sessions are created per source automatically")
	}
	if uploadContents {
		return fmt.Errorf("--contents cannot be used with multiple upload sources; upload each contents-mode directory separately")
	}
	seen := make(map[string]string, len(sources))
	for _, source := range sources {
		if uploadSourceRequestsContents(source) {
			return fmt.Errorf("trailing-separator contents mode cannot be used in a multi-source upload: %s", source)
		}
		base := filepath.Base(filepath.Clean(source))
		key := localCollisionKey(base)
		if previous, ok := seen[key]; ok {
			return fmt.Errorf("multiple upload sources map to the same remote name %q: %s and %s", base, previous, source)
		}
		seen[key] = source
	}
	return nil
}

func validateBatchUploadSources(sources []string) error {
	if err := validateBatchUploadGlobalSources(sources); err != nil {
		return err
	}
	for _, source := range sources {
		info, err := os.Lstat(source)
		if err != nil {
			return fmt.Errorf("cannot stat local path %q: %w", source, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic-link upload source is not supported: %s", source)
		}
		if info.IsDir() {
			if !uploadRecursive {
				return fmt.Errorf("local path %q is a directory; use --recursive to upload it", source)
			}
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("local upload source must be a regular file or directory: %s", source)
		}
	}
	return nil
}

func runBatchDownloadCommand(cmd *cobra.Command, args []string) error {
	remotePaths := args[:len(args)-1]
	localDir := args[len(args)-1]
	if strings.TrimSpace(downloadSession) != "" {
		return &exitError{code: output.ExitArgs, msg: "--session cannot be used with multiple download sources; managed sessions are created per source automatically"}
	}
	if downloadSingleRunE == nil {
		return &exitError{code: output.ExitError, msg: "download command is not initialized"}
	}
	continueOnError := batchContinueOnError(cmd)
	jobs, err := batchJobs(cmd)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	parallelism := jobs
	if parallelism > len(remotePaths) {
		parallelism = len(remotePaths)
	}
	if parallelism > 1 && !continueOnError {
		return &exitError{code: output.ExitArgs, msg: "--jobs > 1 requires --continue-on-error so concurrent failure semantics remain explicit"}
	}
	plans, err := prepareBatchDownloadPlans(client, remotePaths, localDir)
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
	if batchDownloadHasReadyPlan(plans) {
		if err := os.MkdirAll(localDir, 0755); err != nil {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("cannot create batch download destination %q: %v", localDir, err)}
		}
	}

	workerLimit := 0
	if parallelism > 1 {
		workerLimit, err = resolveBatchTransferWorkerLimit(cmd, parallelism, downloadWorkersPerInterface)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
	}
	successes := make([]map[string]interface{}, 0, len(plans))
	items := make([]batchItemResult, 0, len(plans))
	if parallelism > 1 {
		errorsByIndex := runParallelBatch(len(plans), parallelism, workerLimit, func(i int) error {
			plan := plans[i]
			if plan.Err != nil {
				return plan.Err
			}
			if !jsonOutput {
				fmt.Fprintf(os.Stderr, "[%d/%d] Download source: %s\n", i+1, len(plans), plan.Source.RemotePath)
			}
			return downloadSingleRunE(cmd, []string{plan.Source.RemotePath, plan.Source.LocalPath})
		})
		for i, plan := range plans {
			input := remotePaths[i]
			itemData := map[string]interface{}{"remote_path": input}
			if plan.Err == nil {
				itemData["remote_path"] = plan.Source.RemotePath
				itemData["local_path"] = plan.Source.LocalPath
			}
			if errorsByIndex[i] != nil {
				items = append(items, failedBatchItem(input, itemData, errorsByIndex[i]))
				printBatchItemFailure(i, len(plans), "download "+input, errorsByIndex[i])
				continue
			}
			successes = append(successes, itemData)
			items = append(items, successfulBatchItem(input, itemData))
		}
	} else {
		for i, plan := range plans {
			input := remotePaths[i]
			if plan.Err != nil {
				items = append(items, failedBatchItem(input, map[string]interface{}{"remote_path": input}, plan.Err))
				printBatchItemFailure(i, len(plans), "download "+input, plan.Err)
				continue
			}
			if !jsonOutput {
				fmt.Fprintf(os.Stderr, "[%d/%d] Download source: %s\n", i+1, len(plans), plan.Source.RemotePath)
			}
			itemData := map[string]interface{}{"remote_path": plan.Source.RemotePath, "local_path": plan.Source.LocalPath}
			err := runBatchItem(func() error {
				return downloadSingleRunE(cmd, []string{plan.Source.RemotePath, plan.Source.LocalPath})
			})
			if err != nil {
				items = append(items, failedBatchItem(input, itemData, err))
				printBatchItemFailure(i, len(plans), "download "+input, err)
				if !continueOnError {
					break
				}
				continue
			}
			successes = append(successes, itemData)
			items = append(items, successfulBatchItem(input, itemData))
		}
	}
	base := map[string]interface{}{"local_dir": localDir, "sources": successes, "jobs": parallelism}
	if workerLimit > 0 {
		base["workers_per_interface_per_job"] = workerLimit
	}
	data := batchResultData(len(remotePaths), items, base)
	if batchFailedCount(items) > 0 {
		return batchIncompleteError("batch download", len(remotePaths), items, data)
	}
	printer.PrintSuccess(data)
	if !jsonOutput {
		fmt.Printf("Batch download complete: %d source(s) -> %s\n", len(successes), localDir)
	}
	return nil
}

func prepareBatchDownloadPlans(client downloadCommandClient, remotePaths []string, localDir string) ([]batchDownloadPlan, error) {
	if strings.TrimSpace(localDir) == "" {
		return nil, &exitError{code: output.ExitArgs, msg: "batch download destination directory is empty"}
	}
	if info, err := os.Stat(localDir); err == nil {
		if !info.IsDir() {
			return nil, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("batch download destination %q is not a directory", localDir)}
		}
	} else if !os.IsNotExist(err) {
		return nil, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("cannot inspect batch download destination %q: %v", localDir, err)}
	}

	bases := make([]string, len(remotePaths))
	preparationErrors := make([]error, len(remotePaths))
	seen := make(map[string]string, len(remotePaths))
	for i, remotePath := range remotePaths {
		base, err := remoteBatchBaseName(remotePath)
		if err != nil {
			preparationErrors[i] = &exitError{code: output.ExitArgs, msg: err.Error()}
			continue
		}
		bases[i] = base
		localPath := filepath.Join(localDir, base)
		key := localCollisionKey(filepath.Clean(localPath))
		if previous, ok := seen[key]; ok {
			return nil, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("multiple download sources map to the same local path %q: %s and %s", localPath, previous, remotePath)}
		}
		seen[key] = remotePath
	}
	plans := make([]batchDownloadPlan, len(remotePaths))
	pathResolver := resolver.New(client)
	for i, remotePath := range remotePaths {
		if preparationErrors[i] != nil {
			plans[i] = batchDownloadPlan{Err: preparationErrors[i]}
			continue
		}
		_, isDir, err := pathResolver.ResolvePath(remotePath)
		if err != nil {
			plans[i] = batchDownloadPlan{Err: &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("cannot resolve remote path %s: %v", remotePath, err)}}
			continue
		}
		if isDir && !downloadRecursive {
			plans[i] = batchDownloadPlan{Err: &exitError{code: output.ExitArgs, msg: fmt.Sprintf("remote path %q is a directory; use --recursive to download it", remotePath)}}
			continue
		}
		localPath := filepath.Join(localDir, bases[i])
		if info, statErr := os.Stat(localPath); statErr == nil {
			if isDir != info.IsDir() {
				kind := "file"
				if isDir {
					kind = "directory"
				}
				plans[i] = batchDownloadPlan{Err: &exitError{code: output.ExitArgs, msg: fmt.Sprintf("cannot download remote %s %q onto incompatible local path %q", kind, remotePath, localPath)}}
				continue
			}
		} else if !os.IsNotExist(statErr) {
			plans[i] = batchDownloadPlan{Err: &exitError{code: output.ExitArgs, msg: fmt.Sprintf("cannot inspect local target %q: %v", localPath, statErr)}}
			continue
		}
		plans[i] = batchDownloadPlan{Source: batchDownloadSource{RemotePath: remotePath, LocalPath: localPath}}
	}
	return plans, nil
}

func batchDownloadHasReadyPlan(plans []batchDownloadPlan) bool {
	for _, plan := range plans {
		if plan.Err == nil {
			return true
		}
	}
	return false
}

func prepareBatchDownloadSources(client downloadCommandClient, remotePaths []string, localDir string) ([]batchDownloadSource, error) {
	plans, err := prepareBatchDownloadPlans(client, remotePaths, localDir)
	if err != nil {
		return nil, err
	}
	sources := make([]batchDownloadSource, 0, len(plans))
	for _, plan := range plans {
		if plan.Err != nil {
			return nil, plan.Err
		}
		sources = append(sources, plan.Source)
	}
	if len(sources) > 0 {
		if err := os.MkdirAll(localDir, 0755); err != nil {
			return nil, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("cannot create batch download destination %q: %v", localDir, err)}
		}
	}
	return sources, nil
}

func remoteBatchBaseName(remotePath string) (string, error) {
	clean := pathpkg.Clean(strings.TrimSpace(remotePath))
	if clean == "." || clean == "/" || clean == "" {
		return "", fmt.Errorf("remote root cannot be one source in a multi-source download; download it separately")
	}
	base := pathpkg.Base(clean)
	if base == "." || base == "/" || base == "" {
		return "", fmt.Errorf("cannot determine local basename for remote path %q", remotePath)
	}
	return base, nil
}

func localCollisionKey(value string) string {
	value = filepath.Clean(value)
	if runtime.GOOS == "windows" {
		return strings.ToLower(value)
	}
	return value
}
