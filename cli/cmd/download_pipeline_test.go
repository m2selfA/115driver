package cmd

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

func TestExecuteDownloadCommandRecursiveWiresP1P2P4AndPreservesTree(t *testing.T) {
	managedRoot := filepath.Join(t.TempDir(), "sessions")
	t.Setenv("115DRIVER_SESSION_DIR", managedRoot)
	client := &fakeDownloadCommandClient{
		dirIDs: map[string]string{"root": "d1"},
		lists: map[string][]driver.File{
			"d1": {
				{FileID: "fa", Name: "a.bin", PickCode: "pa", Size: 10},
				{FileID: "ds", Name: "sub", IsDirectory: true},
				{FileID: "de", Name: "empty", IsDirectory: true},
			},
			"ds": {{FileID: "fb", Name: "b.bin", PickCode: "pb", Size: 20}},
			"de": {},
		},
		downloads: map[string]*driver.DownloadInfo{
			"pa": testDriverDownloadInfo("a.bin", 10, "https://cdn-a.example.invalid/a?token=secret-a"),
			"pb": testDriverDownloadInfo("b.bin", 20, "https://cdn-b.example.invalid/b?token=secret-b"),
		},
	}

	iface1v4 := testCLIPath("Ethernet", 1, "10.0.0.1")
	iface1v6 := testCLIPath("Ethernet", 1, "2001:db8::1")
	iface2v4 := testCLIPath("Ethernet 2", 2, "10.0.1.1")
	var scheduled []transfer.FileTransferJob
	deps := downloadPipelineDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			return transfer.NetworkDiscoveryResult{
				Paths: []transfer.NetworkPath{iface1v4, iface2v4},
				Probes: []transfer.NetworkPathProbe{
					{Path: iface1v4, Reachable: true},
					{Path: iface1v6, Reachable: true},
					{Path: iface2v4, Reachable: true},
				},
			}, nil
		},
		listNetworkPaths: func() ([]transfer.NetworkPath, error) { return nil, errors.New("manual listing should not run") },
		probeCDNPaths: func(_ context.Context, rawURL string, headers http.Header, candidates []transfer.NetworkPath, _ *transfer.CDNProbeCache, _ ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			if len(candidates) != 3 {
				t.Fatalf("expected all reachable P1 addresses as P2 candidates, got %#v", candidates)
			}
			if values, ok := headers["User-Agent"]; !ok || len(values) != 1 || values[0] != "" {
				t.Fatalf("empty User-Agent not preserved for P2: %#v", headers)
			}
			if strings.Contains(rawURL, "cdn-a") {
				return transfer.CDNDiscoveryResult{Host: "cdn-a.example.invalid", Paths: []transfer.NetworkPath{iface1v6, iface2v4}}, nil
			}
			return transfer.CDNDiscoveryResult{Host: "cdn-b.example.invalid", Paths: []transfer.NetworkPath{iface2v4}}, nil
		},
		scheduleFiles: func(_ context.Context, paths []transfer.NetworkPath, jobs []transfer.FileTransferJob, opts ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			if !reflect.DeepEqual(paths, []transfer.NetworkPath{iface1v4, iface2v4}) {
				t.Fatalf("unexpected scheduler workers: %#v", paths)
			}
			options := transfer.DefaultFileSchedulerOptions()
			for _, opt := range opts {
				opt(&options)
			}
			if options.HealthTracker == nil {
				t.Fatal("file scheduler did not receive P8 health tracker")
			}
			scheduled = append([]transfer.FileTransferJob(nil), jobs...)
			report := transfer.FileScheduleReport{Results: make([]transfer.FileScheduleResult, len(jobs))}
			for i, job := range jobs {
				report.Results[i] = transfer.FileScheduleResult{
					JobID:           job.ID,
					DestinationPath: job.DestinationPath,
					ExpectedSize:    job.ExpectedSize,
					Result: transfer.FileDownloadResult{
						NetworkPath:     job.NetworkPaths[0],
						DestinationPath: job.DestinationPath,
						BytesWritten:    job.ExpectedSize,
					},
				}
			}
			return report, nil
		},
	}

	root := filepath.Join(t.TempDir(), "download-root")
	summary, err := executeDownloadCommand(context.Background(), client, "/root", root, testDownloadCommandOptions(true), deps)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FileCount != 2 || summary.SucceededCount != 2 || summary.TotalBytes != 30 || summary.TransferredBytes != 30 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
	if len(scheduled) != 2 {
		t.Fatalf("expected two scheduled jobs, got %d", len(scheduled))
	}
	byID := map[string]transfer.FileTransferJob{}
	for _, job := range scheduled {
		byID[job.ID] = job
	}
	if got := byID["fa"].DestinationPath; got != filepath.Join(root, "a.bin") {
		t.Fatalf("unexpected a destination: %q", got)
	}
	if got := byID["fb"].DestinationPath; got != filepath.Join(root, "sub", "b.bin") {
		t.Fatalf("unexpected b destination: %q", got)
	}
	if len(byID["fa"].NetworkPaths) != 2 || byID["fa"].NetworkPaths[0].InterfaceIndex != 1 || !byID["fa"].NetworkPaths[0].LocalIP.Equal(iface1v6.LocalIP) {
		t.Fatalf("P2 path selection was not attached to file a: %#v", byID["fa"].NetworkPaths)
	}
	if len(byID["fb"].NetworkPaths) != 1 || byID["fb"].NetworkPaths[0].InterfaceIndex != 2 {
		t.Fatalf("P2 path selection was not attached to file b: %#v", byID["fb"].NetworkPaths)
	}
	if byID["fa"].ResumeKey != "pa" || byID["fb"].ResumeKey != "pb" || byID["fa"].MaxRefreshes != transfer.DefaultDownloadURLRefreshes || byID["fa"].Refresh == nil {
		t.Fatalf("P9 resume/refresh settings were not attached to file jobs: a=%#v b=%#v", byID["fa"], byID["fb"])
	}
	for _, job := range []transfer.FileTransferJob{byID["fa"], byID["fb"]} {
		inside, err := pathIsWithin(managedRoot, job.ResumeStatePath)
		if err != nil || !inside || filepath.Base(filepath.Dir(job.ResumeStatePath)) != "parts" || !strings.HasSuffix(filepath.Base(job.ResumeStatePath), ".download.json") {
			t.Fatalf("recursive resume metadata was not centralized below tree parts: path=%q inside=%v err=%v", job.ResumeStatePath, inside, err)
		}
	}
	for _, relative := range []string{"sub", "empty"} {
		info, err := os.Stat(filepath.Join(root, relative))
		if err != nil || !info.IsDir() {
			t.Fatalf("expected recursive directory %q to exist: info=%v err=%v", relative, info, err)
		}
	}
}

