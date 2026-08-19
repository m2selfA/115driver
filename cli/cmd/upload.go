package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/auth"
	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/internal/transfer"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/spf13/cobra"
)

var (
	uploadInterfaces string
	uploadChunkSize  string
	uploadTimeout    time.Duration
	uploadRecursive  bool
	uploadSession    string
)

func buildUploadOptions(config auth.TransferConfig, interfacesOverride, chunkSizeOverride string, timeout time.Duration) (uploadpkg.Options, error) {
	if config.WorkersPerInterface != 1 {
		return uploadpkg.Options{}, fmt.Errorf("transfer currently requires workers_per_interface = 1, got %d", config.WorkersPerInterface)
	}
	if timeout < 0 {
		return uploadpkg.Options{}, fmt.Errorf("upload timeout must be >= 0")
	}
	interfaces := strings.TrimSpace(config.Interfaces)
	if strings.TrimSpace(interfacesOverride) != "" {
		interfaces = strings.TrimSpace(interfacesOverride)
	}
	chunkSizeText := strings.TrimSpace(config.ChunkSize)
	if strings.TrimSpace(chunkSizeOverride) != "" {
		chunkSizeText = strings.TrimSpace(chunkSizeOverride)
	}
	chunkSize, err := transfer.ParseByteSize(chunkSizeText)
	if err != nil {
		return uploadpkg.Options{}, fmt.Errorf("invalid upload chunk size %q: %w", chunkSizeText, err)
	}
	if chunkSize < uploadpkg.MinPartSize {
		return uploadpkg.Options{}, fmt.Errorf("upload chunk size must be at least 100KiB")
	}
	health, err := transfer.NewNetworkHealthTracker(transfer.NetworkHealthOptions{
		Cooldown: config.HealthCooldown, CooldownMax: config.HealthCooldownMax,
	})
	if err != nil {
		return uploadpkg.Options{}, err
	}
	return uploadpkg.Options{
		Interfaces: interfaces, ChunkSize: chunkSize, Retries: config.Retries, Timeout: timeout, HealthTracker: health,
	}, nil
}

