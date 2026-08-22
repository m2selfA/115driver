package tools

import (
	"fmt"
	"strings"
	"sync"

	"github.com/SheltonZhu/115driver/internal/remoteresolver"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

const (
	maxMCPReadSnapshotCachedFiles  = 50000
	maxMCPReadSnapshotFetchEntries = 100000
)

// mcpReadPageCache is intentionally scoped to one MCP tool call. It reuses
// successful read-only ListPage snapshots for overlapping roots without
// creating cross-request staleness. Errors are never cached.
type mcpReadPageCache struct {
	mu              sync.Mutex
	pages           map[mcpReadPageCacheKey][]driver.File
	inFlight        map[mcpReadPageCacheKey]*mcpReadPageFlight
	cachedFiles     int
	maxFiles        int
	fetchedEntries  int64
	maxFetchEntries int64
}

type mcpReadPageFlight struct {
	done     chan struct{}
	files    []driver.File
	hasFiles bool
	err      error
}

type mcpReadPageCacheKey struct {
	dirID   string
	offset  int64
	limit   int64
	apiURLs string
}

func newMCPReadPageCache() *mcpReadPageCache {
	return newMCPReadPageCacheWithBudgets(maxMCPReadSnapshotCachedFiles, maxMCPReadSnapshotFetchEntries)
}

func newMCPReadPageCacheWithLimit(maxFiles int) *mcpReadPageCache {
	return newMCPReadPageCacheWithBudgets(maxFiles, maxMCPReadSnapshotFetchEntries)
}

func newMCPReadPageCacheWithBudgets(maxFiles int, maxFetchEntries int64) *mcpReadPageCache {
	if maxFiles < 0 {
		maxFiles = 0
	}
	if maxFetchEntries < 0 {
		maxFetchEntries = 0
	}
	return &mcpReadPageCache{
		pages:           make(map[mcpReadPageCacheKey][]driver.File),
		inFlight:        make(map[mcpReadPageCacheKey]*mcpReadPageFlight),
		maxFiles:        maxFiles,
		maxFetchEntries: maxFetchEntries,
	}
}

func cloneMCPReadFiles(files []driver.File) []driver.File {
	cloned := append([]driver.File(nil), files...)
	for i := range cloned {
		if files[i].Labels == nil {
			continue
		}
		cloned[i].Labels = make([]*driver.Label, len(files[i].Labels))
		for j, label := range files[i].Labels {
			if label == nil {
				continue
			}
			labelCopy := *label
			cloned[i].Labels[j] = &labelCopy
		}
	}
	return cloned
}

func cloneMCPReadFilePointer(files *[]driver.File) *[]driver.File {
	if files == nil {
		return nil
	}
	cloned := cloneMCPReadFiles(*files)
	return &cloned
}

func mcpReadOnlyListOptions(opts []driver.ListOption) ([]driver.ListOption, string) {
	effective := append([]driver.ListOption(nil), opts...)
	// The final option wins and guarantees this request-scoped cache can never
	// turn a read-only traversal into a record-open-time side effect.
	effective = append(effective, driver.WithRecordOpenTime(false))
	parsed := driver.DefaultListOptions()
	for _, opt := range effective {
		if opt != nil {
			opt(parsed)
		}
	}
	return effective, strings.Join(parsed.ApiURLs, "\x00")
}

func (c *mcpReadPageCache) listPage(client interface {
	ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error)
}, dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error) {
	effective, apiURLs := mcpReadOnlyListOptions(opts)
	key := mcpReadPageCacheKey{dirID: dirID, offset: offset, limit: limit, apiURLs: apiURLs}

	c.mu.Lock()
	if cached, ok := c.pages[key]; ok {
		copyOfCached := cloneMCPReadFiles(cached)
		c.mu.Unlock()
		return &copyOfCached, nil
	}
	if flight, ok := c.inFlight[key]; ok {
		done := flight.done
		c.mu.Unlock()
		<-done
		if flight.err != nil {
			if !flight.hasFiles {
				return nil, flight.err
			}
			copyOfFailedFlight := cloneMCPReadFiles(flight.files)
			return &copyOfFailedFlight, flight.err
		}
		if !flight.hasFiles {
			return nil, nil
		}
		copyOfFlight := cloneMCPReadFiles(flight.files)
		return &copyOfFlight, nil
	}
	if limit > 0 && c.maxFetchEntries > 0 {
		if c.fetchedEntries > c.maxFetchEntries-limit {
			used := c.fetchedEntries
			budget := c.maxFetchEntries
			c.mu.Unlock()
			return nil, fmt.Errorf("read-only page budget exhausted before requesting %d more entries (used=%d max=%d)", limit, used, budget)
		}
		c.fetchedEntries += limit
	}
	flight := &mcpReadPageFlight{done: make(chan struct{})}
	c.inFlight[key] = flight
	c.mu.Unlock()

	files, err := client.ListPage(dirID, offset, limit, effective...)
	var snapshot []driver.File
	hasFiles := files != nil
	if hasFiles {
		snapshot = cloneMCPReadFiles(*files)
	}

	c.mu.Lock()
	flight.files = snapshot
	flight.hasFiles = hasFiles
	flight.err = err
	if err == nil && hasFiles && (len(snapshot) == 0 || c.cachedFiles+len(snapshot) <= c.maxFiles) {
		c.pages[key] = cloneMCPReadFiles(snapshot)
		c.cachedFiles += len(snapshot)
	}
	close(flight.done)
	delete(c.inFlight, key)
	c.mu.Unlock()

	if err != nil {
		return cloneMCPReadFilePointer(files), err
	}
	if !hasFiles {
		return nil, nil
	}
	copyOfSnapshot := cloneMCPReadFiles(snapshot)
	return &copyOfSnapshot, nil
}

type mcpResolveSnapshotClient struct {
	remoteresolver.Client
	pages *mcpReadPageCache
}

func newMCPResolveSnapshotClient(client remoteresolver.Client) *mcpResolveSnapshotClient {
	return &mcpResolveSnapshotClient{Client: client, pages: newMCPReadPageCache()}
}

func (c *mcpResolveSnapshotClient) ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error) {
	return c.pages.listPage(c.Client, dirID, offset, limit, opts...)
}

type mcpListTreeSnapshotClient struct {
	mcpListTreeClient
	pages *mcpReadPageCache
}

func newMCPListTreeSnapshotClient(client mcpListTreeClient) *mcpListTreeSnapshotClient {
	return &mcpListTreeSnapshotClient{mcpListTreeClient: client, pages: newMCPReadPageCache()}
}

func (c *mcpListTreeSnapshotClient) ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error) {
	return c.pages.listPage(c.mcpListTreeClient, dirID, offset, limit, opts...)
}

type mcpUsageSnapshotClient struct {
	mcpUsageClient
	pages *mcpReadPageCache
}

func newMCPUsageSnapshotClient(client mcpUsageClient) *mcpUsageSnapshotClient {
	return &mcpUsageSnapshotClient{mcpUsageClient: client, pages: newMCPReadPageCache()}
}

func (c *mcpUsageSnapshotClient) ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error) {
	return c.pages.listPage(c.mcpUsageClient, dirID, offset, limit, opts...)
}
