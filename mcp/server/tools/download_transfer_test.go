package tools

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDefaultDownloadTransferConfig(t *testing.T) {
	config := DefaultDownloadTransferConfig()
	if config.Interfaces != "auto" || config.Strategy != "file" {
		t.Fatalf("unexpected default selector/strategy: %#v", config)
	}
	if config.WorkersPerInterface != 1 {
		t.Fatalf("unexpected default workers per interface: %d", config.WorkersPerInterface)
	}
	if config.ProbeCacheTTL != transfer.DefaultCDNProbeCacheTTL {
		t.Fatalf("unexpected default probe cache TTL: %s", config.ProbeCacheTTL)
	}
	if config.Retries != transfer.DefaultFileScheduleRetries {
		t.Fatalf("unexpected default retries: %d", config.Retries)
	}
	if config.HealthCooldown != transfer.DefaultNetworkHealthCooldown || config.HealthCooldownMax != transfer.DefaultNetworkHealthCooldownMax {
		t.Fatalf("unexpected default health cooldowns: %#v", config)
	}
	if !config.Resume || config.URLRefreshes != transfer.DefaultDownloadURLRefreshes {
		t.Fatalf("unexpected default P9 resume/refresh config: %#v", config)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}
}

func TestDownloadTransferConfigRejectsUnsupportedSettings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*DownloadTransferConfig)
	}{
		{name: "empty interfaces", mutate: func(c *DownloadTransferConfig) { c.Interfaces = "" }},
		{name: "unknown strategy", mutate: func(c *DownloadTransferConfig) { c.Strategy = "future" }},
		{name: "zero workers", mutate: func(c *DownloadTransferConfig) { c.WorkersPerInterface = 0 }},
		{name: "zero cache ttl", mutate: func(c *DownloadTransferConfig) { c.ProbeCacheTTL = 0 }},
		{name: "negative retries", mutate: func(c *DownloadTransferConfig) { c.Retries = -1 }},
		{name: "negative URL refreshes", mutate: func(c *DownloadTransferConfig) { c.URLRefreshes = -1 }},
		{name: "empty chunk size", mutate: func(c *DownloadTransferConfig) { c.ChunkSize = "" }},
		{name: "invalid chunk size", mutate: func(c *DownloadTransferConfig) { c.ChunkSize = "1.5MiB" }},
		{name: "zero health cooldown", mutate: func(c *DownloadTransferConfig) { c.HealthCooldown = 0 }},
		{name: "health max below base", mutate: func(c *DownloadTransferConfig) {
			c.HealthCooldown = 10 * time.Second
			c.HealthCooldownMax = 5 * time.Second
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := DefaultDownloadTransferConfig()
			tt.mutate(&config)
			if err := config.Validate(); err == nil {
				t.Fatal("expected invalid transfer config to fail")
			}
		})
	}
}

