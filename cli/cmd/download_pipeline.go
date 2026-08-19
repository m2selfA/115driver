package cmd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/resolver"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

var (
	errDownloadUsage          = errors.New("invalid download arguments")
	errDownloadRemoteNotFound = errors.New("remote path not found")
)

type downloadCommandClient interface {
	DirName2CID(dir string) (*driver.APIGetDirIDResp, error)
	List(dirID string, opts ...driver.ListOption) (*[]driver.File, error)
	ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error)
	GetFile(fileID string) (*driver.File, error)
	Download(pickCode string) (*driver.DownloadInfo, error)
}

type downloadPipelineDeps struct {
	discoverNetworkPaths func(context.Context) (transfer.NetworkDiscoveryResult, error)
	listNetworkPaths     func() ([]transfer.NetworkPath, error)
	probeCDNPaths        func(context.Context, string, http.Header, []transfer.NetworkPath, *transfer.CDNProbeCache, ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error)
	scheduleFiles        func(context.Context, []transfer.NetworkPath, []transfer.FileTransferJob, ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error)
	downloadChunks       func(context.Context, transfer.ChunkDownloadRequest) (transfer.ChunkDownloadResult, error)
}

func defaultDownloadPipelineDeps() downloadPipelineDeps {
	return downloadPipelineDeps{
		discoverNetworkPaths: func(ctx context.Context) (transfer.NetworkDiscoveryResult, error) {
			return transfer.Discover115NetworkPaths(ctx)
		},
		listNetworkPaths: transfer.ListNetworkPaths,
		probeCDNPaths: func(ctx context.Context, rawURL string, headers http.Header, paths []transfer.NetworkPath, cache *transfer.CDNProbeCache, opts ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			return transfer.ProbeCDNNetworkPaths(ctx, rawURL, headers, paths, cache, opts...)
		},
		scheduleFiles:  transfer.ScheduleFileDownloads,
		downloadChunks: transfer.DownloadFileByChunks,
	}
}

type downloadCommandOptions struct {
	Recursive           bool
	Timeout             time.Duration
	Interfaces          string
	Strategy            string
	WorkersPerInterface int
	ProbeCacheTTL       time.Duration
	Retries             int
	ChunkSize           int64
	HealthCooldown      time.Duration
	HealthCooldownMax   time.Duration
	Resume              bool
	URLRefreshes        int
	Progress            func(string)
}

type downloadCommandFailure struct {
	RemotePath string
	Err        error
}

type downloadCommandSummary struct {
	RemotePath       string
	LocalPath        string
	Strategy         string
	Interfaces       []string
	FileCount        int
	SucceededCount   int
	FailedCount      int
	TotalBytes       int64
	TransferredBytes int64
	Failures         []downloadCommandFailure
	Report           transfer.FileScheduleReport
	ChunkResults     []transfer.ChunkDownloadResult
}

type remoteDownloadFile struct {
	File         driver.File
	RelativePath string
	RemotePath   string
}

type remoteDownloadTree struct {
	Files       []remoteDownloadFile
	Directories []string
}

type downloadPathSelection struct {
	Workers    []transfer.NetworkPath
	Candidates []transfer.NetworkPath
	Warning    error
}

type preparedDownloadJob struct {
	Job        transfer.FileTransferJob
	RemotePath string
}

