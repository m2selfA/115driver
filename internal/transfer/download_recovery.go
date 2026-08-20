package transfer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

var ErrTransientDownloadStatus = errors.New("transient download HTTP status")

func isTransientDownloadStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

func transientDownloadStatusError(status int) error {
	return errors.Join(
		fmt.Errorf("%w: HTTP %d", ErrTransientDownloadStatus, status),
		fmt.Errorf("%w: %d", ErrUnexpectedDownloadStatus, status),
	)
}

// IsRecoverableDownloadError reports whether retrying the same logical file or
// byte range can reasonably succeed without changing user input. Physical-path
// failures, bounded server throttling/outages, timeouts, and short/mismatched
// transfers are recoverable. Protocol/argument/local-filesystem failures are not.
func IsRecoverableDownloadError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, ErrNetworkPathFailure) || errors.Is(err, ErrTransientDownloadStatus) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrDownloadSizeMismatch) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

type chunkRecoveryWait func(context.Context, int) error

func downloadFileByChunksWithRecovery(ctx context.Context, request ChunkDownloadRequest, factory transportFactory, wait chunkRecoveryWait) (ChunkDownloadResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	retries := request.RecoveryRetries
	if retries < 0 {
		retries = 0
	}
	source, err := newDownloadSourceState(DownloadSource{URL: request.URL, Header: request.Header}, request.Refresh, request.MaxRefreshes)
	if err != nil {
		return ChunkDownloadResult{DestinationPath: request.DestinationPath, ChunkSize: request.ChunkSize}, err
	}
	request.source = source
	var allAttempts []ChunkDownloadAttempt
	var lastResult ChunkDownloadResult
	for attempt := 0; ; attempt++ {
		result, err := downloadFileByChunks(ctx, request, factory)
		allAttempts = append(allAttempts, result.Attempts...)
		result.Attempts = append([]ChunkDownloadAttempt(nil), allAttempts...)
		result.Refreshes = source.refreshCount()
		result.Duration = time.Since(started)
		lastResult = result
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return result, errors.Join(err, ctx.Err())
		}
		if attempt >= retries || !IsRecoverableDownloadError(err) {
			return result, err
		}
		if wait != nil {
			if waitErr := wait(ctx, attempt+1); waitErr != nil {
				return lastResult, errors.Join(err, waitErr)
			}
		}
	}
}

func waitDownloadRecoveryBackoff(ctx context.Context, retryNumber int) error {
	if retryNumber < 1 {
		retryNumber = 1
	}
	shift := retryNumber - 1
	if shift > 3 {
		shift = 3
	}
	delay := 250 * time.Millisecond * time.Duration(1<<shift)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
