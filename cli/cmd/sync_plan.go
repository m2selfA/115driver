package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

const (
	syncPlanMode          = syncplanpkg.ModeConservative
	syncDirectionBoth     = syncplanpkg.DirectionBoth
	syncDirectionUpload   = syncplanpkg.DirectionUpload
	syncDirectionDownload = syncplanpkg.DirectionDownload
	syncConflictError     = syncplanpkg.ConflictError
	syncConflictLocal     = syncplanpkg.ConflictLocal
	syncConflictRemote    = syncplanpkg.ConflictRemote
)

type syncPlanOptions = syncplanpkg.Options

func resolveSyncPlanOptions(direction, conflictPolicy string) (syncPlanOptions, error) {
	return syncplanpkg.ResolveOptions(direction, conflictPolicy)
}

func resolveSyncPlanOptionsWithDelete(direction, conflictPolicy string, deleteExtraneous bool) (syncPlanOptions, error) {
	return syncplanpkg.ResolveOptionsWithDelete(direction, conflictPolicy, deleteExtraneous)
}

type syncPlanClient interface {
	remotePathResolveClient
	GetFile(fileID string) (*driver.File, error)
}

type syncTreeEntry = syncplanpkg.Entry
type syncPlanItem = syncplanpkg.Item
type syncPlan = syncplanpkg.Plan

func buildSyncPlan(planClient syncPlanClient, localRoot, remoteRoot string) (syncPlan, error) {
	return buildSyncPlanWithOptions(planClient, localRoot, remoteRoot, syncPlanOptions{Direction: syncDirectionBoth, ConflictPolicy: syncConflictError})
}

func buildSyncPlanWithOptions(planClient syncPlanClient, localRoot, remoteRoot string, requested syncPlanOptions) (syncPlan, error) {
	options, optionErr := resolveSyncPlanOptionsWithDelete(requested.Direction, requested.ConflictPolicy, requested.DeleteExtraneous)
	plan := syncPlan{
		Operation: "sync", DryRun: true, Mode: syncPlanMode, Direction: options.Direction, ConflictPolicy: options.ConflictPolicy, DeleteExtraneous: options.DeleteExtraneous, RemoteRoot: remoteRoot,
	}
	if optionErr != nil {
		return plan, &exitError{code: output.ExitArgs, msg: optionErr.Error()}
	}
	if planClient == nil {
		return plan, &exitError{code: output.ExitError, msg: "sync plan client is nil"}
	}
	localTree, err := scanLocalUploadTree(localRoot)
	if err != nil {
		return plan, &exitError{code: output.ExitArgs, msg: fmt.Sprintf("scan local sync root: %v", err)}
	}
	plan.LocalRoot = localTree.Root
	remoteRootID, err := resolver.ResolveDir(planClient, remoteRoot)
	if err != nil {
		return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: fmt.Sprintf("Cannot resolve remote sync directory %s: %v", remoteRoot, err)}
	}
	plan.RemoteRootID = remoteRootID
	plan.RemoteRoot = canonicalSyncRemoteRoot(remoteRoot)

	localEntries, err := collectLocalSyncEntries(localTree, plan.RemoteRoot)
	if err != nil {
		return plan, &exitError{code: output.ExitArgs, msg: err.Error()}
	}
	remoteEntries, err := collectRemoteSyncEntries(planClient, remoteRootID, plan.RemoteRoot, plan.LocalRoot)
	if err != nil {
		return plan, &exitError{code: classifyRemoteError(err, output.ExitError), msg: err.Error()}
	}
	planned, err := syncplanpkg.Build(localEntries, remoteEntries, plan.LocalRoot, plan.RemoteRoot, plan.RemoteRootID, options, syncplanpkg.Resolvers{
		RemoteSHA1: func(entry syncplanpkg.Entry) (string, error) {
			return resolveSyncRemoteSHA1(planClient, entry)
		},
		LocalDigest: func(entry syncplanpkg.Entry) (*uploadpkg.PreparedDigest, error) {
			return checksumSyncLocalFile(entry)
		},
	})
	if err != nil {
		return plan, &exitError{code: output.ExitError, msg: err.Error()}
	}
	return planned, nil
}

