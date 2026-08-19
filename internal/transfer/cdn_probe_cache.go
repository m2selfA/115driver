package transfer

import (
	"errors"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"
)

const DefaultCDNProbeCacheTTL = 15 * time.Minute

type cdnProbeCacheKey struct {
	endpoint       string
	interfaceName  string
	interfaceIndex int
	localIP        string
}

type cdnProbeCacheEntry struct {
	reachable bool
	latency   time.Duration
	err       error
	expiresAt time.Time
}

// CDNProbeCache caches only network reachability for a normalized CDN endpoint
// and local network path. It intentionally does not cache per-file HTTP status
// or Range support because those properties can depend on the signed URL and
// object being downloaded.
type CDNProbeCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[cdnProbeCacheKey]cdnProbeCacheEntry
	now     func() time.Time
}

// NewCDNProbeCache creates an in-memory host/path reachability cache.
func NewCDNProbeCache(ttl time.Duration) (*CDNProbeCache, error) {
	if ttl <= 0 {
		return nil, errors.New("CDN probe cache TTL must be > 0")
	}
	return &CDNProbeCache{
		ttl:     ttl,
		entries: make(map[cdnProbeCacheKey]cdnProbeCacheEntry),
		now:     time.Now,
	}, nil
}

// NewDefaultCDNProbeCache creates a cache with the default 15-minute TTL.
func NewDefaultCDNProbeCache() *CDNProbeCache {
	cache, _ := NewCDNProbeCache(DefaultCDNProbeCacheTTL)
	return cache
}

func (cache *CDNProbeCache) get(parsedURL *url.URL, path NetworkPath) (CDNPathProbe, bool) {
	key := makeCDNProbeCacheKey(parsedURL, path)
	now := cache.now()

	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry, ok := cache.entries[key]
	if !ok {
		return CDNPathProbe{}, false
	}
	if !now.Before(entry.expiresAt) {
		delete(cache.entries, key)
		return CDNPathProbe{}, false
	}
	return CDNPathProbe{
		Path:      path,
		Reachable: entry.reachable,
		Latency:   entry.latency,
		Cached:    true,
		Err:       entry.err,
	}, true
}

func (cache *CDNProbeCache) put(parsedURL *url.URL, probe CDNPathProbe) {
	key := makeCDNProbeCacheKey(parsedURL, probe.Path)
	cache.mu.Lock()
	cache.entries[key] = cdnProbeCacheEntry{
		reachable: probe.Reachable,
		latency:   probe.Latency,
		err:       probe.Err,
		expiresAt: cache.now().Add(cache.ttl),
	}
	cache.mu.Unlock()
}

// Invalidate removes the cached reachability result for one CDN endpoint and
// local path. Transfer failures can call this before reprobe/retry.
func (cache *CDNProbeCache) Invalidate(rawURL string, path NetworkPath) error {
	parsedURL, err := parseCDNProbeURL(rawURL)
	if err != nil {
		return err
	}
	cache.mu.Lock()
	delete(cache.entries, makeCDNProbeCacheKey(parsedURL, path))
	cache.mu.Unlock()
	return nil
}

// InvalidateHost removes every cached local path for the CDN endpoint used by
// rawURL. Signed path/query components are intentionally ignored.
func (cache *CDNProbeCache) InvalidateHost(rawURL string) error {
	parsedURL, err := parseCDNProbeURL(rawURL)
	if err != nil {
		return err
	}
	endpoint := cdnEndpointKey(parsedURL)
	cache.mu.Lock()
	for key := range cache.entries {
		if key.endpoint == endpoint {
			delete(cache.entries, key)
		}
	}
	cache.mu.Unlock()
	return nil
}

// Clear removes all cached CDN reachability results.
func (cache *CDNProbeCache) Clear() {
	cache.mu.Lock()
	clear(cache.entries)
	cache.mu.Unlock()
}

func makeCDNProbeCacheKey(parsedURL *url.URL, path NetworkPath) cdnProbeCacheKey {
	return cdnProbeCacheKey{
		endpoint:       cdnEndpointKey(parsedURL),
		interfaceName:  path.InterfaceName,
		interfaceIndex: path.InterfaceIndex,
		localIP:        canonicalIP(path.LocalIP).String(),
	}
}

func cdnEndpointKey(parsedURL *url.URL) string {
	host := strings.ToLower(parsedURL.Hostname())
	port := parsedURL.Port()
	if port == "" {
		switch strings.ToLower(parsedURL.Scheme) {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return strings.ToLower(parsedURL.Scheme) + "://" + net.JoinHostPort(host, port)
}
