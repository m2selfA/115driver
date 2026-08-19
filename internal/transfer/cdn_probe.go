package transfer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	defaultCDNProbeTimeout     = 5 * time.Second
	defaultCDNProbeConcurrency = 8
)

// CDNProbeOptions configures reachability probing for a concrete 115 download
// URL. The probe deliberately uses a one-byte Range GET so it exercises the
// same data-plane route as a real download without reading the file body.
type CDNProbeOptions struct {
	Timeout                time.Duration
	MaxConcurrency         int
	RequireRangeValidation bool
}

// DefaultCDNProbeOptions returns the zero-configuration defaults for probing a
// concrete download CDN URL.
func DefaultCDNProbeOptions() CDNProbeOptions {
	return CDNProbeOptions{
		Timeout:        defaultCDNProbeTimeout,
		MaxConcurrency: defaultCDNProbeConcurrency,
	}
}

// CDNProbeOption customizes concrete CDN probing.
type CDNProbeOption func(*CDNProbeOptions)

// WithCDNProbeTimeout sets the timeout for one bound-path CDN request.
func WithCDNProbeTimeout(timeout time.Duration) CDNProbeOption {
	return func(options *CDNProbeOptions) {
		options.Timeout = timeout
	}
}

// WithCDNProbeConcurrency limits how many local paths probe the CDN at once.
func WithCDNProbeConcurrency(maxConcurrency int) CDNProbeOption {
	return func(options *CDNProbeOptions) {
		options.MaxConcurrency = maxConcurrency
	}
}

// WithCDNProbeRequireRangeValidation forces a live one-byte Range request for
// every candidate path instead of accepting cached reachability. Use this for
// chunk mode, where Range support must be proven for the current signed object.
func WithCDNProbeRequireRangeValidation(required bool) CDNProbeOption {
	return func(options *CDNProbeOptions) {
		options.RequireRangeValidation = required
	}
}

// CDNPathProbe records the result of probing one concrete download URL through
// one local network path. Any HTTP response proves network reachability. Range
// support is recorded separately and is only authoritative when RangeChecked is
// true; cached host reachability never claims Range support for a different file.
type CDNPathProbe struct {
	Path           NetworkPath
	Reachable      bool
	RangeChecked   bool
	RangeSupported bool
	StatusCode     int
	ContentRange   string
	FinalHost      string
	Latency        time.Duration
	Cached         bool
	Err            error
}

// CDNDiscoveryResult contains per-path diagnostics and at most one reachable
// path for each physical interface.
type CDNDiscoveryResult struct {
	Host       string
	Paths      []NetworkPath
	RangePaths []NetworkPath
	Probes     []CDNPathProbe
}

// ProbeCDNNetworkPaths probes a concrete 115 download URL through the supplied
// candidate paths. headers should be the DownloadInfo headers returned by the
// 115 driver so UA- and cookie-bound links are tested exactly as they will be
// downloaded. cache may be nil to disable host/path reachability caching.
func ProbeCDNNetworkPaths(
	ctx context.Context,
	rawURL string,
	headers http.Header,
	paths []NetworkPath,
	cache *CDNProbeCache,
	opts ...CDNProbeOption,
) (CDNDiscoveryResult, error) {
	parsedURL, err := parseCDNProbeURL(rawURL)
	if err != nil {
		return CDNDiscoveryResult{}, err
	}

	options := DefaultCDNProbeOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if err := options.validate(); err != nil {
		return CDNDiscoveryResult{}, err
	}

	probes := probeCDNNetworkPaths(ctx, parsedURL, headers, paths, cache, options, func(path NetworkPath) (http.RoundTripper, error) {
		return NewTransport(path)
	})
	return CDNDiscoveryResult{
		Host:       parsedURL.Hostname(),
		Paths:      SelectReachableCDNNetworkPaths(probes),
		RangePaths: SelectRangeSupportedCDNNetworkPaths(probes),
		Probes:     probes,
	}, nil
}

func (options CDNProbeOptions) validate() error {
	if options.Timeout <= 0 {
		return errors.New("CDN probe timeout must be > 0")
	}
	if options.MaxConcurrency <= 0 {
		return errors.New("CDN probe concurrency must be > 0")
	}
	return nil
}

func parseCDNProbeURL(rawURL string) (*url.URL, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse CDN URL: %w", stripURLErrorURL(err))
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("CDN URL uses unsupported scheme %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return nil, errors.New("CDN URL has no host")
	}
	return parsed, nil
}

