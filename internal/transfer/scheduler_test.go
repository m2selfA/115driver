package transfer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestScheduleFileDownloadsLargestFirstAndResultsStayInInputOrder(t *testing.T) {
	dir := t.TempDir()
	paths := []NetworkPath{testNetworkPath(1, "10.0.0.1")}
	jobs := []FileTransferJob{
		testScheduledFileJob(dir, "small", 10),
		testScheduledFileJob(dir, "unknown-a", UnknownFileSize),
		testScheduledFileJob(dir, "large", 30),
		testScheduledFileJob(dir, "medium", 20),
		testScheduledFileJob(dir, "unknown-b", UnknownFileSize),
	}

	var dispatchOrder []string
	download := func(_ context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		dispatchOrder = append(dispatchOrder, filepath.Base(request.DestinationPath))
		return successfulScheduledDownload(request), nil
	}

	report, err := scheduleFileDownloads(context.Background(), paths, jobs, download, WithFileScheduleRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	wantDispatch := []string{"large.bin", "medium.bin", "small.bin", "unknown-a.bin", "unknown-b.bin"}
	if !reflect.DeepEqual(dispatchOrder, wantDispatch) {
		t.Fatalf("unexpected dispatch order: got %v want %v", dispatchOrder, wantDispatch)
	}
	wantResults := []string{"small", "unknown-a", "large", "medium", "unknown-b"}
	gotResults := make([]string, len(report.Results))
	for i, result := range report.Results {
		gotResults[i] = result.JobID
	}
	if !reflect.DeepEqual(gotResults, wantResults) {
		t.Fatalf("results did not preserve input order: got %v want %v", gotResults, wantResults)
	}
	if report.SucceededCount() != len(jobs) || report.FailedCount() != 0 {
		t.Fatalf("unexpected report counts: %#v", report)
	}
}

func TestScheduleFileDownloadsCanPreserveInputQueueOrder(t *testing.T) {
	dir := t.TempDir()
	paths := []NetworkPath{testNetworkPath(1, "10.0.0.1")}
	jobs := []FileTransferJob{
		testScheduledFileJob(dir, "small", 10),
		testScheduledFileJob(dir, "large", 30),
		testScheduledFileJob(dir, "medium", 20),
	}

	var dispatchOrder []string
	download := func(_ context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		dispatchOrder = append(dispatchOrder, filepath.Base(request.DestinationPath))
		return successfulScheduledDownload(request), nil
	}

	_, err := scheduleFileDownloads(
		context.Background(),
		paths,
		jobs,
		download,
		WithFileScheduleRetries(0),
		WithFileScheduleLargestFirst(false),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"small.bin", "large.bin", "medium.bin"}
	if !reflect.DeepEqual(dispatchOrder, want) {
		t.Fatalf("unexpected input-order dispatch: got %v want %v", dispatchOrder, want)
	}
}

func TestScheduleFileDownloadsUsesOneConcurrentWorkerPerInterface(t *testing.T) {
	dir := t.TempDir()
	paths := []NetworkPath{
		testNetworkPath(1, "10.0.0.1"),
		testNetworkPath(2, "10.0.0.2"),
		testNetworkPath(3, "10.0.0.3"),
	}
	jobs := make([]FileTransferJob, 6)
	for i := range jobs {
		jobs[i] = testScheduledFileJob(dir, fmt.Sprintf("job-%d", i), int64(100-i))
	}

	var mu sync.Mutex
	activeByInterface := make(map[int]int)
	maxByInterface := make(map[int]int)
	globalActive := 0
	maxGlobal := 0
	var firstWave atomic.Int32
	release := make(chan struct{})

	download := func(ctx context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		index := request.NetworkPath.InterfaceIndex
		mu.Lock()
		activeByInterface[index]++
		if activeByInterface[index] > maxByInterface[index] {
			maxByInterface[index] = activeByInterface[index]
		}
		globalActive++
		if globalActive > maxGlobal {
			maxGlobal = globalActive
		}
		mu.Unlock()

		if firstWave.Add(1) == int32(len(paths)) {
			close(release)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return FileDownloadResult{}, ctx.Err()
		case <-time.After(time.Second):
			return FileDownloadResult{}, errors.New("workers did not overlap")
		}
		time.Sleep(5 * time.Millisecond)

		mu.Lock()
		activeByInterface[index]--
		globalActive--
		mu.Unlock()
		return successfulScheduledDownload(request), nil
	}

	report, err := scheduleFileDownloads(context.Background(), paths, jobs, download, WithFileScheduleRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	if report.SucceededCount() != len(jobs) {
		t.Fatalf("unexpected report: %#v", report)
	}
	mu.Lock()
	defer mu.Unlock()
	if maxGlobal != len(paths) {
		t.Fatalf("expected %d concurrent interface workers, saw %d", len(paths), maxGlobal)
	}
	for _, path := range paths {
		if maxByInterface[path.InterfaceIndex] != 1 {
			t.Fatalf("interface %d had %d concurrent files", path.InterfaceIndex, maxByInterface[path.InterfaceIndex])
		}
	}
}

func TestScheduleFileDownloadsUsesMultipleConnectionSlotsOnOneInterface(t *testing.T) {
	dir := t.TempDir()
	path := testNetworkPath(1, "10.0.0.1")
	jobs := make([]FileTransferJob, 6)
	for i := range jobs {
		jobs[i] = testScheduledFileJob(dir, fmt.Sprintf("slot-%d", i), int64(100-i))
	}
	var active atomic.Int32
	var maxActive atomic.Int32
	var entered atomic.Int32
	release := make(chan struct{})
	download := func(ctx context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			prior := maxActive.Load()
			if current <= prior || maxActive.CompareAndSwap(prior, current) {
				break
			}
		}
		if entered.Add(1) == 3 {
			close(release)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return FileDownloadResult{}, ctx.Err()
		case <-time.After(time.Second):
			return FileDownloadResult{}, errors.New("single-interface slots did not overlap")
		}
		return successfulScheduledDownload(request), nil
	}
	report, err := scheduleFileDownloads(context.Background(), []NetworkPath{path}, jobs, download,
		WithFileScheduleRetries(0), WithFileScheduleWorkersPerInterface(3))
	if err != nil {
		t.Fatal(err)
	}
	if report.SucceededCount() != len(jobs) || maxActive.Load() != 3 {
		t.Fatalf("unexpected single-interface connection concurrency: max=%d report=%#v", maxActive.Load(), report)
	}
}

func TestScheduleFileDownloadsSuccessHookRunsImmediatelyAndHookFailureStopsPending(t *testing.T) {
	dir := t.TempDir()
	paths := []NetworkPath{testNetworkPath(1, "10.0.0.1")}
	jobs := []FileTransferJob{
		testScheduledFileJob(dir, "first", 30),
		testScheduledFileJob(dir, "second", 20),
		testScheduledFileJob(dir, "third", 10),
	}
	var downloaded []string
	download := func(_ context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		downloaded = append(downloaded, filepath.Base(request.DestinationPath))
		return successfulScheduledDownload(request), nil
	}
	hookErr := errors.New("session fsync failed")
	var hooked []string
	report, err := scheduleFileDownloads(
		context.Background(), paths, jobs, download,
		WithFileScheduleRetries(0),
		WithFileScheduleSuccessHook(func(result FileScheduleResult) error {
			hooked = append(hooked, result.JobID)
			if result.JobID == "first" {
				return hookErr
			}
			return nil
		}),
	)
	if !errors.Is(err, ErrFileScheduleIncomplete) {
		t.Fatalf("expected schedule failure, got %v", err)
	}
	if len(downloaded) != 1 || downloaded[0] != "first.bin" {
		t.Fatalf("new jobs continued after durable hook failure: %v", downloaded)
	}
	if len(hooked) != 1 || hooked[0] != "first" {
		t.Fatalf("unexpected hook calls: %v", hooked)
	}
	if len(report.Results) != 3 || !errors.Is(report.Results[0].Err, hookErr) {
		t.Fatalf("hook failure was not attached to first job: %#v", report.Results)
	}
	if !errors.Is(report.Results[1].Err, context.Canceled) || !errors.Is(report.Results[2].Err, context.Canceled) {
		t.Fatalf("pending jobs were not cancelled after hook failure: %#v", report.Results)
	}
}

func TestScheduleFileDownloadsUsesPerJobCDNPaths(t *testing.T) {
	dir := t.TempDir()
	paths := []NetworkPath{
		testNetworkPath(1, "10.0.0.1"),
		testNetworkPath(2, "10.0.0.2"),
		testNetworkPath(3, "10.0.0.3"),
	}
	job := testScheduledFileJob(dir, "cdn-specific", 10)
	job.NetworkPaths = []NetworkPath{testNetworkPath(2, "10.9.0.2")}
	failure := errors.New("first attempt failed")
	var attempts []NetworkPath
	download := func(_ context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		attempts = append(attempts, cloneNetworkPath(request.NetworkPath))
		if len(attempts) == 1 {
			return FileDownloadResult{NetworkPath: request.NetworkPath}, failure
		}
		return successfulScheduledDownload(request), nil
	}

	report, err := scheduleFileDownloads(
		context.Background(),
		paths,
		[]FileTransferJob{job},
		download,
		WithFileScheduleRetries(1),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("expected two attempts, got %v", attempts)
	}
	for _, path := range attempts {
		if path.InterfaceIndex != 2 || path.LocalIP.String() != "10.9.0.2" {
			t.Fatalf("scheduler ignored per-file CDN path: %#v", path)
		}
	}
	if len(report.Results[0].Attempts) != 2 || report.Results[0].Attempts[0].NetworkPath.InterfaceIndex != 2 {
		t.Fatalf("unexpected per-file path report: %#v", report.Results[0])
	}
}

func TestScheduleFileDownloadsRetriesTransientServerStatusWithoutCoolingInterface(t *testing.T) {
	dir := t.TempDir()
	path := testNetworkPath(1, "10.0.0.1")
	health := NewDefaultNetworkHealthTracker()
	calls := 0
	report, err := scheduleFileDownloads(context.Background(), []NetworkPath{path}, []FileTransferJob{testScheduledFileJob(dir, "transient", 10)}, func(_ context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		calls++
		if calls == 1 {
			return FileDownloadResult{NetworkPath: request.NetworkPath}, transientDownloadStatusError(http.StatusServiceUnavailable)
		}
		return successfulScheduledDownload(request), nil
	}, WithFileScheduleRetries(1), WithFileScheduleHealthTracker(health))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || report.SucceededCount() != 1 {
		t.Fatalf("transient status was not retried: calls=%d report=%#v", calls, report)
	}
	snapshot := health.Snapshot(path)
	if snapshot.Failures != 0 || snapshot.Successes != 1 {
		t.Fatalf("server transient status polluted interface health: %#v", snapshot)
	}
}

func TestScheduleFileDownloadsRetriesOnDifferentInterface(t *testing.T) {
	dir := t.TempDir()
	paths := []NetworkPath{
		testNetworkPath(1, "10.0.0.1"),
		testNetworkPath(2, "10.0.0.2"),
	}
	job := testScheduledFileJob(dir, "retry", 10)
	job.Header = http.Header{"X-Test": []string{"original"}}

	var mu sync.Mutex
	var interfaces []int
	calls := 0
	download := func(_ context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		mu.Lock()
		calls++
		call := calls
		interfaces = append(interfaces, request.NetworkPath.InterfaceIndex)
		mu.Unlock()
		if got := request.Header.Get("X-Test"); got != "original" {
			return FileDownloadResult{NetworkPath: request.NetworkPath}, fmt.Errorf("retry request header was mutated: %q", got)
		}
		request.Header.Set("X-Test", "mutated")
		if call == 1 {
			return FileDownloadResult{NetworkPath: request.NetworkPath}, errors.New("first interface failed")
		}
		return successfulScheduledDownload(request), nil
	}

	report, err := scheduleFileDownloads(context.Background(), paths, []FileTransferJob{job}, download, WithFileScheduleRetries(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || len(report.Results[0].Attempts) != 2 {
		t.Fatalf("unexpected retry report: %#v", report)
	}
	if len(interfaces) != 2 || interfaces[0] == interfaces[1] {
		t.Fatalf("expected retry to switch interfaces, got %v", interfaces)
	}
	if report.Results[0].Attempts[0].Attempt != 1 || report.Results[0].Attempts[1].Attempt != 2 {
		t.Fatalf("unexpected attempt numbering: %#v", report.Results[0].Attempts)
	}
	if job.Header.Get("X-Test") != "original" {
		t.Fatalf("caller job header was mutated: %#v", job.Header)
	}
}

func TestScheduleFileDownloadsExhaustsRetriesAndAlternatesInterfaces(t *testing.T) {
	dir := t.TempDir()
	paths := []NetworkPath{
		testNetworkPath(1, "10.0.0.1"),
		testNetworkPath(2, "10.0.0.2"),
	}
	failure := errors.New("transfer failed")
	var interfaces []int
	download := func(_ context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		interfaces = append(interfaces, request.NetworkPath.InterfaceIndex)
		return FileDownloadResult{NetworkPath: request.NetworkPath}, failure
	}

	report, err := scheduleFileDownloads(
		context.Background(),
		paths,
		[]FileTransferJob{testScheduledFileJob(dir, "fail", 10)},
		download,
		WithFileScheduleRetries(2),
	)
	if !errors.Is(err, ErrFileScheduleIncomplete) {
		t.Fatalf("expected incomplete schedule error, got %v", err)
	}
	if report.FailedCount() != 1 || report.SucceededCount() != 0 {
		t.Fatalf("unexpected report counts: %#v", report)
	}
	if len(report.Results[0].Attempts) != 3 {
		t.Fatalf("expected three total attempts, got %#v", report.Results[0].Attempts)
	}
	if !errors.Is(report.Results[0].Err, failure) {
		t.Fatalf("expected terminal transfer error, got %v", report.Results[0].Err)
	}
	if len(interfaces) != 3 || interfaces[0] == interfaces[1] || interfaces[1] == interfaces[2] {
		t.Fatalf("expected alternating interfaces, got %v", interfaces)
	}
}

func TestScheduleFileDownloadsWaitsForCooldownAndRecoversSinglePath(t *testing.T) {
	dir := t.TempDir()
	path := testNetworkPath(1, "10.0.0.1")
	health, err := NewNetworkHealthTracker(NetworkHealthOptions{Cooldown: 15 * time.Millisecond, CooldownMax: 15 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	download := func(_ context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		calls++
		if calls == 1 {
			return FileDownloadResult{NetworkPath: request.NetworkPath}, fmt.Errorf("%w: reset", ErrNetworkPathFailure)
		}
		return successfulScheduledDownload(request), nil
	}
	started := time.Now()
	report, err := scheduleFileDownloads(context.Background(), []NetworkPath{path}, []FileTransferJob{testScheduledFileJob(dir, "recover", 10)}, download,
		WithFileScheduleRetries(1), WithFileScheduleHealthTracker(health))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || report.SucceededCount() != 1 {
		t.Fatalf("unexpected recovery result: calls=%d report=%#v", calls, report)
	}
	if time.Since(started) < 10*time.Millisecond {
		t.Fatal("scheduler retried before cooldown elapsed")
	}
	snapshot := health.Snapshot(path)
	if snapshot.Failures != 1 || snapshot.Successes != 1 || snapshot.ConsecutiveFailures != 0 || snapshot.InCooldown || snapshot.Score != 85 {
		t.Fatalf("unexpected recovered health: %#v", snapshot)
	}
}

func TestScheduleFileDownloadsPrefersHigherHealthScore(t *testing.T) {
	dir := t.TempDir()
	path1 := testNetworkPath(1, "10.0.0.1")
	path2 := testNetworkPath(2, "10.0.0.2")
	health := NewDefaultNetworkHealthTracker()
	health.RecordFailure(path1)
	health.RecordSuccess(path1)
	var used int
	download := func(_ context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		used = request.NetworkPath.InterfaceIndex
		return successfulScheduledDownload(request), nil
	}
	_, err := scheduleFileDownloads(context.Background(), []NetworkPath{path1, path2}, []FileTransferJob{testScheduledFileJob(dir, "score", 10)}, download,
		WithFileScheduleRetries(0), WithFileScheduleHealthTracker(health))
	if err != nil {
		t.Fatal(err)
	}
	if used != path2.InterfaceIndex {
		t.Fatalf("expected healthier interface %d, used %d", path2.InterfaceIndex, used)
	}
}

func TestScheduleFileDownloadsDeterministicFailureDoesNotCooldownPath(t *testing.T) {
	dir := t.TempDir()
	path := testNetworkPath(1, "10.0.0.1")
	health := NewDefaultNetworkHealthTracker()
	_, err := scheduleFileDownloads(context.Background(), []NetworkPath{path}, []FileTransferJob{testScheduledFileJob(dir, "http", 10)},
		func(_ context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
			return FileDownloadResult{NetworkPath: request.NetworkPath, StatusCode: http.StatusForbidden}, fmt.Errorf("%w: 403", ErrUnexpectedDownloadStatus)
		}, WithFileScheduleRetries(0), WithFileScheduleHealthTracker(health))
	if !errors.Is(err, ErrFileScheduleIncomplete) {
		t.Fatalf("expected schedule failure, got %v", err)
	}
	snapshot := health.Snapshot(path)
	if snapshot.Failures != 0 || snapshot.Score != 100 || snapshot.InCooldown {
		t.Fatalf("deterministic HTTP failure changed NIC health: %#v", snapshot)
	}
}

func TestScheduleFileDownloadsCancellationInterruptsCooldownWait(t *testing.T) {
	dir := t.TempDir()
	path := testNetworkPath(1, "10.0.0.1")
	health, err := NewNetworkHealthTracker(NetworkHealthOptions{Cooldown: time.Second, CooldownMax: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, scheduleErr := scheduleFileDownloads(ctx, []NetworkPath{path}, []FileTransferJob{testScheduledFileJob(dir, "cancel-cooldown", 10)},
			func(_ context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
				return FileDownloadResult{NetworkPath: request.NetworkPath}, fmt.Errorf("%w: reset", ErrNetworkPathFailure)
			}, WithFileScheduleRetries(1), WithFileScheduleHealthTracker(health))
		finished <- scheduleErr
	}()
	deadline := time.Now().Add(time.Second)
	for !health.Snapshot(path).InCooldown && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !health.Snapshot(path).InCooldown {
		t.Fatal("scheduler never entered cooldown")
	}
	cancel()
	select {
	case scheduleErr := <-finished:
		if !errors.Is(scheduleErr, context.Canceled) || !errors.Is(scheduleErr, ErrFileScheduleIncomplete) {
			t.Fatalf("unexpected cancellation error: %v", scheduleErr)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("cooldown wait did not stop after context cancellation")
	}
}

func TestScheduleFileDownloadsCancellationStopsNewDispatches(t *testing.T) {
	dir := t.TempDir()
	paths := []NetworkPath{
		testNetworkPath(1, "10.0.0.1"),
		testNetworkPath(2, "10.0.0.2"),
	}
	jobs := make([]FileTransferJob, 5)
	for i := range jobs {
		jobs[i] = testScheduledFileJob(dir, fmt.Sprintf("job-%d", i), int64(100-i))
	}

	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, len(paths))
	download := func(ctx context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		started <- struct{}{}
		<-ctx.Done()
		return FileDownloadResult{NetworkPath: request.NetworkPath}, ctx.Err()
	}

	type outcome struct {
		report FileScheduleReport
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		report, err := scheduleFileDownloads(ctx, paths, jobs, download)
		finished <- outcome{report: report, err: err}
	}()

	for range paths {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("initial interface workers did not start")
		}
	}
	cancel()

	var result outcome
	select {
	case result = <-finished:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop after cancellation")
	}
	if !errors.Is(result.err, context.Canceled) || !errors.Is(result.err, ErrFileScheduleIncomplete) {
		t.Fatalf("unexpected cancellation error: %v", result.err)
	}
	if len(result.report.Results) != len(jobs) || result.report.FailedCount() != len(jobs) {
		t.Fatalf("unexpected cancellation report: %#v", result.report)
	}
	attempts := 0
	for _, fileResult := range result.report.Results {
		attempts += len(fileResult.Attempts)
		if !errors.Is(fileResult.Err, context.Canceled) {
			t.Fatalf("job %q did not retain cancellation: %v", fileResult.JobID, fileResult.Err)
		}
	}
	if attempts != len(paths) {
		t.Fatalf("expected only %d in-flight attempts, got %d", len(paths), attempts)
	}
}

func TestValidateFileScheduleRejectsUnsafeInputsBeforeDownloading(t *testing.T) {
	dir := t.TempDir()
	path1 := testNetworkPath(1, "10.0.0.1")
	path1Alt := testNetworkPath(1, "10.0.0.9")
	base := testScheduledFileJob(dir, "one", 10)
	other := testScheduledFileJob(dir, "two", 20)

	tests := []struct {
		name  string
		paths []NetworkPath
		jobs  []FileTransferJob
		opts  []FileSchedulerOption
	}{
		{name: "no paths", jobs: []FileTransferJob{base}},
		{name: "duplicate physical interface", paths: []NetworkPath{path1, path1Alt}, jobs: []FileTransferJob{base}},
		{name: "duplicate job id", paths: []NetworkPath{path1}, jobs: []FileTransferJob{base, func() FileTransferJob { j := other; j.ID = base.ID; return j }()}},
		{name: "duplicate destination", paths: []NetworkPath{path1}, jobs: []FileTransferJob{base, func() FileTransferJob { j := other; j.DestinationPath = filepath.Join(dir, ".", "one.bin"); return j }()}},
		{name: "invalid retries", paths: []NetworkPath{path1}, jobs: []FileTransferJob{base}, opts: []FileSchedulerOption{WithFileScheduleRetries(-1)}},
		{name: "known oversize", paths: []NetworkPath{path1}, jobs: []FileTransferJob{func() FileTransferJob { j := base; j.MaxBytes = 9; return j }()}},
		{name: "explicit no CDN paths", paths: []NetworkPath{path1}, jobs: []FileTransferJob{func() FileTransferJob { j := base; j.NetworkPaths = []NetworkPath{}; return j }()}},
		{name: "CDN path without worker", paths: []NetworkPath{path1}, jobs: []FileTransferJob{func() FileTransferJob {
			j := base
			j.NetworkPaths = []NetworkPath{testNetworkPath(3, "10.0.0.3")}
			return j
		}()}},
		{name: "duplicate CDN interface paths", paths: []NetworkPath{path1}, jobs: []FileTransferJob{func() FileTransferJob { j := base; j.NetworkPaths = []NetworkPath{path1, path1Alt}; return j }()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var called atomic.Bool
			_, err := scheduleFileDownloads(context.Background(), tt.paths, tt.jobs, func(context.Context, FileDownloadRequest) (FileDownloadResult, error) {
				called.Store(true)
				return FileDownloadResult{}, nil
			}, tt.opts...)
			if err == nil {
				t.Fatal("expected invalid schedule to fail")
			}
			if called.Load() {
				t.Fatal("download started before schedule validation completed")
			}
		})
	}
}

func TestValidateFileScheduleSanitizesInvalidSignedURL(t *testing.T) {
	dir := t.TempDir()
	job := testScheduledFileJob(dir, "bad-url", 10)
	job.URL = "https://cdn.example.invalid/%zz?token=super-secret"
	_, err := scheduleFileDownloads(
		context.Background(),
		[]NetworkPath{testNetworkPath(1, "10.0.0.1")},
		[]FileTransferJob{job},
		func(context.Context, FileDownloadRequest) (FileDownloadResult, error) {
			return FileDownloadResult{}, nil
		},
	)
	if err == nil {
		t.Fatal("expected invalid signed URL to fail")
	}
	if stringsContainsAny(err.Error(), "super-secret", "cdn.example.invalid/%zz") {
		t.Fatalf("signed URL leaked through validation error: %v", err)
	}
}

func TestScheduleFileDownloadsAllowsEmptyJobSetWithoutNetworkPaths(t *testing.T) {
	report, err := scheduleFileDownloads(context.Background(), nil, nil, func(context.Context, FileDownloadRequest) (FileDownloadResult, error) {
		return FileDownloadResult{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 0 || report.FailedCount() != 0 {
		t.Fatalf("unexpected empty report: %#v", report)
	}
}

func testScheduledFileJob(dir, id string, size int64) FileTransferJob {
	return FileTransferJob{
		ID:              id,
		URL:             "https://cdn.example.invalid/" + id + "?token=secret",
		DestinationPath: filepath.Join(dir, id+".bin"),
		ExpectedSize:    size,
	}
}

func successfulScheduledDownload(request FileDownloadRequest) FileDownloadResult {
	return FileDownloadResult{
		NetworkPath:     request.NetworkPath,
		DestinationPath: request.DestinationPath,
		BytesWritten:    maxInt64(request.ExpectedSize, 0),
		StatusCode:      http.StatusOK,
		FinalHost:       "cdn.example.invalid",
	}
}

func maxInt64(value, floor int64) int64 {
	if value < floor {
		return floor
	}
	return value
}

func stringsContainsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
