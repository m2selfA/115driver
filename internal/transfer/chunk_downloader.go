package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrChunkRequiresKnownSize  = errors.New("chunk download requires a known file size")
	ErrChunkRangeUnsupported   = errors.New("chunk range response is invalid or unsupported")
	ErrChunkDownloadIncomplete = errors.New("chunk download incomplete")
)

// ChunkDownloadRequest describes one file split into byte ranges and downloaded
// concurrently across multiple physical interfaces. ExpectedSize must be known.
// Retries is the number of retries allowed for each chunk after its first attempt.
type ChunkDownloadRequest struct {
	URL             string
	Header          http.Header
	DestinationPath string
	NetworkPaths    []NetworkPath
	ExpectedSize    int64
	ChunkSize       int64
	MaxBytes        int64
	Timeout         time.Duration
	Retries         int
	HealthTracker   *NetworkHealthTracker
	ResumeKey       string
	Refresh         DownloadSourceRefreshFunc
	MaxRefreshes    int
}

// ChunkDownloadAttempt records one range request. Signed URLs and request
// headers are intentionally excluded from results.
type ChunkDownloadAttempt struct {
	ChunkIndex   int
	Start        int64
	End          int64
	Attempt      int
	NetworkPath  NetworkPath
	BytesWritten int64
	StatusCode   int
	Duration     time.Duration
	Err          error
}

// ChunkDownloadResult records a completed or failed chunked transfer.
type ChunkDownloadResult struct {
	DestinationPath string
	BytesWritten    int64
	ChunkSize       int64
	ChunkCount      int
	Attempts        []ChunkDownloadAttempt
	Duration        time.Duration
	ResumedChunks   int
	Refreshes       int
}

type byteChunk struct {
	index int
	start int64
	end   int64
}

func (chunk byteChunk) length() int64 { return chunk.end - chunk.start + 1 }

type chunkState struct {
	chunk         byteChunk
	attempts      int
	lastInterface int
	done          bool
}

type chunkWorkerTask struct {
	stateIndex int
	attempt    int
	chunk      byteChunk
	path       NetworkPath
}

type chunkWorkerResult struct {
	stateIndex  int
	workerIndex int
	attempt     ChunkDownloadAttempt
}

// DownloadFileByChunks downloads a known-size file with HTTP Range requests.
// Exactly one worker is created for each supplied physical interface. The file
// is assembled in a same-directory temporary file and replaces the destination
// only after every range has succeeded.
func DownloadFileByChunks(ctx context.Context, request ChunkDownloadRequest) (ChunkDownloadResult, error) {
	return downloadFileByChunks(ctx, request, func(path NetworkPath) (http.RoundTripper, error) {
		return NewTransport(path)
	})
}

func (request ChunkDownloadRequest) validate() error {
	if request.DestinationPath == "" {
		return errors.New("chunk download destination path is empty")
	}
	if request.ExpectedSize < 0 {
		return ErrChunkRequiresKnownSize
	}
	if request.ChunkSize <= 0 {
		return errors.New("chunk size must be > 0")
	}
	if request.MaxBytes < 0 {
		return errors.New("chunk download max bytes must be >= 0")
	}
	if request.Timeout < 0 {
		return errors.New("chunk download timeout must be >= 0")
	}
	if request.Retries < 0 {
		return errors.New("chunk download retries must be >= 0")
	}
	if request.MaxRefreshes < 0 {
		return errors.New("chunk download URL refresh count must be >= 0")
	}
	if request.MaxBytes > 0 && request.ExpectedSize > request.MaxBytes {
		return fmt.Errorf("%w: expected %d bytes, limit is %d bytes", ErrDownloadExceedsLimit, request.ExpectedSize, request.MaxBytes)
	}
	if request.ExpectedSize > 0 && len(request.NetworkPaths) == 0 {
		return errors.New("chunk download requires at least one network path")
	}
	seenInterfaces := make(map[int]struct{}, len(request.NetworkPaths))
	for _, path := range request.NetworkPaths {
		if err := path.Validate(); err != nil {
			return fmt.Errorf("invalid chunk download network path: %w", err)
		}
		if _, exists := seenInterfaces[path.InterfaceIndex]; exists {
			return fmt.Errorf("chunk download received multiple paths for interface index %d", path.InterfaceIndex)
		}
		seenInterfaces[path.InterfaceIndex] = struct{}{}
	}
	return nil
}

