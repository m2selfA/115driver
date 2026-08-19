package transfer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const DefaultFileScheduleRetries = 3

var ErrFileScheduleIncomplete = errors.New("file download schedule incomplete")

// FileTransferJob describes one whole-file download before a network path has
// been assigned. ID must be unique within one schedule. ExpectedSize,
// MaxBytes, and Timeout use the same semantics as FileDownloadRequest.
//
// NetworkPaths optionally restricts this file to the concrete paths selected by
// P2 for its CDN URL. A nil slice means every scheduler interface is eligible.
// A non-nil empty slice means P2 found no reachable path and is rejected.
type FileTransferJob struct {
	ID              string
	URL             string
	Header          http.Header
	DestinationPath string
	NetworkPaths    []NetworkPath
	ExpectedSize    int64
	MaxBytes        int64
	Timeout         time.Duration
	ResumeKey       string
	Refresh         DownloadSourceRefreshFunc
	MaxRefreshes    int
}

// FileSchedulerOptions configures coarse-grained whole-file scheduling.
// Retries is the number of retries after the first attempt. LargestFirst sorts
// known-size files from largest to smallest before the initial dispatch; files
// with unknown size retain their input order after all known-size files.
type FileSchedulerOptions struct {
	Retries       int
	LargestFirst  bool
	HealthTracker *NetworkHealthTracker
}

// DefaultFileSchedulerOptions returns the defaults for the stable coarse-grain
// strategy: one worker per physical interface, three retries, and large files first.
func DefaultFileSchedulerOptions() FileSchedulerOptions {
	return FileSchedulerOptions{
		Retries:      DefaultFileScheduleRetries,
		LargestFirst: true,
	}
}

// FileSchedulerOption customizes whole-file scheduling.
type FileSchedulerOption func(*FileSchedulerOptions)

// WithFileScheduleRetries sets how many retries are allowed after the initial
// attempt. A value of zero means every file is attempted at most once.
func WithFileScheduleRetries(retries int) FileSchedulerOption {
	return func(options *FileSchedulerOptions) {
		options.Retries = retries
	}
}

// WithFileScheduleLargestFirst controls size-aware initial queue ordering.
func WithFileScheduleLargestFirst(enabled bool) FileSchedulerOption {
	return func(options *FileSchedulerOptions) {
		options.LargestFirst = enabled
	}
}

// WithFileScheduleHealthTracker shares interface health and cooldown state with
// the scheduler. A nil tracker preserves the pre-P8 scheduling behavior.
func WithFileScheduleHealthTracker(tracker *NetworkHealthTracker) FileSchedulerOption {
	return func(options *FileSchedulerOptions) {
		options.HealthTracker = tracker
	}
}

// FileScheduleAttempt records one complete-file attempt on one network path.
type FileScheduleAttempt struct {
	Attempt     int
	NetworkPath NetworkPath
	Result      FileDownloadResult
	StartedAt   time.Time
	FinishedAt  time.Time
	Err         error
}

// FileScheduleResult records the terminal state for one input job. Results are
// returned in the same order as the input jobs even when dispatch order differs.
// The signed source URL and request headers are intentionally not retained here.
type FileScheduleResult struct {
	JobID           string
	DestinationPath string
	ExpectedSize    int64
	Attempts        []FileScheduleAttempt
	Result          FileDownloadResult
	Err             error
}

// FileScheduleReport contains all job results for one scheduling run.
type FileScheduleReport struct {
	Results   []FileScheduleResult
	StartedAt time.Time
	Duration  time.Duration
}

// SucceededCount returns the number of jobs that completed successfully.
func (report FileScheduleReport) SucceededCount() int {
	count := 0
	for _, result := range report.Results {
		if result.Err == nil {
			count++
		}
	}
	return count
}

// FailedCount returns the number of jobs that did not complete successfully.
func (report FileScheduleReport) FailedCount() int {
	return len(report.Results) - report.SucceededCount()
}

type fileDownloadFunc func(context.Context, FileDownloadRequest) (FileDownloadResult, error)

type scheduledFileState struct {
	job      FileTransferJob
	source   *downloadSourceState
	attempts []FileScheduleAttempt
	result   FileDownloadResult
	err      error
	done     bool
}

