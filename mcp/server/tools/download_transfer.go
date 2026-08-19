package tools

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

const (
	defaultMCPTransferInterfaces          = "auto"
	defaultMCPTransferStrategy            = "file"
	defaultMCPTransferWorkersPerInterface = 1
	defaultMCPTransferChunkSize           = "32MiB"
)

// DownloadTransferConfig configures the MCP download_file data path. It mirrors
// the machine-wide [transfer] settings used by the CLI.
type DownloadTransferConfig struct {
	Interfaces          string
	Strategy            string
	WorkersPerInterface int
	ProbeCacheTTL       time.Duration
	Retries             int
	ChunkSize           string
	HealthCooldown      time.Duration
	HealthCooldownMax   time.Duration
	Resume              bool
	URLRefreshes        int
}

// DefaultDownloadTransferConfig returns the current file-strategy defaults.
func DefaultDownloadTransferConfig() DownloadTransferConfig {
	return DownloadTransferConfig{
		Interfaces:          defaultMCPTransferInterfaces,
		Strategy:            defaultMCPTransferStrategy,
		WorkersPerInterface: defaultMCPTransferWorkersPerInterface,
		ProbeCacheTTL:       transfer.DefaultCDNProbeCacheTTL,
		Retries:             transfer.DefaultFileScheduleRetries,
		ChunkSize:           defaultMCPTransferChunkSize,
		HealthCooldown:      transfer.DefaultNetworkHealthCooldown,
		HealthCooldownMax:   transfer.DefaultNetworkHealthCooldownMax,
		Resume:              true,
		URLRefreshes:        transfer.DefaultDownloadURLRefreshes,
	}
}

// Validate checks settings supported by the current MCP download path.
func (config DownloadTransferConfig) Validate() error {
	if strings.TrimSpace(config.Interfaces) == "" {
		return errors.New("transfer.interfaces must not be empty")
	}
	strategy := strings.ToLower(strings.TrimSpace(config.Strategy))
	if strategy != "file" && strategy != "chunk" {
		return fmt.Errorf("unsupported transfer strategy %q; use %q or %q", config.Strategy, "file", "chunk")
	}
	if config.WorkersPerInterface != 1 {
		return fmt.Errorf("transfer currently requires workers_per_interface = 1, got %d", config.WorkersPerInterface)
	}
	if config.ProbeCacheTTL <= 0 {
		return errors.New("transfer.probe_cache_ttl must be > 0")
	}
	if config.Retries < 0 {
		return errors.New("transfer.retries must be >= 0")
	}
	if _, err := transfer.ParseByteSize(config.ChunkSize); err != nil {
		return fmt.Errorf("invalid transfer.chunk_size %q: %w", config.ChunkSize, err)
	}
	if config.HealthCooldown <= 0 {
		return errors.New("transfer.health_cooldown must be > 0")
	}
	if config.HealthCooldownMax < config.HealthCooldown {
		return errors.New("transfer.health_cooldown_max must be >= transfer.health_cooldown")
	}
	if config.URLRefreshes < 0 {
		return errors.New("transfer.url_refreshes must be >= 0")
	}
	return nil
}

func normalizeDownloadTransferConfig(config DownloadTransferConfig) DownloadTransferConfig {
	config.Interfaces = strings.TrimSpace(config.Interfaces)
	config.Strategy = strings.ToLower(strings.TrimSpace(config.Strategy))
	config.ChunkSize = strings.TrimSpace(config.ChunkSize)
	if config.HealthCooldown == 0 {
		config.HealthCooldown = transfer.DefaultNetworkHealthCooldown
	}
	if config.HealthCooldownMax == 0 {
		config.HealthCooldownMax = transfer.DefaultNetworkHealthCooldownMax
	}
	return config
}

// WithDownloadTransferConfig applies the machine-wide transfer settings to
// trusted 115 data planes. For upload_from_url, the external fetch remains on
// the SSRF-restricted HTTP client; only the resulting local tempfile -> OSS
// stage uses these interface settings.
func WithDownloadTransferConfig(config DownloadTransferConfig) FileToolsOption {
	return func(ft *FileTools) {
		config = normalizeDownloadTransferConfig(config)
		if ft.downloadTransfer == nil {
			ft.downloadTransfer = newMCPDownloadTransferState()
		}
		ft.downloadTransfer.config = config
		ft.downloadTransfer.resetRuntimeState()
		if ft.uploadTransfer == nil {
			ft.uploadTransfer = newMCPUploadTransferState()
		}
		ft.uploadTransfer.config = config
		ft.uploadTransfer.resetRuntimeState()
	}
}

