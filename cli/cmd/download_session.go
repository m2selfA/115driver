package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

func prepareRecursiveDownloadSession(remotePath, localRoot string, tree remoteDownloadTree, strategy, override string) (*transfer.TransferTreeSession, []remoteDownloadFile, int, int64, error) {
	sessionPath, _, err := deriveTransferSessionPaths("download", localRoot, remotePath, override)
	if err != nil {
		return nil, nil, 0, 0, err
	}
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
	session, err := transfer.OpenTransferTreeSession(sessionPath, transfer.TransferTreeSessionSpec{
		Direction: "download", Source: remotePath, Destination: absoluteRoot, Strategy: strategy,
	}, directories, files)
	if err != nil {
		return nil, nil, 0, 0, err
	}

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
	return session, pending, resumed, totalBytes, nil
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