func executeDownloadCommand(
	ctx context.Context,
	client downloadCommandClient,
	remotePath string,
	localTarget string,
	options downloadCommandOptions,
	deps downloadPipelineDeps,
) (downloadCommandSummary, error) {
	summary := downloadCommandSummary{RemotePath: remotePath, LocalPath: localTarget, Strategy: options.Strategy}
	if err := validateDownloadCommandOptions(options); err != nil {
		return summary, fmt.Errorf("%w: %v", errDownloadUsage, err)
	}
	if client == nil {
		return summary, errors.New("download client is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	remoteID, isDirectory, err := resolver.ResolvePath(client, remotePath)
	if err != nil {
		return summary, fmt.Errorf("%w: %v", errDownloadRemoteNotFound, err)
	}
	tree, err := collectRemoteDownloadTree(client, remoteID, remotePath, isDirectory, options.Recursive)
	if err != nil {
		return summary, err
	}
	if isDirectory {
		localTarget, err = prepareRecursiveDownloadDirectories(localTarget, tree.Directories)
		if err != nil {
			return summary, err
		}
		summary.LocalPath = localTarget
	}
	summary.FileCount = len(tree.Files)
	if len(tree.Files) == 0 {
		summary.SucceededCount = 0
		return summary, nil
	}
	progress(options, fmt.Sprintf("Preparing %d file(s) for %s transfer...", len(tree.Files), options.Strategy))

	selection, err := resolveDownloadPathSelection(ctx, options.Interfaces, deps)
	if err != nil {
		return summary, err
	}
	if selection.Warning != nil {
		progress(options, fmt.Sprintf("Network discovery warning: %v", selection.Warning))
	}
	summary.Interfaces = make([]string, len(selection.Workers))
	for i, networkPath := range selection.Workers {
		summary.Interfaces[i] = networkPath.String()
	}
	progress(options, fmt.Sprintf("Using %d network interface(s): %s", len(selection.Workers), strings.Join(summary.Interfaces, ", ")))

	cache, err := transfer.NewCDNProbeCache(options.ProbeCacheTTL)
	if err != nil {
		return summary, err
	}
	health, err := transfer.NewNetworkHealthTracker(transfer.NetworkHealthOptions{
		Cooldown:    options.HealthCooldown,
		CooldownMax: options.HealthCooldownMax,
	})
	if err != nil {
		return summary, err
	}
	prepared, totalBytes, err := prepareDownloadJobs(ctx, client, tree.Files, isDirectory, localTarget, options.Strategy, options.Timeout, options.Resume, options.URLRefreshes, selection.Candidates, cache, deps)
	if err != nil {
		return summary, err
	}
	summary.TotalBytes = totalBytes

	jobs := make([]transfer.FileTransferJob, len(prepared))
	remoteByJobID := make(map[string]string, len(prepared))
	for i, item := range prepared {
		jobs[i] = item.Job
		remoteByJobID[item.Job.ID] = item.RemotePath
	}
	progress(options, fmt.Sprintf("Downloading %d file(s)...", len(jobs)))
	if strings.EqualFold(options.Strategy, "chunk") {
		for _, item := range prepared {
			job := item.Job
			chunkResult, chunkErr := deps.downloadChunks(ctx, transfer.ChunkDownloadRequest{
				URL:             job.URL,
				Header:          job.Header,
				DestinationPath: job.DestinationPath,
				NetworkPaths:    job.NetworkPaths,
				ExpectedSize:    job.ExpectedSize,
				ChunkSize:       options.ChunkSize,
				MaxBytes:        job.MaxBytes,
				Timeout:         job.Timeout,
				Retries:         options.Retries,
				HealthTracker:   health,
				ResumeKey:       job.ResumeKey,
				Refresh:         job.Refresh,
				MaxRefreshes:    job.MaxRefreshes,
			})
			summary.ChunkResults = append(summary.ChunkResults, chunkResult)
			if chunkErr != nil {
				summary.Failures = append(summary.Failures, downloadCommandFailure{RemotePath: item.RemotePath, Err: chunkErr})
				continue
			}
			summary.SucceededCount++
			summary.TransferredBytes += chunkResult.BytesWritten
		}
		summary.FailedCount = len(prepared) - summary.SucceededCount
		if len(prepared) == 1 && summary.SucceededCount == 1 {
			summary.LocalPath = prepared[0].Job.DestinationPath
		}
		if summary.FailedCount > 0 {
			return summary, fmt.Errorf("%d of %d chunk downloads failed", summary.FailedCount, len(prepared))
		}
		return summary, nil
	}

	report, scheduleErr := deps.scheduleFiles(ctx, selection.Workers, jobs,
		transfer.WithFileScheduleRetries(options.Retries),
		transfer.WithFileScheduleHealthTracker(health),
	)
	summary.Report = report
	summary.SucceededCount = report.SucceededCount()
	summary.FailedCount = report.FailedCount()
	for _, result := range report.Results {
		if result.Err != nil {
			summary.Failures = append(summary.Failures, downloadCommandFailure{RemotePath: remoteByJobID[result.JobID], Err: result.Err})
			continue
		}
		summary.TransferredBytes += result.Result.BytesWritten
	}
	if len(prepared) == 1 && summary.SucceededCount == 1 {
		summary.LocalPath = prepared[0].Job.DestinationPath
	}
	if scheduleErr != nil {
		return summary, scheduleErr
	}
	return summary, nil
}

func validateDownloadCommandOptions(options downloadCommandOptions) error {
	if options.Timeout < 0 {
		return errors.New("timeout must be >= 0")
	}
	if strings.TrimSpace(options.Interfaces) == "" {
		return errors.New("transfer interfaces must not be empty")
	}
	strategy := strings.ToLower(strings.TrimSpace(options.Strategy))
	if strategy != "file" && strategy != "chunk" {
		return fmt.Errorf("unsupported transfer strategy %q; use \"file\" or \"chunk\"", options.Strategy)
	}
	if options.WorkersPerInterface != 1 {
		return fmt.Errorf("transfer currently requires workers_per_interface = 1, got %d", options.WorkersPerInterface)
	}
	if options.ChunkSize <= 0 {
		return errors.New("transfer chunk size must be > 0")
	}
	if options.ProbeCacheTTL <= 0 {
		return errors.New("transfer probe cache TTL must be > 0")
	}
	if options.Retries < 0 {
		return errors.New("transfer retries must be >= 0")
	}
	if options.HealthCooldown <= 0 {
		return errors.New("transfer health cooldown must be > 0")
	}
	if options.HealthCooldownMax < options.HealthCooldown {
		return errors.New("transfer health maximum cooldown must be >= cooldown")
	}
	if options.URLRefreshes < 0 {
		return errors.New("transfer URL refreshes must be >= 0")
	}
	return nil
}

func collectRemoteDownloadTree(client downloadCommandClient, remoteID, remotePath string, isDirectory, recursive bool) (remoteDownloadTree, error) {
	if !isDirectory {
		file, err := client.GetFile(remoteID)
		if err != nil {
			return remoteDownloadTree{}, fmt.Errorf("get remote file: %w", err)
		}
		if file.IsDirectory {
			return remoteDownloadTree{}, errors.New("resolved file unexpectedly became a directory")
		}
		return remoteDownloadTree{Files: []remoteDownloadFile{{File: *file, RemotePath: remotePath}}}, nil
	}
	if !recursive {
		return remoteDownloadTree{}, fmt.Errorf("%w: remote path is a directory; use --recursive to download it", errDownloadUsage)
	}

	tree := remoteDownloadTree{Directories: []string{""}}
	type pendingDirectory struct {
		ID       string
		Relative string
		Remote   string
	}
	queue := []pendingDirectory{{ID: remoteID, Remote: remotePath}}
	seenDirectories := map[string]struct{}{remoteID: {}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		entries, err := client.List(current.ID)
		if err != nil {
			return remoteDownloadTree{}, fmt.Errorf("list remote directory %q: %w", current.Remote, err)
		}
		for _, entry := range *entries {
			if err := validateRemoteDownloadName(entry.Name); err != nil {
				return remoteDownloadTree{}, fmt.Errorf("unsafe remote entry %q below %q: %w", entry.Name, current.Remote, err)
			}
			relative := filepath.Join(current.Relative, entry.Name)
			remoteChild := pathpkg.Join(current.Remote, entry.Name)
			if !strings.HasPrefix(remoteChild, "/") && strings.HasPrefix(remotePath, "/") {
				remoteChild = "/" + remoteChild
			}
			if entry.IsDirectory {
				if _, exists := seenDirectories[entry.FileID]; exists {
					return remoteDownloadTree{}, fmt.Errorf("remote directory ID %q was encountered more than once", entry.FileID)
				}
				seenDirectories[entry.FileID] = struct{}{}
				tree.Directories = append(tree.Directories, relative)
				queue = append(queue, pendingDirectory{ID: entry.FileID, Relative: relative, Remote: remoteChild})
				continue
			}
			tree.Files = append(tree.Files, remoteDownloadFile{File: entry, RelativePath: relative, RemotePath: remoteChild})
		}
	}
	return tree, nil
}

func validateRemoteDownloadName(name string) error {
	if name == "" || name == "." || name == ".." {
		return errors.New("empty or dot path component")
	}
	if strings.ContainsAny(name, `/\\`) {
		return errors.New("name contains a path separator")
	}
	if filepath.IsAbs(name) || filepath.VolumeName(name) != "" {
		return errors.New("name is absolute or contains a volume prefix")
	}
	return nil
}

func prepareRecursiveDownloadDirectories(localTarget string, directories []string) (string, error) {
	if localTarget == "" {
		localTarget = "."
	}
	if info, err := os.Stat(localTarget); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("recursive download target %q is not a directory", localTarget)
		}
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(localTarget, 0755); err != nil {
		return "", fmt.Errorf("create recursive download root: %w", err)
	}
	for _, relative := range directories {
		if relative == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Join(localTarget, relative), 0755); err != nil {
			return "", fmt.Errorf("create local directory %q: %w", relative, err)
		}
	}
	return localTarget, nil
}