type mcpDownloadTransferDeps struct {
	discoverNetworkPaths func(context.Context) (transfer.NetworkDiscoveryResult, error)
	listNetworkPaths     func() ([]transfer.NetworkPath, error)
	probeCDNPaths        func(context.Context, string, http.Header, []transfer.NetworkPath, *transfer.CDNProbeCache, ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error)
	scheduleFiles        func(context.Context, []transfer.NetworkPath, []transfer.FileTransferJob, ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error)
	downloadChunks       func(context.Context, transfer.ChunkDownloadRequest) (transfer.ChunkDownloadResult, error)
}

type mcpDownloadTransferState struct {
	config DownloadTransferConfig
	deps   mcpDownloadTransferDeps

	cacheMu           sync.Mutex
	cacheTTL          time.Duration
	cache             *transfer.CDNProbeCache
	health            *transfer.NetworkHealthTracker
	healthCooldown    time.Duration
	healthCooldownMax time.Duration
}

func newMCPDownloadTransferState() *mcpDownloadTransferState {
	return &mcpDownloadTransferState{
		config: DefaultDownloadTransferConfig(),
		deps: mcpDownloadTransferDeps{
			discoverNetworkPaths: func(ctx context.Context) (transfer.NetworkDiscoveryResult, error) {
				return transfer.Discover115NetworkPaths(ctx)
			},
			listNetworkPaths: transfer.ListNetworkPaths,
			probeCDNPaths: func(ctx context.Context, rawURL string, headers http.Header, paths []transfer.NetworkPath, cache *transfer.CDNProbeCache, opts ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
				return transfer.ProbeCDNNetworkPaths(ctx, rawURL, headers, paths, cache, opts...)
			},
			scheduleFiles:  transfer.ScheduleFileDownloads,
			downloadChunks: transfer.DownloadFileByChunks,
		},
	}
}

func (state *mcpDownloadTransferState) resetRuntimeState() {
	state.cacheMu.Lock()
	state.cache = nil
	state.cacheTTL = 0
	state.health = nil
	state.healthCooldown = 0
	state.healthCooldownMax = 0
	state.cacheMu.Unlock()
}

func (state *mcpDownloadTransferState) probeCache() (*transfer.CDNProbeCache, error) {
	state.cacheMu.Lock()
	defer state.cacheMu.Unlock()
	if state.cache != nil && state.cacheTTL == state.config.ProbeCacheTTL {
		return state.cache, nil
	}
	cache, err := transfer.NewCDNProbeCache(state.config.ProbeCacheTTL)
	if err != nil {
		return nil, err
	}
	state.cache = cache
	state.cacheTTL = state.config.ProbeCacheTTL
	return cache, nil
}

func (state *mcpDownloadTransferState) healthTracker() (*transfer.NetworkHealthTracker, error) {
	state.cacheMu.Lock()
	defer state.cacheMu.Unlock()
	if state.health != nil && state.healthCooldown == state.config.HealthCooldown && state.healthCooldownMax == state.config.HealthCooldownMax {
		return state.health, nil
	}
	health, err := transfer.NewNetworkHealthTracker(transfer.NetworkHealthOptions{
		Cooldown:    state.config.HealthCooldown,
		CooldownMax: state.config.HealthCooldownMax,
	})
	if err != nil {
		return nil, err
	}
	state.health = health
	state.healthCooldown = state.config.HealthCooldown
	state.healthCooldownMax = state.config.HealthCooldownMax
	return health, nil
}

type mcpDownloadPathSelection struct {
	workers    []transfer.NetworkPath
	candidates []transfer.NetworkPath
}

