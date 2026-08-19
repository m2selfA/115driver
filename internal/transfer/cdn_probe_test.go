package transfer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestProbeCDNNetworkPathPreservesHeadersAndChecksRange(t *testing.T) {
	path := testNetworkPath(7, "10.0.0.7")
	parsedURL, err := parseCDNProbeURL("https://cdn.example.invalid/file?token=secret")
	if err != nil {
		t.Fatal(err)
	}
	headers := http.Header{
		"User-Agent": []string{""},
		"Cookie":     []string{"session=abc"},
		"Range":      []string{"bytes=10-20"},
	}
	options := CDNProbeOptions{Timeout: time.Second, MaxConcurrency: 1}

	probe := probeCDNNetworkPath(context.Background(), parsedURL, headers, path, options, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("expected GET, got %s", req.Method)
			}
			if got := req.Header.Get("Range"); got != "bytes=0-0" {
				t.Fatalf("unexpected Range header: %q", got)
			}
			if values, ok := req.Header["User-Agent"]; !ok || len(values) != 1 || values[0] != "" {
				t.Fatalf("empty User-Agent was not preserved: %#v", req.Header["User-Agent"])
			}
			if got := req.Header.Get("Cookie"); got != "session=abc" {
				t.Fatalf("unexpected Cookie header: %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Header:     http.Header{"Content-Range": []string{"bytes 0-0/123"}},
				Body:       io.NopCloser(strings.NewReader("x")),
				Request:    req,
			}, nil
		}), nil
	})

	if !probe.Reachable || !probe.RangeChecked || !probe.RangeSupported {
		t.Fatalf("unexpected probe result: %#v", probe)
	}
	if probe.StatusCode != http.StatusPartialContent || probe.ContentRange != "bytes 0-0/123" {
		t.Fatalf("unexpected HTTP diagnostics: %#v", probe)
	}
	if probe.FinalHost != "cdn.example.invalid" {
		t.Fatalf("unexpected final host: %q", probe.FinalHost)
	}
	if got := headers.Get("Range"); got != "bytes=10-20" {
		t.Fatalf("caller headers were mutated: Range=%q", got)
	}
}

func TestProbeCDNNetworkPathHTTPResponseIsReachableWithoutRangeSupport(t *testing.T) {
	path := testNetworkPath(7, "10.0.0.7")
	parsedURL, _ := parseCDNProbeURL("https://cdn.example.invalid/file")
	options := CDNProbeOptions{Timeout: time.Second, MaxConcurrency: 1}

	probe := probeCDNNetworkPath(context.Background(), parsedURL, nil, path, options, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("denied")),
				Request:    req,
			}, nil
		}), nil
	})

	if !probe.Reachable || !probe.RangeChecked || probe.RangeSupported {
		t.Fatalf("expected reachable non-range response: %#v", probe)
	}
	if probe.StatusCode != http.StatusForbidden {
		t.Fatalf("unexpected status: %d", probe.StatusCode)
	}
}

func TestProbeCDNNetworkPathsRespectsConcurrencyLimit(t *testing.T) {
	parsedURL, _ := parseCDNProbeURL("https://cdn.example.invalid/file")
	paths := []NetworkPath{
		testNetworkPath(1, "10.0.0.1"),
		testNetworkPath(2, "10.0.0.2"),
		testNetworkPath(3, "10.0.0.3"),
		testNetworkPath(4, "10.0.0.4"),
	}
	options := CDNProbeOptions{Timeout: time.Second, MaxConcurrency: 2}
	var active atomic.Int32
	var maxActive atomic.Int32

	results := probeCDNNetworkPaths(context.Background(), parsedURL, nil, paths, nil, options, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			now := active.Add(1)
			defer active.Add(-1)
			for {
				old := maxActive.Load()
				if now <= old || maxActive.CompareAndSwap(old, now) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    req,
			}, nil
		}), nil
	})
	if len(results) != len(paths) {
		t.Fatalf("expected %d results, got %d", len(paths), len(results))
	}
	if maxActive.Load() > 2 {
		t.Fatalf("expected at most 2 concurrent probes, saw %d", maxActive.Load())
	}
}

func TestSelectReachableCDNNetworkPathsUsesOneBestPathPerInterface(t *testing.T) {
	probes := []CDNPathProbe{
		{Path: testNetworkPath(2, "10.0.0.2"), Reachable: true, Latency: 40 * time.Millisecond},
		{Path: testNetworkPath(2, "2001:db8::2"), Reachable: true, Latency: 10 * time.Millisecond},
		{Path: testNetworkPath(3, "10.0.0.3"), Reachable: false},
		{Path: testNetworkPath(5, "2001:db8::5"), Reachable: true, Latency: 15 * time.Millisecond},
		{Path: testNetworkPath(5, "10.0.0.5"), Reachable: true, Latency: 15 * time.Millisecond},
	}

	paths := SelectReachableCDNNetworkPaths(probes)
	if len(paths) != 2 {
		t.Fatalf("expected 2 paths, got %#v", paths)
	}
	if paths[0].InterfaceIndex != 2 || paths[0].LocalIP.String() != "2001:db8::2" {
		t.Fatalf("unexpected selected path for interface 2: %#v", paths[0])
	}
	if paths[1].InterfaceIndex != 5 || paths[1].LocalIP.String() != "10.0.0.5" {
		t.Fatalf("unexpected selected path for interface 5: %#v", paths[1])
	}
}