func resolveDownloadPathSelection(ctx context.Context, selector string, deps downloadPipelineDeps) (downloadPathSelection, error) {
	selector = strings.TrimSpace(selector)
	if strings.EqualFold(selector, "auto") {
		discovery, err := deps.discoverNetworkPaths(ctx)
		if err != nil {
			return downloadPathSelection{}, fmt.Errorf("auto-detect network interfaces: %w", err)
		}
		if len(discovery.Paths) == 0 {
			return downloadPathSelection{}, errors.New("no network interface can reach the 115 control plane")
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
		return downloadPathSelection{Workers: discovery.Paths, Candidates: candidates, Warning: discovery.EnumerationError}, nil
	}
	for _, rawToken := range strings.Split(selector, ",") {
		if strings.EqualFold(strings.TrimSpace(rawToken), "auto") {
			return downloadPathSelection{}, errors.New("transfer.interfaces cannot combine auto with manual selectors")
		}
	}

	paths, enumerationErr := deps.listNetworkPaths()
	if len(paths) == 0 {
		if enumerationErr != nil {
			return downloadPathSelection{}, fmt.Errorf("list network interfaces: %w", enumerationErr)
		}
		return downloadPathSelection{}, errors.New("no usable local network interfaces found")
	}
	selected, err := selectManualNetworkPaths(paths, selector)
	if err != nil {
		return downloadPathSelection{}, err
	}
	probes := make([]transfer.NetworkPathProbe, len(selected))
	for i, networkPath := range selected {
		probes[i] = transfer.NetworkPathProbe{Path: networkPath, Reachable: true}
	}
	workers := transfer.SelectReachableNetworkPaths(probes)
	if len(workers) == 0 {
		return downloadPathSelection{}, errors.New("manual interface selection produced no usable workers")
	}
	return downloadPathSelection{Workers: workers, Candidates: selected, Warning: enumerationErr}, nil
}

func selectManualNetworkPaths(paths []transfer.NetworkPath, selector string) ([]transfer.NetworkPath, error) {
	tokens := strings.Split(selector, ",")
	selected := make([]transfer.NetworkPath, 0)
	seen := make(map[string]struct{})
	for _, rawToken := range tokens {
		token := strings.TrimSpace(rawToken)
		if token == "" {
			return nil, errors.New("transfer.interfaces contains an empty selector")
		}
		matched := false
		for _, networkPath := range paths {
			if !networkPathMatchesSelector(networkPath, token) {
				continue
			}
			matched = true
			key := fmt.Sprintf("%d|%s", networkPath.InterfaceIndex, networkPath.LocalIP.String())
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			selected = append(selected, networkPath)
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

func networkPathMatchesSelector(networkPath transfer.NetworkPath, selector string) bool {
	if strings.EqualFold(networkPath.InterfaceName, selector) {
		return true
	}
	if strconv.Itoa(networkPath.InterfaceIndex) == selector {
		return true
	}
	if ip := net.ParseIP(selector); ip != nil {
		return networkPath.LocalIP.Equal(ip)
	}
	return false
}

func prepareDownloadJobs(
	ctx context.Context,
	client downloadCommandClient,
	files []remoteDownloadFile,
	isDirectory bool,
	localTarget string,
	strategy string,
	timeout time.Duration,
	resume bool,
	urlRefreshes int,
	candidates []transfer.NetworkPath,
	cache *transfer.CDNProbeCache,
	deps downloadPipelineDeps,
) ([]preparedDownloadJob, int64, error) {
	prepared := make([]preparedDownloadJob, 0, len(files))
	var totalBytes int64
	for _, source := range files {
		if err := ctx.Err(); err != nil {
			return nil, totalBytes, err
		}
		info, err := client.Download(source.File.PickCode)
		if err != nil {
			return nil, totalBytes, fmt.Errorf("get download URL for %q: %w", source.RemotePath, err)
		}
		if info == nil || strings.TrimSpace(info.Url.Url) == "" {
			return nil, totalBytes, fmt.Errorf("download URL for %q is empty", source.RemotePath)
		}
		expectedSize := int64(info.FileSize)
		if strings.EqualFold(strategy, "chunk") && expectedSize < 0 {
			return nil, totalBytes, fmt.Errorf("prepare chunk download for %q: %w", source.RemotePath, transfer.ErrChunkRequiresKnownSize)
		}
		probeOptions := []transfer.CDNProbeOption(nil)
		if strings.EqualFold(strategy, "chunk") && expectedSize > 0 {
			probeOptions = append(probeOptions, transfer.WithCDNProbeRequireRangeValidation(true))
		}
		cdn, err := deps.probeCDNPaths(ctx, info.Url.Url, info.Header, candidates, cache, probeOptions...)
		if err != nil {
			return nil, totalBytes, fmt.Errorf("probe CDN for %q: %w", source.RemotePath, err)
		}
		selectedPaths := cdn.Paths
		if strings.EqualFold(strategy, "chunk") && expectedSize > 0 {
			selectedPaths = cdn.RangePaths
			if len(selectedPaths) == 0 {
				return nil, totalBytes, fmt.Errorf("CDN host %q does not support byte ranges on any selected interface for %q", cdn.Host, source.RemotePath)
			}
		}
		if len(selectedPaths) == 0 && expectedSize > 0 {
			return nil, totalBytes, fmt.Errorf("no selected network interface can reach CDN host %q for %q", cdn.Host, source.RemotePath)
		}

		destination := localTarget
		if isDirectory {
			destination = filepath.Join(localTarget, source.RelativePath)
		} else {
			destination = resolver.ResolveLocalDownloadPath(localTarget, info.FileName)
		}
		jobID := source.File.FileID
		if jobID == "" {
			jobID = source.File.PickCode
		}
		if jobID == "" {
			return nil, totalBytes, fmt.Errorf("remote file %q has no stable ID or pick code", source.RemotePath)
		}
		pickCode := source.File.PickCode
		refresh := transfer.DownloadSourceRefreshFunc(nil)
		if pickCode != "" && urlRefreshes > 0 {
			refresh = func(refreshCtx context.Context) (transfer.DownloadSource, error) {
				if refreshCtx != nil {
					if err := refreshCtx.Err(); err != nil {
						return transfer.DownloadSource{}, err
					}
				}
				fresh, err := client.Download(pickCode)
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
				if strings.EqualFold(strategy, "chunk") && expectedSize > 0 {
					refreshProbeOptions = append(refreshProbeOptions, transfer.WithCDNProbeRequireRangeValidation(true))
				}
				refreshedCDN, probeErr := deps.probeCDNPaths(refreshCtx, fresh.Url.Url, fresh.Header, selectedPaths, cache, refreshProbeOptions...)
				if probeErr != nil {
					return transfer.DownloadSource{}, fmt.Errorf("probe refreshed CDN: %w", probeErr)
				}
				usablePaths := refreshedCDN.Paths
				if strings.EqualFold(strategy, "chunk") && expectedSize > 0 {
					usablePaths = refreshedCDN.RangePaths
				}
				if expectedSize > 0 && len(usablePaths) == 0 {
					return transfer.DownloadSource{}, fmt.Errorf("refreshed CDN host %q is not usable through the current transfer paths", refreshedCDN.Host)
				}
				return transfer.DownloadSource{URL: fresh.Url.Url, Header: fresh.Header}, nil
			}
		}
		resumeKey := ""
		if resume {
			resumeKey = pickCode
			if resumeKey == "" {
				resumeKey = jobID
			}
		}
		prepared = append(prepared, preparedDownloadJob{
			Job: transfer.FileTransferJob{
				ID:              jobID,
				URL:             info.Url.Url,
				Header:          info.Header,
				DestinationPath: destination,
				NetworkPaths:    selectedPaths,
				ExpectedSize:    expectedSize,
				Timeout:         timeout,
				ResumeKey:       resumeKey,
				Refresh:         refresh,
				MaxRefreshes:    urlRefreshes,
			},
			RemotePath: source.RemotePath,
		})
		if expectedSize > 0 {
			totalBytes += expectedSize
		}
	}
	return prepared, totalBytes, nil
}

func progress(options downloadCommandOptions, message string) {
	if options.Progress != nil {
		options.Progress(message)
	}
}