func (ft *FileTools) downloadThroughTransfer(ctx context.Context, info *driver.DownloadInfo, localPath, pickCode string, userAgents ...string) (transfer.FileDownloadResult, error) {
	if ft.downloadTransfer == nil {
		ft.downloadTransfer = newMCPDownloadTransferState()
	}
	state := ft.downloadTransfer
	config := normalizeDownloadTransferConfig(state.config)
	if err := config.Validate(); err != nil {
		return transfer.FileDownloadResult{}, err
	}
	if info == nil || strings.TrimSpace(info.Url.Url) == "" {
		return transfer.FileDownloadResult{}, errors.New("download info has no CDN URL")
	}

	expectedSize := int64(info.FileSize)
	if expectedSize < 0 {
		expectedSize = transfer.UnknownFileSize
	}
	if ft.downloadMaxBytes > 0 && expectedSize >= 0 && expectedSize > ft.downloadMaxBytes {
		return transfer.FileDownloadResult{}, fmt.Errorf("%w: expected %d bytes, limit is %d bytes", transfer.ErrDownloadExceedsLimit, expectedSize, ft.downloadMaxBytes)
	}
	if config.Strategy == "chunk" && expectedSize < 0 {
		return transfer.FileDownloadResult{}, transfer.ErrChunkRequiresKnownSize
	}
	stablePickCode := strings.TrimSpace(pickCode)
	if stablePickCode == "" {
		stablePickCode = strings.TrimSpace(info.PickCode)
	}
	userAgent := ""
	if len(userAgents) > 0 {
		userAgent = userAgents[0]
	}
	var cache *transfer.CDNProbeCache
	var selectedPaths []transfer.NetworkPath
	refresh := transfer.DownloadSourceRefreshFunc(nil)
	if config.URLRefreshes > 0 && ft.client != nil && stablePickCode != "" {
		refresh = func(refreshCtx context.Context) (transfer.DownloadSource, error) {
			if refreshCtx != nil {
				if err := refreshCtx.Err(); err != nil {
					return transfer.DownloadSource{}, err
				}
			}
			fresh, err := ft.client.DownloadWithUA(stablePickCode, userAgent)
			if err != nil {
				return transfer.DownloadSource{}, err
			}
			if fresh == nil || strings.TrimSpace(fresh.Url.Url) == "" {
				return transfer.DownloadSource{}, errors.New("refreshed download URL is empty")
			}
			if expectedSize >= 0 && int64(fresh.FileSize) != expectedSize {
				return transfer.DownloadSource{}, fmt.Errorf("refreshed file size changed from %d to %d", expectedSize, int64(fresh.FileSize))
			}
			refreshProbeOptions := []transfer.CDNProbeOption(nil)
			if config.Strategy == "chunk" && expectedSize > 0 {
				refreshProbeOptions = append(refreshProbeOptions, transfer.WithCDNProbeRequireRangeValidation(true))
			}
			refreshedCDN, probeErr := state.deps.probeCDNPaths(refreshCtx, fresh.Url.Url, fresh.Header, selectedPaths, cache, refreshProbeOptions...)
			if probeErr != nil {
				return transfer.DownloadSource{}, fmt.Errorf("probe refreshed 115 CDN: %w", probeErr)
			}
			usablePaths := refreshedCDN.Paths
			if config.Strategy == "chunk" && expectedSize > 0 {
				usablePaths = refreshedCDN.RangePaths
			}
			if expectedSize > 0 && len(usablePaths) == 0 {
				return transfer.DownloadSource{}, fmt.Errorf("refreshed 115 CDN host %q is not usable through the current transfer paths", refreshedCDN.Host)
			}
			return transfer.DownloadSource{URL: fresh.Url.Url, Header: fresh.Header}, nil
		}
	}
	resumeKey := ""
	if config.Resume {
		resumeKey = stablePickCode
	}

	selection, err := resolveMCPDownloadPathSelection(ctx, config.Interfaces, state.deps)
	if err != nil {
		return transfer.FileDownloadResult{}, err
	}
	cache, err = state.probeCache()
	if err != nil {
		return transfer.FileDownloadResult{}, err
	}
	health, err := state.healthTracker()
	if err != nil {
		return transfer.FileDownloadResult{}, err
	}
	probeOptions := []transfer.CDNProbeOption(nil)
	if config.Strategy == "chunk" && expectedSize > 0 {
		probeOptions = append(probeOptions, transfer.WithCDNProbeRequireRangeValidation(true))
	}
	cdn, err := state.deps.probeCDNPaths(ctx, info.Url.Url, info.Header, selection.candidates, cache, probeOptions...)
	if err != nil {
		return transfer.FileDownloadResult{}, fmt.Errorf("probe 115 CDN: %w", err)
	}
	selectedPaths = cdn.Paths
	if config.Strategy == "chunk" && expectedSize > 0 {
		selectedPaths = cdn.RangePaths
		if len(selectedPaths) == 0 {
			return transfer.FileDownloadResult{}, fmt.Errorf("115 CDN host %q does not support byte ranges on any selected interface", cdn.Host)
		}
	}
	if len(selectedPaths) == 0 && expectedSize > 0 {
		return transfer.FileDownloadResult{}, fmt.Errorf("no selected network interface can reach 115 CDN host %q", cdn.Host)
	}
	if config.Strategy == "chunk" {
		chunkSize, err := transfer.ParseByteSize(config.ChunkSize)
		if err != nil {
			return transfer.FileDownloadResult{}, err
		}
		chunkResult, chunkErr := state.deps.downloadChunks(ctx, transfer.ChunkDownloadRequest{
			URL:             info.Url.Url,
			Header:          info.Header,
			DestinationPath: localPath,
			NetworkPaths:    selectedPaths,
			ExpectedSize:    expectedSize,
			ChunkSize:       chunkSize,
			MaxBytes:        ft.downloadMaxBytes,
			Timeout:         ft.downloadTimeout,
			Retries:         config.Retries,
			HealthTracker:   health,
			ResumeKey:       resumeKey,
			Refresh:         refresh,
			MaxRefreshes:    config.URLRefreshes,
		})
		fileResult := transfer.FileDownloadResult{
			DestinationPath: localPath,
			BytesWritten:    chunkResult.BytesWritten,
			Duration:        chunkResult.Duration,
			Refreshes:       chunkResult.Refreshes,
		}
		if expectedSize > 0 {
			fileResult.StatusCode = http.StatusPartialContent
		}
		if len(selectedPaths) == 1 {
			fileResult.NetworkPath = selectedPaths[0]
		}
		return fileResult, chunkErr
	}

	jobID := stablePickCode
	if jobID == "" {
		jobID = "mcp-download"
	}
	report, scheduleErr := state.deps.scheduleFiles(ctx, selection.workers, []transfer.FileTransferJob{{
		ID:              jobID,
		URL:             info.Url.Url,
		Header:          info.Header,
		DestinationPath: localPath,
		NetworkPaths:    selectedPaths,
		ExpectedSize:    expectedSize,
		MaxBytes:        ft.downloadMaxBytes,
		Timeout:         ft.downloadTimeout,
		ResumeKey:       resumeKey,
		Refresh:         refresh,
		MaxRefreshes:    config.URLRefreshes,
	}}, transfer.WithFileScheduleRetries(config.Retries), transfer.WithFileScheduleHealthTracker(health))
	if len(report.Results) != 1 {
		if scheduleErr != nil {
			return transfer.FileDownloadResult{}, scheduleErr
		}
		return transfer.FileDownloadResult{}, fmt.Errorf("download scheduler returned %d results, expected 1", len(report.Results))
	}
	result := report.Results[0]
	if result.Err != nil {
		if scheduleErr != nil {
			return result.Result, errors.Join(scheduleErr, result.Err)
		}
		return result.Result, result.Err
	}
	if scheduleErr != nil {
		return result.Result, scheduleErr
	}
	return result.Result, nil
}