type scheduledWorkerTask struct {
	jobIndex int
	attempt  int
	job      FileTransferJob
	path     NetworkPath
	source   *downloadSourceState
}

type scheduledWorkerResult struct {
	jobIndex    int
	workerIndex int
	attempt     FileScheduleAttempt
}

// ScheduleFileDownloads downloads whole files across the supplied network
// paths. Exactly one worker is created per physical interface path. A failed
// file is requeued and, when more than one healthy interface is available, its
// next attempt is assigned to a different interface. When a P8 health tracker
// is supplied, cooling interfaces are skipped and automatically retried after
// their cooldown expires.
func ScheduleFileDownloads(
	ctx context.Context,
	paths []NetworkPath,
	jobs []FileTransferJob,
	opts ...FileSchedulerOption,
) (FileScheduleReport, error) {
	return scheduleFileDownloads(ctx, paths, jobs, DownloadFile, opts...)
}

func scheduleFileDownloads(
	ctx context.Context,
	paths []NetworkPath,
	jobs []FileTransferJob,
	download fileDownloadFunc,
	opts ...FileSchedulerOption,
) (FileScheduleReport, error) {
	started := time.Now()
	report := FileScheduleReport{StartedAt: started}
	if ctx == nil {
		ctx = context.Background()
	}
	if download == nil {
		return report, errors.New("file scheduler download function is nil")
	}

	options := DefaultFileSchedulerOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if err := options.validate(); err != nil {
		return report, err
	}
	if len(jobs) == 0 {
		report.Duration = time.Since(started)
		return report, nil
	}
	if err := validateFileSchedule(paths, jobs); err != nil {
		return report, err
	}
	paths = cloneNetworkPaths(paths)

	states := make([]scheduledFileState, len(jobs))
	for i, job := range jobs {
		states[i].job = cloneFileTransferJob(job)
		source, sourceErr := newDownloadSourceState(
			DownloadSource{URL: states[i].job.URL, Header: states[i].job.Header},
			states[i].job.Refresh,
			states[i].job.MaxRefreshes,
		)
		if sourceErr != nil {
			return report, fmt.Errorf("initialize file scheduler job %q source: %w", states[i].job.ID, sourceErr)
		}
		states[i].source = source
	}
	pending := initialFileQueue(states, options.LargestFirst)

	taskChannels := make([]chan scheduledWorkerTask, len(paths))
	attemptResults := make(chan scheduledWorkerResult, len(paths))
	var workers sync.WaitGroup
	workers.Add(len(paths))
	for i := range paths {
		tasks := make(chan scheduledWorkerTask)
		taskChannels[i] = tasks
		go func(workerIndex int, workerTasks <-chan scheduledWorkerTask) {
			defer workers.Done()
			runScheduledFileWorker(ctx, workerIndex, workerTasks, attemptResults, download)
		}(i, tasks)
	}

	idleWorkers := make([]int, len(paths))
	for i := range paths {
		idleWorkers[i] = i
	}
	inFlight := 0
	completed := 0
	cancelled := false

	for completed < len(states) {
		if !cancelled {
			if ctx.Err() != nil {
				cancelled = true
			} else {
				inFlight += dispatchFileSchedule(states, paths, &pending, &idleWorkers, taskChannels, options.HealthTracker)
			}
		}

		if inFlight == 0 {
			if cancelled {
				completed += cancelPendingFileJobs(states, pending, ctx.Err())
				pending = nil
				break
			}
			if len(pending) > 0 {
				waited, waitErr := waitForNetworkHealth(ctx, options.HealthTracker, pendingFileHealthPaths(states, paths, pending))
				if waitErr != nil {
					cancelled = true
					completed += cancelPendingFileJobs(states, pending, waitErr)
					pending = nil
					break
				}
				if waited {
					continue
				}
				for _, jobIndex := range pending {
					if !states[jobIndex].done {
						states[jobIndex].err = errors.New("no eligible network path for pending file")
						states[jobIndex].done = true
						completed++
					}
				}
				pending = nil
				break
			}
		}

		var workerResult scheduledWorkerResult
		if cancelled {
			workerResult = <-attemptResults
		} else {
			select {
			case workerResult = <-attemptResults:
			case <-ctx.Done():
				cancelled = true
				continue
			}
		}

		inFlight--
		idleWorkers = append(idleWorkers, workerResult.workerIndex)
		state := &states[workerResult.jobIndex]
		state.attempts = append(state.attempts, workerResult.attempt)
		state.result = workerResult.attempt.Result

		if workerResult.attempt.Err == nil {
			options.HealthTracker.RecordSuccess(workerResult.attempt.NetworkPath)
			state.done = true
			completed++
			continue
		}
		if shouldPenalizeNetworkPath(ctx, workerResult.attempt.Err) {
			options.HealthTracker.RecordFailure(workerResult.attempt.NetworkPath)
		}
		if cancelled || ctx.Err() != nil {
			cancelled = true
			state.err = ctx.Err()
			if state.err == nil {
				state.err = workerResult.attempt.Err
			}
			state.done = true
			completed++
			continue
		}
		if isTerminalDownloadSourceError(workerResult.attempt.Err) {
			state.err = workerResult.attempt.Err
			state.done = true
			completed++
			continue
		}
		if len(state.attempts) <= options.Retries {
			pending = append(pending, workerResult.jobIndex)
			continue
		}
		state.err = workerResult.attempt.Err
		state.done = true
		completed++
	}

	for _, tasks := range taskChannels {
		close(tasks)
	}
	workers.Wait()

	report.Results = make([]FileScheduleResult, len(states))
	for i, state := range states {
		report.Results[i] = FileScheduleResult{
			JobID:           state.job.ID,
			DestinationPath: state.job.DestinationPath,
			ExpectedSize:    state.job.ExpectedSize,
			Attempts:        append([]FileScheduleAttempt(nil), state.attempts...),
			Result:          state.result,
			Err:             state.err,
		}
	}
	report.Duration = time.Since(started)

	failed := report.FailedCount()
	if failed == 0 {
		return report, nil
	}
	incomplete := fmt.Errorf("%w: %d of %d files failed", ErrFileScheduleIncomplete, failed, len(report.Results))
	if ctx.Err() != nil {
		return report, errors.Join(incomplete, ctx.Err())
	}
	return report, incomplete
}

