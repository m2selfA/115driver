package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

func prepareRecursiveDownloadSession(remotePath, localRoot string, tree remoteDownloadTree, strategy, override string) (*transfer.TransferTreeSession, []remoteDownloadFile, int, int64, error) {
	sessionResolution, err := resolveTransferSessionPaths("download", "tree", localRoot, remotePath, strategy, "directory", override)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	sessionPath := sessionResolution.SessionPath
	defer sessionResolution.closeLock()
	directories := make([]transfer.TransferTreeSessionDirectory, len(tree.Directories))
	for i, relative := range tree.Directories {
		directories[i] = transfer.TransferTreeSessionDirectory{RelativePath: relative}
	}
	files := make([]transfer.TransferTreeSessionFile, len(tree.Files))
	var totalBytes int64
	for i, source := range tree.Files {
		files[i] = transfer.TransferTreeSessionFile{
			RelativePath: source.RelativePath,
			Size:         source.File.Size,
			StableID:     source.File.FileID,
			PickCode:     source.File.PickCode,
			SHA1:         source.File.Sha1,
		}
		if source.File.Size > 0 {
			totalBytes += source.File.Size
		}
	}
	absoluteRoot, err := filepath.Abs(localRoot)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	inside, err := pathIsWithin(absoluteRoot, sessionPath)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	if inside {
		return nil, nil, 0, 0, fmt.Errorf("%w: recursive download session must be outside the destination directory", errDownloadUsage)
	}
	spec := transfer.TransferTreeSessionSpec{
		Direction: "download", Source: remotePath, Destination: absoluteRoot, Strategy: strategy,
	}
	if err := importLegacyTransferSession(&sessionResolution, func(path string) (bool, error) {
		return transfer.ValidateTransferTreeSession(path, spec)
	}); err != nil {
		return nil, nil, 0, 0, err
	}
	session, err := transfer.OpenTransferTreeSession(sessionPath, spec, directories, files)
	if err != nil {
		return nil, nil, 0, 0, err
	}
	commitLegacyTransferSessionImport(sessionResolution)
	session.SetPartsDir(sessionResolution.PartsDir)

	stateByRelative := make(map[string]transfer.TransferTreeSessionFile, len(files))
	for _, state := range session.Snapshot().Files {
		stateByRelative[state.RelativePath] = state
	}
	pending := make([]remoteDownloadFile, 0, len(tree.Files))
	resumed := 0
	for _, source := range tree.Files {
		state := stateByRelative[source.RelativePath]
		valid, localModTime, err := completedDownloadFileStillValid(localRoot, state)
		if err != nil {
			return nil, nil, 0, 0, fmt.Errorf("validate completed download %q: %w", source.RelativePath, err)
		}
		if valid {
			if state.LocalModTimeUnixNano != localModTime {
				if err := session.SetFileLocalModTime(source.RelativePath, localModTime); err != nil {
					return nil, nil, 0, 0, err
				}
			}
			if !state.Completed {
				if err := session.MarkFileCompleted(source.RelativePath); err != nil {
					return nil, nil, 0, 0, err
				}
			}
			resumed++
			continue
		}
		if state.Completed {
			if err := session.MarkFilePending(source.RelativePath, errors.New("completed local file is missing or changed")); err != nil {
				return nil, nil, 0, 0, err
			}
		}
		pending = append(pending, source)
	}
	session.AttachLock(sessionResolution.Lock)
	sessionResolution.Lock = nil
	return session, pending, resumed, totalBytes, nil
}

func cleanupCompletedDownloadTreeSession(session *transfer.TransferTreeSession) error {
	if session == nil {
		return nil
	}
	partsDir := session.PartsDir()
	if err := session.Remove(); err != nil {
		return err
	}
	if strings.HasSuffix(filepath.Base(partsDir), ".parts") {
		return safeRemoveUploadPartsDir(partsDir)
	}
	return nil
}

func markDownloadSessionCompleted(session *transfer.TransferTreeSession, relative, destination string, expectedSize int64) error {
	if session == nil {
		return nil
	}
	info, err := os.Lstat(destination)
	if err != nil {
		return fmt.Errorf("stat completed download %q: %w", relative, err)
	}
	if !info.Mode().IsRegular() || expectedSize >= 0 && info.Size() != expectedSize {
		return fmt.Errorf("completed download %q has unexpected local file state", relative)
	}
	if err := session.SetFileLocalModTime(relative, info.ModTime().UnixNano()); err != nil {
		return err
	}
	return session.MarkFileCompleted(relative)
}

func markDownloadSessionPending(session *transfer.TransferTreeSession, relative string, cause error) error {
	if session == nil {
		return nil
	}
	return session.MarkFilePending(relative, cause)
}