func resolveMCPDownloadPathSelection(ctx context.Context, selector string, deps mcpDownloadTransferDeps) (mcpDownloadPathSelection, error) {
	selector = strings.TrimSpace(selector)
	if strings.EqualFold(selector, "auto") {
		discovery, err := deps.discoverNetworkPaths(ctx)
		if err != nil {
			return mcpDownloadPathSelection{}, fmt.Errorf("auto-detect network interfaces: %w", err)
		}
		if len(discovery.Paths) == 0 {
			return mcpDownloadPathSelection{}, errors.New("no network interface can reach the 115 control plane")
		}
		candidates := make([]transfer.NetworkPath, 0, len(discovery.Probes))
		for _, probe := range discovery.Probes {
			if probe.Reachable {
				candidates = append(candidates, probe.Path)
			}
		}
		if len(candidates) == 0 {
			candidates = append(candidates, discovery.Paths...)
		}
		return mcpDownloadPathSelection{workers: discovery.Paths, candidates: candidates}, nil
	}

	for _, token := range strings.Split(selector, ",") {
		if strings.EqualFold(strings.TrimSpace(token), "auto") {
			return mcpDownloadPathSelection{}, errors.New("transfer.interfaces cannot combine auto with manual selectors")
		}
	}
	paths, enumerationErr := deps.listNetworkPaths()
	if len(paths) == 0 {
		if enumerationErr != nil {
			return mcpDownloadPathSelection{}, fmt.Errorf("list network interfaces: %w", enumerationErr)
		}
		return mcpDownloadPathSelection{}, errors.New("no usable local network interfaces found")
	}
	selected, err := selectMCPManualNetworkPaths(paths, selector)
	if err != nil {
		return mcpDownloadPathSelection{}, err
	}
	probes := make([]transfer.NetworkPathProbe, len(selected))
	for i, path := range selected {
		probes[i] = transfer.NetworkPathProbe{Path: path, Reachable: true}
	}
	workers := transfer.SelectReachableNetworkPaths(probes)
	if len(workers) == 0 {
		return mcpDownloadPathSelection{}, errors.New("manual interface selection produced no usable workers")
	}
	return mcpDownloadPathSelection{workers: workers, candidates: selected}, nil
}

func selectMCPManualNetworkPaths(paths []transfer.NetworkPath, selector string) ([]transfer.NetworkPath, error) {
	tokens := strings.Split(selector, ",")
	selected := make([]transfer.NetworkPath, 0)
	seen := make(map[string]struct{})
	for _, rawToken := range tokens {
		token := strings.TrimSpace(rawToken)
		if token == "" {
			return nil, errors.New("transfer.interfaces contains an empty selector")
		}
		matched := false
		for _, path := range paths {
			if !mcpNetworkPathMatchesSelector(path, token) {
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

func mcpNetworkPathMatchesSelector(path transfer.NetworkPath, selector string) bool {
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