func (options FileSchedulerOptions) validate() error {
	if options.Retries < 0 {
		return errors.New("file scheduler retries must be >= 0")
	}
	return nil
}

func validateFileSchedule(paths []NetworkPath, jobs []FileTransferJob) error {
	if len(paths) == 0 {
		return errors.New("file scheduler requires at least one network path")
	}
	seenInterfaces := make(map[int]struct{}, len(paths))
	for _, path := range paths {
		if err := path.Validate(); err != nil {
			return fmt.Errorf("invalid file scheduler network path: %w", err)
		}
		if _, exists := seenInterfaces[path.InterfaceIndex]; exists {
			return fmt.Errorf("file scheduler received multiple paths for interface index %d", path.InterfaceIndex)
		}
		seenInterfaces[path.InterfaceIndex] = struct{}{}
	}

	seenIDs := make(map[string]struct{}, len(jobs))
	seenDestinations := make(map[string]string, len(jobs))
	for _, job := range jobs {
		if strings.TrimSpace(job.ID) == "" {
			return errors.New("file scheduler job ID is empty")
		}
		if _, exists := seenIDs[job.ID]; exists {
			return fmt.Errorf("duplicate file scheduler job ID %q", job.ID)
		}
		seenIDs[job.ID] = struct{}{}

		validationPath := paths[0]
		if job.NetworkPaths != nil {
			if len(job.NetworkPaths) == 0 {
				return fmt.Errorf("file scheduler job %q has no eligible network paths", job.ID)
			}
			seenJobInterfaces := make(map[int]struct{}, len(job.NetworkPaths))
			for _, jobPath := range job.NetworkPaths {
				if err := jobPath.Validate(); err != nil {
					return fmt.Errorf("invalid network path for file scheduler job %q: %w", job.ID, err)
				}
				if _, exists := seenInterfaces[jobPath.InterfaceIndex]; !exists {
					return fmt.Errorf("file scheduler job %q references interface index %d without a worker", job.ID, jobPath.InterfaceIndex)
				}
				if _, exists := seenJobInterfaces[jobPath.InterfaceIndex]; exists {
					return fmt.Errorf("file scheduler job %q has multiple paths for interface index %d", job.ID, jobPath.InterfaceIndex)
				}
				seenJobInterfaces[jobPath.InterfaceIndex] = struct{}{}
			}
			validationPath = job.NetworkPaths[0]
		}

		request := job.downloadRequest(validationPath, nil)
		if err := request.validate(); err != nil {
			return fmt.Errorf("invalid file scheduler job %q: %w", job.ID, err)
		}
		if _, err := parseCDNProbeURL(job.URL); err != nil {
			return fmt.Errorf("invalid file scheduler job %q URL: %w", job.ID, err)
		}

		destinationKey, err := canonicalDestinationPath(job.DestinationPath)
		if err != nil {
			return fmt.Errorf("invalid file scheduler job %q destination: %w", job.ID, err)
		}
		if priorID, exists := seenDestinations[destinationKey]; exists {
			return fmt.Errorf("file scheduler jobs %q and %q target the same destination", priorID, job.ID)
		}
		seenDestinations[destinationKey] = job.ID
	}
	return nil
}

