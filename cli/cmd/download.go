package cmd

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	transferpkg "github.com/SheltonZhu/115driver/internal/transfer"
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
)

var downloadCmd = &cobra.Command{
	Use:   "download <remote_path> <local_path>",
	Short: "Download a file or recursively download a directory",
	Long:  "Download through the configured transfer strategy. 'file' assigns whole files across interface connection slots; 'chunk' splits each file into HTTP byte ranges and can use multiple connections on every Range-capable interface. transfer.workers_per_interface controls connection slots per physical interface. Recoverable range/file failures resume automatically. For directories, use --recursive; directory contents are written below local_path and transfer.resume maintains a persistent resumable directory session.",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateDownloadTimeout(downloadTimeout); err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		transferConfig, err := auth.ResolveTransferConfig(configPath)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if cmd.Flags().Changed("workers-per-interface") {
			if downloadWorkersPerInterface <= 0 {
				return &exitError{code: output.ExitArgs, msg: "--workers-per-interface must be > 0"}
			}
			transferConfig.WorkersPerInterface = downloadWorkersPerInterface
		}
		if strings.TrimSpace(downloadInterfaces) != "" {
			transferConfig.Interfaces = strings.TrimSpace(downloadInterfaces)
		}
		if strings.TrimSpace(downloadStrategy) != "" {
			transferConfig.Strategy = strings.ToLower(strings.TrimSpace(downloadStrategy))
		}
		chunkSizeText := transferConfig.ChunkSize
		if strings.TrimSpace(downloadChunkSize) != "" {
			chunkSizeText = strings.TrimSpace(downloadChunkSize)
		}
		chunkSize, err := transferpkg.ParseByteSize(chunkSizeText)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("invalid transfer chunk size %q: %v", chunkSizeText, err)}
		}

		options := downloadCommandOptions{
			Recursive:           downloadRecursive,
			Timeout:             downloadTimeout,
			Interfaces:          transferConfig.Interfaces,
			Strategy:            transferConfig.Strategy,
			WorkersPerInterface: transferConfig.WorkersPerInterface,
			ProbeCacheTTL:       transferConfig.ProbeCacheTTL,
			Retries:             transferConfig.Retries,
			ChunkSize:           chunkSize,
			HealthCooldown:      transferConfig.HealthCooldown,
			HealthCooldownMax:   transferConfig.HealthCooldownMax,
			Resume:              transferConfig.Resume,
			SessionPath:         downloadSession,
			URLRefreshes:        transferConfig.URLRefreshes,
		}
		if !jsonOutput {
			options.Progress = func(message string) {
				fmt.Fprintln(os.Stderr, message)
			}
		}

		summary, err := executeDownloadCommand(cmd.Context(), client, args[0], args[1], options, defaultDownloadPipelineDeps())
		if err != nil {
			exitCode := output.ExitError
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
	},
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
	downloadCmd.Flags().DurationVar(&downloadTimeout, "timeout", defaultDownloadTimeout, "Download timeout per file, use 0 to disable")
	downloadCmd.Flags().BoolVarP(&downloadRecursive, "recursive", "r", false, "Recursively download a directory into local_path")
	downloadCmd.Flags().StringVarP(&downloadSession, "session", "s", "", "Override persistent recursive-download session file path (requires transfer.resume=true)")
	downloadCmd.Flags().StringVar(&downloadInterfaces, "interfaces", "", "Override transfer interfaces (auto, or comma-separated interface names/indexes/IPs)")
	downloadCmd.Flags().StringVar(&downloadStrategy, "strategy", "", "Override transfer strategy (file or chunk)")
	downloadCmd.Flags().StringVar(&downloadChunkSize, "chunk-size", "", "Override chunk strategy range size (for example 32MiB)")
	downloadCmd.Flags().IntVar(&downloadWorkersPerInterface, "workers-per-interface", 0, "Override independent connections per physical interface")
	rootCmd.AddCommand(downloadCmd)
}
