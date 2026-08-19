package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const resumeStateVersion = 1

var ErrDownloadResumeState = errors.New("download resume state is invalid")

type resumeMetadata struct {
	Version      int    `json:"version"`
	Mode         string `json:"mode"`
	KeyHash      string `json:"key_hash"`
	ExpectedSize int64  `json:"expected_size"`
	ChunkSize    int64  `json:"chunk_size,omitempty"`
	Completed    []int  `json:"completed,omitempty"`
}

type resumeArtifacts struct {
	mu         sync.Mutex
	file       *os.File
	partPath   string
	statePath  string
	persistent bool
	fresh      bool
	metadata   resumeMetadata
}

func openFileResume(destination, key string, expectedSize int64) (*resumeArtifacts, int64, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return nil, 0, fmt.Errorf("create download directory: %w", err)
	}
	if key == "" || expectedSize < 0 {
		file, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".*")
		if err != nil {
			return nil, 0, fmt.Errorf("create download temp file: %w", err)
		}
		return &resumeArtifacts{file: file, partPath: file.Name()}, 0, nil
	}

	artifacts, err := openPersistentResume(destination, resumeMetadata{
		Version: resumeStateVersion, Mode: "file", KeyHash: resumeKeyHash(key), ExpectedSize: expectedSize,
	})
	if err != nil {
		return nil, 0, err
	}
	info, err := artifacts.file.Stat()
	if err != nil {
		artifacts.closeOnFailure()
		return nil, 0, fmt.Errorf("stat resumable download part: %w", err)
	}
	if info.Size() < 0 || info.Size() > expectedSize {
		artifacts.closeOnFailure()
		if err := clearPersistentResume(destination); err != nil {
			return nil, 0, err
		}
		return openFileResume(destination, key, expectedSize)
	}
	return artifacts, info.Size(), nil
}

func openChunkResume(destination, key string, expectedSize, chunkSize int64, chunkCount int) (*resumeArtifacts, map[int]struct{}, error) {
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return nil, nil, fmt.Errorf("create chunk download directory: %w", err)
	}
	if key == "" {
		file, err := os.CreateTemp(filepath.Dir(destination), "."+filepath.Base(destination)+".*")
		if err != nil {
			return nil, nil, fmt.Errorf("create chunk download temp file: %w", err)
		}
		if err := file.Truncate(expectedSize); err != nil {
			file.Close()
			os.Remove(file.Name())
			return nil, nil, fmt.Errorf("size chunk download temp file: %w", err)
		}
		return &resumeArtifacts{file: file, partPath: file.Name()}, map[int]struct{}{}, nil
	}

	wanted := resumeMetadata{
		Version: resumeStateVersion, Mode: "chunk", KeyHash: resumeKeyHash(key), ExpectedSize: expectedSize, ChunkSize: chunkSize,
	}
	artifacts, err := openPersistentResume(destination, wanted)
	if err != nil {
		return nil, nil, err
	}
	info, err := artifacts.file.Stat()
	if err != nil {
		artifacts.closeOnFailure()
		return nil, nil, fmt.Errorf("stat resumable chunk part: %w", err)
	}
	if artifacts.fresh {
		if err := artifacts.file.Truncate(expectedSize); err != nil {
			artifacts.closeOnFailure()
			return nil, nil, fmt.Errorf("size resumable chunk part: %w", err)
		}
	} else if info.Size() != expectedSize {
		artifacts.closeOnFailure()
		if err := clearPersistentResume(destination); err != nil {
			return nil, nil, err
		}
		return openChunkResume(destination, key, expectedSize, chunkSize, chunkCount)
	}
	completed := make(map[int]struct{}, len(artifacts.metadata.Completed))
	for _, index := range artifacts.metadata.Completed {
		if index < 0 || index >= chunkCount {
			artifacts.closeOnFailure()
			if err := clearPersistentResume(destination); err != nil {
				return nil, nil, err
			}
			return openChunkResume(destination, key, expectedSize, chunkSize, chunkCount)
		}
		completed[index] = struct{}{}
	}
	return artifacts, completed, nil
}

func openPersistentResume(destination string, wanted resumeMetadata) (*resumeArtifacts, error) {
	partPath, statePath := resumeArtifactPaths(destination)
	if err := rejectResumeSymlink(partPath); err != nil {
		return nil, err
	}
	if err := rejectResumeSymlink(statePath); err != nil {
		return nil, err
	}

	metadata, stateErr := readResumeMetadata(statePath)
	partInfo, partErr := os.Stat(partPath)
	validExisting := stateErr == nil && partErr == nil && metadata.matches(wanted) && partInfo.Mode().IsRegular()
	if !validExisting {
		if err := removeIfExists(partPath); err != nil {
			return nil, fmt.Errorf("clear stale resume part: %w", err)
		}
		if err := removeIfExists(statePath); err != nil {
			return nil, fmt.Errorf("clear stale resume metadata: %w", err)
		}
		file, err := os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("create resumable download part: %w", err)
		}
		artifacts := &resumeArtifacts{file: file, partPath: partPath, statePath: statePath, persistent: true, fresh: true, metadata: wanted}
		if err := artifacts.persistMetadata(); err != nil {
			file.Close()
			removeIfExists(partPath)
			return nil, err
		}
		return artifacts, nil
	}

	file, err := os.OpenFile(partPath, os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open resumable download part: %w", err)
	}
	return &resumeArtifacts{file: file, partPath: partPath, statePath: statePath, persistent: true, metadata: metadata}, nil
}

