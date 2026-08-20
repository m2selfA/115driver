package upload

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

type pathSelection struct {
	Paths   []transfer.NetworkPath
	Warning error
}

func resolveUploadPaths(ctx context.Context, selector, probeURL string) (pathSelection, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return pathSelection{}, errors.New("upload interface selector is empty")
	}
	if strings.EqualFold(selector, "auto") {
		discovery, err := transfer.Discover115NetworkPaths(ctx, transfer.WithProbeURLs(probeURL))
		if err != nil {
			return pathSelection{}, fmt.Errorf("auto-detect OSS network interfaces: %w", err)
		}
		if len(discovery.Paths) == 0 {
			return pathSelection{}, fmt.Errorf("%w: no network interface can reach the OSS upload endpoint", transfer.ErrNetworkPathFailure)
		}
		return pathSelection{Paths: discovery.Paths, Warning: discovery.EnumerationError}, nil
	}
	for _, rawToken := range strings.Split(selector, ",") {
		if strings.EqualFold(strings.TrimSpace(rawToken), "auto") {
			return pathSelection{}, errors.New("upload interfaces cannot combine auto with manual selectors")
		}
	}

	paths, enumerationErr := transfer.ListNetworkPaths()
	if len(paths) == 0 {
		if enumerationErr != nil {
			return pathSelection{}, fmt.Errorf("list upload network interfaces: %w", enumerationErr)
		}
		return pathSelection{}, errors.New("no usable local network interfaces found")
	}
	selected, err := selectManualPaths(paths, selector)
	if err != nil {
		return pathSelection{}, err
	}
	probes, err := transfer.Probe115NetworkPaths(ctx, selected, transfer.WithProbeURLs(probeURL))
	if err != nil {
		return pathSelection{}, fmt.Errorf("probe selected OSS upload interfaces: %w", err)
	}
	workers := transfer.SelectReachableNetworkPaths(probes)
	if len(workers) == 0 {
		return pathSelection{}, fmt.Errorf("%w: none of the selected interfaces can reach the OSS upload endpoint", transfer.ErrNetworkPathFailure)
	}
	return pathSelection{Paths: workers, Warning: enumerationErr}, nil
}

func applyUploadCompatibilitySelection(options Options, selection pathSelection) pathSelection {
	if !options.forceSequential || len(selection.Paths) <= 1 {
		return selection
	}
	// Sequential compatibility constrains part ordering, not the lifetime of a
	// transfer to one physical NIC. Retain every candidate so a failed ordered
	// part can retry on another path, while putting healthy paths first.
	selection.Paths = append([]transfer.NetworkPath(nil), selection.Paths...)
	if options.HealthTracker == nil {
		return selection
	}
	sort.SliceStable(selection.Paths, func(i, j int) bool {
		leftAvailable := options.HealthTracker.Available(selection.Paths[i])
		rightAvailable := options.HealthTracker.Available(selection.Paths[j])
		if leftAvailable != rightAvailable {
			return leftAvailable
		}
		leftScore := options.HealthTracker.Score(selection.Paths[i])
		rightScore := options.HealthTracker.Score(selection.Paths[j])
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return selection.Paths[i].InterfaceIndex < selection.Paths[j].InterfaceIndex
	})
	return selection
}

func selectManualPaths(paths []transfer.NetworkPath, selector string) ([]transfer.NetworkPath, error) {
	tokens := strings.Split(selector, ",")
	selected := make([]transfer.NetworkPath, 0)
	seen := make(map[string]struct{})
	for _, rawToken := range tokens {
		token := strings.TrimSpace(rawToken)
		if token == "" {
			return nil, errors.New("upload interfaces contains an empty selector")
		}
		matched := false
		for _, path := range paths {
			if !uploadPathMatchesSelector(path, token) {
				continue
			}
			matched = true
			key := fmt.Sprintf("%d|%s", path.InterfaceIndex, path.LocalIP.String())
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			selected = append(selected, path)
		}
		if !matched {
			return nil, fmt.Errorf("network interface selector %q did not match any usable local path", token)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].InterfaceIndex != selected[j].InterfaceIndex {
			return selected[i].InterfaceIndex < selected[j].InterfaceIndex
		}
		return selected[i].LocalIP.String() < selected[j].LocalIP.String()
	})
	return selected, nil
}

func uploadPathMatchesSelector(path transfer.NetworkPath, selector string) bool {
	if strings.EqualFold(path.InterfaceName, selector) {
		return true
	}
	if strconv.Itoa(path.InterfaceIndex) == selector {
		return true
	}
	if ip := net.ParseIP(selector); ip != nil {
		return path.LocalIP.Equal(ip)
	}
	return false
}

func buildOSSProbeURL(endpoint, bucket string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	bucket = strings.TrimSpace(bucket)
	if endpoint == "" || bucket == "" {
		return "", errors.New("OSS endpoint and bucket must not be empty")
	}
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse OSS endpoint: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported OSS endpoint scheme %q", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return "", errors.New("OSS endpoint has no host")
	}
	port := parsed.Port()
	host := bucket + "." + parsed.Hostname()
	if port != "" {
		host = net.JoinHostPort(host, port)
	}
	parsed.Host = host
	parsed.Path = "/"
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
