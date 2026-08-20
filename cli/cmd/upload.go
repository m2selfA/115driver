package cmd

import (
	"fmt"
	"os"
	pathpkg "path"
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
	uploadInterfaces          string
	uploadChunkSize           string
	uploadTimeout             time.Duration
	uploadRecursive           bool
	uploadContents            bool
	uploadSession             string
	uploadWorkersPerInterface int
)

func buildUploadOptions(config auth.TransferConfig, interfacesOverride, chunkSizeOverride string, timeout time.Duration) (uploadpkg.Options, error) {
	if config.WorkersPerInterface <= 0 {
		return uploadpkg.Options{}, fmt.Errorf("transfer.workers_per_interface must be > 0")
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
		Interfaces: interfaces, ChunkSize: chunkSize, Retries: config.Retries, WorkersPerInterface: config.WorkersPerInterface,
		Timeout: timeout, HealthTracker: health, Compatibility: uploadpkg.NewUploadCompatibilityState(),
	}, nil
}

var uploadCmd = &cobra.Command{
	Use:     "upload <local_path> <remote_dir>",
	Short:   "Upload a file or recursively upload a directory",
	Long:    "Upload verifies an existing same-name, same-size remote file by SHA1 before starting rapid-upload/OSS and skips the upload when contents already match. The computed digest is reused if upload is still required. If OSS data transfer is required, reachable interfaces are selected automatically. Recoverable network/finalization failures retry using transfer.retries. When the 115 callback requires OSS SHA1 context, or if 115 rejects multipart verification, upload automatically uses strict sequential OSS mode with interface failover for protocol compatibility. transfer.workers_per_interface controls independent connections per physical interface for ordinary multipart uploads and concurrent recursive files. With --recursive, a source directory is copied by name into remote_dir by default. A trailing slash on local_path (or a trailing backslash on Windows), or --contents, uploads only the directory contents directly into remote_dir. Resumable transfer sessions are enabled by transfer.resume.",
	Example: "  # Preserve the source directory name: /remote/dir/source/...\n  115driver upload -r ./source /remote/dir\n\n  # Upload only source contents: /remote/dir/...\n  115driver upload -r ./source/ /remote/dir\n  115driver upload -r --contents ./source /remote/dir",
	Args:    cobra.ExactArgs(2),
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
		if cmd.Flags().Changed("workers-per-interface") {
			if uploadWorkersPerInterface <= 0 {
				return &exitError{code: output.ExitArgs, msg: "--workers-per-interface must be > 0"}
			}
			transferConfig.WorkersPerInterface = uploadWorkersPerInterface
		}
		uploadOptions, err := buildUploadOptions(transferConfig, uploadInterfaces, uploadChunkSize, uploadTimeout)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		if strings.TrimSpace(uploadSession) != "" && !transferConfig.Resume {
			return &exitError{code: output.ExitArgs, msg: "--session requires transfer.resume=true"}
		}
		finishUploadProgress := configureCLIUploadProgress(&uploadOptions)
		defer finishUploadProgress()

		stat, err := os.Lstat(localPath)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("Cannot stat local path: %v", err)}
		}
		if stat.Mode()&os.ModeSymlink != 0 {
			return &exitError{code: output.ExitArgs, msg: "Symbolic-link upload sources are not supported"}
		}
		if stat.IsDir() {
			if !uploadRecursive {
				return &exitError{code: output.ExitArgs, msg: "Local path is a directory; use --recursive to upload it"}
			}
			contentsMode := uploadContents || uploadSourceRequestsContents(localPath)
			summary, err := executeRecursiveUpload(cmd.Context(), client, client, localPath, remoteDir, dirID, contentsMode, transferConfig.Resume, uploadSession, uploadOptions, defaultUploadPipelineDeps())
			if err != nil {
				message := fmt.Sprintf("Upload failed: %v", err)
				if len(summary.Failures) > 0 {
					message = fmt.Sprintf("%s; first failed file %s: %v", message, summary.Failures[0].RelativePath, summary.Failures[0].Err)
				}
				return &exitError{code: output.ExitError, msg: message}
			}
			finishUploadProgress()
			printer.PrintSuccess(map[string]interface{}{
				"local_path": localPath, "remote_dir": remoteDir, "destination": summary.RemoteDir, "contents": contentsMode, "files": summary.FileCount,
				"succeeded": summary.SucceededCount, "uploaded": summary.UploadedCount, "verified": summary.VerifiedCount, "skipped": summary.SkippedCount,
				"rapid": summary.RapidCount, "resumed": summary.ResumedCount, "size": summary.TotalBytes, "transferred_bytes": summary.TransferredBytes,
			})
			if !jsonOutput {
				fmt.Printf("Upload complete: %d files (%s), %d uploaded, %d verified/skipped -> %s\n", summary.SucceededCount, output.FormatFileSize(summary.TotalBytes), summary.UploadedCount, summary.SkippedCount, summary.RemoteDir)
			}
			return nil
		}
		if uploadContents {
			return &exitError{code: output.ExitArgs, msg: "--contents is only valid for recursive directory uploads"}
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
		f, err := os.Open(localPath)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("Cannot open local file: %v", err)}
		}
		defer f.Close()
		resumeStatePresent, err := uploadResumeStatePresent(uploadOptions.ResumePath)
		if err != nil {
			return &exitError{code: output.ExitError, msg: fmt.Sprintf("Cannot inspect upload resume state: %v", err)}
		}
		if !resumeStatePresent {
			entries, err := client.List(dirID)
			if err != nil {
				return &exitError{code: output.ExitError, msg: fmt.Sprintf("Cannot inspect remote directory before upload: %v", err)}
			}
			if entries == nil {
				return &exitError{code: output.ExitError, msg: "Cannot inspect remote directory before upload: empty listing response"}
			}
			if remoteUploadHasComparableFile(*entries, fileName, stat.Size()) {
				if !jsonOutput {
					fmt.Printf("Verifying existing %s by SHA1...\n", fileName)
				}
				digest, identical, err := prepareExistingUploadDigest(f, stat.Size(), *entries, fileName)
				if err != nil {
					return &exitError{code: output.ExitError, msg: fmt.Sprintf("Verify existing remote file failed: %v", err)}
				}
				uploadOptions.PreparedDigest = digest
				if identical {
					finishUploadProgress()
					remotePath := pathpkg.Join(remoteDir, fileName)
					if strings.HasPrefix(remoteDir, "/") && !strings.HasPrefix(remotePath, "/") {
						remotePath = "/" + remotePath
					}
					printer.PrintSuccess(map[string]interface{}{
						"local_path": localPath, "remote_dir": remoteDir, "destination": remotePath, "size": stat.Size(), "sha1": digest.SHA1,
						"uploaded": false, "verified": true, "skipped": true, "rapid": false, "resumed": false,
					})
					if !jsonOutput {
						fmt.Printf("Upload skipped: %s already matches %s\n", fileName, remotePath)
					}
					return nil
				}
			}
		}
		if !jsonOutput {
			fmt.Printf("Uploading %s (%s)...\n", fileName, output.FormatFileSize(stat.Size()))
		}
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
		finishUploadProgress()
		printer.PrintSuccess(map[string]interface{}{
			"local_path": localPath, "remote_dir": remoteDir, "size": stat.Size(), "sha1": uploadResult.SHA1,
			"uploaded": !uploadResult.Skipped, "verified": uploadResult.Verified, "skipped": uploadResult.Skipped, "rapid": uploadResult.Rapid,
			"multipart": uploadResult.Multipart, "parts": uploadResult.PartCount, "resumed": uploadResult.Resumed,
			"resumed_parts": uploadResult.ResumedParts, "interfaces": interfaces,
		})
		if !jsonOutput {
			if uploadResult.Skipped {
				fmt.Printf("Upload skipped: %s already matches %s\n", fileName, pathpkg.Join(remoteDir, fileName))
			} else {
				fmt.Printf("Upload complete: %s -> %s\n", fileName, remoteDir)
			}
		}
		return nil
	},
}

func init() {
	uploadCmd.Flags().BoolVarP(&uploadRecursive, "recursive", "r", false, "Recursively upload a directory; preserves the source directory name by default")
	uploadCmd.Flags().BoolVar(&uploadContents, "contents", false, "Upload only a recursive source directory's contents directly into remote_dir")
	uploadCmd.Flags().StringVarP(&uploadSession, "session", "s", "", "Override persistent upload session file path (requires transfer.resume=true)")
	uploadCmd.Flags().StringVar(&uploadInterfaces, "interfaces", "", "Override upload interfaces (auto, or comma-separated interface names/indexes/IPs)")
	uploadCmd.Flags().StringVar(&uploadChunkSize, "chunk-size", "", "Override OSS multipart part size (for example 32MiB)")
	uploadCmd.Flags().IntVar(&uploadWorkersPerInterface, "workers-per-interface", 0, "Override independent connections per physical interface")
	uploadCmd.Flags().DurationVar(&uploadTimeout, "timeout", uploadpkg.DefaultTimeout, "Upload timeout, use 0 to disable")
	rootCmd.AddCommand(uploadCmd)
}
