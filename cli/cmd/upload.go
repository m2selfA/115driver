package cmd

import (
	"errors"
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
	uploadDryRun              bool
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

func resolveUploadCommandOptions(cmd *cobra.Command, workerLimit int) (auth.TransferConfig, uploadpkg.Options, error) {
	config, err := auth.ResolveTransferConfig(configPath)
	if err != nil {
		return auth.TransferConfig{}, uploadpkg.Options{}, err
	}
	if workerLimit > 0 {
		config.WorkersPerInterface = workerLimit
	} else {
		if commandFlagChanged(cmd, "workers-per-interface") {
			if uploadWorkersPerInterface <= 0 {
				return auth.TransferConfig{}, uploadpkg.Options{}, errors.New("--workers-per-interface must be > 0")
			}
			config.WorkersPerInterface = uploadWorkersPerInterface
		}
		config.WorkersPerInterface = applyParallelBatchWorkerLimit(config.WorkersPerInterface)
	}
	options, err := buildUploadOptions(config, uploadInterfaces, uploadChunkSize, uploadTimeout)
	if err != nil {
		return auth.TransferConfig{}, uploadpkg.Options{}, err
	}
	if strings.TrimSpace(uploadSession) != "" && !config.Resume {
		return auth.TransferConfig{}, uploadpkg.Options{}, errors.New("--session requires transfer.resume=true")
	}
	return config, options, nil
}

var uploadCmd = &cobra.Command{
	Use:     "upload <local_path>... <remote_dir>",
	Short:   "Upload one or more files or directories",
	Long:    "Upload one or more local sources into remote_dir. --from-file FILE reads additional source paths one per line; use --from-file - for stdin. Explicit positional sources are processed before file-provided sources. --dry-run resolves the destination, scans local metadata, inspects remote directory conflicts, previews resume/session paths, and validates batch worker budgeting without creating sessions, remote directories, or uploads; network probing and SHA1 verification are deferred. Multiple sources are processed sequentially by default. With --continue-on-error, --jobs N can run several sources concurrently; the configured workers-per-interface value is treated as a shared per-interface budget and divided across concurrent targets instead of multiplied by N. Upload verifies an existing same-name, same-size remote file by SHA1 before starting rapid-upload/OSS and skips the upload when contents already match. The computed digest is reused if upload is still required. If OSS data transfer is required, reachable interfaces are selected automatically. Recoverable network/finalization failures retry using transfer.retries. When the 115 callback requires OSS SHA1 context, or if 115 rejects multipart verification, upload automatically uses strict sequential OSS mode with interface failover for protocol compatibility. transfer.workers_per_interface controls independent connections per physical interface for ordinary multipart uploads and concurrent recursive files. With --recursive, a source directory is copied by name into remote_dir by default. A trailing slash on local_path (or a trailing backslash on Windows), or --contents, uploads only the directory contents directly into remote_dir for a single source. Explicit --session and contents-mode directory uploads are intentionally rejected for multi-source batches because one state path or merged directory root would be ambiguous. Resumable transfer sessions are enabled by transfer.resume.",
	Example: "  # Upload several files into one remote directory\n  115driver upload a.bin b.bin /remote/dir\n\n  # Read a large source list from a file or stdin\n  115driver upload --from-file files.txt /remote/dir\n  Get-Content files.txt | 115driver upload --from-file - /remote/dir\n\n  # Upload several directories, preserving each source directory name\n  115driver upload -r ./photos ./docs /remote/dir\n\n  # Preserve one source directory name: /remote/dir/source/...\n  115driver upload -r ./source /remote/dir\n\n  # Upload only one source directory's contents: /remote/dir/...\n  115driver upload -r ./source/ /remote/dir\n  115driver upload -r --contents ./source /remote/dir",
	Args:    uploadInputArgs,
	RunE:    runUploadCommand,
}

func uploadInputArgs(cmd *cobra.Command, args []string) error {
	if err := transferSourceArgs(cmd, args); err != nil {
		return normalizeArgumentError(err)
	}
	jobs, err := batchJobs(cmd)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if err := validateStaticTransferBatchPolicy(cmd, args, jobs, "uploads"); err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if uploadTimeout < 0 {
		return &exitError{code: output.ExitArgs, msg: "upload timeout must be >= 0"}
	}
	if commandFlagChanged(cmd, "workers-per-interface") && uploadWorkersPerInterface <= 0 {
		return &exitError{code: output.ExitArgs, msg: "--workers-per-interface must be > 0"}
	}
	if chunkSizeText := strings.TrimSpace(uploadChunkSize); chunkSizeText != "" {
		chunkSize, err := transfer.ParseByteSize(chunkSizeText)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("invalid upload chunk size %q: %v", chunkSizeText, err)}
		}
		if chunkSize < uploadpkg.MinPartSize {
			return &exitError{code: output.ExitArgs, msg: "upload chunk size must be at least 100KiB"}
		}
	}
	if uploadContents && !uploadRecursive {
		return &exitError{code: output.ExitArgs, msg: "--contents requires --recursive"}
	}
	return nil
}

