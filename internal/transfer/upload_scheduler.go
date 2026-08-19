package transfer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"
)

const DefaultUploadPartRetries = 3

var ErrUploadPartScheduleIncomplete = errors.New("upload part schedule incomplete")

// UploadPartJob describes one byte range of a multipart upload. PartNumber must
// be in OSS-compatible range 1..10000; Offset and Size describe the local file
// range supplied to the upload callback.
type UploadPartJob struct {
	PartNumber int
	Offset     int64
	Size       int64
}

// UploadPartResult is transport-neutral metadata returned after one part has
// been accepted by the remote object store.
type UploadPartResult struct {
	PartNumber    int
	ETag          string
	BytesUploaded int64
}

// UploadPartFunc uploads one part through the concrete network path selected by
// the scheduler. Implementations may use any object-store client as long as the
// same logical multipart upload ID is shared between workers.
type UploadPartFunc func(context.Context, NetworkPath, UploadPartJob) (UploadPartResult, error)

type UploadPartSchedulerOptions struct {
	Retries       int
	HealthTracker *NetworkHealthTracker
}

type UploadPartSchedulerOption func(*UploadPartSchedulerOptions)

func DefaultUploadPartSchedulerOptions() UploadPartSchedulerOptions {
	return UploadPartSchedulerOptions{Retries: DefaultUploadPartRetries}
}

func WithUploadPartRetries(retries int) UploadPartSchedulerOption {
	return func(options *UploadPartSchedulerOptions) { options.Retries = retries }
}

func WithUploadPartHealthTracker(tracker *NetworkHealthTracker) UploadPartSchedulerOption {
	return func(options *UploadPartSchedulerOptions) { options.HealthTracker = tracker }
}

type UploadPartAttempt struct {
	Attempt     int
	NetworkPath NetworkPath
	Result      UploadPartResult
	StartedAt   time.Time
	FinishedAt  time.Time
	Err         error
}

type UploadPartScheduleResult struct {
	Job      UploadPartJob
	Attempts []UploadPartAttempt
	Result   UploadPartResult
	Err      error
}

type UploadPartScheduleReport struct {
	Results   []UploadPartScheduleResult
	StartedAt time.Time
	Duration  time.Duration
}

func (report UploadPartScheduleReport) SucceededCount() int {
	count := 0
	for _, result := range report.Results {
		if result.Err == nil {
			count++
		}
	}
	return count
}

func (report UploadPartScheduleReport) FailedCount() int {
	return len(report.Results) - report.SucceededCount()
}

type uploadPartState struct {
	job           UploadPartJob
	attempts      []UploadPartAttempt
	lastInterface int
	result        UploadPartResult
	err           error
	done          bool
}

type uploadPartTask struct {
	stateIndex int
	attempt    int
	job        UploadPartJob
}

type uploadPartWorkerResult struct {
	stateIndex  int
	workerIndex int
	attempt     UploadPartAttempt
}