var uploadCmd = &cobra.Command{
	Use:   "upload <local_path> <remote_dir>",
	Short: "Upload a file or recursively upload a directory",
	Long:  "Upload with 115 rapid-upload first. If OSS data transfer is required, the default automatically selects reachable interfaces and uses one multipart worker per physical interface. Use --recursive for directories; resumable transfer sessions are enabled by transfer.resume.",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		localPath := args[0]
		remoteDir := args[1]

		dirID, err := resolver.ResolveDir(client, remoteDir)
		if err != nil {
			return &exitError{code: output.ExitNotFound, msg: fmt.Sprintf("Remote directory not found: %s", remoteDir)}
		}
		transferConfig, err := auth.ResolveTransferConfig(configPath)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		uploadOptions, err := buildUploadOptions(transferConfig, uploadInterfaces, uploadChunkSize, uploadTimeout)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if strings.TrimSpace(uploadSession) != "" && !transferConfig.Resume {
			return &exitError{code: output.ExitArgs, msg: "--session requires transfer.resume=true"}
		}
		if !jsonOutput {
			uploadOptions.Progress = func(message string) { fmt.Fprintln(os.Stderr, message) }
		}

		stat, err := os.Lstat(localPath)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("Cannot stat local path: %v", err)}
		}
		if stat.Mode()&os.ModeSymlink != 0 {
			return &exitError{code: output.ExitArgs, msg: "Symbolic-link upload sources are not supported"}
		}
		if stat.IsDir() {
			if !uploadRecursive {
				return &exitError{code: output.ExitArgs, msg: "Local path is a directory; use --recursive to upload its contents"}
			}
			summary, err := executeRecursiveUpload(cmd.Context(), client, client, localPath, remoteDir, dirID, transferConfig.Resume, uploadSession, uploadOptions, defaultUploadPipelineDeps())
			if err != nil {
				message := fmt.Sprintf("Upload failed: %v", err)
				if len(summary.Failures) > 0 {
					message = fmt.Sprintf("%s; first failed file %s: %v", message, summary.Failures[0].RelativePath, summary.Failures[0].Err)
				}
				return &exitError{code: output.ExitError, msg: message}
			}
			printer.PrintSuccess(map[string]interface{}{
				"local_path": localPath, "remote_dir": remoteDir, "files": summary.FileCount,
				"succeeded": summary.SucceededCount, "rapid": summary.RapidCount, "resumed": summary.ResumedCount,
				"size": summary.TotalBytes,
			})
			if !jsonOutput {
				fmt.Printf("Upload complete: %d files (%s) -> %s\n", summary.SucceededCount, output.FormatFileSize(summary.TotalBytes), remoteDir)
			}
			return nil
		}
		if !stat.Mode().IsRegular() {
			return &exitError{code: output.ExitArgs, msg: "Local upload source must be a regular file"}
		}

		fileName := filepath.Base(localPath)
		if transferConfig.Resume {
			sessionPath, _, err := deriveTransferSessionPaths("upload", localPath, remoteDir, uploadSession)
			if err != nil {
				return &exitError{code: output.ExitArgs, msg: err.Error()}
			}
			uploadOptions.ResumePath = sessionPath
		}
		if !jsonOutput {
			fmt.Printf("Uploading %s (%s)...\n", fileName, output.FormatFileSize(stat.Size()))
		}
		f, err := os.Open(localPath)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("Cannot open local file: %v", err)}
		}
		defer f.Close()
		uploadResult, err := uploadpkg.UploadFile(cmd.Context(), client, dirID, fileName, stat.Size(), f, uploadOptions)
		if err != nil {
			return &exitError{code: output.ExitError, msg: fmt.Sprintf("Upload failed: %v", err)}
		}
		if uploadOptions.ResumePath != "" {
			if cleanupErr := uploadpkg.RemoveResumeState(uploadOptions.ResumePath); cleanupErr != nil && uploadOptions.Progress != nil {
				uploadOptions.Progress(fmt.Sprintf("Warning: upload succeeded but resume state cleanup failed: %v", cleanupErr))
			}
		}
		interfaces := make([]string, 0, len(uploadResult.NetworkPaths))
		for _, path := range uploadResult.NetworkPaths {
			interfaces = append(interfaces, path.String())
		}
		printer.PrintSuccess(map[string]interface{}{
			"local_path": localPath, "remote_dir": remoteDir, "size": stat.Size(), "rapid": uploadResult.Rapid,
			"multipart": uploadResult.Multipart, "parts": uploadResult.PartCount, "resumed": uploadResult.Resumed,
			"resumed_parts": uploadResult.ResumedParts, "interfaces": interfaces,
		})
		if !jsonOutput {
			fmt.Printf("Upload complete: %s -> %s\n", fileName, remoteDir)
		}
		return nil
	},
}

func init() {
	uploadCmd.Flags().BoolVarP(&uploadRecursive, "recursive", "r", false, "Recursively upload a directory's contents while preserving hierarchy")
	uploadCmd.Flags().StringVar(&uploadSession, "session", "", "Override persistent upload session file path (requires transfer.resume=true)")
	uploadCmd.Flags().StringVar(&uploadInterfaces, "interfaces", "", "Override upload interfaces (auto, or comma-separated interface names/indexes/IPs)")
	uploadCmd.Flags().StringVar(&uploadChunkSize, "chunk-size", "", "Override OSS multipart part size (for example 32MiB)")
	uploadCmd.Flags().DurationVar(&uploadTimeout, "timeout", uploadpkg.DefaultTimeout, "Upload timeout, use 0 to disable")
	rootCmd.AddCommand(uploadCmd)
}
