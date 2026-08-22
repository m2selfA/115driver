package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/spf13/cobra"
)

const defaultDownloadTimeout = 2 * time.Hour

var (
	downloadTimeout             = defaultDownloadTimeout
	downloadRecursive           bool
	downloadInterfaces          string
	downloadStrategy            string
	downloadChunkSize           string
	downloadSession             string
	downloadWorkersPerInterface int
	downloadDryRun              bool
)

var downloadCmd = &cobra.Command{
	Use:   "download <remote_path>... <local_path>",
	Short: "Download one or more files or directories",
	Long:  "Download one or more remote sources through the configured transfer strategy. --from-file FILE reads additional remote paths one per line; use --from-file - for stdin. Explicit positional sources are processed before file-provided sources. --dry-run resolves remote trees, computes destination mappings and byte counts, detects local type conflicts, previews resume/session paths, and validates batch worker budgeting without creating local directories, session metadata, download parts, or network transfers; interface/CDN probing is deferred. With multiple sources, local_path is a destination directory and every source is placed below it using its remote basename; duplicate destination basenames are rejected before transfer. Batch sources are processed sequentially by default. With --continue-on-error, --jobs N can run several sources concurrently; workers-per-interface is treated as a shared per-interface budget and divided across concurrent targets instead of multiplied by N. 'file' assigns whole files across interface connection slots; 'chunk' splits each file into HTTP byte ranges and can use multiple connections on every Range-capable interface. transfer.workers_per_interface controls connection slots per physical interface. Recoverable range/file failures resume automatically. For directories, use --recursive. Explicit --session is intentionally limited to a single recursive directory download; managed sessions remain available automatically for batch transfers.",
	Args:  downloadInputArgs,
	RunE:  runDownloadCommand,
}

func downloadInputArgs(cmd *cobra.Command, args []string) error {
	if err := transferSourceArgs(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	jobs, err := batchJobs(cmd)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if err := validateStaticTransferBatchPolicy(cmd, args, jobs, "downloads"); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if err := validateDownloadTimeout(downloadTimeout); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if commandFlagChanged(cmd, "workers-per-interface") && downloadWorkersPerInterface <= 0 {
		return &exitError{code: output.ExitArgs, msg: "--workers-per-interface must be > 0"}
	}
	strategy := strings.ToLower(strings.TrimSpace(downloadStrategy))
	if strategy != "" && strategy != "file" && strategy != "chunk" {
		return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("unsupported transfer strategy %q; use \"file\" or \"chunk\"", downloadStrategy)}
	}
	if chunkSizeText := strings.TrimSpace(downloadChunkSize); chunkSizeText != "" {
		chunkSize, err := transfer.ParseByteSize(chunkSizeText)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("invalid transfer chunk size %q: %v", chunkSizeText, err)}
		}
		if chunkSize <= 0 {
			return &exitError{code: output.ExitArgs, msg: "transfer chunk size must be > 0"}
		}
	}
	if strings.TrimSpace(downloadSession) != "" && !downloadRecursive {
		return &exitError{code: output.ExitArgs, msg: "--session is only used for recursive directory downloads"}
	}
	return nil
}

func runDownloadCommand(cmd *cobra.Command, args []string) error {
	jobs, err := batchJobs(cmd)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	expandedArgs, err := expandTransferSourceArgs(cmd, args)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if downloadDryRun {
		return runDownloadDryRun(cmd, expandedArgs, jobs)
	}
	if len(expandedArgs) > 2 {
		return runBatchDownloadCommand(cmd, expandedArgs)
	}
	if jobs > 1 {
		return &exitError{code: output.ExitArgs, msg: "--jobs > 1 is only valid for multi-source downloads"}
	}
	if downloadSingleRunE == nil {
		return &exitError{code: output.ExitError, msg: "download command is not initialized"}
	}
	return downloadSingleRunE(cmd, expandedArgs)
}

func runSingleDownloadCommand(cmd *cobra.Command, args []string) error {
	options, err := resolveDownloadCommandOptions(cmd, 0)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if !jsonOutput {
		if batchParallelActive() {
			remoteLabel := args[0]
			options.Progress = func(message string) {
				fmt.Fprintf(os.Stderr, "[%s] %s\n", remoteLabel, message)
			}
		} else {
			options.Progress = func(message string) {
				fmt.Fprintln(os.Stderr, message)
			}
		}
	}

	summary, err := executeDownloadCommand(cmd.Context(), client, args[0], args[1], options, defaultDownloadPipelineDeps())
	if err != nil {
		exitCode := classifyRemoteError(err, output.ExitError)
		message := fmt.Sprintf("Download failed: %v", err)
		switch {
		case errors.Is(err, errDownloadRemoteNotFound):
			exitCode = output.ExitNotFound
			message = err.Error()
		case errors.Is(err, errDownloadUsage):
			exitCode = output.ExitArgs
			message = err.Error()
		}
		if len(summary.Failures) > 0 {
			first := summary.Failures[0]
			message = fmt.Sprintf("%s; first failed file %s: %v", message, first.RemotePath, first.Err)
		}
		return &exitError{code: exitCode, msg: message}
	}

	printer.PrintSuccess(map[string]interface{}{
		"remote_path": summary.RemotePath,
		"local_path":  summary.LocalPath,
		"strategy":    summary.Strategy,
		"interfaces":  summary.Interfaces,
		"files":       summary.FileCount,
		"succeeded":   summary.SucceededCount,
		"resumed":     summary.ResumedCount,
		"size":        summary.TotalBytes,
	})
	if !jsonOutput {
		if summary.FileCount <= 1 {
			fmt.Printf("Download complete: %s\n", summary.LocalPath)
		} else {
			fmt.Printf("Download complete: %d files (%s) -> %s\n", summary.SucceededCount, output.FormatFileSize(summary.TransferredBytes), summary.LocalPath)
		}
	}
	return nil
}

func resolveDownloadTargetPath(localTarget, fileName string) string {
	return resolver.ResolveLocalDownloadPath(localTarget, fileName)
}

func validateDownloadTimeout(timeout time.Duration) error {
	if timeout < 0 {
		return fmt.Errorf("timeout must be >= 0")
	}
	return nil
}

func init() {
	downloadSingleRunE = runSingleDownloadCommand
	downloadCmd.Flags().DurationVar(&downloadTimeout, "timeout", defaultDownloadTimeout, "Download timeout per file, use 0 to disable")
	downloadCmd.Flags().BoolVarP(&downloadRecursive, "recursive", "r", false, "Recursively download a directory into local_path")
	downloadCmd.Flags().StringVarP(&downloadSession, "session", "s", "", "Override persistent recursive-download session file path (requires transfer.resume=true)")
	downloadCmd.Flags().StringVar(&downloadInterfaces, "interfaces", "", "Override transfer interfaces (auto, or comma-separated interface names/indexes/IPs)")
	downloadCmd.Flags().StringVar(&downloadStrategy, "strategy", "", "Override transfer strategy (file or chunk)")
	downloadCmd.Flags().StringVar(&downloadChunkSize, "chunk-size", "", "Override chunk strategy range size (for example 32MiB)")
	downloadCmd.Flags().IntVar(&downloadWorkersPerInterface, "workers-per-interface", 0, "Override independent connections per physical interface")
	addContinueOnErrorFlag(downloadCmd)
	addBatchJobsFlag(downloadCmd)
	addBatchFromFileFlag(downloadCmd)
	downloadCmd.Flags().BoolVar(&downloadDryRun, "dry-run", false, "Plan and validate downloads without creating sessions, local paths, or transferring data")
	rootCmd.AddCommand(downloadCmd)
}