func TestProbeCDNNetworkPathStripsSignedURLFromErrors(t *testing.T) {
	path := testNetworkPath(7, "10.0.0.7")
	parsedURL, _ := parseCDNProbeURL("https://cdn.example.invalid/file?token=super-secret")
	options := CDNProbeOptions{Timeout: time.Second, MaxConcurrency: 1}

	probe := probeCDNNetworkPath(context.Background(), parsedURL, nil, path, options, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("network down")}
		}), nil
	})
	if probe.Err == nil {
		t.Fatal("expected probe failure")
	}
	if strings.Contains(probe.Err.Error(), "super-secret") || strings.Contains(probe.Err.Error(), "/file") {
		t.Fatalf("signed URL leaked through error: %v", probe.Err)
	}
	if !strings.Contains(probe.Err.Error(), "cdn.example.invalid") || !strings.Contains(probe.Err.Error(), "network down") {
		t.Fatalf("expected sanitized host/network error, got %v", probe.Err)
	}
}

func TestParseCDNProbeURLStripsSignedInputFromErrors(t *testing.T) {
	_, err := parseCDNProbeURL("https://cdn.example.invalid/%zz?token=super-secret")
	if err == nil {
		t.Fatal("expected invalid escape to fail")
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "cdn.example.invalid") || strings.Contains(err.Error(), "token=") {
		t.Fatalf("invalid signed URL leaked through parse error: %v", err)
	}
}

func TestProbeCDNNetworkPathsRequireRangeValidationBypassesReachabilityCache(t *testing.T) {
	path := testNetworkPath(7, "10.0.0.7")
	parsedURL, _ := parseCDNProbeURL("https://cdn.example.invalid/file?token=one")
	cache, err := NewCDNProbeCache(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cache.put(parsedURL, CDNPathProbe{Path: path, Reachable: true, Latency: time.Millisecond})
	options := DefaultCDNProbeOptions()
	options.RequireRangeValidation = true
	calls := 0
	probes := probeCDNNetworkPaths(context.Background(), parsedURL, http.Header{"User-Agent": []string{""}}, []NetworkPath{path}, cache, options, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if req.Header.Get("Range") != "bytes=0-0" {
				t.Fatalf("unexpected probe range: %q", req.Header.Get("Range"))
			}
			return &http.Response{
				StatusCode: http.StatusPartialContent,
				Header:     http.Header{"Content-Range": []string{"bytes 0-0/10"}},
				Body:       io.NopCloser(strings.NewReader("x")),
				Request:    req,
			}, nil
		}), nil
	})
	if calls != 1 || len(probes) != 1 || probes[0].Cached || !probes[0].RangeSupported {
		t.Fatalf("range validation used cached reachability: calls=%d probes=%#v", calls, probes)
	}
	paths := SelectRangeSupportedCDNNetworkPaths(probes)
	if len(paths) != 1 || paths[0].InterfaceIndex != path.InterfaceIndex {
		t.Fatalf("unexpected range paths: %#v", paths)
	}
}

func TestSelectRangeSupportedCDNNetworkPathsExcludesCachedAndNonRangeProbes(t *testing.T) {
	probes := []CDNPathProbe{
		{Path: testNetworkPath(1, "10.0.0.1"), Reachable: true, Cached: true},
		{Path: testNetworkPath(2, "10.0.0.2"), Reachable: true, RangeChecked: true, RangeSupported: false},
		{Path: testNetworkPath(3, "10.0.0.3"), Reachable: true, RangeChecked: true, RangeSupported: true, Latency: 20 * time.Millisecond},
		{Path: testNetworkPath(3, "2001:db8::3"), Reachable: true, RangeChecked: true, RangeSupported: true, Latency: 10 * time.Millisecond},
	}
	paths := SelectRangeSupportedCDNNetworkPaths(probes)
	if len(paths) != 1 || paths[0].InterfaceIndex != 3 || paths[0].LocalIP.String() != "2001:db8::3" {
		t.Fatalf("unexpected range-capable selection: %#v", paths)
	}
}

func TestProbeCDNNetworkPathsValidatesURLAndOptions(t *testing.T) {
	path := testNetworkPath(7, "10.0.0.7")
	if _, err := ProbeCDNNetworkPaths(context.Background(), "file:///tmp/data", nil, []NetworkPath{path}, nil); err == nil {
		t.Fatal("expected unsupported URL scheme to fail")
	}
	if _, err := ProbeCDNNetworkPaths(context.Background(), "https://cdn.example.invalid/file", nil, []NetworkPath{path}, nil, WithCDNProbeTimeout(0)); err == nil {
		t.Fatal("expected zero timeout to fail")
	}
	if _, err := ProbeCDNNetworkPaths(context.Background(), "https://cdn.example.invalid/file", nil, []NetworkPath{path}, nil, WithCDNProbeConcurrency(0)); err == nil {
		t.Fatal("expected zero concurrency to fail")
	}
}