func downloadFileByChunks(ctx context.Context, request ChunkDownloadRequest, factory transportFactory) (ChunkDownloadResult, error) {
	started := time.Now()
	result := ChunkDownloadResult{
		DestinationPath: request.DestinationPath,
		ChunkSize:       request.ChunkSize,
	}
	if err := request.validate(); err != nil {
		return result, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	source, err := newDownloadSourceState(DownloadSource{URL: request.URL, Header: request.Header}, request.Refresh, request.MaxRefreshes)
	if err != nil {
		return result, err
	}
	chunks := buildByteChunks(request.ExpectedSize, request.ChunkSize)
	result.ChunkCount = len(chunks)
	artifacts, completedSet, err := openChunkResume(request.DestinationPath, request.ResumeKey, request.ExpectedSize, request.ChunkSize, len(chunks))
	if err != nil {
		return result, err
	}
	defer artifacts.closeOnFailure()

	if request.ExpectedSize == 0 {
		if err := artifacts.replaceDestination(request.DestinationPath); err != nil {
			return result, fmt.Errorf("replace chunk download destination: %w", err)
		}
		result.Duration = time.Since(started)
		return result, nil
	}

	states := make([]chunkState, len(chunks))
	pending := make([]int, 0, len(chunks))
	completed := 0
	for i, chunk := range chunks {
		states[i].chunk = chunk
		states[i].lastInterface = -1
		if _, ok := completedSet[i]; ok {
			states[i].done = true
			completed++
			continue
		}
		pending = append(pending, i)
	}
	result.ResumedChunks = completed
	if completed == len(states) {
		if err := artifacts.replaceDestination(request.DestinationPath); err != nil {
			return result, fmt.Errorf("replace fully resumed chunk destination: %w", err)
		}
		result.BytesWritten = request.ExpectedSize
		result.Duration = time.Since(started)
		return result, nil
	}

	workerCtx := ctx
	var timeoutCancel context.CancelFunc
	if request.Timeout > 0 {
		workerCtx, timeoutCancel = context.WithTimeout(ctx, request.Timeout)
		defer timeoutCancel()
	}
	workerCtx, cancelWorkers := context.WithCancel(workerCtx)
	defer cancelWorkers()

	clients := make([]*http.Client, len(request.NetworkPaths))
	closers := make([]interface{ CloseIdleConnections() }, 0, len(request.NetworkPaths))
	defer func() {
		for _, closer := range closers {
			closer.CloseIdleConnections()
		}
	}()
	for i, path := range request.NetworkPaths {
		transport, err := factory(path)
		if err != nil {
			return result, fmt.Errorf("create bound chunk transport for %s: %w", path, err)
		}
		if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
			closers = append(closers, closer)
		}
		clients[i] = &http.Client{Transport: transport}
	}

	taskChannels := make([]chan chunkWorkerTask, len(request.NetworkPaths))
	attemptResults := make(chan chunkWorkerResult, len(request.NetworkPaths))
	var workers sync.WaitGroup
	workers.Add(len(request.NetworkPaths))
	for i, path := range request.NetworkPaths {
		tasks := make(chan chunkWorkerTask)
		taskChannels[i] = tasks
		go func(workerIndex int, workerPath NetworkPath, client *http.Client, workerTasks <-chan chunkWorkerTask) {
			defer workers.Done()
			for task := range workerTasks {
				attempt := downloadChunkRange(workerCtx, client, source, artifacts.file, task.chunk, workerPath, task.attempt, request.ExpectedSize)
				attemptResults <- chunkWorkerResult{stateIndex: task.stateIndex, workerIndex: workerIndex, attempt: attempt}
			}
		}(i, path, clients[i], tasks)
	}

	idleWorkers := make([]int, len(request.NetworkPaths))
	for i := range idleWorkers {
		idleWorkers[i] = i
	}
	inFlight := 0
	var terminalErr error

	for completed < len(states) {
		if terminalErr == nil {
			if err := workerCtx.Err(); err != nil {
				terminalErr = err
				cancelWorkers()
			} else {
				inFlight += dispatchChunkTasks(states, request.NetworkPaths, &pending, &idleWorkers, taskChannels, request.HealthTracker)
			}
		}

		if inFlight == 0 {
			if terminalErr != nil {
				break
			}
			if len(pending) > 0 {
				waited, waitErr := waitForNetworkHealth(workerCtx, request.HealthTracker, request.NetworkPaths)
				if waitErr != nil {
					terminalErr = waitErr
					cancelWorkers()
					break
				}
				if waited {
					continue
				}
				terminalErr = errors.New("no eligible network path for pending chunk")
				cancelWorkers()
				break
			}
		}

		var workerResult chunkWorkerResult
		if terminalErr != nil {
			workerResult = <-attemptResults
		} else {
			select {
			case workerResult = <-attemptResults:
			case <-workerCtx.Done():
				terminalErr = workerCtx.Err()
				cancelWorkers()
				continue
			}
		}
		inFlight--
		idleWorkers = append(idleWorkers, workerResult.workerIndex)
		result.Attempts = append(result.Attempts, workerResult.attempt)
		state := &states[workerResult.stateIndex]
		state.attempts++
		state.lastInterface = workerResult.attempt.NetworkPath.InterfaceIndex

		if workerResult.attempt.Err == nil {
			request.HealthTracker.RecordSuccess(workerResult.attempt.NetworkPath)
			if !state.done {
				if persistErr := artifacts.markChunkComplete(state.chunk.index); persistErr != nil {
					terminalErr = persistErr
					cancelWorkers()
					continue
				}
				state.done = true
				completed++
			}
			continue
		}
		if shouldPenalizeNetworkPath(workerCtx, workerResult.attempt.Err) {
			request.HealthTracker.RecordFailure(workerResult.attempt.NetworkPath)
		}
		if terminalErr != nil || workerCtx.Err() != nil {
			if terminalErr == nil {
				terminalErr = workerCtx.Err()
				cancelWorkers()
			}
			continue
		}
		if isTerminalDownloadSourceError(workerResult.attempt.Err) {
			terminalErr = workerResult.attempt.Err
			cancelWorkers()
			continue
		}
		if state.attempts <= request.Retries {
			pending = append(pending, workerResult.stateIndex)
			continue
		}
		terminalErr = fmt.Errorf("chunk %d bytes %d-%d failed after %d attempt(s): %w", state.chunk.index, state.chunk.start, state.chunk.end, state.attempts, workerResult.attempt.Err)
		cancelWorkers()
	}

	for _, tasks := range taskChannels {
		close(tasks)
	}
	workers.Wait()

	result.Refreshes = source.refreshCount()
	if terminalErr != nil {
		result.Duration = time.Since(started)
		return result, errors.Join(ErrChunkDownloadIncomplete, terminalErr)
	}
	if completed != len(states) {
		result.Duration = time.Since(started)
		return result, fmt.Errorf("%w: completed %d of %d chunks", ErrChunkDownloadIncomplete, completed, len(states))
	}
	if err := workerCtx.Err(); err != nil {
		result.Duration = time.Since(started)
		return result, errors.Join(ErrChunkDownloadIncomplete, err)
	}
	if err := artifacts.file.Sync(); err != nil {
		return result, fmt.Errorf("sync chunk download part file: %w", err)
	}
	if err := artifacts.replaceDestination(request.DestinationPath); err != nil {
		return result, fmt.Errorf("replace chunk download destination: %w", err)
	}
	result.BytesWritten = request.ExpectedSize
	result.Duration = time.Since(started)
	return result, nil
}