func probeCDNNetworkPaths(
	ctx context.Context,
	parsedURL *url.URL,
	headers http.Header,
	paths []NetworkPath,
	cache *CDNProbeCache,
	options CDNProbeOptions,
	factory transportFactory,
) []CDNPathProbe {
	results := make([]CDNPathProbe, len(paths))
	semaphore := make(chan struct{}, options.MaxConcurrency)
	done := make(chan int, len(paths))

	for i, path := range paths {
		i, path := i, path
		go func() {
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[i] = CDNPathProbe{Path: path, Err: ctx.Err()}
				done <- i
				return
			}

			if err := path.Validate(); err != nil {
				results[i] = CDNPathProbe{Path: path, Err: err}
				done <- i
				return
			}

			if cache != nil && !options.RequireRangeValidation {
				if cached, ok := cache.get(parsedURL, path); ok {
					results[i] = cached
					done <- i
					return
				}
			}

			probe := probeCDNNetworkPath(ctx, parsedURL, headers, path, options, factory)
			results[i] = probe
			if cache != nil && ctx.Err() == nil {
				cache.put(parsedURL, probe)
			}
			done <- i
		}()
	}

	for range paths {
		<-done
	}
	return results
}

func probeCDNNetworkPath(
	ctx context.Context,
	parsedURL *url.URL,
	headers http.Header,
	path NetworkPath,
	options CDNProbeOptions,
	factory transportFactory,
) CDNPathProbe {
	result := CDNPathProbe{Path: path}
	if err := path.Validate(); err != nil {
		result.Err = err
		return result
	}

	transport, err := factory(path)
	if err != nil {
		result.Err = err
		return result
	}
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   options.Timeout,
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		result.Err = fmt.Errorf("create CDN probe request for host %q: %w", parsedURL.Hostname(), err)
		return result
	}
	if headers != nil {
		request.Header = headers.Clone()
	}
	// Override a caller-provided Range value: P2 always uses the smallest useful
	// byte range so a server that ignores Range cannot make the probe consume a
	// meaningful portion of the file before Body.Close stops the request.
	request.Header.Set("Range", "bytes=0-0")
	request.Close = true

	started := time.Now()
	response, err := client.Do(request)
	result.Latency = time.Since(started)
	if err != nil {
		result.Err = fmt.Errorf("probe CDN host %q: %w", parsedURL.Hostname(), stripURLErrorURL(err))
		return result
	}
	response.Body.Close()

	result.Reachable = true
	result.RangeChecked = true
	result.StatusCode = response.StatusCode
	result.ContentRange = response.Header.Get("Content-Range")
	result.RangeSupported = response.StatusCode == http.StatusPartialContent && contentRangeStartsAtZero(result.ContentRange)
	if response.Request != nil && response.Request.URL != nil {
		result.FinalHost = response.Request.URL.Hostname()
	}
	if result.FinalHost == "" {
		result.FinalHost = parsedURL.Hostname()
	}
	return result
}

func stripURLErrorURL(err error) error {
	for {
		var urlErr *url.Error
		if !errors.As(err, &urlErr) || urlErr.Err == nil || urlErr.Err == err {
			return err
		}
		err = urlErr.Err
	}
}

func contentRangeStartsAtZero(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(value, "bytes 0-0/")
}

// SelectReachableCDNNetworkPaths reduces CDN probe results to one path per
// physical interface. The lowest-latency reachable address wins, with IPv4 as
// the deterministic tie-breaker inherited from the control-plane selector.
func SelectReachableCDNNetworkPaths(probes []CDNPathProbe) []NetworkPath {
	best := make(map[int]CDNPathProbe)
	for _, probe := range probes {
		if !probe.Reachable {
			continue
		}
		current, exists := best[probe.Path.InterfaceIndex]
		if !exists || probeIsBetter(
			NetworkPathProbe{Path: probe.Path, Reachable: true, Latency: probe.Latency},
			NetworkPathProbe{Path: current.Path, Reachable: true, Latency: current.Latency},
		) {
			best[probe.Path.InterfaceIndex] = probe
		}
	}

	indexes := make([]int, 0, len(best))
	for index := range best {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	paths := make([]NetworkPath, 0, len(indexes))
	for _, index := range indexes {
		paths = append(paths, best[index].Path)
	}
	return paths
}

// SelectRangeSupportedCDNNetworkPaths reduces live Range-capable probe results
// to one path per physical interface. Cached reachability entries are excluded
// because Range support is object-specific and is never stored in the cache.
func SelectRangeSupportedCDNNetworkPaths(probes []CDNPathProbe) []NetworkPath {
	best := make(map[int]CDNPathProbe)
	for _, probe := range probes {
		if !probe.Reachable || !probe.RangeChecked || !probe.RangeSupported {
			continue
		}
		current, exists := best[probe.Path.InterfaceIndex]
		if !exists || probeIsBetter(
			NetworkPathProbe{Path: probe.Path, Reachable: true, Latency: probe.Latency},
			NetworkPathProbe{Path: current.Path, Reachable: true, Latency: current.Latency},
		) {
			best[probe.Path.InterfaceIndex] = probe
		}
	}

	indexes := make([]int, 0, len(best))
	for index := range best {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	paths := make([]NetworkPath, 0, len(indexes))
	for _, index := range indexes {
		paths = append(paths, best[index].Path)
	}
	return paths
}
