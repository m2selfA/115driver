package transfer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const DefaultCompletedSessionRetention = 24 * time.Hour
const DefaultSessionTempRetention = 24 * time.Hour
const DefaultSessionStaleAfter = 7 * 24 * time.Hour

type SessionEntry struct {
	ID           string          `json:"id"`
	Dir          string          `json:"dir"`
	ManifestPath string          `json:"manifest_path"`
	Manifest     SessionManifest `json:"manifest"`
	InUse        bool            `json:"in_use"`
	Stale        bool            `json:"stale,omitempty"`
	NewerVersion bool            `json:"newer_version,omitempty"`
	Corrupt      bool            `json:"corrupt,omitempty"`
	Error        string          `json:"error,omitempty"`
}

type SessionGCOptions struct {
	Now                time.Time
	Retention          time.Duration
	TrashRetention     time.Duration
	CompletedRetention time.Duration
	OlderThan          time.Duration
	DryRun             bool
}

type SessionGCAction struct {
	SessionID string `json:"session_id,omitempty"`
	Path      string `json:"path"`
	Action    string `json:"action"`
	Reason    string `json:"reason"`
}

func (store SessionStore) ListSessions() ([]SessionEntry, error) {
	root, err := store.absoluteRoot()
	if err != nil {
		return nil, err
	}
	v2Root := filepath.Join(root, "v2")
	entries := make([]SessionEntry, 0)
	now := time.Now().UTC()
	err = filepath.WalkDir(v2Root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if os.IsNotExist(walkErr) {
				return nil
			}
			return walkErr
		}
		if !entry.IsDir() || current == v2Root {
			return nil
		}
		relative, err := filepath.Rel(v2Root, current)
		if err != nil {
			return err
		}
		parts := strings.Split(relative, string(filepath.Separator))
		if len(parts) == 1 && parts[0] != "upload" && parts[0] != "download" {
			return filepath.SkipDir
		}
		if len(parts) == 2 && parts[1] != "file" && parts[1] != "tree" {
			return filepath.SkipDir
		}
		if len(parts) < 4 {
			return nil
		}
		if len(parts) > 4 {
			return filepath.SkipDir
		}
		id := sessionIDFromDir(current)
		if !validSessionID(id) {
			return filepath.SkipDir
		}
		entries = append(entries, store.readSessionEntry(root, current, now))
		return filepath.SkipDir
	})
	if os.IsNotExist(err) {
		err = nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan session store: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Manifest.LastUsedAt.Equal(entries[j].Manifest.LastUsedAt) {
			return entries[i].ID < entries[j].ID
		}
		return entries[i].Manifest.LastUsedAt.After(entries[j].Manifest.LastUsedAt)
	})
	return entries, nil
}