func TestDownloadThroughTransferWiresP1P2AndScheduler(t *testing.T) {
	worker1 := mcpTestNetworkPath("Ethernet 1", 1, "10.0.0.1")
	worker2 := mcpTestNetworkPath("Ethernet 2", 2, "10.0.1.1")
	worker1v6 := mcpTestNetworkPath("Ethernet 1", 1, "2001:db8::1")

	ft := NewFileTools(nil,
		WithDownloadTimeout(90*time.Second),
		WithDownloadMaxBytes(1024),
		WithDownloadTransferConfig(DownloadTransferConfig{
			Interfaces:          "auto",
			Strategy:            "file",
			WorkersPerInterface: 3,
			ProbeCacheTTL:       5 * time.Minute,
			Retries:             2,
			ChunkSize:           "32MiB",
			Resume:              true,
			URLRefreshes:        3,
		}),
	)

	var firstCache *transfer.CDNProbeCache
	var firstHealth *transfer.NetworkHealthTracker
	discoverCalls := 0
	probeCalls := 0
	scheduleCalls := 0
	ft.downloadTransfer.deps = mcpDownloadTransferDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			discoverCalls++
			return transfer.NetworkDiscoveryResult{
				Paths: []transfer.NetworkPath{worker1, worker2},
				Probes: []transfer.NetworkPathProbe{
					{Path: worker1, Reachable: true},
					{Path: worker1v6, Reachable: true},
					{Path: worker2, Reachable: true},
				},
			}, nil
		},
		listNetworkPaths: func() ([]transfer.NetworkPath, error) {
			t.Fatal("manual interface enumeration should not run in auto mode")
			return nil, nil
		},
		probeCDNPaths: func(_ context.Context, rawURL string, headers http.Header, paths []transfer.NetworkPath, cache *transfer.CDNProbeCache, _ ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			probeCalls++
			if rawURL != "https://cdn.example.invalid/file?token=secret" {
				t.Fatalf("unexpected CDN URL: %q", rawURL)
			}
			if got := headers.Values("User-Agent"); len(got) != 1 || got[0] != "" {
				t.Fatalf("empty User-Agent binding was not preserved: %#v", got)
			}
			if got := headers.Get("X-Download"); got != "required" {
				t.Fatalf("download header was not preserved: %q", got)
			}
			wantCandidates := []transfer.NetworkPath{worker1, worker1v6, worker2}
			if !reflect.DeepEqual(paths, wantCandidates) {
				t.Fatalf("unexpected P2 candidates: got %#v want %#v", paths, wantCandidates)
			}
			if cache == nil {
				t.Fatal("expected persistent CDN probe cache")
			}
			if firstCache == nil {
				firstCache = cache
			} else if firstCache != cache {
				t.Fatal("CDN probe cache was not reused across MCP downloads")
			}
			return transfer.CDNDiscoveryResult{
				Host:  "cdn.example.invalid",
				Paths: []transfer.NetworkPath{worker1v6, worker2},
			}, nil
		},
		scheduleFiles: func(_ context.Context, workers []transfer.NetworkPath, jobs []transfer.FileTransferJob, opts ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			scheduleCalls++
			if !reflect.DeepEqual(workers, []transfer.NetworkPath{worker1, worker2}) {
				t.Fatalf("unexpected scheduler workers: %#v", workers)
			}
			if len(jobs) != 1 {
				t.Fatalf("expected one MCP file job, got %d", len(jobs))
			}
			job := jobs[0]
			if job.ID != "pick-code" || job.DestinationPath != filepath.Join("root", "file.bin") {
				t.Fatalf("unexpected job identity/path: %#v", job)
			}
			if !reflect.DeepEqual(job.NetworkPaths, []transfer.NetworkPath{worker1v6, worker2}) {
				t.Fatalf("P2 paths were not passed to P4: %#v", job.NetworkPaths)
			}
			if job.ExpectedSize != 100 || job.MaxBytes != 1024 || job.Timeout != 90*time.Second {
				t.Fatalf("download limits were not preserved: %#v", job)
			}
			if job.ResumeKey != "pick-code" || job.MaxRefreshes != 3 {
				t.Fatalf("P9 file resume/refresh settings were not preserved: %#v", job)
			}
			options := transfer.DefaultFileSchedulerOptions()
			for _, opt := range opts {
				opt(&options)
			}
			if options.Retries != 2 || options.WorkersPerInterface != 3 {
				t.Fatalf("unexpected scheduler tuning: %#v", options)
			}
			if options.HealthTracker == nil {
				t.Fatal("expected P8 health tracker")
			}
			if firstHealth == nil {
				firstHealth = options.HealthTracker
			} else if firstHealth != options.HealthTracker {
				t.Fatal("MCP health tracker was not reused across downloads")
			}
			result := transfer.FileDownloadResult{NetworkPath: worker2, DestinationPath: job.DestinationPath, BytesWritten: 100, StatusCode: http.StatusOK}
			return transfer.FileScheduleReport{Results: []transfer.FileScheduleResult{{JobID: job.ID, Result: result}}}, nil
		},
	}

	info := &driver.DownloadInfo{
		FileName: "file.bin",
		FileSize: driver.StringInt64(100),
		PickCode: "pick-code",
		Url:      driver.FileDownloadUrl{Url: "https://cdn.example.invalid/file?token=secret"},
		Header: http.Header{
			"User-Agent": []string{""},
			"X-Download": []string{"required"},
		},
	}
	for i := 0; i < 2; i++ {
		result, err := ft.downloadThroughTransfer(context.Background(), info, filepath.Join("root", "file.bin"), "pick-code")
		if err != nil {
			t.Fatalf("downloadThroughTransfer failed: %v", err)
		}
		if result.NetworkPath.InterfaceIndex != worker2.InterfaceIndex || result.BytesWritten != 100 {
			t.Fatalf("unexpected result: %#v", result)
		}
	}
	if discoverCalls != 2 || probeCalls != 2 || scheduleCalls != 2 {
		t.Fatalf("unexpected pipeline call counts: discover=%d probe=%d schedule=%d", discoverCalls, probeCalls, scheduleCalls)
	}
}