func buildByteChunks(size, chunkSize int64) []byteChunk {
	if size <= 0 || chunkSize <= 0 {
		return nil
	}
	chunks := make([]byteChunk, 0)
	for start, index := int64(0), 0; start < size; index++ {
		length := chunkSize
		if remaining := size - start; length > remaining {
			length = remaining
		}
		end := start + length - 1
		chunks = append(chunks, byteChunk{index: index, start: start, end: end})
		start = end + 1
	}
	return chunks
}

func dispatchChunkTasks(states []chunkState, paths []NetworkPath, pending *[]int, idleWorkers *[]int, taskChannels []chan chunkWorkerTask, health *NetworkHealthTracker) int {
	dispatched := 0
	for len(*pending) > 0 && len(*idleWorkers) > 0 {
		workerPos, jobPos := findEligibleChunkDispatch(states, paths, *pending, *idleWorkers, health)
		if workerPos < 0 {
			break
		}
		workerIndex := (*idleWorkers)[workerPos]
		stateIndex := (*pending)[jobPos]
		state := states[stateIndex]
		*idleWorkers = append((*idleWorkers)[:workerPos], (*idleWorkers)[workerPos+1:]...)
		*pending = append((*pending)[:jobPos], (*pending)[jobPos+1:]...)
		taskChannels[workerIndex] <- chunkWorkerTask{stateIndex: stateIndex, attempt: state.attempts + 1, chunk: state.chunk, path: paths[workerIndex]}
		dispatched++
	}
	return dispatched
}