func TestExecuteDownloadCommandRecursiveSessionSkipsCompletedFilesOnSecondRun(t *testing.T) {
	client := &fakeDownloadCommandClient{
		dirIDs: map[string]string{"root": "d1"},
		lists: map[string][]driver.File{
			"d1": {
				{FileID: "fa", Name: "a.bin", PickCode: "pa", Size: 4, Sha1: "AAAA"},
				{FileID: "fb", Name: "b.bin", PickCode: "pb", Size: 6, Sha1: "BBBB"},
			},
		},
		downloads: map[string]*driver.DownloadInfo{
			"pa": testDriverDownloadInfo("a.bin", 4, "https://cdn.example.invalid/a?token=1"),
			"pb": testDriverDownloadInfo("b.bin", 6, "https://cdn.example.invalid/b?token=2"),
		},
	}
	iface := testCLIPath("Ethernet", 1, "10.0.0.1")
	var firstRun = true
	var scheduledRuns [][]string
	deps := downloadPipelineDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			return transfer.NetworkDiscoveryResult{Paths: []transfer.NetworkPath{iface}, Probes: []transfer.NetworkPathProbe{{Path: iface, Reachable: true}}}, nil
		},
		listNetworkPaths: func() ([]transfer.NetworkPath, error) { return nil, errors.New("manual listing should not run") },
		probeCDNPaths: func(_ context.Context, _ string, _ http.Header, _ []transfer.NetworkPath, _ *transfer.CDNProbeCache, _ ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			return transfer.CDNDiscoveryResult{Host: "cdn.example.invalid", Paths: []transfer.NetworkPath{iface}}, nil
		},
		scheduleFiles: func(_ context.Context, _ []transfer.NetworkPath, jobs []transfer.FileTransferJob, opts ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			options := transfer.DefaultFileSchedulerOptions()
			for _, opt := range opts {
				opt(&options)
			}
			ids := make([]string, len(jobs))
			report := transfer.FileScheduleReport{Results: make([]transfer.FileScheduleResult, len(jobs))}
			failed := 0
			for i, job := range jobs {
				ids[i] = job.ID
				result := transfer.FileScheduleResult{JobID: job.ID, DestinationPath: job.DestinationPath, ExpectedSize: job.ExpectedSize}
				if firstRun && job.ID == "fb" {
					result.Err = errors.New("simulated disconnect")
					failed++
					report.Results[i] = result
					continue
				}
				if err := os.WriteFile(job.DestinationPath, []byte(strings.Repeat("x", int(job.ExpectedSize))), 0600); err != nil {
					t.Fatal(err)
				}
				result.Result = transfer.FileDownloadResult{DestinationPath: job.DestinationPath, BytesWritten: job.ExpectedSize, NetworkPath: iface}
				if options.SuccessHook != nil {
					if err := options.SuccessHook(result); err != nil {
						result.Err = err
						failed++
					}
				}
				report.Results[i] = result
			}
			scheduledRuns = append(scheduledRuns, ids)
			if failed > 0 {
				return report, transfer.ErrFileScheduleIncomplete
			}
			return report, nil
		},
		downloadChunks: func(context.Context, transfer.ChunkDownloadRequest) (transfer.ChunkDownloadResult, error) {
			t.Fatal("chunk downloader should not run")
			return transfer.ChunkDownloadResult{}, nil
		},
	}
	root := filepath.Join(t.TempDir(), "download-root")
	sessionPath := filepath.Join(t.TempDir(), "recursive-download.session.json")
	options := testDownloadCommandOptions(true)
	options.SessionPath = sessionPath

	first, err := executeDownloadCommand(context.Background(), client, "/root", root, options, deps)
	if err == nil {
		t.Fatalf("expected first run failure, got %#v", first)
	}
	if first.SucceededCount != 1 || first.FailedCount != 1 || first.SessionPath != sessionPath {
		t.Fatalf("unexpected first session summary: %#v", first)
	}
	if _, err := os.Stat(sessionPath); err != nil {
		t.Fatalf("failed directory session was not preserved: %v", err)
	}

	firstRun = false
	second, err := executeDownloadCommand(context.Background(), client, "/root", root, options, deps)
	if err != nil {
		t.Fatal(err)
	}
	if second.FileCount != 2 || second.SucceededCount != 2 || second.ResumedCount != 1 || second.TotalBytes != 10 || second.SessionPath != "" {
		t.Fatalf("unexpected resumed download summary: %#v", second)
	}
	if len(scheduledRuns) != 2 || !reflect.DeepEqual(scheduledRuns[0], []string{"fa", "fb"}) || !reflect.DeepEqual(scheduledRuns[1], []string{"fb"}) {
		t.Fatalf("completed file was scheduled again: %#v", scheduledRuns)
	}
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Fatalf("successful directory session was not cleaned: %v", err)
	}
}

