package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const TransferTreeSessionVersion = 1

var ErrTransferTreeSession = errors.New("transfer tree session is invalid")

type TransferTreeSessionSpec struct {
	Direction   string `json:"direction"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Strategy    string `json:"strategy,omitempty"`
}

type TransferTreeSessionFile struct {
	RelativePath         string `json:"relative_path"`
	Size                 int64  `json:"size"`
	ModTimeUnixNano      int64  `json:"mod_time_unix_nano,omitempty"`
	LocalModTimeUnixNano int64  `json:"local_mod_time_unix_nano,omitempty"`
	StableID             string `json:"stable_id,omitempty"`
	PickCode             string `json:"pick_code,omitempty"`
	SHA1                 string `json:"sha1,omitempty"`
	Completed            bool   `json:"completed,omitempty"`
	LastError            string `json:"last_error,omitempty"`
}

type TransferTreeSessionDirectory struct {
	RelativePath string `json:"relative_path"`
	RemoteID     string `json:"remote_id,omitempty"`
}

type TransferTreeSessionSnapshot struct {
	Version     int                            `json:"version"`
	KeyHash     string                         `json:"key_hash"`
	Spec        TransferTreeSessionSpec        `json:"spec"`
	CreatedAt   time.Time                      `json:"created_at"`
	UpdatedAt   time.Time                      `json:"updated_at"`
	Directories []TransferTreeSessionDirectory `json:"directories,omitempty"`
	Files       []TransferTreeSessionFile      `json:"files,omitempty"`
}

type TransferTreeSession struct {
	mu       sync.Mutex
	path     string
	snapshot TransferTreeSessionSnapshot
}

func OpenTransferTreeSession(path string, spec TransferTreeSessionSpec, directories []TransferTreeSessionDirectory, files []TransferTreeSessionFile) (*TransferTreeSession, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: session path is empty", ErrTransferTreeSession)
	}
	if strings.TrimSpace(spec.Direction) == "" || strings.TrimSpace(spec.Source) == "" || strings.TrimSpace(spec.Destination) == "" {
		return nil, fmt.Errorf("%w: direction, source, and destination are required", ErrTransferTreeSession)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create transfer session directory: %w", err)
	}
	if err := rejectTransferSessionSymlink(path); err != nil {
		return nil, err
	}
	for i := range directories {
		relative, err := cleanSessionRelativePath(directories[i].RelativePath, true)
		if err != nil {
			return nil, err
		}
		directories[i].RelativePath = relative
	}
	for i := range files {
		relative, err := cleanSessionRelativePath(files[i].RelativePath, false)
		if err != nil {
			return nil, err
		}
		files[i].RelativePath = relative
		if files[i].Size < 0 {
			return nil, fmt.Errorf("%w: file %q has negative size", ErrTransferTreeSession, relative)
		}
	}

	spec = canonicalTransferTreeSessionSpec(spec)
	keyHash := transferTreeSessionKeyHash(spec)
	now := time.Now().UTC()
	snapshot := TransferTreeSessionSnapshot{
		Version: TransferTreeSessionVersion, KeyHash: keyHash, Spec: spec, CreatedAt: now, UpdatedAt: now,
	}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return nil, fmt.Errorf("%w: decode %s: %v", ErrTransferTreeSession, filepath.Base(path), err)
		}
		legacySpec := canonicalTransferTreeSessionSpec(snapshot.Spec)
		if snapshot.Version != TransferTreeSessionVersion || snapshot.KeyHash != keyHash && transferTreeSessionKeyHash(legacySpec) != keyHash {
			return nil, fmt.Errorf("%w: session belongs to a different transfer", ErrTransferTreeSession)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read transfer session: %w", err)
	}
	if snapshot.CreatedAt.IsZero() {
		snapshot.CreatedAt = now
	}
	snapshot.Version = TransferTreeSessionVersion
	snapshot.KeyHash = keyHash
	snapshot.Spec = spec
	snapshot.UpdatedAt = now
	snapshot.Directories = reconcileTransferSessionDirectories(snapshot.Directories, directories)
	snapshot.Files = reconcileTransferSessionFiles(snapshot.Files, files)

	session := &TransferTreeSession{path: path, snapshot: snapshot}
	if err := session.persistLocked(); err != nil {
		return nil, err
	}
	return session, nil
}

func (session *TransferTreeSession) Path() string {
	if session == nil {
		return ""
	}
	return session.path
}

func (session *TransferTreeSession) Snapshot() TransferTreeSessionSnapshot {
	if session == nil {
		return TransferTreeSessionSnapshot{}
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	return cloneTransferTreeSessionSnapshot(session.snapshot)
}

func (session *TransferTreeSession) MarkFileCompleted(relative string) error {
	return session.updateFile(relative, func(file *TransferTreeSessionFile) {
		file.Completed = true
		file.LastError = ""
	})
}

func (session *TransferTreeSession) MarkFilePending(relative string, cause error) error {
	return session.updateFile(relative, func(file *TransferTreeSessionFile) {
		file.Completed = false
		file.LastError = ""
		if cause != nil {
			file.LastError = cause.Error()
		}
	})
}

func (session *TransferTreeSession) SetFileSHA1(relative, sha1 string) error {
	return session.updateFile(relative, func(file *TransferTreeSessionFile) {
		file.SHA1 = strings.TrimSpace(sha1)
	})
}

func (session *TransferTreeSession) SetFileLocalModTime(relative string, unixNano int64) error {
	return session.updateFile(relative, func(file *TransferTreeSessionFile) {
		file.LocalModTimeUnixNano = unixNano
	})
}

func (session *TransferTreeSession) SetDirectoryRemoteID(relative, remoteID string) error {
	if session == nil {
		return errors.New("transfer session is nil")
	}
	relative, err := cleanSessionRelativePath(relative, true)
	if err != nil {
		return err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	for i := range session.snapshot.Directories {
		if session.snapshot.Directories[i].RelativePath == relative {
			session.snapshot.Directories[i].RemoteID = remoteID
			session.snapshot.UpdatedAt = time.Now().UTC()
			return session.persistLocked()
		}
	}
	return fmt.Errorf("%w: directory %q is not part of the session", ErrTransferTreeSession, relative)
}

func (session *TransferTreeSession) Remove() error {
	if session == nil || session.path == "" {
		return nil
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if err := rejectTransferSessionSymlink(session.path); err != nil {
		return err
	}
	err := os.Remove(session.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (session *TransferTreeSession) updateFile(relative string, update func(*TransferTreeSessionFile)) error {
	if session == nil {
		return errors.New("transfer session is nil")
	}
	cleaned, err := cleanSessionRelativePath(relative, false)
	if err != nil {
		return err
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	for i := range session.snapshot.Files {
		if session.snapshot.Files[i].RelativePath == cleaned {
			update(&session.snapshot.Files[i])
			session.snapshot.UpdatedAt = time.Now().UTC()
			return session.persistLocked()
		}
	}
	return fmt.Errorf("%w: file %q is not part of the session", ErrTransferTreeSession, cleaned)
}

func (session *TransferTreeSession) persistLocked() error {
	if session == nil {
		return errors.New("transfer session is nil")
	}
	encoded, err := json.MarshalIndent(session.snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode transfer session: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(session.path), "."+filepath.Base(session.path)+".*")
	if err != nil {
		return fmt.Errorf("create transfer session temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure transfer session temp file: %w", err)
	}
	if _, err := tmp.Write(encoded); err != nil {
		tmp.Close()
		return fmt.Errorf("write transfer session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync transfer session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close transfer session: %w", err)
	}
	if err := replaceDownloadedFile(tmpPath, session.path); err != nil {
		return fmt.Errorf("replace transfer session: %w", err)
	}
	cleanup = false
	return nil
}

func reconcileTransferSessionDirectories(existing, wanted []TransferTreeSessionDirectory) []TransferTreeSessionDirectory {
	prior := make(map[string]TransferTreeSessionDirectory, len(existing))
	for _, item := range existing {
		prior[item.RelativePath] = item
	}
	result := make([]TransferTreeSessionDirectory, len(wanted))
	for i, item := range wanted {
		if old, ok := prior[item.RelativePath]; ok && item.RemoteID == "" {
			item.RemoteID = old.RemoteID
		}
		result[i] = item
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RelativePath < result[j].RelativePath })
	return result
}

func reconcileTransferSessionFiles(existing, wanted []TransferTreeSessionFile) []TransferTreeSessionFile {
	prior := make(map[string]TransferTreeSessionFile, len(existing))
	for _, item := range existing {
		prior[item.RelativePath] = item
	}
	result := make([]TransferTreeSessionFile, len(wanted))
	for i, item := range wanted {
		if old, ok := prior[item.RelativePath]; ok && sameTransferSessionFileIdentity(old, item) {
			item.Completed = old.Completed
			item.LastError = old.LastError
			item.LocalModTimeUnixNano = old.LocalModTimeUnixNano
			if item.SHA1 == "" {
				item.SHA1 = old.SHA1
			}
		}
		result[i] = item
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RelativePath < result[j].RelativePath })
	return result
}

func sameTransferSessionFileIdentity(old, wanted TransferTreeSessionFile) bool {
	if old.Size != wanted.Size || old.ModTimeUnixNano != wanted.ModTimeUnixNano {
		return false
	}
	if wanted.StableID != "" && old.StableID != wanted.StableID {
		return false
	}
	if wanted.PickCode != "" && old.PickCode != wanted.PickCode {
		return false
	}
	if wanted.SHA1 != "" && !strings.EqualFold(old.SHA1, wanted.SHA1) {
		return false
	}
	return true
}

func cleanSessionRelativePath(relative string, allowRoot bool) (string, error) {
	if relative == "" && allowRoot {
		return "", nil
	}
	if relative == "" {
		return "", fmt.Errorf("%w: file relative path is empty", ErrTransferTreeSession)
	}
	cleaned := filepath.Clean(relative)
	if cleaned == "." && allowRoot {
		return "", nil
	}
	if cleaned == "." || cleaned == ".." || filepath.IsAbs(cleaned) || filepath.VolumeName(cleaned) != "" {
		return "", fmt.Errorf("%w: unsafe relative path %q", ErrTransferTreeSession, relative)
	}
	prefix := ".." + string(filepath.Separator)
	if strings.HasPrefix(cleaned, prefix) {
		return "", fmt.Errorf("%w: relative path escapes root: %q", ErrTransferTreeSession, relative)
	}
	return cleaned, nil
}

func canonicalTransferTreeSessionSpec(spec TransferTreeSessionSpec) TransferTreeSessionSpec {
	if runtime.GOOS != "windows" {
		return spec
	}
	switch strings.ToLower(strings.TrimSpace(spec.Direction)) {
	case "upload":
		spec.Source = strings.ToLower(filepath.Clean(spec.Source))
	case "download":
		spec.Destination = strings.ToLower(filepath.Clean(spec.Destination))
	}
	return spec
}

func transferTreeSessionKeyHash(spec TransferTreeSessionSpec) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{spec.Direction, spec.Source, spec.Destination, spec.Strategy}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func rejectTransferSessionSymlink(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: session file %q is a symbolic link", ErrTransferTreeSession, filepath.Base(path))
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%w: session path %q is not a regular file", ErrTransferTreeSession, filepath.Base(path))
	}
	return nil
}

func cloneTransferTreeSessionSnapshot(snapshot TransferTreeSessionSnapshot) TransferTreeSessionSnapshot {
	clone := snapshot
	clone.Directories = append([]TransferTreeSessionDirectory(nil), snapshot.Directories...)
	clone.Files = append([]TransferTreeSessionFile(nil), snapshot.Files...)
	return clone
}