func findEligibleChunkDispatch(states []chunkState, paths []NetworkPath, pending, idleWorkers []int, health *NetworkHealthTracker) (int, int) {
	bestWorkerPos, bestJobPos, bestScore := -1, -1, -1
	availablePathCount := 0
	for _, path := range paths {
		if health.Available(path) {
			availablePathCount++
		}
	}
	for workerPos, workerIndex := range idleWorkers {
		path := paths[workerIndex]
		if !health.Available(path) {
			continue
		}
		score := health.Score(path)
		for jobPos, stateIndex := range pending {
			state := states[stateIndex]
			if state.done || (availablePathCount > 1 && state.lastInterface == path.InterfaceIndex) {
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

func downloadChunkRange(ctx context.Context, client *http.Client, source *downloadSourceState, dst *os.File, chunk byteChunk, path NetworkPath, attemptNumber int, expectedSize int64) ChunkDownloadAttempt {
	attempt := ChunkDownloadAttempt{
		ChunkIndex: chunk.index, Start: chunk.start, End: chunk.end, Attempt: attemptNumber, NetworkPath: cloneNetworkPath(path),
	}
	started := time.Now()
	defer func() { attempt.Duration = time.Since(started) }()

	for {
		snapshot := source.current()
		parsedURL, err := url.Parse(snapshot.URL)
		if err != nil {
			attempt.Err = fmt.Errorf("parse chunk source: %w", stripURLErrorURL(err))
			return attempt
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
		if err != nil {
			attempt.Err = fmt.Errorf("create chunk request for host %q: %w", parsedURL.Hostname(), stripURLErrorURL(err))
			return attempt
		}
		if snapshot.Header != nil {
			request.Header = snapshot.Header.Clone()
		}
		request.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", chunk.start, chunk.end))

		response, err := client.Do(request)
		if err != nil {
			attempt.Err = fmt.Errorf("download chunk from host %q: %w", parsedURL.Hostname(), markNetworkPathFailure(stripURLErrorURL(err)))
			return attempt
		}
		attempt.StatusCode = response.StatusCode
		if isDownloadSourceExpiredStatus(response.StatusCode) {
			_ = response.Body.Close()
			_, refreshErr := source.refreshIfStale(ctx, snapshot.Version)
			if refreshErr != nil {
				attempt.Err = errors.Join(sourceExpiredError(response.StatusCode), refreshErr)
				return attempt
			}
			continue
		}
		if response.StatusCode != http.StatusPartialContent {
			_ = response.Body.Close()
			attempt.Err = fmt.Errorf("%w: HTTP %d", ErrChunkRangeUnsupported, response.StatusCode)
			return attempt
		}
		start, end, total, err := parseContentRange(response.Header.Get("Content-Range"))
		if err != nil || start != chunk.start || end != chunk.end || total != expectedSize {
			_ = response.Body.Close()
			attempt.Err = fmt.Errorf("%w: got Content-Range %q, expected bytes %d-%d/%d", ErrChunkRangeUnsupported, response.Header.Get("Content-Range"), chunk.start, chunk.end, expectedSize)
			return attempt
		}
		length := chunk.length()
		if response.ContentLength >= 0 && response.ContentLength != length {
			_ = response.Body.Close()
			attempt.Err = fmt.Errorf("%w: chunk Content-Length is %d, expected %d", ErrDownloadSizeMismatch, response.ContentLength, length)
			return attempt
		}

		writer := io.NewOffsetWriter(dst, chunk.start)
		limited := &io.LimitedReader{R: response.Body, N: length}
		written, err := io.Copy(writer, limited)
		attempt.BytesWritten = written
		if err != nil {
			_ = response.Body.Close()
			attempt.Err = fmt.Errorf("write chunk %d bytes %d-%d: %w", chunk.index, chunk.start, chunk.end, markLikelyNetworkPathFailure(err))
			return attempt
		}
		if written != length || limited.N != 0 {
			_ = response.Body.Close()
			attempt.Err = fmt.Errorf("%w: chunk %d wrote %d bytes, expected %d", ErrDownloadSizeMismatch, chunk.index, written, length)
			return attempt
		}
		var extra [1]byte
		n, readErr := response.Body.Read(extra[:])
		_ = response.Body.Close()
		if n > 0 {
			attempt.Err = fmt.Errorf("%w: chunk %d response exceeded requested range", ErrDownloadSizeMismatch, chunk.index)
			return attempt
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			attempt.Err = fmt.Errorf("read end of chunk %d response: %w", chunk.index, markLikelyNetworkPathFailure(readErr))
			return attempt
		}
		return attempt
	}
}

func parseContentRange(value string) (int64, int64, int64, error) {
	value = strings.TrimSpace(value)
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bytes") {
		return 0, 0, 0, errors.New("invalid Content-Range unit")
	}
	rangeAndTotal := strings.Split(parts[1], "/")
	if len(rangeAndTotal) != 2 || rangeAndTotal[1] == "*" {
		return 0, 0, 0, errors.New("invalid Content-Range total")
	}
	bounds := strings.Split(rangeAndTotal[0], "-")
	if len(bounds) != 2 {
		return 0, 0, 0, errors.New("invalid Content-Range bounds")
	}
	start, err := strconv.ParseInt(bounds[0], 10, 64)
	if err != nil || start < 0 {
		return 0, 0, 0, errors.New("invalid Content-Range start")
	}
	end, err := strconv.ParseInt(bounds[1], 10, 64)
	if err != nil || end < start {
		return 0, 0, 0, errors.New("invalid Content-Range end")
	}
	total, err := strconv.ParseInt(rangeAndTotal[1], 10, 64)
	if err != nil || total <= end {
		return 0, 0, 0, errors.New("invalid Content-Range size")
	}
	return start, end, total, nil
}

// chunkAttemptsByIndex is used only to provide deterministic test/debug views
// when concurrency causes attempts to complete out of order.
func chunkAttemptsByIndex(attempts []ChunkDownloadAttempt) []ChunkDownloadAttempt {
	cloned := append([]ChunkDownloadAttempt(nil), attempts...)
	sort.SliceStable(cloned, func(i, j int) bool {
		if cloned[i].ChunkIndex != cloned[j].ChunkIndex {
			return cloned[i].ChunkIndex < cloned[j].ChunkIndex
		}
		return cloned[i].Attempt < cloned[j].Attempt
	})
	return cloned
}
