package transfer

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
)

const (
	SessionIdentitySchemaV2 = "115driver-session-id-v2"
	SessionManifestVersion  = 2
)

// Managed session manifests carry GC/activity metadata, while payload.json and
// parts/* contain the authoritative resumable transfer state. Coalescing
// timestamp-only manifest writes avoids thousands of atomic replacements during
// large recursive transfers without weakening resume durability.
const managedSessionManifestTouchInterval = 30 * time.Second

var (
	ErrSessionStore           = errors.New("session store is invalid")
	ErrSessionAccountMismatch = errors.New("session belongs to a different account")
	ErrSessionNewerVersion    = errors.New("session was created by a newer 115driver")
	ErrSessionCompleted       = errors.New("session was already completed")
)

type SessionIdentityV2 struct {
	Schema       string `json:"schema"`
	Direction    string `json:"direction"`
	Kind         string `json:"kind"`
	ProfileScope string `json:"profile_scope"`
	LocalPath    string `json:"local_path"`
	RemotePath   string `json:"remote_path"`
	Strategy     string `json:"strategy"`
	TransferMode string `json:"transfer_mode"`
}

type SessionManifest struct {
	Version        int               `json:"version"`
	SessionID      string            `json:"session_id"`
	IdentitySHA256 string            `json:"identity_sha256"`
	Identity       SessionIdentityV2 `json:"identity"`
	AccountID      int64             `json:"account_id,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	UpdatedAt      time.Time         `json:"updated_at"`
	LastUsedAt     time.Time         `json:"last_used_at"`
	State          string            `json:"state"`
}

type SessionStore struct {
	Root string
}

type SessionLocation struct {
	ID           string
	Dir          string
	ManifestPath string
	PayloadPath  string
	PartsDir     string
	LockPath     string
	LeasePath    string
}

func NewSessionIdentityV2(direction, kind, profileScope, localPath, remotePath, strategy, transferMode string) (SessionIdentityV2, error) {
	for name, value := range map[string]string{
		"direction": direction, "kind": kind, "profile_scope": profileScope, "local_path": localPath,
		"remote_path": remotePath, "strategy": strategy, "transfer_mode": transferMode,
	} {
		if strings.ContainsRune(value, '\x00') {
			return SessionIdentityV2{}, fmt.Errorf("%w: %s contains NUL", ErrSessionStore, name)
		}
	}
	localAbs, err := filepath.Abs(strings.TrimSpace(localPath))
	if err != nil {
		return SessionIdentityV2{}, fmt.Errorf("%w: resolve local path: %v", ErrSessionStore, err)
	}
	identity := SessionIdentityV2{
		Schema:       SessionIdentitySchemaV2,
		Direction:    strings.ToLower(strings.TrimSpace(direction)),
		Kind:         strings.ToLower(strings.TrimSpace(kind)),
		ProfileScope: strings.TrimSpace(profileScope),
		LocalPath:    canonicalSessionLocalPath(localAbs),
		RemotePath:   canonicalSessionRemotePath(remotePath),
		Strategy:     strings.ToLower(strings.TrimSpace(strategy)),
		TransferMode: strings.ToLower(strings.TrimSpace(transferMode)),
	}
	if identity.Direction != "upload" && identity.Direction != "download" {
		return SessionIdentityV2{}, fmt.Errorf("%w: unsupported direction %q", ErrSessionStore, direction)
	}
	if identity.Kind != "file" && identity.Kind != "tree" {
		return SessionIdentityV2{}, fmt.Errorf("%w: unsupported kind %q", ErrSessionStore, kind)
	}
	if identity.ProfileScope == "" || identity.LocalPath == "" || identity.RemotePath == "" || identity.Strategy == "" || identity.TransferMode == "" {
		return SessionIdentityV2{}, fmt.Errorf("%w: identity fields must not be empty", ErrSessionStore)
	}
	return identity, nil
}

func SessionProfileScope(configPath, profileName string) (string, error) {
	if strings.ContainsRune(configPath, '\x00') || strings.ContainsRune(profileName, '\x00') {
		return "", fmt.Errorf("%w: config path and profile name must not contain NUL", ErrSessionStore)
	}
	absolute, err := filepath.Abs(strings.TrimSpace(configPath))
	if err != nil {
		return "", fmt.Errorf("resolve config path for session scope: %w", err)
	}
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return "", fmt.Errorf("%w: profile name is empty", ErrSessionStore)
	}
	canonical := canonicalSessionLocalPath(absolute)
	digest := sha256.Sum256([]byte(canonical + "\x00" + profileName))
	return hex.EncodeToString(digest[:]), nil
}

func (identity SessionIdentityV2) SessionID() (string, string, error) {
	canonical, err := identity.canonicalBytes()
	if err != nil {
		return "", "", err
	}
	digest := sha256.Sum256(canonical)
	encoded := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(digest[:20])
	return strings.ToLower(encoded), hex.EncodeToString(digest[:]), nil
}

func (identity SessionIdentityV2) canonicalBytes() ([]byte, error) {
	if identity.Schema != SessionIdentitySchemaV2 {
		return nil, fmt.Errorf("%w: unsupported identity schema %q", ErrSessionStore, identity.Schema)
	}
	fields := []string{
		"schema", identity.Schema,
		"direction", identity.Direction,
		"kind", identity.Kind,
		"profile_scope", identity.ProfileScope,
		"local_path", identity.LocalPath,
		"remote_path", identity.RemotePath,
		"strategy", identity.Strategy,
		"transfer_mode", identity.TransferMode,
	}
	for i := 1; i < len(fields); i += 2 {
		if strings.ContainsRune(fields[i], '\x00') {
			return nil, fmt.Errorf("%w: identity field %s contains NUL", ErrSessionStore, fields[i-1])
		}
	}
	return []byte(strings.Join(fields, "\x00")), nil
}

func (store SessionStore) Location(identity SessionIdentityV2, displayName string) (SessionLocation, error) {
	root := strings.TrimSpace(store.Root)
	if root == "" {
		return SessionLocation{}, fmt.Errorf("%w: store root is empty", ErrSessionStore)
	}
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return SessionLocation{}, fmt.Errorf("resolve session store root: %w", err)
	}
	id, fullHash, err := identity.SessionID()
	if err != nil {
		return SessionLocation{}, err
	}
	slug := SessionDisplaySlug(displayName)
	if slug == "" {
		slug = "transfer"
	}
	shardDir := filepath.Join(absoluteRoot, "v2", identity.Direction, identity.Kind, id[:2])
	expectedDir := filepath.Join(shardDir, slug+"--"+id)
	dir := expectedDir
	entries, readErr := os.ReadDir(shardDir)
	if readErr == nil {
		matched := ""
		for _, entry := range entries {
			if !entry.IsDir() || !strings.HasSuffix(entry.Name(), "--"+id) {
				continue
			}
			candidate := filepath.Join(shardDir, entry.Name())
			data, manifestErr := os.ReadFile(filepath.Join(candidate, "session.json"))
			if manifestErr != nil {
				continue
			}
			var manifest SessionManifest
			if json.Unmarshal(data, &manifest) != nil || manifest.SessionID != id || manifest.IdentitySHA256 != fullHash || manifest.Identity != identity {
				continue
			}
			if matched != "" && matched != candidate {
				return SessionLocation{}, fmt.Errorf("%w: multiple directories match session %s", ErrSessionStore, id)
			}
			matched = candidate
		}
		if matched != "" {
			dir = matched
		}
	} else if !os.IsNotExist(readErr) {
		return SessionLocation{}, fmt.Errorf("read session shard: %w", readErr)
	}
	return SessionLocation{
		ID: id, Dir: dir, ManifestPath: filepath.Join(dir, "session.json"), PayloadPath: filepath.Join(dir, "payload.json"), PartsDir: filepath.Join(dir, "parts"),
		LockPath: filepath.Join(absoluteRoot, "locks", id[:2], id+".lock"), LeasePath: filepath.Join(dir, "lease.json"),
	}, nil
}

func (store SessionStore) Open(identity SessionIdentityV2, displayName string, accountID int64) (SessionLocation, SessionManifest, error) {
	location, err := store.Location(identity, displayName)
	if err != nil {
		return SessionLocation{}, SessionManifest{}, err
	}
	if err := os.MkdirAll(location.Dir, 0700); err != nil {
		return SessionLocation{}, SessionManifest{}, fmt.Errorf("create session directory: %w", err)
	}
	id, fullHash, err := identity.SessionID()
	if err != nil {
		return SessionLocation{}, SessionManifest{}, err
	}
	unlockManifest := lockSessionManifestPath(location.ManifestPath)
	defer unlockManifest()
	now := time.Now().UTC()
	manifest := SessionManifest{
		Version: SessionManifestVersion, SessionID: id, IdentitySHA256: fullHash, Identity: identity,
		AccountID: accountID, CreatedAt: now, UpdatedAt: now, LastUsedAt: now, State: "active",
	}
	data, readErr := os.ReadFile(location.ManifestPath)
	if readErr == nil {
		var stored SessionManifest
		if err := json.Unmarshal(data, &stored); err != nil {
			return SessionLocation{}, SessionManifest{}, fmt.Errorf("%w: decode session manifest: %v", ErrSessionStore, err)
		}
		if stored.Version > SessionManifestVersion {
			return SessionLocation{}, SessionManifest{}, fmt.Errorf("%w: manifest version %d", ErrSessionNewerVersion, stored.Version)
		}
		if stored.Version != SessionManifestVersion || stored.SessionID != id || stored.IdentitySHA256 != fullHash || stored.Identity != identity {
			return SessionLocation{}, SessionManifest{}, fmt.Errorf("%w: session manifest identity mismatch", ErrSessionStore)
		}
		if stored.AccountID != 0 && accountID != 0 && stored.AccountID != accountID {
			return SessionLocation{}, SessionManifest{}, fmt.Errorf("%w: stored=%d current=%d", ErrSessionAccountMismatch, stored.AccountID, accountID)
		}
		if stored.State == "completed" {
			return location, stored, fmt.Errorf("%w: %s", ErrSessionCompleted, stored.SessionID)
		}
		if stored.State != "active" {
			return location, stored, fmt.Errorf("%w: unsupported session state %q", ErrSessionStore, stored.State)
		}
		manifest = stored
		mutated := false
		if manifest.AccountID == 0 && accountID != 0 {
			manifest.AccountID = accountID
			mutated = true
		}
		if manifest.CreatedAt.IsZero() {
			manifest.CreatedAt = now
			mutated = true
		}
		if manifest.UpdatedAt.IsZero() {
			mutated = true
		}
		if mutated {
			manifest.UpdatedAt = now
		}
		manifest.LastUsedAt = now
	} else if !os.IsNotExist(readErr) {
		return SessionLocation{}, SessionManifest{}, fmt.Errorf("read session manifest: %w", readErr)
	}
	if err := writeSessionManifestAtomic(location.ManifestPath, manifest); err != nil {
		return SessionLocation{}, SessionManifest{}, err
	}
	return location, manifest, nil
}

// QuarantineCorruptLocation moves a corrupt current-version session aside.
// The caller must already hold location.LockPath for the duration of this call.
func (store SessionStore) QuarantineCorruptLocation(location SessionLocation) (string, error) {
	if strings.TrimSpace(location.Dir) == "" || len(location.ID) < 2 {
		return "", fmt.Errorf("%w: invalid session location", ErrSessionStore)
	}
	data, err := os.ReadFile(location.ManifestPath)
	if err == nil {
		var header struct {
			Version int `json:"version"`
		}
		if json.Unmarshal(data, &header) == nil && header.Version > SessionManifestVersion {
			return "", fmt.Errorf("%w: manifest version %d", ErrSessionNewerVersion, header.Version)
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("read corrupt session manifest before quarantine: %w", err)
	}
	if _, err := os.Lstat(location.Dir); err != nil {
		return "", err
	}
	return store.trashDir(location.Dir, location.ID, "corrupt", time.Now().UTC())
}

func ImportLegacySession(location SessionLocation, legacyPayload, legacyParts string) (bool, error) {
	if strings.TrimSpace(legacyPayload) == "" {
		return false, nil
	}
	if _, err := os.Lstat(location.PayloadPath); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, fmt.Errorf("inspect managed session payload: %w", err)
	}
	info, err := os.Lstat(legacyPayload)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect legacy session: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%w: legacy session is not a regular file", ErrSessionStore)
	}
	if err := os.MkdirAll(location.Dir, 0700); err != nil {
		return false, fmt.Errorf("create managed session directory: %w", err)
	}
	stagingDir, err := os.MkdirTemp(location.Dir, ".legacy-import.*")
	if err != nil {
		return false, fmt.Errorf("create legacy import staging directory: %w", err)
	}
	if err := os.Chmod(stagingDir, 0700); err != nil {
		_ = os.RemoveAll(stagingDir)
		return false, fmt.Errorf("secure legacy import staging directory: %w", err)
	}
	defer os.RemoveAll(stagingDir)
	stagedPayload := filepath.Join(stagingDir, "payload.json")
	stagedParts := filepath.Join(stagingDir, "parts")
	if err := copySessionFileAtomic(legacyPayload, stagedPayload); err != nil {
		return false, err
	}
	hasParts := false
	if strings.TrimSpace(legacyParts) != "" {
		partsInfo, statErr := os.Lstat(legacyParts)
		if statErr == nil {
			if partsInfo.Mode()&os.ModeSymlink != 0 || !partsInfo.IsDir() {
				return false, fmt.Errorf("%w: legacy session parts are not a real directory", ErrSessionStore)
			}
			if err := copySessionDirectory(legacyParts, stagedParts); err != nil {
				return false, err
			}
			hasParts = true
		} else if !os.IsNotExist(statErr) {
			return false, fmt.Errorf("inspect legacy session parts: %w", statErr)
		}
	}
	if err := os.RemoveAll(location.PartsDir); err != nil {
		return false, fmt.Errorf("clear stale managed session parts: %w", err)
	}
	if hasParts {
		if err := os.Rename(stagedParts, location.PartsDir); err != nil {
			return false, fmt.Errorf("publish imported session parts: %w", err)
		}
	}
	if err := os.Rename(stagedPayload, location.PayloadPath); err != nil {
		return false, fmt.Errorf("publish imported session payload: %w", err)
	}
	return true, nil
}

func RemoveLegacySessionBestEffort(payloadPath, partsDir string) {
	if strings.TrimSpace(payloadPath) != "" {
		_ = os.Remove(payloadPath)
	}
	if strings.TrimSpace(partsDir) != "" {
		_ = os.RemoveAll(partsDir)
	}
}

func validateSessionManifest(manifest SessionManifest) error {
	if manifest.Version > SessionManifestVersion {
		return fmt.Errorf("%w: manifest version %d", ErrSessionNewerVersion, manifest.Version)
	}
	if manifest.Version != SessionManifestVersion {
		return fmt.Errorf("%w: unsupported manifest version %d", ErrSessionStore, manifest.Version)
	}
	id, fullHash, err := manifest.Identity.SessionID()
	if err != nil {
		return err
	}
	if manifest.SessionID != id || manifest.IdentitySHA256 != fullHash {
		return fmt.Errorf("%w: manifest identity hash mismatch", ErrSessionStore)
	}
	if manifest.State != "active" && manifest.State != "completed" {
		return fmt.Errorf("%w: unsupported session state %q", ErrSessionStore, manifest.State)
	}
	return nil
}

func TouchManagedSessionForPayload(payloadPath string, persistentMutation bool) (bool, error) {
	return TouchManagedSessionForStatePath(payloadPath, persistentMutation)
}

func TouchManagedSessionForStatePath(statePath string, persistentMutation bool) (bool, error) {
	dir, ok := managedSessionDirForStatePath(statePath)
	if !ok {
		return false, nil
	}
	manifestPath := filepath.Join(dir, "session.json")
	unlockManifest := lockSessionManifestPath(manifestPath)
	defer unlockManifest()
	data, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("read managed session manifest: %w", err)
	}
	var manifest SessionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return true, fmt.Errorf("%w: decode managed session manifest: %v", ErrSessionStore, err)
	}
	if err := validateSessionManifest(manifest); err != nil {
		return true, err
	}
	now := time.Now().UTC()
	if !managedSessionManifestTouchDue(manifest, persistentMutation, now) {
		return true, nil
	}
	if persistentMutation {
		manifest.UpdatedAt = now
	}
	manifest.LastUsedAt = now
	if err := writeSessionManifestAtomic(manifestPath, manifest); err != nil {
		return true, err
	}
	return true, nil
}

func managedSessionManifestTouchDue(manifest SessionManifest, persistentMutation bool, now time.Time) bool {
	reference := manifest.LastUsedAt
	if persistentMutation {
		if manifest.UpdatedAt.IsZero() || manifest.LastUsedAt.IsZero() {
			return true
		}
		reference = manifest.UpdatedAt
		if manifest.LastUsedAt.Before(reference) {
			reference = manifest.LastUsedAt
		}
	}
	if reference.IsZero() || now.Before(reference) {
		return true
	}
	return now.Sub(reference) >= managedSessionManifestTouchInterval
}

func managedSessionDirForStatePath(statePath string) (string, bool) {
	statePath = strings.TrimSpace(statePath)
	if statePath == "" {
		return "", false
	}
	cleaned := filepath.Clean(statePath)
	var dir string
	switch {
	case filepath.Base(cleaned) == "payload.json":
		dir = filepath.Dir(cleaned)
	case filepath.Base(filepath.Dir(cleaned)) == "parts":
		dir = filepath.Dir(filepath.Dir(cleaned))
	default:
		return "", false
	}
	if _, ok := managedSessionStoreRootForDir(dir); !ok {
		return "", false
	}
	return dir, true
}

func managedSessionStoreRootForDir(dir string) (string, bool) {
	v2Root := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(dir))))
	if filepath.Base(v2Root) != "v2" {
		return "", false
	}
	return filepath.Dir(v2Root), true
}

func RemoveManagedSessionForPayload(payloadPath string) (bool, error) {
	if filepath.Base(payloadPath) != "payload.json" {
		return false, nil
	}
	dir := filepath.Dir(payloadPath)
	manifestPath := filepath.Join(dir, "session.json")
	unlockManifest := lockSessionManifestPath(manifestPath)
	defer unlockManifest()
	data, err := os.ReadFile(manifestPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var manifest SessionManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return false, nil
	}
	if err := validateSessionManifest(manifest); err != nil {
		return false, err
	}
	now := time.Now().UTC()
	manifest.State = "completed"
	manifest.UpdatedAt = now
	manifest.LastUsedAt = now
	if err := writeSessionManifestAtomic(manifestPath, manifest); err != nil {
		return true, err
	}
	storeRoot, ok := managedSessionStoreRootForDir(dir)
	if !ok {
		return true, fmt.Errorf("%w: managed session is not below a v2 root", ErrSessionStore)
	}
	trashRoot := filepath.Join(storeRoot, "trash")
	if err := os.MkdirAll(trashRoot, 0700); err != nil {
		return true, fmt.Errorf("create completed session trash: %w", err)
	}
	trashName := now.Format("20060102T150405.000000000Z") + "--" + manifest.SessionID + "--completed"
	trashPath := filepath.Join(trashRoot, trashName)
	if err := os.Rename(dir, trashPath); err != nil {
		return true, fmt.Errorf("trash completed session: %w", err)
	}
	if err := os.Chtimes(trashPath, now, now); err != nil {
		return true, fmt.Errorf("stamp completed session trash: %w", err)
	}
	_ = os.RemoveAll(trashPath)
	return true, nil
}

func copySessionFileAtomic(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open legacy session: %w", err)
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return fmt.Errorf("create managed session directory: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(destination), ".payload.import.*")
	if err != nil {
		return fmt.Errorf("create session import temp file: %w", err)
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
		return err
	}
	if _, err := io.Copy(tmp, input); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("copy legacy session: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync imported session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close imported session: %w", err)
	}
	if err := os.Rename(tmpPath, destination); err != nil {
		return fmt.Errorf("publish imported session: %w", err)
	}
	cleanup = false
	return nil
}

func copySessionDirectory(source, destination string) error {
	return filepath.WalkDir(source, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, current)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: legacy parts contain a symbolic link", ErrSessionStore)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: legacy parts contain a special file", ErrSessionStore)
		}
		return copySessionFileAtomic(current, target)
	})
}

func SessionDisplaySlug(value string) string {
	value = strings.TrimSpace(value)
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '-' || r == '_' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
		if builder.Len() >= 40 {
			break
		}
	}
	return strings.Trim(builder.String(), ".")
}

func canonicalSessionLocalPath(value string) string {
	cleaned := filepath.Clean(value)
	if runtime.GOOS == "windows" {
		return strings.ToLower(cleaned)
	}
	return cleaned
}

func canonicalSessionRemotePath(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\\", "/"))
	if value == "" {
		return ""
	}
	cleaned := pathpkg.Clean(value)
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(cleaned, "/") {
		cleaned = "/" + cleaned
	}
	return cleaned
}

func writeSessionManifestAtomic(path string, manifest SessionManifest) error {
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session manifest: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".session.json.*")
	if err != nil {
		return fmt.Errorf("create session manifest temp file: %w", err)
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
		return fmt.Errorf("secure session manifest temp file: %w", err)
	}
	if _, err := tmp.Write(encoded); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write session manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync session manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close session manifest: %w", err)
	}
	if err := replaceDownloadedFile(tmpPath, path); err != nil {
		return fmt.Errorf("replace session manifest: %w", err)
	}
	cleanup = false
	return nil
}
