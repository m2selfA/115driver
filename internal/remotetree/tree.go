package remotetree

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

const WalkPageLimit int64 = 500

// Client is the compatibility surface for read-only remote tree traversal.
// Walk intentionally preserves the historical full-directory List semantics.
// Bounded consumers that need page-level early stop should call WalkPaged.
type Client interface {
	List(dirID string, opts ...driver.ListOption) (*[]driver.File, error)
}

// PagedClient is the optional streaming traversal surface.
type PagedClient interface {
	ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error)
}

func ListDirectoryReadOnly(client Client, dirID string) (*[]driver.File, error) {
	if client == nil {
		return nil, errors.New("remote tree client is nil")
	}
	return client.List(dirID, driver.WithRecordOpenTime(false))
}

type Entry struct {
	File         driver.File
	RelativePath string
	RemotePath   string
	ParentPath   string
	Depth        int
}

type Result struct {
	StoppedEarly bool
	DepthLimited bool
}

func ValidateEntryName(name string) error {
	if name == "" || name == "." || name == ".." {
		return errors.New("empty or dot path component")
	}
	if strings.ContainsAny(name, `/\\`) {
		return errors.New("name contains a path separator")
	}
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return errors.New("name is absolute or contains a volume prefix")
	}
	return nil
}

type pendingDirectory struct {
	ID       string
	Relative string
	Remote   string
	Depth    int
}

type walkerState struct {
	maxDepth        int
	visit           func(Entry) (bool, error)
	result          Result
	queue           []pendingDirectory
	seenDirectories map[string]struct{}
}

func newWalkerState(rootID, rootPath string, maxDepth int, visit func(Entry) (bool, error)) (*walkerState, error) {
	if strings.TrimSpace(rootID) == "" {
		return nil, errors.New("remote tree root ID is empty")
	}
	if maxDepth < 0 {
		return nil, errors.New("max depth must be >= 0")
	}
	if visit == nil {
		return nil, errors.New("remote tree visitor is nil")
	}
	if rootPath == "" {
		rootPath = "/"
	}
	return &walkerState{
		maxDepth:        maxDepth,
		visit:           visit,
		queue:           []pendingDirectory{{ID: rootID, Remote: rootPath}},
		seenDirectories: map[string]struct{}{rootID: {}},
	}, nil
}

func (w *walkerState) processEntries(current pendingDirectory, entries []driver.File) (bool, error) {
	for _, entry := range entries {
		if err := ValidateEntryName(entry.Name); err != nil {
			return false, fmt.Errorf("unsafe remote entry %q below %q: %w", entry.Name, current.Remote, err)
		}
		if entry.IsDirectory && strings.TrimSpace(entry.FileID) == "" {
			return false, fmt.Errorf("remote directory %q has no stable ID: %w", JoinDisplayPath(current.Remote, entry.Name), driver.ErrUnexpected)
		}
		depth := current.Depth + 1
		relative := entry.Name
		if current.Relative != "" {
			relative = current.Relative + "/" + entry.Name
		}
		remotePath := JoinDisplayPath(current.Remote, entry.Name)
		stop, err := w.visit(Entry{
			File: entry, RelativePath: relative, RemotePath: remotePath, ParentPath: current.Remote, Depth: depth,
		})
		if err != nil {
			return false, err
		}
		if stop {
			w.result.StoppedEarly = true
			return true, nil
		}
		if !entry.IsDirectory {
			continue
		}
		if w.maxDepth > 0 && depth >= w.maxDepth {
			w.result.DepthLimited = true
			continue
		}
		if _, exists := w.seenDirectories[entry.FileID]; exists {
			return false, fmt.Errorf("remote directory ID %q was encountered more than once", entry.FileID)
		}
		w.seenDirectories[entry.FileID] = struct{}{}
		w.queue = append(w.queue, pendingDirectory{ID: entry.FileID, Relative: relative, Remote: remotePath, Depth: depth})
	}
	return false, nil
}

func Walk(client Client, rootID, rootPath string, maxDepth int, visit func(Entry) (bool, error)) (Result, error) {
	if client == nil {
		return Result{}, errors.New("remote tree client is nil")
	}
	w, err := newWalkerState(rootID, rootPath, maxDepth, visit)
	if err != nil {
		return Result{}, err
	}
	for len(w.queue) > 0 {
		current := w.queue[0]
		w.queue = w.queue[1:]
		entries, err := ListDirectoryReadOnly(client, current.ID)
		if err != nil {
			return w.result, fmt.Errorf("list remote directory %q: %w", current.Remote, err)
		}
		if entries == nil {
			return w.result, fmt.Errorf("list remote directory %q returned an empty response: %w", current.Remote, driver.ErrUnexpected)
		}
		stopped, err := w.processEntries(current, *entries)
		if err != nil {
			return w.result, err
		}
		if stopped {
			return w.result, nil
		}
	}
	return w.result, nil
}

// WalkPaged traverses with explicit page-level reads so bounded consumers can
// stop early without fetching an entire large directory first.
func WalkPaged(client PagedClient, rootID, rootPath string, maxDepth int, visit func(Entry) (bool, error)) (Result, error) {
	if client == nil {
		return Result{}, errors.New("remote tree paged client is nil")
	}
	w, err := newWalkerState(rootID, rootPath, maxDepth, visit)
	if err != nil {
		return Result{}, err
	}
	for len(w.queue) > 0 {
		current := w.queue[0]
		w.queue = w.queue[1:]
		for offset := int64(0); ; {
			entries, err := client.ListPage(current.ID, offset, WalkPageLimit, driver.WithRecordOpenTime(false))
			if err != nil {
				return w.result, fmt.Errorf("list remote directory %q: %w", current.Remote, err)
			}
			if entries == nil {
				return w.result, fmt.Errorf("list remote directory %q returned an empty response: %w", current.Remote, driver.ErrUnexpected)
			}
			if len(*entries) == 0 {
				break
			}
			stopped, err := w.processEntries(current, *entries)
			if err != nil {
				return w.result, err
			}
			if stopped {
				return w.result, nil
			}
			if int64(len(*entries)) < WalkPageLimit {
				break
			}
			offset += int64(len(*entries))
		}
	}
	return w.result, nil
}

func JoinDisplayPath(parent, name string) string {
	if parent == "" || parent == "/" {
		return "/" + strings.TrimPrefix(name, "/")
	}
	return strings.TrimRight(parent, "/") + "/" + strings.TrimPrefix(name, "/")
}