func runUploadCommand(cmd *cobra.Command, args []string) error {
	jobs, err := batchJobs(cmd)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	expandedArgs, err := expandTransferSourceArgs(cmd, args)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	if uploadDryRun {
		return runUploadDryRun(cmd, expandedArgs, jobs)
	}
	if len(expandedArgs) > 2 {
		return runBatchUploadCommand(cmd, expandedArgs)
	}
	if jobs > 1 {
		return &exitError{code: output.ExitArgs, msg: "--jobs > 1 is only valid for multi-source uploads"}
	}
	if uploadSingleRunE == nil {
		return &exitError{code: output.ExitError, msg: "upload command is not initialized"}
	}
	return uploadSingleRunE(cmd, expandedArgs)
}

func runSingleUploadCommand(cmd *cobra.Command, args []string) error {
	localPath := args[0]
	remoteDir := args[1]

	dirID, err := resolver.ResolveDir(client, remoteDir)
	if err != nil {
		return &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot resolve remote directory %s: %v", remoteDir, err)}
	}
	transferConfig, uploadOptions, err := resolveUploadCommandOptions(cmd, 0)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	finishUploadProgress := configureCLIUploadProgress(&uploadOptions, localPath)
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
			return &exitError{code: classifyRemoteError(err, output.ExitError), msg: message}
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
	var sessionResolution transferSessionResolution
	if transferConfig.Resume {
		sessionResolution, err = resolveTransferSessionPaths("upload", "file", localPath, remoteDir, "multipart", "single-file", uploadSession)
		if err != nil {
			return &exitError{code: output.ExitArgs, msg: err.Error()}
		}
		uploadOptions.ResumePath = sessionResolution.SessionPath
		defer sessionResolution.closeLock()
	}
	f, err := os.Open(localPath)
	if err != nil {
		return &exitError{code: output.ExitArgs, msg: fmt.Sprintf("Cannot open local file: %v", err)}
	}
	defer f.Close()
	if transferConfig.Resume {
		needsLegacyImport, err := legacyTransferSessionImportNeeded(sessionResolution)
		if err != nil {
			return &exitError{code: output.ExitError, msg: fmt.Sprintf("Inspect legacy upload session failed: %v", err)}
		}
		if needsLegacyImport {
			digest, err := uploadpkg.PrepareFileDigest(f, stat.Size())
			if err != nil {
				return &exitError{code: output.ExitError, msg: fmt.Sprintf("Prepare upload resume identity failed: %v", err)}
			}
			uploadOptions.PreparedDigest = digest
			if err := importLegacyTransferSession(&sessionResolution, func(path string) (bool, error) {
				return uploadpkg.ValidateResumeStateIdentity(path, dirID, fileName, stat.Size(), digest.SHA1)
			}); err != nil {
				return &exitError{code: output.ExitError, msg: fmt.Sprintf("Import legacy upload session failed: %v", err)}
			}
		}
	}
	resumeStatePresent, err := uploadResumeStatePresent(uploadOptions.ResumePath)
	if err != nil {
		return &exitError{code: output.ExitError, msg: fmt.Sprintf("Cannot inspect upload resume state: %v", err)}
	}
	if !resumeStatePresent {
		entries, err := listRemoteDirectoryReadOnly(client, dirID)
		if err != nil {
			return &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot inspect remote directory before upload: %v", err)}
		}
		if entries == nil {
			return &exitError{code: output.ExitError, msg: "Cannot inspect remote directory before upload: empty listing response"}
		}
		if remoteUploadHasComparableFile(*entries, fileName, stat.Size()) {
			if !jsonOutput {
				fmt.Printf("Verifying existing %s by SHA1...\n", fileName)
			}
			digest := uploadOptions.PreparedDigest
			identical := false
			if digest == nil {
				digest, identical, err = prepareExistingUploadDigest(f, stat.Size(), *entries, fileName)
				if err != nil {
					return &exitError{code: output.ExitError, msg: fmt.Sprintf("Verify existing remote file failed: %v", err)}
				}
				uploadOptions.PreparedDigest = digest
			} else {
				identical = remoteUploadFileExists(*entries, fileName, stat.Size(), digest.SHA1)
			}
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
				if err := cleanupResolvedTransferSession(sessionResolution); err != nil {
					return &exitError{code: output.ExitError, msg: fmt.Sprintf("Upload already matches but session cleanup failed: %v", err)}
				}
				return nil
			}
		}
	}
	if !jsonOutput {
		fmt.Printf("Uploading %s (%s)...\n", fileName, output.FormatFileSize(stat.Size()))
	}
	uploadResult, err := uploadpkg.UploadFile(cmd.Context(), client, dirID, fileName, stat.Size(), f, uploadOptions)
	if err == nil {
		commitLegacyTransferSessionImport(sessionResolution)
	}
	if err != nil {
		return &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Upload failed: %v", err)}
	}
	if uploadOptions.ResumePath != "" {
		if cleanupErr := uploadpkg.RemoveResumeState(uploadOptions.ResumePath); cleanupErr != nil && uploadOptions.Progress != nil {
			uploadOptions.Progress(fmt.Sprintf("Warning: upload succeeded but resume state cleanup failed: %v", cleanupErr))
		}
	}
	if cleanupErr := cleanupResolvedTransferSession(sessionResolution); cleanupErr != nil && uploadOptions.Progress != nil {
		uploadOptions.Progress(fmt.Sprintf("Warning: upload succeeded but managed session cleanup failed: %v", cleanupErr))
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
}