func (artifacts *resumeArtifacts) markChunkComplete(index int) error {
	if artifacts == nil || !artifacts.persistent {
		return nil
	}
	artifacts.mu.Lock()
	defer artifacts.mu.Unlock()
	if artifacts.file == nil {
		return errors.New("resume artifact file is closed")
	}
	if err := artifacts.file.Sync(); err != nil {
		return fmt.Errorf("sync completed chunk before resume metadata: %w", err)
	}
	seen := make(map[int]struct{}, len(artifacts.metadata.Completed)+1)
	for _, item := range artifacts.metadata.Completed {
		seen[item] = struct{}{}
	}
	seen[index] = struct{}{}
	artifacts.metadata.Completed = artifacts.metadata.Completed[:0]
	for item := range seen {
		artifacts.metadata.Completed = append(artifacts.metadata.Completed, item)
	}
	sort.Ints(artifacts.metadata.Completed)
	return artifacts.persistMetadata()
}

func (artifacts *resumeArtifacts) persistMetadata() error {
	if artifacts == nil || !artifacts.persistent {
		return nil
	}
	encoded, err := json.Marshal(artifacts.metadata)
	if err != nil {
		return fmt.Errorf("encode resume metadata: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(artifacts.statePath), "."+filepath.Base(artifacts.statePath)+".*")
	if err != nil {
		return fmt.Errorf("create resume metadata temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		return fmt.Errorf("write resume metadata: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync resume metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close resume metadata: %w", err)
	}
	if err := replaceDownloadedFile(tmpPath, artifacts.statePath); err != nil {
		return fmt.Errorf("replace resume metadata: %w", err)
	}
	cleanup = false
	return nil
}

func (artifacts *resumeArtifacts) replaceDestination(destination string) error {
	if artifacts == nil || artifacts.file == nil {
		return errors.New("resume artifact file is nil")
	}
	if err := artifacts.file.Close(); err != nil {
		return fmt.Errorf("close download part file: %w", err)
	}
	artifacts.file = nil
	if err := replaceDownloadedFile(artifacts.partPath, destination); err != nil {
		return err
	}
	if artifacts.persistent {
		_ = removeIfExists(artifacts.statePath)
	}
	artifacts.partPath = ""
	return nil
}

func (artifacts *resumeArtifacts) closeOnFailure() {
	if artifacts == nil {
		return
	}
	if artifacts.file != nil {
		_ = artifacts.file.Close()
		artifacts.file = nil
	}
	if !artifacts.persistent && artifacts.partPath != "" {
		_ = os.Remove(artifacts.partPath)
	}
}

func (metadata resumeMetadata) matches(wanted resumeMetadata) bool {
	return metadata.Version == wanted.Version && metadata.Mode == wanted.Mode && metadata.KeyHash == wanted.KeyHash &&
		metadata.ExpectedSize == wanted.ExpectedSize && metadata.ChunkSize == wanted.ChunkSize
}

func readResumeMetadata(path string) (resumeMetadata, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return resumeMetadata{}, err
	}
	var metadata resumeMetadata
	if err := json.Unmarshal(data, &metadata); err != nil {
		return resumeMetadata{}, err
	}
	return metadata, nil
}

func resumeArtifactPaths(destination string) (string, string) {
	base := "." + filepath.Base(destination) + ".115driver"
	dir := filepath.Dir(destination)
	return filepath.Join(dir, base+".part"), filepath.Join(dir, base+".resume.json")
}

func clearPersistentResume(destination string) error {
	partPath, statePath := resumeArtifactPaths(destination)
	if err := rejectResumeSymlink(partPath); err != nil {
		return err
	}
	if err := rejectResumeSymlink(statePath); err != nil {
		return err
	}
	return errors.Join(removeIfExists(partPath), removeIfExists(statePath))
}

func rejectResumeSymlink(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: resume artifact %q is a symbolic link", ErrDownloadResumeState, filepath.Base(path))
	}
	return nil
}

func removeIfExists(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func resumeKeyHash(key string) string {
	digest := sha256.Sum256([]byte(key))
	return hex.EncodeToString(digest[:])
}