func TestDownloadThroughTransferChunkUsesRangePathsAndChunkSettings(t *testing.T) {
	worker1 := mcpTestNetworkPath("Ethernet 1", 1, "10.0.0.1")
	worker2 := mcpTestNetworkPath("Ethernet 2", 2, "10.0.1.1")
	ft := NewFileTools(nil,
		WithDownloadTimeout(90*time.Second),
		WithDownloadMaxBytes(1024),
		WithDownloadTransferConfig(DownloadTransferConfig{
			Interfaces: "auto", Strategy: "chunk", WorkersPerInterface: 3,
			ProbeCacheTTL: 5 * time.Minute, Retries: 2, ChunkSize: "4MiB", Resume: true, URLRefreshes: 3,
		}),
	)
	var captured transfer.ChunkDownloadRequest
	ft.downloadTransfer.deps = mcpDownloadTransferDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			return transfer.NetworkDiscoveryResult{
				Paths:  []transfer.NetworkPath{worker1, worker2},
				Probes: []transfer.NetworkPathProbe{{Path: worker1, Reachable: true}, {Path: worker2, Reachable: true}},
			}, nil
		},
		probeCDNPaths: func(_ context.Context, _ string, headers http.Header, _ []transfer.NetworkPath, cache *transfer.CDNProbeCache, opts ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			if cache == nil {
				t.Fatal("expected persistent CDN cache")
			}
			if values := headers.Values("User-Agent"); len(values) != 1 || values[0] != "" {
				t.Fatalf("empty User-Agent not preserved: %#v", values)
			}
			probeOptions := transfer.DefaultCDNProbeOptions()
			for _, opt := range opts {
				opt(&probeOptions)
			}
			if !probeOptions.RequireRangeValidation {
				t.Fatal("chunk MCP download did not force live Range validation")
			}
			return transfer.CDNDiscoveryResult{
				Host: "cdn.example.invalid", Paths: []transfer.NetworkPath{worker1, worker2}, RangePaths: []transfer.NetworkPath{worker2},
			}, nil
		},
		scheduleFiles: func(context.Context, []transfer.NetworkPath, []transfer.FileTransferJob, ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			t.Fatal("file scheduler must not run for MCP chunk strategy")
			return transfer.FileScheduleReport{}, nil
		},
		downloadChunks: func(_ context.Context, request transfer.ChunkDownloadRequest) (transfer.ChunkDownloadResult, error) {
			captured = request
			if request.HealthTracker == nil {
				t.Fatal("MCP chunk download did not receive P8 health tracker")
			}
			return transfer.ChunkDownloadResult{DestinationPath: request.DestinationPath, BytesWritten: request.ExpectedSize, Duration: time.Second}, nil
		},
	}
	info := &driver.DownloadInfo{
		FileSize: 100, PickCode: "pick", Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/file?token=secret"},
		Header: http.Header{"User-Agent": []string{""}, "Cookie": []string{"session=abc"}},
	}
	result, err := ft.downloadThroughTransfer(context.Background(), info, filepath.Join("root", "file.bin"), "pick")
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesWritten != 100 || result.StatusCode != http.StatusPartialContent || result.NetworkPath.InterfaceIndex != 2 {
		t.Fatalf("unexpected converted chunk result: %#v", result)
	}
	if captured.ChunkSize != 4<<20 || captured.Retries != 2 || captured.RecoveryRetries != 2 || captured.WorkersPerInterface != 3 || captured.Timeout != 90*time.Second || captured.MaxBytes != 1024 {
		t.Fatalf("chunk settings were not preserved: %#v", captured)
	}
	if captured.ResumeKey != "pick" || captured.MaxRefreshes != 3 {
		t.Fatalf("P9 MCP chunk resume/refresh settings were not preserved: %#v", captured)
	}
	if len(captured.NetworkPaths) != 1 || captured.NetworkPaths[0].InterfaceIndex != 2 {
		t.Fatalf("MCP chunk download did not use RangePaths: %#v", captured.NetworkPaths)
	}
}