func canonicalDestinationPath(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" {
		absolute = strings.ToLower(absolute)
	}
	return absolute, nil
}

func cloneFileTransferJob(job FileTransferJob) FileTransferJob {
	clone := job
	if job.Header != nil {
		clone.Header = job.Header.Clone()
	}
	if job.NetworkPaths != nil {
		clone.NetworkPaths = cloneNetworkPaths(job.NetworkPaths)
	}
	return clone
}

func cloneNetworkPaths(paths []NetworkPath) []NetworkPath {
	clones := make([]NetworkPath, len(paths))
	for i, path := range paths {
		clones[i] = cloneNetworkPath(path)
	}
	return clones
}

func cloneNetworkPath(path NetworkPath) NetworkPath {
	clone := path
	clone.LocalIP = canonicalIP(path.LocalIP)
	return clone
}

func (job FileTransferJob) downloadRequest(path NetworkPath, source *downloadSourceState) FileDownloadRequest {
	headers := job.Header
	if headers != nil {
		headers = headers.Clone()
	}
	return FileDownloadRequest{
		URL:             job.URL,
		Header:          headers,
		DestinationPath: job.DestinationPath,
		NetworkPath:     cloneNetworkPath(path),
		ExpectedSize:    job.ExpectedSize,
		MaxBytes:        job.MaxBytes,
		Timeout:         job.Timeout,
		ResumeKey:       job.ResumeKey,
		Refresh:         job.Refresh,
		MaxRefreshes:    job.MaxRefreshes,
		source:          source,
	}
}

func initialFileQueue(states []scheduledFileState, largestFirst bool) []int {
	pending := make([]int, len(states))
	for i := range states {
		pending[i] = i
	}
	if !largestFirst {
		return pending
	}
	sort.SliceStable(pending, func(i, j int) bool {
		left := states[pending[i]].job.ExpectedSize
		right := states[pending[j]].job.ExpectedSize
		leftKnown := left >= 0
		rightKnown := right >= 0
		if leftKnown != rightKnown {
			return leftKnown
		}
		if !leftKnown {
			return false
		}
		return left > right
	})
	return pending
}

func dispatchFileSchedule(
	states []scheduledFileState,
	paths []NetworkPath,
	pending *[]int,
	idleWorkers *[]int,
	taskChannels []chan scheduledWorkerTask,
	health *NetworkHealthTracker,
) int {
	dispatched := 0
	for len(*pending) > 0 && len(*idleWorkers) > 0 {
		workerPos, jobPos, assignedPath := findEligibleFileDispatch(states, paths, *pending, *idleWorkers, health)
		if workerPos < 0 {
			break
		}
		workerIndex := (*idleWorkers)[workerPos]
		jobIndex := (*pending)[jobPos]
		*idleWorkers = append((*idleWorkers)[:workerPos], (*idleWorkers)[workerPos+1:]...)
		*pending = append((*pending)[:jobPos], (*pending)[jobPos+1:]...)
		taskChannels[workerIndex] <- scheduledWorkerTask{
			jobIndex: jobIndex,
			attempt:  len(states[jobIndex].attempts) + 1,
			job:      states[jobIndex].job,
			path:     assignedPath,
			source:   states[jobIndex].source,
		}
		dispatched++
	}
	return dispatched
}