// ScheduleUploadParts fans one multipart upload across physical interfaces.
// Each supplied interface owns exactly one worker. Parts are drawn from a
// shared queue, so faster paths naturally consume more work; a failed part is
// retried on another healthy interface when possible. Only network-classified
// failures affect P8 health/cooldown.
func ScheduleUploadParts(
	ctx context.Context,
	paths []NetworkPath,
	jobs []UploadPartJob,
	upload UploadPartFunc,
	opts ...UploadPartSchedulerOption,
) (UploadPartScheduleReport, error) {
	started := time.Now()
	report := UploadPartScheduleReport{StartedAt: started}
	if ctx == nil {
		ctx = context.Background()
	}
	options := DefaultUploadPartSchedulerOptions()
	for _, opt := range opts {
		if opt != nil {
			opt(&options)
		}
	}
	if err := validateUploadPartSchedule(paths, jobs, upload, options); err != nil {
		return report, err
	}
	if len(jobs) == 0 {
		report.Duration = time.Since(started)
		return report, nil
	}
	if options.HealthTracker == nil {
		options.HealthTracker = NewDefaultNetworkHealthTracker()
	}

	states := make([]uploadPartState, len(jobs))
	pending := make([]int, len(jobs))
	for i, job := range jobs {
		states[i] = uploadPartState{job: job, lastInterface: -1}
		pending[i] = i
	}

	taskChannels := make([]chan uploadPartTask, len(paths))
	attemptResults := make(chan uploadPartWorkerResult, len(paths))
	var workers sync.WaitGroup
	workers.Add(len(paths))
	for workerIndex, path := range paths {
		tasks := make(chan uploadPartTask)
		taskChannels[workerIndex] = tasks
		go func(workerIndex int, workerPath NetworkPath, workerTasks <-chan uploadPartTask) {
			defer workers.Done()
			for task := range workerTasks {
				attempt := UploadPartAttempt{
					Attempt: task.attempt, NetworkPath: cloneNetworkPath(workerPath), StartedAt: time.Now(),
				}
				attempt.Result, attempt.Err = upload(ctx, cloneNetworkPath(workerPath), task.job)
				attempt.Err = markLikelyNetworkPathFailure(attempt.Err)
				attempt.FinishedAt = time.Now()
				attemptResults <- uploadPartWorkerResult{stateIndex: task.stateIndex, workerIndex: workerIndex, attempt: attempt}
			}
		}(workerIndex, path, tasks)
	}
	idleWorkers := make([]int, len(paths))
	for i := range idleWorkers {
		idleWorkers[i] = i
	}
	inFlight := 0
	finished := 0

	for finished < len(states) {
		if ctx.Err() == nil {
			inFlight += dispatchUploadPartTasks(states, paths, &pending, &idleWorkers, taskChannels, options.HealthTracker)
		} else if len(pending) > 0 {
			for _, stateIndex := range pending {
				state := &states[stateIndex]
				if !state.done {
					state.done = true
					state.err = ctx.Err()
					finished++
				}
			}
			pending = nil
		}

		if inFlight == 0 {
			if finished == len(states) {
				break
			}
			if len(pending) == 0 {
				break
			}
			waited, waitErr := waitForNetworkHealth(ctx, options.HealthTracker, paths)
			if waitErr != nil {
				continue
			}
			if waited {
				continue
			}
			// No cooldown explains the lack of dispatch. This should only be
			// reachable for inconsistent scheduler state; fail deterministically.
			for _, stateIndex := range pending {
				state := &states[stateIndex]
				if !state.done {
					state.done = true
					state.err = errors.New("no eligible network path for upload part")
					finished++
				}
			}
			pending = nil
			continue
		}

		var workerResult uploadPartWorkerResult
		if ctx.Err() != nil {
			workerResult = <-attemptResults
		} else {
			select {
			case workerResult = <-attemptResults:
			case <-ctx.Done():
				continue
			}
		}
		inFlight--
		idleWorkers = append(idleWorkers, workerResult.workerIndex)
		state := &states[workerResult.stateIndex]
		state.attempts = append(state.attempts, workerResult.attempt)
		state.lastInterface = workerResult.attempt.NetworkPath.InterfaceIndex

		if workerResult.attempt.Err == nil {
			options.HealthTracker.RecordSuccess(workerResult.attempt.NetworkPath)
			state.done = true
			state.result = workerResult.attempt.Result
			state.result.PartNumber = state.job.PartNumber
			if state.result.BytesUploaded == 0 {
				state.result.BytesUploaded = state.job.Size
			}
			finished++
			continue
		}
		if shouldPenalizeNetworkPath(ctx, workerResult.attempt.Err) {
			options.HealthTracker.RecordFailure(workerResult.attempt.NetworkPath)
		}
		if ctx.Err() != nil {
			state.done = true
			state.err = errors.Join(workerResult.attempt.Err, ctx.Err())
			finished++
			continue
		}
		if len(state.attempts) <= options.Retries {
			pending = append(pending, workerResult.stateIndex)
			continue
		}
		state.done = true
		state.err = fmt.Errorf("upload part %d failed after %d attempt(s): %w", state.job.PartNumber, len(state.attempts), workerResult.attempt.Err)
		finished++
	}

	// Drain in-flight work after cancellation before closing worker channels.
	for inFlight > 0 {
		workerResult := <-attemptResults
		inFlight--
		state := &states[workerResult.stateIndex]
		state.attempts = append(state.attempts, workerResult.attempt)
		if !state.done {
			state.done = true
			state.err = errors.Join(workerResult.attempt.Err, ctx.Err())
			finished++
		}
	}
	for _, tasks := range taskChannels {
		close(tasks)
	}
	workers.Wait()

	report.Results = make([]UploadPartScheduleResult, len(states))
	for i, state := range states {
		report.Results[i] = UploadPartScheduleResult{
			Job: state.job, Attempts: append([]UploadPartAttempt(nil), state.attempts...), Result: state.result, Err: state.err,
		}
	}
	report.Duration = time.Since(started)
	if failed := report.FailedCount(); failed > 0 {
		incomplete := fmt.Errorf("%w: %d of %d parts failed", ErrUploadPartScheduleIncomplete, failed, len(states))
		if ctx.Err() != nil {
			return report, errors.Join(incomplete, ctx.Err())
		}
		return report, incomplete
	}
	return report, nil
}