func TestDownloadThroughTransferZeroByteFileDoesNotTurnEmptyProbeIntoNoEligiblePaths(t *testing.T) {
	worker := mcpTestNetworkPath("Ethernet 1", 1, "10.0.0.1")
	ft := NewFileTools(nil)
	ft.downloadTransfer.deps = mcpDownloadTransferDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			return transfer.NetworkDiscoveryResult{Paths: []transfer.NetworkPath{worker}, Probes: []transfer.NetworkPathProbe{{Path: worker, Reachable: true}}}, nil
		},
		listNetworkPaths: func() ([]transfer.NetworkPath, error) { return nil, errors.New("unexpected manual listing") },
		probeCDNPaths: func(context.Context, string, http.Header, []transfer.NetworkPath, *transfer.CDNProbeCache, ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			return transfer.CDNDiscoveryResult{Host: "cdn.example.invalid", Paths: []transfer.NetworkPath{}}, nil
		},
		scheduleFiles: func(_ context.Context, workers []transfer.NetworkPath, jobs []transfer.FileTransferJob, _ ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			if len(workers) != 1 || len(jobs) != 1 {
				t.Fatalf("unexpected zero-byte schedule shape: workers=%#v jobs=%#v", workers, jobs)
			}
			if jobs[0].NetworkPaths != nil {
				t.Fatalf("zero-byte job kept non-nil empty path restriction: %#v", jobs[0].NetworkPaths)
			}
			return transfer.FileScheduleReport{Results: []transfer.FileScheduleResult{{JobID: jobs[0].ID, Result: transfer.FileDownloadResult{DestinationPath: jobs[0].DestinationPath}}}}, nil
		},
		downloadChunks: func(context.Context, transfer.ChunkDownloadRequest) (transfer.ChunkDownloadResult, error) {
			t.Fatal("chunk downloader should not run in file strategy")
			return transfer.ChunkDownloadResult{}, nil
		},
	}
	result, err := ft.downloadThroughTransfer(context.Background(), &driver.DownloadInfo{
		FileSize: 0,
		Url:      driver.FileDownloadUrl{Url: "https://cdn.example.invalid/empty"},
	}, filepath.Join("root", "empty.bin"), "empty-pick")
	if err != nil || result.BytesWritten != 0 {
		t.Fatalf("zero-byte single download = %#v, %v", result, err)
	}
}

func TestDownloadThroughTransferRejectsKnownOversizeBeforeNetworkProbe(t *testing.T) {
	ft := NewFileTools(nil, WithDownloadMaxBytes(10))
	discovered := false
	ft.downloadTransfer.deps.discoverNetworkPaths = func(context.Context) (transfer.NetworkDiscoveryResult, error) {
		discovered = true
		return transfer.NetworkDiscoveryResult{}, nil
	}
	_, err := ft.downloadThroughTransfer(context.Background(), &driver.DownloadInfo{
		FileSize: driver.StringInt64(11),
		Url:      driver.FileDownloadUrl{Url: "https://cdn.example.invalid/file?token=secret"},
	}, filepath.Join("root", "file.bin"), "pick")
	if !errors.Is(err, transfer.ErrDownloadExceedsLimit) {
		t.Fatalf("expected size-limit error, got %v", err)
	}
	if discovered {
		t.Fatal("network discovery ran before known size limit was enforced")
	}
}

func TestResolveMCPDownloadPathSelectionManualSelectors(t *testing.T) {
	paths := []transfer.NetworkPath{
		mcpTestNetworkPath("auto-uplink", 1, "10.0.0.1"),
		mcpTestNetworkPath("Ethernet 2", 2, "10.0.1.1"),
		mcpTestNetworkPath("Other", 3, "10.0.2.1"),
	}
	deps := mcpDownloadTransferDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			t.Fatal("auto discovery should not run for manual selection")
			return transfer.NetworkDiscoveryResult{}, nil
		},
		listNetworkPaths: func() ([]transfer.NetworkPath, error) { return paths, nil },
	}
	selection, err := resolveMCPDownloadPathSelection(context.Background(), "auto-uplink,3", deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.workers) != 2 || selection.workers[0].InterfaceIndex != 1 || selection.workers[1].InterfaceIndex != 3 {
		t.Fatalf("unexpected manual workers: %#v", selection.workers)
	}
	if _, err := resolveMCPDownloadPathSelection(context.Background(), "auto,Ethernet 2", deps); err == nil {
		t.Fatal("expected auto/manual combination to fail")
	}
}

