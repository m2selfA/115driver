package transfer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"time"
)

const (
	// Default115ProbeURL is a lightweight 115 control-plane endpoint used to
	// determine whether a bound network path can reach 115. Any HTTP response
	// counts as reachable; authentication is intentionally not required.
	Default115ProbeURL = "https://webapi.115.com/"

	default115ProbeTimeout     = 5 * time.Second
	default115ProbeConcurrency = 8
)

// ProbeOptions configures 115 control-plane reachability detection.
type ProbeOptions struct {
	URLs           []string
	Timeout        time.Duration
	MaxConcurrency int
}

// DefaultProbeOptions returns the zero-configuration defaults used by automatic
// interface discovery.
func DefaultProbeOptions() ProbeOptions {
	return ProbeOptions{
		URLs:           []string{Default115ProbeURL},
		Timeout:        default115ProbeTimeout,
		MaxConcurrency: default115ProbeConcurrency,
	}
}

// ProbeOption customizes 115 reachability probing.
type ProbeOption func(*ProbeOptions)

// WithProbeURLs replaces the control-plane endpoints used for probing. A path
// is considered reachable as soon as any configured endpoint returns an HTTP
// response, regardless of status code.
func WithProbeURLs(urls ...string) ProbeOption {
	return func(options *ProbeOptions) {
		options.URLs = append([]string(nil), urls...)
	}
}

// WithProbeTimeout sets the timeout for each endpoint attempt.
func WithProbeTimeout(timeout time.Duration) ProbeOption {
	return func(options *ProbeOptions) {
		options.Timeout = timeout
	}
}

// WithProbeConcurrency limits how many local network paths are probed at once.
func WithProbeConcurrency(maxConcurrency int) ProbeOption {
	return func(options *ProbeOptions) {
		options.MaxConcurrency = maxConcurrency
	}
}

// NetworkPathProbe records the reachability result for one local network path.
type NetworkPathProbe struct {
	Path       NetworkPath
	Reachable  bool
	URL        string
	StatusCode int
	Latency    time.Duration
	Err        error
}

// NetworkDiscoveryResult contains all probe diagnostics plus the selected
// usable path for each interface. EnumerationError is non-fatal when at least
// some interfaces could still be enumerated and probed.
type NetworkDiscoveryResult struct {
	Paths            []NetworkPath
	Probes           []NetworkPathProbe
	EnumerationError error
}

// Discover115NetworkPaths enumerates local candidates, probes them through a
// transport bound to each source path, and returns at most one reachable path
// per interface. When multiple addresses on the same interface are reachable,
// the lowest-latency path wins; IPv4 is preferred only as a deterministic
// tie-breaker.
func Discover115NetworkPaths(ctx context.Context, opts ...ProbeOption) (NetworkDiscoveryResult, error) {
	paths, enumerationErr := ListNetworkPaths()
	if len(paths) == 0 {
		if enumerationErr != nil {
			return NetworkDiscoveryResult{}, fmt.Errorf("enumerate network paths: %w", enumerationErr)
		}
		return NetworkDiscoveryResult{}, nil
	}

	probes, err := Probe115NetworkPaths(ctx, paths, opts...)
	if err != nil {
		return NetworkDiscoveryResult{}, err
	}
	return NetworkDiscoveryResult{
		Paths:            SelectReachableNetworkPaths(probes),
		Probes:           probes,
		EnumerationError: enumerationErr,
	}, nil
}

// Probe115NetworkPaths probes the supplied paths concurrently using a transport
// pinned to each path.
func Probe115NetworkPaths(ctx context.Context, paths []NetworkPath, opts ...ProbeOption) ([]NetworkPathProbe, error) {
	options := DefaultProbeOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	return probe115NetworkPaths(ctx, paths, options, func(path NetworkPath) (http.RoundTripper, error) {
		return NewTransport(path)
	})
}

func (options ProbeOptions) validate() error {
	if options.Timeout <= 0 {
		return errors.New("probe timeout must be > 0")
	}
	if options.MaxConcurrency <= 0 {
		return errors.New("probe concurrency must be > 0")
	}
	if len(options.URLs) == 0 {
		return errors.New("at least one probe URL is required")
	}
	for _, rawURL := range options.URLs {
		parsed, err := url.Parse(rawURL)
		if err != nil {
			return fmt.Errorf("parse probe URL %q: %w", rawURL, err)
		}
		if parsed.Scheme != "http" && parsed.Scheme != "https" {
			return fmt.Errorf("probe URL %q uses unsupported scheme %q", rawURL, parsed.Scheme)
		}
		if parsed.Host == "" {
			return fmt.Errorf("probe URL %q has no host", rawURL)
		}
	}
	return nil
}

func probe115NetworkPaths(ctx context.Context, paths []NetworkPath, options ProbeOptions, factory transportFactory) ([]NetworkPathProbe, error) {
	results := make([]NetworkPathProbe, len(paths))
	semaphore := make(chan struct{}, options.MaxConcurrency)
	done := make(chan int, len(paths))

	for i, path := range paths {
		i, path := i, path
		go func() {
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-ctx.Done():
				results[i] = NetworkPathProbe{Path: path, Err: ctx.Err()}
				done <- i
				return
			}

			results[i] = probe115NetworkPath(ctx, path, options, factory)
			done <- i
		}()
	}

	// Always wait for every worker. Each worker inherits ctx, so cancellation
	// makes in-flight HTTP requests and semaphore waiters return promptly while
	// keeping result writes synchronized before this function returns.
	for range paths {
		<-done
	}
	return results, nil
}

func probe115NetworkPath(ctx context.Context, path NetworkPath, options ProbeOptions, factory transportFactory) NetworkPathProbe {
	result := NetworkPathProbe{Path: path}
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
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	var endpointErrors []error
	for _, rawURL := range options.URLs {
		started := time.Now()
		request, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
		if err != nil {
			endpointErrors = append(endpointErrors, fmt.Errorf("create probe request for %s: %w", rawURL, err))
			continue
		}
		request.Header.Set("User-Agent", "115driver-network-probe")

		response, err := client.Do(request)
		latency := time.Since(started)
		if err != nil {
			endpointErrors = append(endpointErrors, fmt.Errorf("probe %s: %w", rawURL, err))
			if ctx.Err() != nil {
				break
			}
			continue
		}
		response.Body.Close()
		result.Reachable = true
		result.URL = rawURL
		result.StatusCode = response.StatusCode
		result.Latency = latency
		return result
	}

	result.Err = errors.Join(endpointErrors...)
	if result.Err == nil && ctx.Err() != nil {
		result.Err = ctx.Err()
	}
	return result
}

// SelectReachableNetworkPaths reduces probe results to one path per interface.
// This prevents IPv4 and IPv6 addresses on the same physical interface from
// being treated as separate bandwidth resources by higher-level schedulers.
func SelectReachableNetworkPaths(probes []NetworkPathProbe) []NetworkPath {
	best := make(map[int]NetworkPathProbe)
	for _, probe := range probes {
		if !probe.Reachable {
			continue
		}
		current, exists := best[probe.Path.InterfaceIndex]
		if !exists || probeIsBetter(probe, current) {
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

func probeIsBetter(candidate, current NetworkPathProbe) bool {
	if candidate.Latency != current.Latency {
		return candidate.Latency < current.Latency
	}
	candidateIPv4 := candidate.Path.LocalIP.To4() != nil
	currentIPv4 := current.Path.LocalIP.To4() != nil
	if candidateIPv4 != currentIPv4 {
		return candidateIPv4
	}
	return candidate.Path.LocalIP.String() < current.Path.LocalIP.String()
}