func findEligibleFileDispatch(states []scheduledFileState, paths []NetworkPath, pending, idleWorkers []int, health *NetworkHealthTracker) (int, int, NetworkPath) {
	bestWorkerPos, bestJobPos, bestScore := -1, -1, -1
	var bestPath NetworkPath
	for workerPos, workerIndex := range idleWorkers {
		workerPath := paths[workerIndex]
		if !health.Available(workerPath) {
			continue
		}
		score := health.Score(workerPath)
		for jobPos, jobIndex := range pending {
			state := states[jobIndex]
			assignedPath, ok := filePathForWorker(state.job, workerPath)
			if !ok || !health.Available(assignedPath) || !canAttemptFileOnPath(state, assignedPath, availableFilePathCount(state.job, paths, health)) {
				continue
			}
			if bestWorkerPos < 0 || score > bestScore {
				bestWorkerPos, bestJobPos, bestScore, bestPath = workerPos, jobPos, score, assignedPath
			}
			break
		}
	}
	return bestWorkerPos, bestJobPos, bestPath
}

func availableFilePathCount(job FileTransferJob, paths []NetworkPath, health *NetworkHealthTracker) int {
	count := 0
	for _, workerPath := range paths {
		assigned, ok := filePathForWorker(job, workerPath)
		if ok && health.Available(assigned) {
			count++
		}
	}
	return count
}

func pendingFileHealthPaths(states []scheduledFileState, paths []NetworkPath, pending []int) []NetworkPath {
	seen := make(map[int]struct{})
	result := make([]NetworkPath, 0, len(paths))
	for _, jobIndex := range pending {
		for _, workerPath := range paths {
			assigned, ok := filePathForWorker(states[jobIndex].job, workerPath)
			if !ok {
				continue
			}
			if _, exists := seen[assigned.InterfaceIndex]; exists {
				continue
			}
			seen[assigned.InterfaceIndex] = struct{}{}
			result = append(result, assigned)
		}
	}
	return result
}

func filePathForWorker(job FileTransferJob, workerPath NetworkPath) (NetworkPath, bool) {
	if job.NetworkPaths == nil {
		return workerPath, true
	}
	for _, path := range job.NetworkPaths {
		if path.InterfaceIndex == workerPath.InterfaceIndex {
			return path, true
		}
	}
	return NetworkPath{}, false
}

func canAttemptFileOnPath(state scheduledFileState, path NetworkPath, pathCount int) bool {
	if len(state.attempts) == 0 || pathCount <= 1 {
		return true
	}
	last := state.attempts[len(state.attempts)-1].NetworkPath
	return last.InterfaceIndex != path.InterfaceIndex
}

func runScheduledFileWorker(
	ctx context.Context,
	workerIndex int,
	tasks <-chan scheduledWorkerTask,
	results chan<- scheduledWorkerResult,
	download fileDownloadFunc,
) {
	for task := range tasks {
		started := time.Now()
		result, err := download(ctx, task.job.downloadRequest(task.path, task.source))
		results <- scheduledWorkerResult{
			jobIndex:    task.jobIndex,
			workerIndex: workerIndex,
			attempt: FileScheduleAttempt{
				Attempt:     task.attempt,
				NetworkPath: task.path,
				Result:      result,
				StartedAt:   started,
				FinishedAt:  time.Now(),
				Err:         err,
			},
		}
	}
}

func cancelPendingFileJobs(states []scheduledFileState, pending []int, cause error) int {
	if cause == nil {
		cause = context.Canceled
	}
	count := 0
	for _, jobIndex := range pending {
		if states[jobIndex].done {
			continue
		}
		states[jobIndex].err = cause
		states[jobIndex].done = true
		count++
	}
	return count
}