func TestDownloadTargetsRejectExistingDirectoryBeforeUsingClient(t *testing.T) {
	root := t.TempDir()
	ft := NewFileTools(nil, WithLocalRoot(root))
	result, _, err := ft.downloadFile(context.Background(), nil, DownloadSingleFileArgs{PickCode: "pick", LocalPath: root})
	if err != nil {
		t.Fatalf("download_file handler returned Go error: %v", err)
	}
	if result == nil || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("download_file directory target did not fail closed: %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "regular file") || strings.Contains(text.Text, "client is unavailable") {
		t.Fatalf("download_file directory target reached client or lost diagnostic: %#v", result.Content[0])
	}

	result, _, err = ft.downloadShareFile(context.Background(), nil, DownloadShareFileArgs{ShareCode: "share", FileID: "file", LocalPath: root})
	if err != nil {
		t.Fatalf("download_share_file handler returned Go error: %v", err)
	}
	if result == nil || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("download_share_file directory target did not fail closed: %#v", result)
	}
	text, ok = result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "regular file") || strings.Contains(text.Text, "client is unavailable") {
		t.Fatalf("download_share_file directory target reached client or lost diagnostic: %#v", result.Content[0])
	}
}

func TestDownloadFileRejectsOutsideLocalRootBeforeUsingClient(t *testing.T) {
	root := t.TempDir()
	ft := NewFileTools(nil, WithLocalRoot(root))
	result, _, err := ft.downloadFile(context.Background(), nil, DownloadSingleFileArgs{
		PickCode:  "pick",
		LocalPath: filepath.Join(filepath.Dir(root), "outside.bin"),
	})
	if err != nil {
		t.Fatalf("MCP handler should return tool error, got Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("expected local-root rejection, got %#v", result)
	}
}

func TestDownloadShareFileRejectsOutsideLocalRootBeforeUsingClient(t *testing.T) {
	root := t.TempDir()
	ft := NewFileTools(nil, WithLocalRoot(root))
	result, _, err := ft.downloadShareFile(context.Background(), nil, DownloadShareFileArgs{
		ShareCode: "share-code", ReceiveCode: "receive-code", FileID: "f1",
		LocalPath: filepath.Join(filepath.Dir(root), "outside.bin"),
	})
	if err != nil {
		t.Fatalf("MCP handler should return tool error, got Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("expected local-root rejection, got %#v", result)
	}
}

func TestDownloadShareFileRedactsReceiveCodeFromAPIErrors(t *testing.T) {
	const receiveCode = "top-secret-receive-code"
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("upstream rejected receive code " + receiveCode)
	})})))
	root := t.TempDir()
	ft := NewFileTools(client, WithLocalRoot(root))
	result, _, err := ft.downloadShareFile(context.Background(), nil, DownloadShareFileArgs{
		ShareCode: "share-code", ReceiveCode: receiveCode, FileID: "f1", LocalPath: filepath.Join(root, "file.bin"),
	})
	if err != nil {
		t.Fatalf("MCP handler should return tool error, got Go error: %v", err)
	}
	if result == nil || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("expected one MCP tool error, got %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected MCP error content: %#v", result.Content[0])
	}
	if strings.Contains(text.Text, receiveCode) {
		t.Fatalf("share receive code leaked in MCP error: %s", text.Text)
	}
	if !strings.Contains(text.Text, "[REDACTED]") {
		t.Fatalf("redacted marker missing from MCP error: %s", text.Text)
	}
}