func validateUploadPartSchedule(paths []NetworkPath, jobs []UploadPartJob, upload UploadPartFunc, options UploadPartSchedulerOptions) error {
	if options.Retries < 0 {
		return errors.New("upload part retries must be >= 0")
	}
	if len(jobs) == 0 {
		return nil
	}
	if upload == nil {
		return errors.New("upload part callback is nil")
	}
	if len(paths) == 0 {
		return errors.New("upload part scheduler requires at least one network path")
	}
	seenInterfaces := make(map[int]struct{}, len(paths))
	for _, path := range paths {
		if err := path.Validate(); err != nil {
			return fmt.Errorf("invalid upload network path: %w", err)
		}
		if _, exists := seenInterfaces[path.InterfaceIndex]; exists {
			return fmt.Errorf("upload scheduler received multiple paths for interface index %d", path.InterfaceIndex)
		}
		seenInterfaces[path.InterfaceIndex] = struct{}{}
	}
	seenParts := make(map[int]struct{}, len(jobs))
	for _, job := range jobs {
		if job.PartNumber < 1 || job.PartNumber > 10000 {
			return fmt.Errorf("upload part number %d is outside 1..10000", job.PartNumber)
		}
		if _, exists := seenParts[job.PartNumber]; exists {
			return fmt.Errorf("duplicate upload part number %d", job.PartNumber)
		}
		seenParts[job.PartNumber] = struct{}{}
		if job.Offset < 0 || job.Size <= 0 {
			return fmt.Errorf("upload part %d has invalid offset/size %d/%d", job.PartNumber, job.Offset, job.Size)
		}
		if job.Offset > math.MaxInt64-job.Size {
			return fmt.Errorf("upload part %d byte range overflows int64", job.PartNumber)
		}
	}
	return nil
}

func dispatchUploadPartTasks(states []uploadPartState, paths []NetworkPath, pending, idleWorkers *[]int, taskChannels []chan uploadPartTask, health *NetworkHealthTracker) int {
	dispatched := 0
	for len(*pending) > 0 && len(*idleWorkers) > 0 {
		workerPos, jobPos := findEligibleUploadPartDispatch(states, paths, *pending, *idleWorkers, health)
		if workerPos < 0 {
			break
		}
		workerIndex := (*idleWorkers)[workerPos]
		stateIndex := (*pending)[jobPos]
		state := states[stateIndex]
		*idleWorkers = append((*idleWorkers)[:workerPos], (*idleWorkers)[workerPos+1:]...)
		*pending = append((*pending)[:jobPos], (*pending)[jobPos+1:]...)
		taskChannels[workerIndex] <- uploadPartTask{stateIndex: stateIndex, attempt: len(state.attempts) + 1, job: state.job}
		dispatched++
	}
	return dispatched
}

func findEligibleUploadPartDispatch(states []uploadPartState, paths []NetworkPath, pending, idleWorkers []int, health *NetworkHealthTracker) (int, int) {
	availableCount := 0
	for _, path := range paths {
		if health.Available(path) {
			availableCount++
		}
	}
	bestWorkerPos, bestJobPos, bestScore := -1, -1, -1
	for workerPos, workerIndex := range idleWorkers {
		path := paths[workerIndex]
		if !health.Available(path) {
			continue
		}
		score := health.Score(path)
		for jobPos, stateIndex := range pending {
			state := states[stateIndex]
			if state.done || (availableCount > 1 && state.lastInterface == path.InterfaceIndex) {
				continue
			}
			if bestWorkerPos < 0 || score > bestScore {
				bestWorkerPos, bestJobPos, bestScore = workerPos, jobPos, score
			}
			break
		}
	}
	return bestWorkerPos, bestJobPos
}