func (store SessionStore) readSessionEntry(root, dir string, now time.Time) SessionEntry {
	manifestPath := filepath.Join(dir, "session.json")
	dirID := sessionIDFromDir(dir)
	item := SessionEntry{ID: dirID, Dir: dir, ManifestPath: manifestPath}
	data, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		item.Corrupt = true
		if os.IsNotExist(readErr) {
			item.Error = "session manifest is missing"
		} else {
			item.Error = readErr.Error()
		}
		probeSessionEntryLock(root, &item)
		return item
	}
	var header struct {
		Version   int    `json:"version"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		item.Corrupt = true
		item.Error = err.Error()
		probeSessionEntryLock(root, &item)
		return item
	}
	if header.Version > SessionManifestVersion {
		item.NewerVersion = true
		item.Manifest.Version = header.Version
		item.Manifest.SessionID = header.SessionID
		if candidate := strings.ToLower(strings.TrimSpace(header.SessionID)); validSessionID(candidate) {
			item.ID = candidate
		}
		probeSessionEntryLock(root, &item)
		return item
	}
	if err := json.Unmarshal(data, &item.Manifest); err != nil {
		item.Corrupt = true
		item.Error = err.Error()
		probeSessionEntryLock(root, &item)
		return item
	}
	if err := validateSessionManifest(item.Manifest); err != nil {
		item.Corrupt = true
		item.Error = err.Error()
		probeSessionEntryLock(root, &item)
		return item
	}
	if item.Manifest.SessionID != dirID {
		item.Corrupt = true
		item.Error = "session directory id does not match manifest identity"
		probeSessionEntryLock(root, &item)
		return item
	}
	item.ID = item.Manifest.SessionID
	probeSessionEntryLock(root, &item)
	activity := sessionManifestActivity(item.Manifest)
	item.Stale = item.Manifest.State == "active" && !activity.IsZero() && now.Sub(activity) >= DefaultSessionStaleAfter
	return item
}

func probeSessionEntryLock(root string, entry *SessionEntry) {
	if entry == nil || !validSessionID(entry.ID) {
		return
	}
	lockPath := filepath.Join(root, "locks", entry.ID[:2], entry.ID+".lock")
	entry.InUse, _ = SessionLockInUse(lockPath)
}

func validSessionID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '2' || r > '7') {
			return false
		}
	}
	return true
}

func (store SessionStore) InspectSession(prefix string) (SessionEntry, error) {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return SessionEntry{}, fmt.Errorf("%w: session id is empty", ErrSessionStore)
	}
	entries, err := store.ListSessions()
	if err != nil {
		return SessionEntry{}, err
	}
	matches := make([]SessionEntry, 0, 1)
	for _, entry := range entries {
		if strings.HasPrefix(strings.ToLower(entry.ID), prefix) {
			matches = append(matches, entry)
		}
	}
	if len(matches) == 0 {
		return SessionEntry{}, os.ErrNotExist
	}
	if len(matches) > 1 {
		return SessionEntry{}, fmt.Errorf("%w: session id prefix %q is ambiguous", ErrSessionStore, prefix)
	}
	return matches[0], nil
}

func (store SessionStore) TrashSession(prefix, reason string) (string, error) {
	return store.TrashSessionWithHook(prefix, reason, nil)
}

func (store SessionStore) TrashSessionWithHook(prefix, reason string, beforeTrash func(SessionEntry) error) (string, error) {
	entry, err := store.InspectSession(prefix)
	if err != nil {
		return "", err
	}
	if entry.NewerVersion {
		return "", fmt.Errorf("%w: version %d", ErrSessionNewerVersion, entry.Manifest.Version)
	}
	root, err := store.absoluteRoot()
	if err != nil {
		return "", err
	}
	id := entry.ID
	if len(id) < 2 {
		return "", fmt.Errorf("%w: invalid session id %q", ErrSessionStore, id)
	}
	lock, err := AcquireSessionLock(filepath.Join(root, "locks", id[:2], id+".lock"), "")
	if err != nil {
		return "", err
	}
	defer lock.Close()
	current, ok, err := store.revalidateTrashCandidate(entry, reason, time.Now().UTC(), 0)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: session changed while waiting for its lock", ErrSessionStore)
	}
	if beforeTrash != nil {
		if err := beforeTrash(current); err != nil {
			return "", err
		}
	}
	return store.trashDir(current.Dir, current.ID, reason, time.Now().UTC())
}

func (store SessionStore) GC(options SessionGCOptions) ([]SessionGCAction, error) {
	root, err := store.absoluteRoot()
	if err != nil {
		return nil, err
	}
	if options.Now.IsZero() {
		options.Now = time.Now().UTC()
	}
	if options.Retention <= 0 {
		options.Retention = 30 * 24 * time.Hour
	}
	if options.OlderThan > 0 {
		options.Retention = options.OlderThan
	}
	if options.TrashRetention <= 0 {
		options.TrashRetention = 7 * 24 * time.Hour
	}
	if options.CompletedRetention <= 0 {
		options.CompletedRetention = DefaultCompletedSessionRetention
	}
	entries, err := store.ListSessions()
	if err != nil {
		return nil, err
	}
	actions := make([]SessionGCAction, 0)
	for _, entry := range entries {
		if entry.NewerVersion {
			actions = append(actions, SessionGCAction{SessionID: entry.ID, Path: entry.Dir, Action: "skip", Reason: "newer-version"})
			continue
		}
		if entry.InUse {
			actions = append(actions, SessionGCAction{SessionID: entry.ID, Path: entry.Dir, Action: "skip", Reason: "locked"})
			continue
		}
		if entry.Corrupt {
			if options.DryRun {
				actions = append(actions, SessionGCAction{SessionID: entry.ID, Path: entry.Dir, Action: "trash", Reason: "corrupt"})
				continue
			}
			_, trashed, err := store.trashUnlockedEntry(root, entry, "corrupt", options.Now, 0)
			if errors.Is(err, ErrSessionLocked) {
				actions = append(actions, SessionGCAction{SessionID: entry.ID, Path: entry.Dir, Action: "skip", Reason: "locked"})
				continue
			}
			if err != nil {
				return actions, err
			}
			if trashed {
				actions = append(actions, SessionGCAction{SessionID: entry.ID, Path: entry.Dir, Action: "trash", Reason: "corrupt"})
			} else {
				actions = append(actions, SessionGCAction{SessionID: entry.ID, Path: entry.Dir, Action: "skip", Reason: "changed"})
			}
			continue
		}
		activity := sessionManifestActivity(entry.Manifest)
		age := options.Now.Sub(activity)
		threshold := options.Retention
		reason := "expired"
		if entry.Manifest.State == "completed" {
			threshold = options.CompletedRetention
			reason = "completed"
		}
		if age < threshold {
			continue
		}
		if options.DryRun {
			actions = append(actions, SessionGCAction{SessionID: entry.ID, Path: entry.Dir, Action: "trash", Reason: reason})
			continue
		}
		_, trashed, err := store.trashUnlockedEntry(root, entry, reason, options.Now, threshold)
		if errors.Is(err, ErrSessionLocked) {
			actions = append(actions, SessionGCAction{SessionID: entry.ID, Path: entry.Dir, Action: "skip", Reason: "locked"})
			continue
		}
		if err != nil {
			return actions, err
		}
		if trashed {
			actions = append(actions, SessionGCAction{SessionID: entry.ID, Path: entry.Dir, Action: "trash", Reason: reason})
		} else {
			actions = append(actions, SessionGCAction{SessionID: entry.ID, Path: entry.Dir, Action: "skip", Reason: "changed"})
		}
	}
	trashRoot := filepath.Join(root, "trash")
	trashEntries, readErr := os.ReadDir(trashRoot)
	if readErr == nil {
		for _, entry := range trashEntries {
			path := filepath.Join(trashRoot, entry.Name())
			info, infoErr := entry.Info()
			if infoErr != nil || !entry.IsDir() {
				continue
			}
			retention := options.TrashRetention
			if strings.HasSuffix(entry.Name(), "--completed") {
				retention = options.CompletedRetention
			}
			if options.Now.Sub(info.ModTime()) < retention {
				continue
			}
			actions = append(actions, SessionGCAction{Path: path, Action: "purge", Reason: "trash-retention"})
			if !options.DryRun {
				if err := os.RemoveAll(path); err != nil {
					return actions, err
				}
			}
		}
	} else if !os.IsNotExist(readErr) {
		return actions, readErr
	}
	tempActions, err := store.gcSessionTempFiles(root, options.Now, options.DryRun)
	if err != nil {
		return actions, err
	}
	actions = append(actions, tempActions...)
	return actions, nil
}

func (store SessionStore) gcSessionTempFiles(root string, now time.Time, dryRun bool) ([]SessionGCAction, error) {
	actions := make([]SessionGCAction, 0)
	roots := []string{filepath.Join(root, "v2"), root}
	seen := make(map[string]struct{})
	for _, scanRoot := range roots {
		err := filepath.WalkDir(scanRoot, func(current string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			if entry.IsDir() {
				if scanRoot == root && current != root && filepath.Base(current) != "v2" {
					return filepath.SkipDir
				}
				if current == scanRoot || !isManagedSessionTempDirName(entry.Name()) {
					return nil
				}
				info, err := entry.Info()
				if err != nil || now.Sub(info.ModTime()) < DefaultSessionTempRetention {
					return err
				}
				locked, err := managedSessionPathLocked(root, current)
				if err != nil {
					return err
				}
				if locked {
					return filepath.SkipDir
				}
				actions = append(actions, SessionGCAction{Path: current, Action: "purge", Reason: "tmp-retention"})
				if !dryRun {
					if err := os.RemoveAll(current); err != nil && !os.IsNotExist(err) {
						return err
					}
				}
				return filepath.SkipDir
			}
			name := entry.Name()
			if !isManagedSessionTempName(name) {
				return nil
			}
			if _, ok := seen[current]; ok {
				return nil
			}
			seen[current] = struct{}{}
			info, err := entry.Info()
			if err != nil || now.Sub(info.ModTime()) < DefaultSessionTempRetention {
				return err
			}
			locked, err := managedSessionPathLocked(root, current)
			if err != nil {
				return err
			}
			if locked {
				return nil
			}
			actions = append(actions, SessionGCAction{Path: current, Action: "purge", Reason: "tmp-retention"})
			if !dryRun {
				if err := os.Remove(current); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return actions, err
		}
	}
	return actions, nil
}

func isManagedSessionTempName(name string) bool {
	return strings.HasPrefix(name, ".session.json.") ||
		strings.HasPrefix(name, ".payload.json.") ||
		strings.HasPrefix(name, ".payload.import.") ||
		strings.HasPrefix(name, ".lease.json.") ||
		strings.HasPrefix(name, ".maintenance.json.") ||
		(strings.HasPrefix(name, ".") && (strings.Contains(name, ".upload.json.") || strings.Contains(name, ".download.json.")))
}

func isManagedSessionTempDirName(name string) bool {
	return strings.HasPrefix(name, ".legacy-import.")
}

func managedSessionPathLocked(root, current string) (bool, error) {
	v2Root := filepath.Join(root, "v2")
	relative, err := filepath.Rel(v2Root, current)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || relative == ".." {
		return false, err
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) < 4 {
		return false, nil
	}
	id := sessionIDFromDir(filepath.Join(v2Root, parts[0], parts[1], parts[2], parts[3]))
	if !validSessionID(id) {
		return false, nil
	}
	return SessionLockInUse(filepath.Join(root, "locks", id[:2], id+".lock"))
}

func SessionLockInUse(lockPath string) (bool, error) {
	if _, err := os.Lstat(lockPath); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	lock, err := AcquireSessionLock(lockPath, "")
	if err == nil {
		return false, lock.Close()
	}
	if errors.Is(err, ErrSessionLocked) {
		return true, nil
	}
	return false, err
}

func sessionManifestActivity(manifest SessionManifest) time.Time {
	if !manifest.LastUsedAt.IsZero() {
		return manifest.LastUsedAt
	}
	if !manifest.UpdatedAt.IsZero() {
		return manifest.UpdatedAt
	}
	return manifest.CreatedAt
}

func (store SessionStore) trashUnlockedEntry(root string, entry SessionEntry, reason string, now time.Time, minAge time.Duration) (string, bool, error) {
	id := entry.ID
	if len(id) < 2 {
		id = sessionIDFromDir(entry.Dir)
	}
	if len(id) < 2 {
		return "", false, fmt.Errorf("%w: cannot determine session id for %q", ErrSessionStore, entry.Dir)
	}
	lock, err := AcquireSessionLock(filepath.Join(root, "locks", id[:2], id+".lock"), "")
	if err != nil {
		return "", false, err
	}
	defer lock.Close()
	current, ok, err := store.revalidateTrashCandidate(entry, reason, now, minAge)
	if err != nil || !ok {
		return "", false, err
	}
	path, err := store.trashDir(current.Dir, current.ID, reason, now)
	return path, err == nil, err
}

func (store SessionStore) revalidateTrashCandidate(entry SessionEntry, reason string, now time.Time, minAge time.Duration) (SessionEntry, bool, error) {
	entries, err := store.ListSessions()
	if err != nil {
		return SessionEntry{}, false, err
	}
	var current *SessionEntry
	for i := range entries {
		if entries[i].Dir == entry.Dir {
			current = &entries[i]
			break
		}
	}
	if current == nil || current.ID != entry.ID || current.NewerVersion {
		return SessionEntry{}, false, nil
	}
	if current.Corrupt {
		return *current, (entry.Corrupt && reason == "corrupt") || reason == "manual", nil
	}
	if entry.Corrupt || current.Manifest.Version != SessionManifestVersion ||
		current.Manifest.SessionID != entry.Manifest.SessionID ||
		current.Manifest.IdentitySHA256 != entry.Manifest.IdentitySHA256 ||
		current.Manifest.Identity != entry.Manifest.Identity {
		return SessionEntry{}, false, nil
	}
	activity := sessionManifestActivity(current.Manifest)
	if minAge > 0 {
		if reason == "expired" && current.Manifest.State == "completed" {
			return SessionEntry{}, false, nil
		}
		if reason == "completed" && current.Manifest.State != "completed" {
			return SessionEntry{}, false, nil
		}
		if now.Sub(activity) < minAge {
			return SessionEntry{}, false, nil
		}
	}
	return *current, true, nil
}

func (store SessionStore) trashDir(sourceDir, id, reason string, now time.Time) (string, error) {
	root, err := store.absoluteRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(root, "trash"), 0700); err != nil {
		return "", err
	}
	reason = SessionDisplaySlug(reason)
	if reason == "" {
		reason = "removed"
	}
	name := now.UTC().Format("20060102T150405.000000000Z") + "--" + id + "--" + reason
	destination := filepath.Join(root, "trash", name)
	if err := os.Rename(sourceDir, destination); err != nil {
		return "", fmt.Errorf("trash session: %w", err)
	}
	if err := os.Chtimes(destination, now, now); err != nil {
		return "", fmt.Errorf("stamp trashed session: %w", err)
	}
	return destination, nil
}

func (store SessionStore) absoluteRoot() (string, error) {
	root := strings.TrimSpace(store.Root)
	if root == "" {
		return "", fmt.Errorf("%w: store root is empty", ErrSessionStore)
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	return absolute, nil
}

func sessionIDFromDir(dir string) string {
	name := filepath.Base(dir)
	index := strings.LastIndex(name, "--")
	if index < 0 || index+2 >= len(name) {
		return ""
	}
	return strings.ToLower(name[index+2:])
}