func TestDownloadShareThroughTransferUsesShareRefreshAndHashedIdentity(t *testing.T) {
	const (
		shareCode   = "secret-share-code"
		receiveCode = "secret-receive-code"
		fileID      = "file-id-with-hyphens"
	)
	worker := mcpTestNetworkPath("Ethernet 1", 1, "10.0.0.1")
	apiCalls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		apiCalls++
		if got := req.URL.Query().Get("share_code"); got != shareCode {
			t.Fatalf("share refresh code = %q, want %q", got, shareCode)
		}
		if got := req.URL.Query().Get("receive_code"); got != receiveCode {
			t.Fatalf("share refresh receive code = %q, want %q", got, receiveCode)
		}
		if got := req.URL.Query().Get("file_id"); got != fileID {
			t.Fatalf("share refresh file id = %q, want %q", got, fileID)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header: http.Header{
				"Content-Type": {"application/json"},
				"Set-Cookie":   {"refresh-cookie=secret; Path=/"},
			},
			Body:    io.NopCloser(strings.NewReader(`{"state":true,"data":{"fid":"file-id-with-hyphens","fn":"file.bin","fs":"100","url":{"url":"https://cdn.example.invalid/fresh?token=two"}}}`)),
			Request: req,
		}, nil
	})})))
	config := DefaultDownloadTransferConfig()
	config.URLRefreshes = 1
	ft := NewFileTools(client, WithDownloadTransferConfig(config))
	probeCalls := 0
	ft.downloadTransfer.deps = mcpDownloadTransferDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			return transfer.NetworkDiscoveryResult{Paths: []transfer.NetworkPath{worker}, Probes: []transfer.NetworkPathProbe{{Path: worker, Reachable: true}}}, nil
		},
		listNetworkPaths: func() ([]transfer.NetworkPath, error) { return nil, errors.New("unexpected manual listing") },
		probeCDNPaths: func(_ context.Context, rawURL string, headers http.Header, paths []transfer.NetworkPath, _ *transfer.CDNProbeCache, _ ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			probeCalls++
			if len(paths) != 1 || paths[0].InterfaceIndex != worker.InterfaceIndex {
				t.Fatalf("unexpected share CDN paths: %#v", paths)
			}
			if probeCalls == 1 && rawURL != "https://cdn.example.invalid/old?token=one" {
				t.Fatalf("unexpected initial share URL: %q", rawURL)
			}
			if probeCalls == 2 {
				if rawURL != "https://cdn.example.invalid/fresh?token=two" {
					t.Fatalf("unexpected refreshed share URL: %q", rawURL)
				}
				if !strings.Contains(headers.Get("Referer"), receiveCode) || !strings.Contains(headers.Get("Cookie"), "refresh-cookie=secret") {
					t.Fatalf("refreshed share headers were not preserved internally: %#v", headers)
				}
			}
			return transfer.CDNDiscoveryResult{Host: "cdn.example.invalid", Paths: []transfer.NetworkPath{worker}}, nil
		},
		scheduleFiles: func(ctx context.Context, _ []transfer.NetworkPath, jobs []transfer.FileTransferJob, _ ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			if len(jobs) != 1 {
				t.Fatalf("expected one share job, got %d", len(jobs))
			}
			job := jobs[0]
			for _, secret := range []string{shareCode, receiveCode, fileID} {
				if strings.Contains(job.ID, secret) || strings.Contains(job.ResumeKey, secret) {
					t.Fatalf("share secret/identity leaked into scheduler identity: job=%q resume=%q", job.ID, job.ResumeKey)
				}
			}
			if job.ID == "" || job.ResumeKey == "" || job.ID != job.ResumeKey {
				t.Fatalf("unexpected hashed share identity: job=%q resume=%q", job.ID, job.ResumeKey)
			}
			if job.Refresh == nil {
				t.Fatal("share URL refresh callback was not configured")
			}
			fresh, err := job.Refresh(ctx)
			if err != nil {
				t.Fatalf("share refresh failed: %v", err)
			}
			if fresh.URL != "https://cdn.example.invalid/fresh?token=two" {
				t.Fatalf("unexpected refreshed source: %#v", fresh)
			}
			return transfer.FileScheduleReport{Results: []transfer.FileScheduleResult{{JobID: job.ID, Result: transfer.FileDownloadResult{DestinationPath: job.DestinationPath, BytesWritten: 100, StatusCode: http.StatusOK}}}}, nil
		},
		downloadChunks: func(context.Context, transfer.ChunkDownloadRequest) (transfer.ChunkDownloadResult, error) {
			t.Fatal("chunk downloader should not run in file strategy")
			return transfer.ChunkDownloadResult{}, nil
		},
	}
	initial := &driver.SharedDownloadRequest{
		SharedDownloadInfo: driver.SharedDownloadInfo{FileID: fileID, FileName: "file.bin", FileSize: 100},
		Header:             http.Header{"Referer": {"https://115cdn.com/s/redacted"}},
	}
	initial.URL.URL = "https://cdn.example.invalid/old?token=one"
	result, err := ft.downloadShareThroughTransfer(context.Background(), initial, filepath.Join("root", "file.bin"), shareCode, receiveCode, fileID, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesWritten != 100 || apiCalls != 1 || probeCalls != 2 {
		t.Fatalf("unexpected share transfer result/calls: result=%#v api=%d probe=%d", result, apiCalls, probeCalls)
	}
}

type mcpTestRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn mcpTestRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func mcpTestNetworkPath(name string, index int, ip string) transfer.NetworkPath {
	return transfer.NetworkPath{InterfaceName: name, InterfaceIndex: index, LocalIP: net.ParseIP(ip)}
}