func syncPlanFingerprint(plan syncPlan) string {
	return syncplanpkg.Fingerprint(plan)
}

func normalizeSyncPlanID(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("invalid --expect-plan %q: expected a 64-character SHA-256 plan ID", value)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("invalid --expect-plan %q: expected hexadecimal SHA-256 plan ID", value)
	}
	return value, nil
}

func validateSyncExpectedPlanID(plan syncPlan, expected string) error {
	if expected == "" {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(plan.PlanID), expected) {
		return fmt.Errorf("sync plan ID mismatch: expected %s but freshly planned %s; review a new --dry-run plan before execution", expected, plan.PlanID)
	}
	return nil
}

func syncPlanChangeCount(plan syncPlan) int {
	return syncplanpkg.ChangeCount(plan)
}

func validateSyncCheck(plan syncPlan) error {
	return syncplanpkg.ValidateCheck(plan)
}

func collectLocalSyncEntries(tree localUploadTree, remoteRoot string) (map[string]syncTreeEntry, error) {
	entries := make(map[string]syncTreeEntry, len(tree.Directories)+len(tree.Files))
	for _, relative := range tree.Directories {
		if relative == "" {
			continue
		}
		relative = filepath.ToSlash(relative)
		if err := validateSyncRelativePath(relative); err != nil {
			return nil, fmt.Errorf("local path %q cannot be synced: %w", relative, err)
		}
		localPath := filepath.Join(tree.Root, filepath.FromSlash(relative))
		info, err := os.Lstat(localPath)
		if err != nil {
			return nil, fmt.Errorf("inspect local sync directory %q: %w", relative, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("local sync directory %q changed after tree scan", relative)
		}
		entry := syncTreeEntry{
			RelativePath: relative, Kind: "directory", LocalPath: localPath, RemotePath: syncRemoteChildPath(remoteRoot, relative),
			ModTimeUnixNano: info.ModTime().UnixNano(),
		}
		if err := addSyncTreeEntry(entries, entry, "local"); err != nil {
			return nil, err
		}
	}
	for _, file := range tree.Files {
		relative := filepath.ToSlash(file.RelativePath)
		if err := validateSyncRelativePath(relative); err != nil {
			return nil, fmt.Errorf("local path %q cannot be synced: %w", relative, err)
		}
		entry := syncTreeEntry{
			RelativePath: relative, Kind: "file", LocalPath: file.FullPath, RemotePath: syncRemoteChildPath(remoteRoot, relative),
			Size: file.Size, ModTimeUnixNano: file.ModTimeUnixNano,
		}
		if err := addSyncTreeEntry(entries, entry, "local"); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

func collectRemoteSyncEntries(planClient syncPlanClient, rootID, remoteRoot, localRoot string) (map[string]syncTreeEntry, error) {
	entries := make(map[string]syncTreeEntry)
	_, err := walkRemoteTree(planClient, rootID, remoteRoot, 0, func(walkEntry remoteWalkEntry) (bool, error) {
		if err := validateRemoteDownloadName(walkEntry.File.Name); err != nil {
			return false, fmt.Errorf("unsafe remote entry %q below %q: %w", walkEntry.File.Name, walkEntry.ParentPath, err)
		}
		relative := strings.ReplaceAll(walkEntry.RelativePath, "\\", "/")
		if err := validateSyncRelativePath(relative); err != nil {
			return false, fmt.Errorf("remote path %q cannot be synced: %w", relative, err)
		}
		kind := "file"
		if walkEntry.File.IsDirectory {
			kind = "directory"
		}
		entry := syncTreeEntry{
			RelativePath: relative, Kind: kind, LocalPath: filepath.Join(localRoot, filepath.FromSlash(relative)), RemotePath: walkEntry.RemotePath,
			RemoteID: walkEntry.File.FileID, Size: walkEntry.File.Size, SHA1: strings.TrimSpace(walkEntry.File.Sha1),
		}
		if !walkEntry.File.UpdateTime.IsZero() {
			entry.ModTimeUnixNano = walkEntry.File.UpdateTime.UnixNano()
		}
		if err := addSyncTreeEntry(entries, entry, "remote"); err != nil {
			return false, err
		}
		return false, nil
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

func addSyncTreeEntry(entries map[string]syncTreeEntry, entry syncTreeEntry, side string) error {
	return syncplanpkg.AddEntry(entries, entry, side)
}

func validateSyncRelativePath(relative string) error {
	return syncplanpkg.ValidateRelativePath(relative)
}

func syncPathKey(relative string) string {
	return syncplanpkg.PathKey(relative)
}

func resolveSyncRemoteSHA1(planClient syncPlanClient, remote syncTreeEntry) (string, error) {
	if sha1 := strings.ToUpper(strings.TrimSpace(remote.SHA1)); sha1 != "" {
		return sha1, nil
	}
	file, err := planClient.GetFile(remote.RemoteID)
	if err != nil {
		return "", fmt.Errorf("inspect remote file %q for sync checksum: %w", remote.RemotePath, err)
	}
	if file == nil || file.IsDirectory {
		return "", fmt.Errorf("remote file %q changed type while planning sync", remote.RemotePath)
	}
	if file.Size != remote.Size {
		return "", fmt.Errorf("remote file %q changed size while planning sync", remote.RemotePath)
	}
	return strings.ToUpper(strings.TrimSpace(file.Sha1)), nil
}

func checksumSyncLocalFile(local syncTreeEntry) (*uploadpkg.PreparedDigest, error) {
	return syncplanpkg.PrepareLocalDigest(local)
}

func canonicalSyncRemoteRoot(remoteRoot string) string {
	return syncplanpkg.CanonicalRemoteRoot(remoteRoot)
}

func syncRemoteChildPath(remoteRoot, relative string) string {
	return syncplanpkg.RemoteChildPath(remoteRoot, relative)
}

func printSyncPlan(plan syncPlan) {
	if jsonOutput {
		return
	}
	fmt.Printf("DRY-RUN sync: %s <-> %s (%s; direction=%s; conflict=%s; delete=%t)\n", plan.LocalRoot, plan.RemoteRoot, plan.Mode, plan.Direction, plan.ConflictPolicy, plan.DeleteExtraneous)
	fmt.Printf("Plan ID: %s\n", plan.PlanID)
	for _, item := range plan.Items {
		detail := ""
		if item.Kind == "file" {
			size := item.LocalSize
			if !item.LocalPresent {
				size = item.RemoteSize
			}
			detail = " " + output.FormatFileSize(size)
		}
		if item.Destructive {
			detail += " destructive"
		}
		fmt.Printf("  %-14s %-9s %s%s [%s]\n", item.Action, item.Kind, item.RelativePath, detail, item.Reason)
	}
	fmt.Printf("Summary: upload %d file(s)/%d dir(s) %s; download %d file(s)/%d dir(s) %s; delete remote %d root(s) affecting %d file(s)/%d dir(s) %s; delete local %d root(s) affecting %d file(s)/%d dir(s) %s; covered-by-delete %d; skip %d file(s)/%d dir(s); unresolved conflicts %d; resolved conflicts %d; destructive actions %d.\n",
		plan.UploadFiles, plan.UploadDirs, output.FormatFileSize(plan.UploadBytes),
		plan.DownloadFiles, plan.DownloadDirs, output.FormatFileSize(plan.DownloadBytes),
		plan.DeleteRemoteRoots, plan.DeleteRemoteFiles, plan.DeleteRemoteDirs, output.FormatFileSize(plan.DeleteRemoteBytes),
		plan.DeleteLocalRoots, plan.DeleteLocalFiles, plan.DeleteLocalDirs, output.FormatFileSize(plan.DeleteLocalBytes), plan.CoveredByDelete,
		plan.SkippedFiles, plan.SkippedDirs, plan.Conflicts, plan.ResolvedConflicts, plan.DestructiveActions)
	fmt.Printf("Checksummed %d local file(s) (%s). No changes were made.\n", plan.ChecksummedFiles, output.FormatFileSize(plan.ChecksummedBytes))
	if plan.DestructiveActions > 0 {
		fmt.Println("Execution of this plan requires --allow-destructive after review.")
	}
	if !plan.Ready {
		fmt.Println("Sync plan contains unresolved conflicts; choose a compatible explicit conflict policy or change the trees before execution.")
	}
}
