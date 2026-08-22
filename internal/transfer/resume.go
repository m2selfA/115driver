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
	"strings"
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
	mu              sync.Mutex
	file            *os.File
	partPath        string
	statePath       string
	legacyStatePath string
	persistent      bool
	fresh           bool
	metadata        resumeMetadata
}

func openFileResume(destination, key string, expectedSize int64) (*resumeArtifacts, int64, error) {
	return openFileResumeWithStatePath(destination, key, expectedSize, "")
}

func openFileResumeWithStatePath(destination, key string, expectedSize int64, statePath string) (*resumeArtifacts, int64, error) {
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
	}, statePath)
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
		if err := clearPersistentResumeWithStatePath(destination, statePath); err != nil {
			return nil, 0, err
		}
		return openFileResumeWithStatePath(destination, key, expectedSize, statePath)
	}
	return artifacts, info.Size(), nil
}

func openChunkResume(destination, key string, expectedSize, chunkSize int64, chunkCount int) (*resumeArtifacts, map[int]struct{}, error) {
	return openChunkResumeWithStatePath(destination, key, expectedSize, chunkSize, chunkCount, "")
}

func openChunkResumeWithStatePath(destination, key string, expectedSize, chunkSize int64, chunkCount int, statePath string) (*resumeArtifacts, map[int]struct{}, error) {
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
	artifacts, err := openPersistentResume(destination, wanted, statePath)
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
		if err := clearPersistentResumeWithStatePath(destination, statePath); err != nil {
			return nil, nil, err
		}
		return openChunkResumeWithStatePath(destination, key, expectedSize, chunkSize, chunkCount, statePath)
	}
	completed := make(map[int]struct{}, len(artifacts.metadata.Completed))
	for _, index := range artifacts.metadata.Completed {
		if index < 0 || index >= chunkCount {
			artifacts.closeOnFailure()
			if err := clearPersistentResumeWithStatePath(destination, statePath); err != nil {
				return nil, nil, err
			}
			return openChunkResumeWithStatePath(destination, key, expectedSize, chunkSize, chunkCount, statePath)
		}
		completed[index] = struct{}{}
	}
	return artifacts, completed, nil
}

func openPersistentResume(destination string, wanted resumeMetadata, statePathOverride string) (*resumeArtifacts, error) {
	partPath, statePath := resumeArtifactPathsWithStatePath(destination, statePathOverride)
	_, legacyStatePath := resumeArtifactPaths(destination)
	if filepath.Clean(legacyStatePath) == filepath.Clean(statePath) {
		legacyStatePath = ""
	}
	if err := rejectResumeSymlink(partPath); err != nil {
		return nil, err
	}
	if err := rejectResumeSymlink(statePath); err != nil {
		return nil, err
	}
	if legacyStatePath != "" {
		if err := rejectResumeSymlink(legacyStatePath); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		return nil, fmt.Errorf("create download resume metadata directory: %w", err)
	}
	if legacyStatePath != "" {
		if _, err := os.Lstat(statePath); os.IsNotExist(err) {
			legacyMetadata, legacyErr := readResumeMetadata(legacyStatePath)
			partInfo, partErr := os.Stat(partPath)
			if legacyErr == nil && partErr == nil && partInfo.Mode().IsRegular() && legacyMetadata.matches(wanted) {
				if err := writeResumeMetadataAtomic(statePath, legacyMetadata); err != nil {
					return nil, fmt.Errorf("migrate download resume metadata: %w", err)
				}
				_ = removeIfExists(legacyStatePath)
			}
		}
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
		if legacyStatePath != "" {
			if err := removeIfExists(legacyStatePath); err != nil {
				return nil, fmt.Errorf("clear stale legacy resume metadata: %w", err)
			}
		}
		file, err := os.OpenFile(partPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return nil, fmt.Errorf("create resumable download part: %w", err)
		}
		artifacts := &resumeArtifacts{file: file, partPath: partPath, statePath: statePath, legacyStatePath: legacyStatePath, persistent: true, fresh: true, metadata: wanted}
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
	return &resumeArtifacts{file: file, partPath: partPath, statePath: statePath, legacyStatePath: legacyStatePath, persistent: true, metadata: metadata}, nil
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
	if err := writeResumeMetadataAtomic(artifacts.statePath, artifacts.metadata); err != nil {
		return err
	}
	if _, err := TouchManagedSessionForStatePath(artifacts.statePath, true); err != nil {
		return err
	}
	return nil
}

func writeResumeMetadataAtomic(statePath string, metadata resumeMetadata) error {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return fmt.Errorf("encode resume metadata: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0700); err != nil {
		return fmt.Errorf("create resume metadata directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(statePath), "."+filepath.Base(statePath)+".*")
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
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure resume metadata temp file: %w", err)
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write resume metadata: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync resume metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close resume metadata: %w", err)
	}
	if err := replaceDownloadedFile(tmpPath, statePath); err != nil {
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
		if artifacts.legacyStatePath != "" {
			_ = removeIfExists(artifacts.legacyStatePath)
		}
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

func resumeArtifactPathsWithStatePath(destination, statePath string) (string, string) {
	partPath, defaultStatePath := resumeArtifactPaths(destination)
	if strings.TrimSpace(statePath) == "" {
		return partPath, defaultStatePath
	}
	return partPath, filepath.Clean(statePath)
}

func clearPersistentResume(destination string) error {
	return clearPersistentResumeWithStatePath(destination, "")
}

func clearPersistentResumeWithStatePath(destination, statePathOverride string) error {
	partPath, statePath := resumeArtifactPathsWithStatePath(destination, statePathOverride)
	if err := rejectResumeSymlink(partPath); err != nil {
		return err
	}
	if err := rejectResumeSymlink(statePath); err != nil {
		return err
	}
	_, legacyStatePath := resumeArtifactPaths(destination)
	legacyErr := error(nil)
	if filepath.Clean(legacyStatePath) != filepath.Clean(statePath) {
		if err := rejectResumeSymlink(legacyStatePath); err != nil {
			return err
		}
		legacyErr = removeIfExists(legacyStatePath)
	}
	return errors.Join(removeIfExists(partPath), removeIfExists(statePath), legacyErr)
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
