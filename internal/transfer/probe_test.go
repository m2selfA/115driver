package transfer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestDefaultProbeOptions(t *testing.T) {
	options := DefaultProbeOptions()
	if len(options.URLs) != 1 || options.URLs[0] != Default115ProbeURL {
		t.Fatalf("unexpected default probe URLs: %#v", options.URLs)
	}
	if options.Timeout <= 0 {
		t.Fatalf("expected positive timeout, got %s", options.Timeout)
	}
	if options.MaxConcurrency <= 0 {
		t.Fatalf("expected positive concurrency, got %d", options.MaxConcurrency)
	}
}

func TestProbeOptionsValidateRejectsInvalidConfiguration(t *testing.T) {
	tests := []ProbeOptions{
		{URLs: []string{Default115ProbeURL}, Timeout: 0, MaxConcurrency: 1},
		{URLs: []string{Default115ProbeURL}, Timeout: time.Second, MaxConcurrency: 0},
		{URLs: nil, Timeout: time.Second, MaxConcurrency: 1},
		{URLs: []string{"file:///tmp/probe"}, Timeout: time.Second, MaxConcurrency: 1},
		{URLs: []string{"https:///missing-host"}, Timeout: time.Second, MaxConcurrency: 1},
	}
	for _, options := range tests {
		if err := options.validate(); err == nil {
			t.Fatalf("expected invalid options to fail: %#v", options)
		}
	}
}

func TestProbeNetworkPathAnyHTTPResponseMeansReachable(t *testing.T) {
	path := testNetworkPath(7, "10.0.0.7")
	options := ProbeOptions{URLs: []string{"https://example.invalid/probe"}, Timeout: time.Second, MaxConcurrency: 1}
	probe := probe115NetworkPath(context.Background(), path, options, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodHead {
				t.Fatalf("expected HEAD request, got %s", req.Method)
			}
			return &http.Response{
				StatusCode: http.StatusForbidden,
				Body:       io.NopCloser(strings.NewReader("denied")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}), nil
	})
	if !probe.Reachable {
		t.Fatalf("expected HTTP response to count as reachable: %v", probe.Err)
	}
	if probe.StatusCode != http.StatusForbidden {
		t.Fatalf("unexpected status code: %d", probe.StatusCode)
	}
	if probe.URL != options.URLs[0] {
		t.Fatalf("unexpected successful URL: %q", probe.URL)
	}
}

func TestProbeNetworkPathFallsBackAcrossProbeURLs(t *testing.T) {
	path := testNetworkPath(7, "10.0.0.7")
	options := ProbeOptions{
		URLs:           []string{"https://first.invalid/", "https://second.invalid/"},
		Timeout:        time.Second,
		MaxConcurrency: 1,
	}
	var attempts atomic.Int32
	probe := probe115NetworkPath(context.Background(), path, options, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempt := attempts.Add(1)
			if attempt == 1 {
				return nil, errors.New("first endpoint unavailable")
			}
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Body:       io.NopCloser(strings.NewReader("not found")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}), nil
	})
	if !probe.Reachable || probe.URL != options.URLs[1] {
		t.Fatalf("expected fallback endpoint to succeed: %#v", probe)
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected 2 attempts, got %d", attempts.Load())
	}
}

func TestProbe115NetworkPathsRespectsConcurrencyLimit(t *testing.T) {
	paths := []NetworkPath{
		testNetworkPath(1, "10.0.0.1"),
		testNetworkPath(2, "10.0.0.2"),
		testNetworkPath(3, "10.0.0.3"),
		testNetworkPath(4, "10.0.0.4"),
	}
	options := ProbeOptions{URLs: []string{"https://example.invalid/"}, Timeout: time.Second, MaxConcurrency: 2}

	var active atomic.Int32
	var maxActive atomic.Int32
	results, err := probe115NetworkPaths(context.Background(), paths, options, func(NetworkPath) (http.RoundTripper, error) {
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
				Body:       io.NopCloser(strings.NewReader("")),
				Header:     make(http.Header),
				Request:    req,
			}, nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != len(paths) {
		t.Fatalf("expected %d results, got %d", len(paths), len(results))
	}
	if maxActive.Load() > 2 {
		t.Fatalf("expected at most 2 concurrent probes, saw %d", maxActive.Load())
	}
}

func TestSelectReachableNetworkPathsUsesOneBestPathPerInterface(t *testing.T) {
	probes := []NetworkPathProbe{
		{Path: testNetworkPath(2, "10.0.0.2"), Reachable: true, Latency: 30 * time.Millisecond},
		{Path: testNetworkPath(2, "2001:db8::2"), Reachable: true, Latency: 10 * time.Millisecond},
		{Path: testNetworkPath(3, "10.0.0.3"), Reachable: false, Latency: time.Millisecond},
		{Path: testNetworkPath(5, "2001:db8::5"), Reachable: true, Latency: 15 * time.Millisecond},
		{Path: testNetworkPath(5, "10.0.0.5"), Reachable: true, Latency: 15 * time.Millisecond},
	}

	paths := SelectReachableNetworkPaths(probes)
	if len(paths) != 2 {
		t.Fatalf("expected 2 selected interfaces, got %#v", paths)
	}
	if paths[0].InterfaceIndex != 2 || paths[0].LocalIP.String() != "2001:db8::2" {
		t.Fatalf("expected fastest address for interface 2, got %#v", paths[0])
	}
	if paths[1].InterfaceIndex != 5 || paths[1].LocalIP.String() != "10.0.0.5" {
		t.Fatalf("expected IPv4 tie-break for interface 5, got %#v", paths[1])
	}
}

func TestProbeNetworkPathReportsTransportFailure(t *testing.T) {
	path := testNetworkPath(7, "10.0.0.7")
	options := ProbeOptions{URLs: []string{"https://example.invalid/"}, Timeout: time.Second, MaxConcurrency: 1}
	probe := probe115NetworkPath(context.Background(), path, options, func(NetworkPath) (http.RoundTripper, error) {
		return nil, errors.New("transport setup failed")
	})
	if probe.Reachable || probe.Err == nil || !strings.Contains(probe.Err.Error(), "transport setup failed") {
		t.Fatalf("expected transport failure to be retained: %#v", probe)
	}
}

func TestDiscover115NetworkPathsLive(t *testing.T) {
	if os.Getenv("DRIVER115_LIVE_NETWORK_PROBE") != "1" {
		t.Skip("set DRIVER115_LIVE_NETWORK_PROBE=1 to probe real local interfaces")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := Discover115NetworkPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, probe := range result.Probes {
		t.Logf("path=%s reachable=%t status=%d latency=%s err=%v", probe.Path, probe.Reachable, probe.StatusCode, probe.Latency, probe.Err)
	}
	if result.EnumerationError != nil {
		t.Logf("interface enumeration warning: %v", result.EnumerationError)
	}
	if len(result.Paths) == 0 {
		t.Fatal("no local network path could reach the default 115 probe endpoint")
	}
}

func testNetworkPath(index int, rawIP string) NetworkPath {
	return NetworkPath{
		InterfaceName:  "iface",
		InterfaceIndex: index,
		LocalIP:        net.ParseIP(rawIP),
	}
}