func init() {
	uploadSingleRunE = runSingleUploadCommand
	uploadCmd.Flags().BoolVarP(&uploadRecursive, "recursive", "r", false, "Recursively upload a directory; preserves the source directory name by default")
	uploadCmd.Flags().BoolVar(&uploadContents, "contents", false, "Upload only a recursive source directory's contents directly into remote_dir")
	uploadCmd.Flags().StringVarP(&uploadSession, "session", "s", "", "Override persistent upload session file path (requires transfer.resume=true)")
	uploadCmd.Flags().StringVar(&uploadInterfaces, "interfaces", "", "Override upload interfaces (auto, or comma-separated interface names/indexes/IPs)")
	uploadCmd.Flags().StringVar(&uploadChunkSize, "chunk-size", "", "Override OSS multipart part size (for example 32MiB)")
	uploadCmd.Flags().IntVar(&uploadWorkersPerInterface, "workers-per-interface", 0, "Override independent connections per physical interface")
	uploadCmd.Flags().DurationVar(&uploadTimeout, "timeout", uploadpkg.DefaultTimeout, "Upload timeout, use 0 to disable")
	addContinueOnErrorFlag(uploadCmd)
	addBatchJobsFlag(uploadCmd)
	addBatchFromFileFlag(uploadCmd)
	uploadCmd.Flags().BoolVar(&uploadDryRun, "dry-run", false, "Plan and validate uploads without creating sessions, directories, or transferring data")
	rootCmd.AddCommand(uploadCmd)
}
