package tools

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

func TestDownloadFileBatchUsesOneDiscoveryAndOneSchedulerRun(t *testing.T) {
	worker1 := mcpTestNetworkPath("Ethernet 1", 1, "10.0.0.1")
	worker2 := mcpTestNetworkPath("Ethernet 2", 2, "10.0.1.1")
	config := DefaultDownloadTransferConfig()
	config.Interfaces = "auto"
	config.Strategy = "file"
	config.WorkersPerInterface = 2
	config.Retries = 1
	ft := NewFileTools(nil, WithDownloadTimeout(90*time.Second), WithDownloadMaxBytes(1024), WithDownloadTransferConfig(config))

	discoverCalls := 0
	probeCalls := 0
	scheduleCalls := 0
	ft.downloadTransfer.deps = mcpDownloadTransferDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			discoverCalls++
			return transfer.NetworkDiscoveryResult{
				Paths:  []transfer.NetworkPath{worker1, worker2},
				Probes: []transfer.NetworkPathProbe{{Path: worker1, Reachable: true}, {Path: worker2, Reachable: true}},
			}, nil
		},
		listNetworkPaths: func() ([]transfer.NetworkPath, error) {
			t.Fatal("manual network listing should not run in auto mode")
			return nil, nil
		},
		probeCDNPaths: func(_ context.Context, rawURL string, _ http.Header, paths []transfer.NetworkPath, _ *transfer.CDNProbeCache, _ ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			probeCalls++
			if !reflect.DeepEqual(paths, []transfer.NetworkPath{worker1, worker2}) {
				t.Fatalf("unexpected batch probe candidates: %#v", paths)
			}
			if rawURL != "https://cdn.example.invalid/a" && rawURL != "https://cdn.example.invalid/b" {
				t.Fatalf("unexpected batch CDN URL: %q", rawURL)
			}
			return transfer.CDNDiscoveryResult{Host: "cdn.example.invalid", Paths: []transfer.NetworkPath{worker1, worker2}}, nil
		},
		scheduleFiles: func(_ context.Context, workers []transfer.NetworkPath, jobs []transfer.FileTransferJob, _ ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			scheduleCalls++
			if !reflect.DeepEqual(workers, []transfer.NetworkPath{worker1, worker2}) {
				t.Fatalf("unexpected scheduler workers: %#v", workers)
			}
			if len(jobs) != 2 {
				t.Fatalf("expected one two-file scheduler run, got %d jobs", len(jobs))
			}
			if jobs[0].ID != "pick-a" || jobs[1].ID != "pick-b" {
				t.Fatalf("unexpected batch job IDs: %q, %q", jobs[0].ID, jobs[1].ID)
			}
			if jobs[0].DestinationPath != filepath.Join("root", "a.bin") || jobs[1].DestinationPath != filepath.Join("root", "b.bin") {
				t.Fatalf("unexpected batch destinations: %#v", jobs)
			}
			return transfer.FileScheduleReport{Results: []transfer.FileScheduleResult{
				{JobID: jobs[0].ID, Result: transfer.FileDownloadResult{DestinationPath: jobs[0].DestinationPath, BytesWritten: 100}},
				{JobID: jobs[1].ID, Result: transfer.FileDownloadResult{DestinationPath: jobs[1].DestinationPath, BytesWritten: 200}},
			}}, nil
		},
		downloadChunks: func(context.Context, transfer.ChunkDownloadRequest) (transfer.ChunkDownloadResult, error) {
			t.Fatal("chunk downloader should not run for file batch strategy")
			return transfer.ChunkDownloadResult{}, nil
		},
	}

	items := []mcpDownloadBatchTransferItem{
		{info: &driver.DownloadInfo{FileName: "a.bin", FileSize: 100, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/a"}}, localPath: filepath.Join("root", "a.bin"), stableID: "pick-a"},
		{info: &driver.DownloadInfo{FileName: "b.bin", FileSize: 200, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/b"}}, localPath: filepath.Join("root", "b.bin"), stableID: "pick-b"},
	}
	results, err := ft.downloadFileBatchThroughTransferWithRefresh(context.Background(), items)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].result.BytesWritten != 100 || results[1].result.BytesWritten != 200 {
		t.Fatalf("unexpected batch results: %#v", results)
	}
	if discoverCalls != 1 || probeCalls != 2 || scheduleCalls != 1 {
		t.Fatalf("unexpected batch control-plane calls: discovery=%d probe=%d schedule=%d", discoverCalls, probeCalls, scheduleCalls)
	}
}

func TestDownloadFileBatchPreservesPerItemFailureAlongsideAggregateSchedulerError(t *testing.T) {
	worker := mcpTestNetworkPath("Ethernet 1", 1, "10.0.0.1")
	itemErr := errors.New("item failed")
	aggregateErr := errors.New("batch incomplete")
	ft := NewFileTools(nil)
	ft.downloadTransfer.deps = mcpDownloadTransferDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			return transfer.NetworkDiscoveryResult{Paths: []transfer.NetworkPath{worker}, Probes: []transfer.NetworkPathProbe{{Path: worker, Reachable: true}}}, nil
		},
		listNetworkPaths: func() ([]transfer.NetworkPath, error) { return nil, errors.New("unexpected manual listing") },
		probeCDNPaths: func(context.Context, string, http.Header, []transfer.NetworkPath, *transfer.CDNProbeCache, ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			return transfer.CDNDiscoveryResult{Host: "cdn.example.invalid", Paths: []transfer.NetworkPath{worker}}, nil
		},
		scheduleFiles: func(_ context.Context, _ []transfer.NetworkPath, jobs []transfer.FileTransferJob, _ ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			return transfer.FileScheduleReport{Results: []transfer.FileScheduleResult{
				{JobID: jobs[0].ID, Result: transfer.FileDownloadResult{DestinationPath: jobs[0].DestinationPath, BytesWritten: 10}},
				{JobID: jobs[1].ID, Result: transfer.FileDownloadResult{DestinationPath: jobs[1].DestinationPath}, Err: itemErr},
			}}, aggregateErr
		},
		downloadChunks: func(context.Context, transfer.ChunkDownloadRequest) (transfer.ChunkDownloadResult, error) {
			t.Fatal("chunk downloader should not run in file strategy")
			return transfer.ChunkDownloadResult{}, nil
		},
	}
	items := []mcpDownloadBatchTransferItem{
		{info: &driver.DownloadInfo{FileName: "a.bin", FileSize: 10, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/a"}}, localPath: filepath.Join("root", "a.bin"), stableID: "a"},
		{info: &driver.DownloadInfo{FileName: "b.bin", FileSize: 10, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/b"}}, localPath: filepath.Join("root", "b.bin"), stableID: "b"},
	}
	results, err := ft.downloadFileBatchThroughTransferWithRefresh(context.Background(), items)
	if !errors.Is(err, aggregateErr) {
		t.Fatalf("aggregate scheduler error = %v, want %v", err, aggregateErr)
	}
	if len(results) != 2 || results[0].err != nil || !errors.Is(results[1].err, itemErr) {
		t.Fatalf("per-item scheduler results were not preserved: %#v", results)
	}
}

func TestDownloadFileBatchZeroByteJobUsesSchedulerWorkersAfterEmptyProbe(t *testing.T) {
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
		scheduleFiles: func(_ context.Context, _ []transfer.NetworkPath, jobs []transfer.FileTransferJob, _ ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			if len(jobs) != 1 || jobs[0].NetworkPaths != nil {
				t.Fatalf("zero-byte batch job kept no-path restriction: %#v", jobs)
			}
			return transfer.FileScheduleReport{Results: []transfer.FileScheduleResult{{JobID: jobs[0].ID, Result: transfer.FileDownloadResult{DestinationPath: jobs[0].DestinationPath}}}}, nil
		},
		downloadChunks: func(context.Context, transfer.ChunkDownloadRequest) (transfer.ChunkDownloadResult, error) {
			t.Fatal("chunk downloader should not run in file strategy")
			return transfer.ChunkDownloadResult{}, nil
		},
	}
	results, err := ft.downloadFileBatchThroughTransferWithRefresh(context.Background(), []mcpDownloadBatchTransferItem{{
		info:      &driver.DownloadInfo{FileName: "empty.bin", FileSize: 0, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/empty"}},
		localPath: filepath.Join("root", "empty.bin"), stableID: "empty-pick",
	}})
	if err != nil || len(results) != 1 || results[0].err != nil {
		t.Fatalf("zero-byte batch download = %#v, %v", results, err)
	}
}

func TestDownloadFilesRejectsDuplicateLocalTargetBeforeUsingClient(t *testing.T) {
	root := t.TempDir()
	ft := NewFileTools(nil, WithLocalRoot(root))
	result, _, err := ft.downloadFiles(context.Background(), nil, DownloadFilesArgs{Files: []DownloadFileArgs{
		{PickCode: "pick-a", LocalPath: filepath.Join(root, "same.bin")},
		{PickCode: "pick-b", LocalPath: filepath.Join(root, "same.bin")},
	}})
	if err != nil {
		t.Fatalf("handler should return MCP tool error, got Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("duplicate local target did not fail closed: %#v", result)
	}
}

func TestDownloadFilesRejectsSymlinkAliasedLocalTargetsBeforeUsingClient(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlink creation unavailable on this runner: %v", err)
	}
	ft := NewFileTools(nil, WithLocalRoot(root))
	result, _, err := ft.downloadFiles(context.Background(), nil, DownloadFilesArgs{Files: []DownloadFileArgs{
		{PickCode: "pick-a", LocalPath: filepath.Join(realDir, "same.bin")},
		{PickCode: "pick-b", LocalPath: filepath.Join(linkDir, "same.bin")},
	}})
	if err != nil {
		t.Fatalf("handler should return MCP tool error, got Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("symlink-aliased local targets did not fail closed: %#v", result)
	}
}

func TestValidateMCPDownloadInfoForTransferPreflightsWholeBatchConstraints(t *testing.T) {
	info := &driver.DownloadInfo{FileSize: 11, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/file"}}
	if _, err := validateMCPDownloadInfoForTransfer(info, 10, "file"); !errors.Is(err, transfer.ErrDownloadExceedsLimit) {
		t.Fatalf("oversized metadata error = %v", err)
	}
	unknown := &driver.DownloadInfo{FileSize: -1, Url: driver.FileDownloadUrl{Url: "https://cdn.example.invalid/file"}}
	if _, err := validateMCPDownloadInfoForTransfer(unknown, 0, "chunk"); !errors.Is(err, transfer.ErrChunkRequiresKnownSize) {
		t.Fatalf("unknown-size chunk metadata error = %v", err)
	}
	if _, err := validateMCPDownloadInfoForTransfer(nil, 0, "file"); err == nil {
		t.Fatal("nil download metadata was accepted")
	}
}