func TestPrepareRecursiveDownloadSessionResetsCompletedFileWhenLocalCopyChanges(t *testing.T) {
	root := t.TempDir()
	remote := remoteDownloadTree{Directories: []string{""}, Files: []remoteDownloadFile{{
		File: driver.File{FileID: "fa", Name: "a.bin", PickCode: "pa", Size: 4, Sha1: "70C881D4A26984DDCE795F6F71817C9CF4480E79"}, RelativePath: "a.bin", RemotePath: "/root/a.bin",
	}}}
	sessionPath := filepath.Join(t.TempDir(), "download.session.json")
	session, pending, _, _, err := prepareRecursiveDownloadSession("/root", root, remote, "file", sessionPath)
	if err != nil || len(pending) != 1 {
		t.Fatalf("unexpected initial session: pending=%v err=%v", pending, err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.bin"), []byte("aaaa"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := markDownloadSessionCompleted(session, "a.bin", filepath.Join(root, "a.bin"), 4); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.bin"), []byte("zzzz"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, pending, resumed, _, err := prepareRecursiveDownloadSession("/root", root, remote, "file", sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if resumed != 0 || len(pending) != 1 {
		t.Fatalf("changed completed file was incorrectly trusted: resumed=%d pending=%#v", resumed, pending)
	}
}

func TestPrepareRecursiveDownloadSessionRejectsSessionInsideDestination(t *testing.T) {
	root := t.TempDir()
	tree := remoteDownloadTree{Directories: []string{""}}
	inside := filepath.Join(root, "session.json")
	_, _, _, _, err := prepareRecursiveDownloadSession("/root", root, tree, "file", inside)
	if !errors.Is(err, errDownloadUsage) {
		t.Fatalf("expected in-destination session rejection, got %v", err)
	}
}

func TestExecuteDownloadCommandEmptyDirectorySkipsNetworkDiscovery(t *testing.T) {
	client := &fakeDownloadCommandClient{
		dirIDs: map[string]string{"empty": "d1"},
		lists:  map[string][]driver.File{"d1": {}},
	}
	var discovered atomic.Bool
	deps := defaultDownloadPipelineDeps()
	deps.discoverNetworkPaths = func(context.Context) (transfer.NetworkDiscoveryResult, error) {
		discovered.Store(true)
		return transfer.NetworkDiscoveryResult{}, errors.New("should not run")
	}
	root := filepath.Join(t.TempDir(), "empty-root")
	summary, err := executeDownloadCommand(context.Background(), client, "/empty", root, testDownloadCommandOptions(true), deps)
	if err != nil {
		t.Fatal(err)
	}
	if summary.FileCount != 0 || discovered.Load() {
		t.Fatalf("unexpected empty-directory behavior: summary=%#v discovered=%v", summary, discovered.Load())
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		t.Fatalf("empty destination root was not created: info=%v err=%v", info, err)
	}
}

func TestCollectRemoteDownloadTreeRequiresRecursiveForDirectory(t *testing.T) {
	client := &fakeDownloadCommandClient{}
	_, err := collectRemoteDownloadTree(client, "d1", "/dir", true, false)
	if err == nil || !strings.Contains(err.Error(), "--recursive") {
		t.Fatalf("expected recursive hint, got %v", err)
	}
}

func TestCollectRemoteDownloadTreeRejectsPathSeparatorsInRemoteNames(t *testing.T) {
	client := &fakeDownloadCommandClient{lists: map[string][]driver.File{
		"d1": {{FileID: "bad", Name: "../escape.bin", PickCode: "p"}},
	}}
	_, err := collectRemoteDownloadTree(client, "d1", "/dir", true, true)
	if err == nil || !strings.Contains(err.Error(), "unsafe remote entry") {
		t.Fatalf("expected unsafe name error, got %v", err)
	}
}

func TestCollectRemoteDownloadTreeRejectsMalformedDirectoryListings(t *testing.T) {
	for name, client := range map[string]*fakeDownloadCommandClient{
		"nil-list":             {nilLists: map[string]bool{"d1": true}},
		"directory-without-id": {lists: map[string][]driver.File{"d1": {{Name: "sub", IsDirectory: true}}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := collectRemoteDownloadTree(client, "d1", "/dir", true, true)
			if err == nil {
				t.Fatal("expected malformed directory listing error")
			}
			if client.listCalls[""] != 0 {
				t.Fatalf("malformed directory listing fell back to root %d time(s)", client.listCalls[""])
			}
		})
	}
}

func TestResolveDownloadPathSelectionAutoKeepsAllReachableAddressesForP2(t *testing.T) {
	iface1v4 := testCLIPath("Ethernet", 1, "10.0.0.1")
	iface1v6 := testCLIPath("Ethernet", 1, "2001:db8::1")
	iface2 := testCLIPath("Ethernet 2", 2, "10.0.1.1")
	selection, err := resolveDownloadPathSelection(context.Background(), "auto", downloadPipelineDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			return transfer.NetworkDiscoveryResult{
				Paths: []transfer.NetworkPath{iface1v4, iface2},
				Probes: []transfer.NetworkPathProbe{
					{Path: iface1v4, Reachable: true},
					{Path: iface1v6, Reachable: true},
					{Path: iface2, Reachable: true},
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Workers) != 2 || len(selection.Candidates) != 3 {
		t.Fatalf("unexpected auto selection: %#v", selection)
	}
}

func TestResolveDownloadPathSelectionManualMatchesNameIndexAndIP(t *testing.T) {
	paths := []transfer.NetworkPath{
		testCLIPath("Ethernet", 1, "10.0.0.1"),
		testCLIPath("Ethernet", 1, "2001:db8::1"),
		testCLIPath("Wi-Fi", 2, "10.0.1.1"),
	}
	selection, err := resolveDownloadPathSelection(context.Background(), "Ethernet,2", downloadPipelineDeps{
		listNetworkPaths: func() ([]transfer.NetworkPath, error) { return paths, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(selection.Candidates) != 3 || len(selection.Workers) != 2 {
		t.Fatalf("unexpected manual selection: %#v", selection)
	}
	selectedByIP, err := selectManualNetworkPaths(paths, "10.0.1.1")
	if err != nil || len(selectedByIP) != 1 || selectedByIP[0].InterfaceIndex != 2 {
		t.Fatalf("IP selector failed: paths=%#v err=%v", selectedByIP, err)
	}
}

func TestExecuteDownloadCommandSingleFileUsesManagedResumeMetadata(t *testing.T) {
	managedRoot := filepath.Join(t.TempDir(), "sessions")
	t.Setenv("115DRIVER_SESSION_DIR", managedRoot)
	client := &fakeDownloadCommandClient{
		lists: map[string][]driver.File{
			"0": {{FileID: "fa", Name: "a.bin", PickCode: "pa", Size: 4}},
		},
		downloads: map[string]*driver.DownloadInfo{
			"pa": testDriverDownloadInfo("a.bin", 4, "https://cdn.example.invalid/a"),
		},
	}
	iface := testCLIPath("Ethernet", 1, "10.0.0.1")
	var captured transfer.FileTransferJob
	deps := downloadPipelineDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			return transfer.NetworkDiscoveryResult{Paths: []transfer.NetworkPath{iface}, Probes: []transfer.NetworkPathProbe{{Path: iface, Reachable: true}}}, nil
		},
		probeCDNPaths: func(_ context.Context, _ string, _ http.Header, _ []transfer.NetworkPath, _ *transfer.CDNProbeCache, _ ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			return transfer.CDNDiscoveryResult{Host: "cdn.example.invalid", Paths: []transfer.NetworkPath{iface}}, nil
		},
		scheduleFiles: func(_ context.Context, _ []transfer.NetworkPath, jobs []transfer.FileTransferJob, _ ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			if len(jobs) != 1 {
				t.Fatalf("expected one single-file job, got %#v", jobs)
			}
			captured = jobs[0]
			if filepath.Base(captured.ResumeStatePath) != "payload.json" {
				t.Fatalf("single-file resume metadata is not managed payload: %q", captured.ResumeStatePath)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(captured.ResumeStatePath), "session.json")); err != nil {
				t.Fatalf("single-file managed manifest missing during transfer: %v", err)
			}
			return transfer.FileScheduleReport{Results: []transfer.FileScheduleResult{{
				JobID: captured.ID, DestinationPath: captured.DestinationPath, ExpectedSize: captured.ExpectedSize,
				Result: transfer.FileDownloadResult{DestinationPath: captured.DestinationPath, BytesWritten: captured.ExpectedSize, NetworkPath: iface},
			}}}, nil
		},
		downloadChunks: func(context.Context, transfer.ChunkDownloadRequest) (transfer.ChunkDownloadResult, error) {
			t.Fatal("chunk downloader should not run")
			return transfer.ChunkDownloadResult{}, nil
		},
	}
	localTarget := filepath.Join(t.TempDir(), "a.bin")
	summary, err := executeDownloadCommand(context.Background(), client, "/a.bin", localTarget, testDownloadCommandOptions(false), deps)
	if err != nil {
		t.Fatal(err)
	}
	inside, err := pathIsWithin(managedRoot, captured.ResumeStatePath)
	if err != nil || !inside {
		t.Fatalf("single-file metadata was not below managed root: %q inside=%v err=%v", captured.ResumeStatePath, inside, err)
	}
	if summary.SessionPath != "" || summary.SucceededCount != 1 {
		t.Fatalf("single-file managed session was not cleaned after success: %#v", summary)
	}
	if _, err := os.Stat(filepath.Dir(captured.ResumeStatePath)); !os.IsNotExist(err) {
		t.Fatalf("completed single-file session directory survived: %v", err)
	}
}

func TestExecuteDownloadCommandChunkUsesLiveRangePathsAndChunkSettings(t *testing.T) {
	managedRoot := filepath.Join(t.TempDir(), "sessions")
	t.Setenv("115DRIVER_SESSION_DIR", managedRoot)
	client := &fakeDownloadCommandClient{
		dirIDs: map[string]string{"root": "d1"},
		lists: map[string][]driver.File{
			"d1": {{FileID: "fa", Name: "a.bin", PickCode: "pa", Size: 10}},
		},
		downloads: map[string]*driver.DownloadInfo{
			"pa": testDriverDownloadInfo("a.bin", 10, "https://cdn.example.invalid/a?token=secret"),
		},
	}
	iface1 := testCLIPath("Ethernet", 1, "10.0.0.1")
	iface2 := testCLIPath("Ethernet 2", 2, "10.0.1.1")
	var captured transfer.ChunkDownloadRequest
	probeCalls := 0
	deps := downloadPipelineDeps{
		discoverNetworkPaths: func(context.Context) (transfer.NetworkDiscoveryResult, error) {
			return transfer.NetworkDiscoveryResult{
				Paths:  []transfer.NetworkPath{iface1, iface2},
				Probes: []transfer.NetworkPathProbe{{Path: iface1, Reachable: true}, {Path: iface2, Reachable: true}},
			}, nil
		},
		probeCDNPaths: func(_ context.Context, _ string, _ http.Header, paths []transfer.NetworkPath, _ *transfer.CDNProbeCache, opts ...transfer.CDNProbeOption) (transfer.CDNDiscoveryResult, error) {
			probeCalls++
			if probeCalls == 1 && !reflect.DeepEqual(paths, []transfer.NetworkPath{iface1, iface2}) {
				t.Fatalf("unexpected initial P2 paths: %#v", paths)
			}
			if probeCalls == 2 && !reflect.DeepEqual(paths, []transfer.NetworkPath{iface2}) {
				t.Fatalf("refresh did not re-probe the current Range paths: %#v", paths)
			}
			probeOptions := transfer.DefaultCDNProbeOptions()
			for _, opt := range opts {
				opt(&probeOptions)
			}
			if !probeOptions.RequireRangeValidation {
				t.Fatal("chunk strategy did not force live Range validation")
			}
			return transfer.CDNDiscoveryResult{
				Host:       "cdn.example.invalid",
				Paths:      []transfer.NetworkPath{iface1, iface2},
				RangePaths: []transfer.NetworkPath{iface2},
			}, nil
		},
		scheduleFiles: func(context.Context, []transfer.NetworkPath, []transfer.FileTransferJob, ...transfer.FileSchedulerOption) (transfer.FileScheduleReport, error) {
			t.Fatal("file scheduler must not run in chunk strategy")
			return transfer.FileScheduleReport{}, nil
		},
		downloadChunks: func(_ context.Context, request transfer.ChunkDownloadRequest) (transfer.ChunkDownloadResult, error) {
			captured = request
			if request.HealthTracker == nil {
				t.Fatal("chunk downloader did not receive P8 health tracker")
			}
			if err := os.WriteFile(request.DestinationPath, []byte(strings.Repeat("x", int(request.ExpectedSize))), 0600); err != nil {
				t.Fatal(err)
			}
			return transfer.ChunkDownloadResult{DestinationPath: request.DestinationPath, BytesWritten: request.ExpectedSize, ChunkSize: request.ChunkSize, ChunkCount: 2}, nil
		},
	}
	options := testDownloadCommandOptions(true)
	options.Strategy = "chunk"
	options.ChunkSize = 6
	options.Retries = 2
	options.WorkersPerInterface = 3
	options.Timeout = 90 * time.Second
	root := filepath.Join(t.TempDir(), "chunk-root")
	summary, err := executeDownloadCommand(context.Background(), client, "/root", root, options, deps)
	if err != nil {
		t.Fatal(err)
	}
	if summary.SucceededCount != 1 || summary.TransferredBytes != 10 || len(summary.ChunkResults) != 1 {
		t.Fatalf("unexpected chunk summary: %#v", summary)
	}
	if captured.ChunkSize != 6 || captured.Retries != 2 || captured.RecoveryRetries != 2 || captured.WorkersPerInterface != 3 || captured.Timeout != 90*time.Second || captured.ExpectedSize != 10 {
		t.Fatalf("chunk settings were not preserved: %#v", captured)
	}
	if captured.ResumeKey != "pa" || captured.MaxRefreshes != transfer.DefaultDownloadURLRefreshes || captured.Refresh == nil {
		t.Fatalf("P9 chunk resume/refresh settings were not preserved: %#v", captured)
	}
	inside, err := pathIsWithin(managedRoot, captured.ResumeStatePath)
	if err != nil || !inside || filepath.Base(filepath.Dir(captured.ResumeStatePath)) != "parts" || !strings.HasSuffix(filepath.Base(captured.ResumeStatePath), ".download.json") {
		t.Fatalf("chunk resume metadata was not centralized below tree parts: path=%q inside=%v err=%v", captured.ResumeStatePath, inside, err)
	}
	client.downloads["pa"] = testDriverDownloadInfo("a.bin", 10, "https://cdn.example.invalid/a?token=refreshed")
	fresh, refreshErr := captured.Refresh(context.Background())
	if refreshErr != nil || fresh.URL != "https://cdn.example.invalid/a?token=refreshed" {
		t.Fatalf("CLI refresh callback did not reacquire download info: source=%#v err=%v", fresh, refreshErr)
	}
	if probeCalls != 2 {
		t.Fatalf("expected refreshed source to trigger a second P2 probe, got %d call(s)", probeCalls)
	}
	if len(captured.NetworkPaths) != 1 || captured.NetworkPaths[0].InterfaceIndex != 2 {
		t.Fatalf("chunk download did not use P2 RangePaths: %#v", captured.NetworkPaths)
	}
	if values := captured.Header.Values("User-Agent"); len(values) != 1 || values[0] != "" {
		t.Fatalf("empty User-Agent was not preserved: %#v", values)
	}
}

func TestValidateDownloadCommandOptionsAcceptsChunkAndRejectsUnknownStrategy(t *testing.T) {
	options := testDownloadCommandOptions(false)
	options.Strategy = "chunk"
	if err := validateDownloadCommandOptions(options); err != nil {
		t.Fatalf("chunk strategy should be accepted: %v", err)
	}
	options.WorkersPerInterface = 3
	if err := validateDownloadCommandOptions(options); err != nil {
		t.Fatalf("multiple workers per interface should be accepted: %v", err)
	}
	options.WorkersPerInterface = 0
	if err := validateDownloadCommandOptions(options); err == nil {
		t.Fatal("expected zero workers per interface to be rejected")
	}
	options.WorkersPerInterface = 1
	options.Strategy = "future"
	if err := validateDownloadCommandOptions(options); err == nil {
		t.Fatal("expected unknown strategy to be rejected")
	}
}

func TestExecuteDownloadCommandPreservesResolverNetworkFailure(t *testing.T) {
	networkErr := &net.DNSError{Err: "temporary resolver failure", Name: "webapi.115.com", IsTemporary: true}
	client := &fakeDownloadCommandClient{dirErrs: map[string]error{"missing": networkErr}}

	_, err := executeDownloadCommand(context.Background(), client, "/missing", filepath.Join(t.TempDir(), "out.bin"), testDownloadCommandOptions(false), defaultDownloadPipelineDeps())
	if err == nil {
		t.Fatal("expected resolver network failure")
	}
	var dnsErr *net.DNSError
	if !errors.As(err, &dnsErr) {
		t.Fatalf("download resolver error = %T %v, want net.DNSError", err, err)
	}
	if errors.Is(err, errDownloadRemoteNotFound) {
		t.Fatalf("network failure was mislabeled as remote-not-found: %v", err)
	}
	if got := classifyRemoteError(err, output.ExitError); got != output.ExitNetwork {
		t.Fatalf("download resolver network exit = %d, want ExitNetwork", got)
	}
}

func testDownloadCommandOptions(recursive bool) downloadCommandOptions {
	return downloadCommandOptions{
		Recursive:           recursive,
		Timeout:             2 * time.Hour,
		Interfaces:          "auto",
		Strategy:            "file",
		WorkersPerInterface: 1,
		ProbeCacheTTL:       15 * time.Minute,
		Retries:             3,
		ChunkSize:           transfer.DefaultChunkSize,
		HealthCooldown:      transfer.DefaultNetworkHealthCooldown,
		HealthCooldownMax:   transfer.DefaultNetworkHealthCooldownMax,
		Resume:              true,
		URLRefreshes:        transfer.DefaultDownloadURLRefreshes,
	}
}

func testDriverDownloadInfo(name string, size int64, rawURL string) *driver.DownloadInfo {
	return &driver.DownloadInfo{
		FileName: name,
		FileSize: driver.StringInt64(size),
		Url:      driver.FileDownloadUrl{Url: rawURL, Valid: true},
		Header:   http.Header{"User-Agent": []string{""}, "Cookie": []string{"session=abc"}},
	}
}

func testCLIPath(name string, index int, ip string) transfer.NetworkPath {
	return transfer.NetworkPath{InterfaceName: name, InterfaceIndex: index, LocalIP: net.ParseIP(ip)}
}

type fakeDownloadCommandClient struct {
	nilLists  map[string]bool
	listCalls map[string]int
	dirIDs    map[string]string
	dirErrs   map[string]error
	lists     map[string][]driver.File
	files     map[string]driver.File
	downloads map[string]*driver.DownloadInfo
	dirCalls  map[string]int
}

func (client *fakeDownloadCommandClient) DirName2CID(dir string) (*driver.APIGetDirIDResp, error) {
	if client.dirCalls == nil {
		client.dirCalls = make(map[string]int)
	}
	client.dirCalls[dir]++
	if err := client.dirErrs[dir]; err != nil {
		return nil, err
	}
	id := "0"
	if client.dirIDs != nil {
		if configured, ok := client.dirIDs[dir]; ok {
			id = configured
		}
	}
	return &driver.APIGetDirIDResp{CategoryID: driver.IntString(id)}, nil
}

func (client *fakeDownloadCommandClient) List(dirID string, _ ...driver.ListOption) (*[]driver.File, error) {
	if client.listCalls == nil {
		client.listCalls = make(map[string]int)
	}
	client.listCalls[dirID]++
	if client.nilLists[dirID] {
		return nil, nil
	}
	entries := append([]driver.File(nil), client.lists[dirID]...)
	return &entries, nil
}

func (client *fakeDownloadCommandClient) ListPage(dirID string, offset, limit int64, _ ...driver.ListOption) (*[]driver.File, error) {
	entries := client.lists[dirID]
	if offset >= int64(len(entries)) {
		empty := []driver.File{}
		return &empty, nil
	}
	end := offset + limit
	if end > int64(len(entries)) {
		end = int64(len(entries))
	}
	page := append([]driver.File(nil), entries[offset:end]...)
	return &page, nil
}

func (client *fakeDownloadCommandClient) GetFile(fileID string) (*driver.File, error) {
	if file, ok := client.files[fileID]; ok {
		clone := file
		return &clone, nil
	}
	for _, entries := range client.lists {
		for _, file := range entries {
			if file.FileID == fileID {
				clone := file
				return &clone, nil
			}
		}
	}
	return nil, errors.New("file not found")
}

func (client *fakeDownloadCommandClient) Download(pickCode string) (*driver.DownloadInfo, error) {
	info, ok := client.downloads[pickCode]
	if !ok {
		return nil, errors.New("download info not found")
	}
	clone := *info
	clone.Header = info.Header.Clone()
	return &clone, nil
}
