package remoteresolver

import (
	"container/list"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

const (
	RootID                           = "0"
	FileResolvePageLimit       int64 = 100
	defaultDirResolveCacheSize       = 256
)

// Client is the minimal read-only 115 surface required for remote path
// resolution. File lookup pages are always requested with record_open_time=0.
type Client interface {
	DirName2CID(dir string) (*driver.APIGetDirIDResp, error)
	ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error)
}

type dirCacheEntry struct {
	path string
	id   string
}

// PathResolver caches successful directory lookups within one read-only
// planning/request scope. Its cache state is concurrency-safe; callers remain
// responsible for using a client that supports their concurrency pattern. A
// resolver must be discarded after remote mutations that can change path IDs.
type PathResolver struct {
	client   Client
	capacity int

	mu    sync.Mutex
	dirs  map[string]*list.Element
	order *list.List
}

func New(client Client) *PathResolver {
	return NewWithCapacity(client, defaultDirResolveCacheSize)
}

// NewWithCapacity is primarily useful for bounded-cache contract tests and
// specialized request scopes. A non-positive capacity disables caching.
func NewWithCapacity(client Client, capacity int) *PathResolver {
	return &PathResolver{
		client:   client,
		capacity: capacity,
		dirs:     make(map[string]*list.Element),
		order:    list.New(),
	}
}

func (r *PathResolver) cachedDir(cleaned string) (string, bool) {
	if r == nil || r.capacity <= 0 {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	element, ok := r.dirs[cleaned]
	if !ok {
		return "", false
	}
	r.order.MoveToFront(element)
	return element.Value.(dirCacheEntry).id, true
}

func (r *PathResolver) cacheDir(cleaned, id string) {
	if r == nil || r.capacity <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if element, ok := r.dirs[cleaned]; ok {
		element.Value = dirCacheEntry{path: cleaned, id: id}
		r.order.MoveToFront(element)
		return
	}
	element := r.order.PushFront(dirCacheEntry{path: cleaned, id: id})
	r.dirs[cleaned] = element
	for r.order.Len() > r.capacity {
		oldest := r.order.Back()
		if oldest == nil {
			break
		}
		entry := oldest.Value.(dirCacheEntry)
		delete(r.dirs, entry.path)
		r.order.Remove(oldest)
	}
}

func cleanRemotePath(remotePath string) string {
	return strings.Trim(remotePath, "/")
}

func ResolveDir(client Client, remotePath string) (string, error) {
	return New(client).ResolveDir(remotePath)
}

func (r *PathResolver) ResolveDir(remotePath string) (string, error) {
	cleaned := cleanRemotePath(remotePath)
	if cleaned == "" {
		return RootID, nil
	}
	if id, ok := r.cachedDir(cleaned); ok {
		return id, nil
	}

	resp, err := r.client.DirName2CID(cleaned)
	if err != nil {
		if errors.Is(err, driver.ErrNotExist) {
			return "", fmt.Errorf("directory not found: %s: %w", remotePath, err)
		}
		return "", fmt.Errorf("resolve directory %s: %w", remotePath, err)
	}
	if resp == nil {
		return "", fmt.Errorf("resolve directory %s returned empty response: %w", remotePath, driver.ErrUnexpected)
	}
	id := strings.TrimSpace(string(resp.CategoryID))
	if id == "" || id == RootID {
		return "", fmt.Errorf("directory not found: %s: %w", remotePath, driver.ErrNotExist)
	}
	r.cacheDir(cleaned, id)
	return id, nil
}

func ResolveFile(client Client, remotePath string) (string, error) {
	return New(client).ResolveFile(remotePath)
}

func (r *PathResolver) ResolveFile(remotePath string) (string, error) {
	cleaned := cleanRemotePath(remotePath)
	dir := path.Dir(cleaned)
	fileName := path.Base(cleaned)

	var dirID string
	if dir == "." || dir == "" {
		dirID = RootID
	} else {
		var err error
		dirID, err = r.ResolveDir(dir)
		if err != nil {
			return "", err
		}
	}

	for offset := int64(0); ; offset += FileResolvePageLimit {
		files, err := r.client.ListPage(dirID, offset, FileResolvePageLimit, driver.WithRecordOpenTime(false))
		if err != nil {
			return "", fmt.Errorf("failed to list directory: %w", err)
		}
		if files == nil {
			return "", fmt.Errorf("failed to list directory: empty response: %w", driver.ErrUnexpected)
		}
		if len(*files) == 0 {
			break
		}
		for _, file := range *files {
			if file.Name == fileName && !file.IsDirectory {
				return file.FileID, nil
			}
		}
		if int64(len(*files)) < FileResolvePageLimit {
			break
		}
	}
	return "", fmt.Errorf("file not found: %s: %w", remotePath, driver.ErrNotExist)
}

func ResolvePath(client Client, remotePath string) (string, bool, error) {
	return New(client).ResolvePath(remotePath)
}

func (r *PathResolver) ResolvePath(remotePath string) (string, bool, error) {
	if cleanRemotePath(remotePath) == "" {
		return RootID, true, nil
	}

	// Try as directory first. Only an explicit not-found result may fall back
	// to file lookup; transport/protocol failures must not trigger a second API
	// path that can obscure the original error.
	dirID, dirErr := r.ResolveDir(remotePath)
	if dirErr == nil && dirID != "" {
		return dirID, true, nil
	}
	if dirErr != nil && !errors.Is(dirErr, driver.ErrNotExist) {
		return "", false, dirErr
	}

	// Try as file using the same scoped directory cache.
	fileID, fileErr := r.ResolveFile(remotePath)
	if fileErr != nil {
		if errors.Is(fileErr, driver.ErrNotExist) {
			return "", false, fmt.Errorf("path not found: %s: %w", remotePath, driver.ErrNotExist)
		}
		return "", false, fileErr
	}
	return fileID, false, nil
}
