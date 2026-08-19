package transfer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestCDNProbeCacheReusesReachabilityAcrossSignedURLsOnSameHost(t *testing.T) {
	cache, err := NewCDNProbeCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	path := testNetworkPath(7, "10.0.0.7")
	firstURL, _ := parseCDNProbeURL("https://CDN.example.invalid/file-a?token=one")
	secondURL, _ := parseCDNProbeURL("https://cdn.example.invalid:443/file-b?token=two")
	options := CDNProbeOptions{Timeout: time.Second, MaxConcurrency: 1}
	calls := 0
	factory := func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Header:     http.Header{"Content-Range": []string{"bytes 0-0/123"}},
				Body:       io.NopCloser(&emptyReader{}),
				Request:    req,
			}, nil
		}), nil
	}

	first := probeCDNNetworkPaths(context.Background(), firstURL, nil, []NetworkPath{path}, cache, options, factory)
	if len(first) != 1 || first[0].Cached || !first[0].RangeSupported {
		t.Fatalf("unexpected first probe: %#v", first)
	}
	second := probeCDNNetworkPaths(context.Background(), secondURL, nil, []NetworkPath{path}, cache, options, factory)
	if len(second) != 1 || !second[0].Cached || !second[0].Reachable {
		t.Fatalf("expected cached reachability: %#v", second)
	}
	if second[0].RangeChecked || second[0].RangeSupported || second[0].StatusCode != 0 {
		t.Fatalf("cache must not claim per-file HTTP/Range facts: %#v", second[0])
	}
	if calls != 1 {
		t.Fatalf("expected one live request, got %d", calls)
	}
}

func TestCDNProbeCacheExpiresAndCanBeInvalidated(t *testing.T) {
	cache, err := NewCDNProbeCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	cache.now = func() time.Time { return now }
	path := testNetworkPath(7, "10.0.0.7")
	parsedURL, _ := parseCDNProbeURL("https://cdn.example.invalid/file?token=one")
	options := CDNProbeOptions{Timeout: time.Second, MaxConcurrency: 1}
	calls := 0
	factory := func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(&emptyReader{}),
				Request:    req,
			}, nil
		}), nil
	}

	probeCDNNetworkPaths(context.Background(), parsedURL, nil, []NetworkPath{path}, cache, options, factory)
	probeCDNNetworkPaths(context.Background(), parsedURL, nil, []NetworkPath{path}, cache, options, factory)
	if calls != 1 {
		t.Fatalf("expected second request to hit cache, calls=%d", calls)
	}

	now = now.Add(time.Minute)
	probeCDNNetworkPaths(context.Background(), parsedURL, nil, []NetworkPath{path}, cache, options, factory)
	if calls != 2 {
		t.Fatalf("expected expired entry to reprobe, calls=%d", calls)
	}

	if err := cache.Invalidate(parsedURL.String(), path); err != nil {
		t.Fatal(err)
	}
	probeCDNNetworkPaths(context.Background(), parsedURL, nil, []NetworkPath{path}, cache, options, factory)
	if calls != 3 {
		t.Fatalf("expected invalidated path to reprobe, calls=%d", calls)
	}
}

func TestCDNProbeCacheCachesUnreachablePathsUntilInvalidated(t *testing.T) {
	cache := NewDefaultCDNProbeCache()
	path := testNetworkPath(7, "10.0.0.7")
	parsedURL, _ := parseCDNProbeURL("https://cdn.example.invalid/file?token=one")
	options := CDNProbeOptions{Timeout: time.Second, MaxConcurrency: 1}
	calls := 0
	factory := func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("dial timeout")
		}), nil
	}

	first := probeCDNNetworkPaths(context.Background(), parsedURL, nil, []NetworkPath{path}, cache, options, factory)
	second := probeCDNNetworkPaths(context.Background(), parsedURL, nil, []NetworkPath{path}, cache, options, factory)
	if first[0].Reachable || second[0].Reachable || !second[0].Cached {
		t.Fatalf("expected cached unreachable result: first=%#v second=%#v", first[0], second[0])
	}
	if calls != 1 {
		t.Fatalf("expected unreachable path to be probed once, calls=%d", calls)
	}

	if err := cache.Invalidate(parsedURL.String(), path); err != nil {
		t.Fatal(err)
	}
	probeCDNNetworkPaths(context.Background(), parsedURL, nil, []NetworkPath{path}, cache, options, factory)
	if calls != 2 {
		t.Fatalf("expected invalidated unreachable path to reprobe, calls=%d", calls)
	}
}

func TestCDNProbeCacheInvalidateHostClearsAllPathsForEndpointOnly(t *testing.T) {
	cache := NewDefaultCDNProbeCache()
	urlA, _ := parseCDNProbeURL("https://cdn.example.invalid/a")
	urlB, _ := parseCDNProbeURL("https://other.example.invalid/b")
	path1 := testNetworkPath(1, "10.0.0.1")
	path2 := testNetworkPath(2, "10.0.0.2")
	cache.put(urlA, CDNPathProbe{Path: path1, Reachable: true})
	cache.put(urlA, CDNPathProbe{Path: path2, Reachable: true})
	cache.put(urlB, CDNPathProbe{Path: path1, Reachable: true})

	if err := cache.InvalidateHost(urlA.String()); err != nil {
		t.Fatal(err)
	}
	if _, ok := cache.get(urlA, path1); ok {
		t.Fatal("expected first path for invalidated host to be removed")
	}
	if _, ok := cache.get(urlA, path2); ok {
		t.Fatal("expected second path for invalidated host to be removed")
	}
	if _, ok := cache.get(urlB, path1); !ok {
		t.Fatal("unrelated host entry was removed")
	}
}

func TestNewCDNProbeCacheRejectsInvalidTTL(t *testing.T) {
	if _, err := NewCDNProbeCache(0); err == nil {
		t.Fatal("expected zero TTL to fail")
	}
}

type emptyReader struct{}

func (*emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
