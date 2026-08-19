package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const UnknownFileSize int64 = -1

var (
	ErrUnexpectedDownloadStatus = errors.New("unexpected download HTTP status")
	ErrDownloadSizeMismatch     = errors.New("download size mismatch")
	ErrDownloadExceedsLimit     = errors.New("download exceeds size limit")
)

// FileDownloadRequest describes one whole-file transfer through one bound
// network path. ExpectedSize uses UnknownFileSize when the remote size is not
// known; zero is a valid expected size for an empty file. MaxBytes uses zero to
// disable the size limit. Timeout uses zero to disable the client timeout.
type FileDownloadRequest struct {
	URL             string
	Header          http.Header
	DestinationPath string
	NetworkPath     NetworkPath
	ExpectedSize    int64
	MaxBytes        int64
	Timeout         time.Duration
	ResumeKey       string
	Refresh         DownloadSourceRefreshFunc
	MaxRefreshes    int

	source *downloadSourceState
}

// FileDownloadResult records the completed whole-file transfer.
type FileDownloadResult struct {
	NetworkPath     NetworkPath
	DestinationPath string
	BytesWritten    int64
	StatusCode      int
	FinalHost       string
	Duration        time.Duration
	ResumedFrom     int64
	Refreshes       int
}

// DownloadFile downloads one complete file through request.NetworkPath. The
// response is first written to a temporary file in the destination directory;
// the destination is replaced only after the HTTP response and size checks have
// succeeded. Existing destination contents therefore survive failed transfers.
func DownloadFile(ctx context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
	return downloadFile(ctx, request, func(path NetworkPath) (http.RoundTripper, error) {
		return NewTransport(path)
	})
}

func (request FileDownloadRequest) validate() error {
	if request.DestinationPath == "" {
		return errors.New("download destination path is empty")
	}
	if request.ExpectedSize < UnknownFileSize {
		return fmt.Errorf("expected size must be >= %d", UnknownFileSize)
	}
	if request.MaxBytes < 0 {
		return errors.New("download max bytes must be >= 0")
	}
	if request.Timeout < 0 {
		return errors.New("download timeout must be >= 0")
	}
	if request.MaxRefreshes < 0 {
		return errors.New("download URL refresh count must be >= 0")
	}
	if request.ExpectedSize >= 0 && request.MaxBytes > 0 && request.ExpectedSize > request.MaxBytes {
		return fmt.Errorf("%w: expected %d bytes, limit is %d bytes", ErrDownloadExceedsLimit, request.ExpectedSize, request.MaxBytes)
	}
	if err := request.NetworkPath.Validate(); err != nil {
		return fmt.Errorf("invalid download network path: %w", err)
	}
	return nil
}

func downloadFile(ctx context.Context, request FileDownloadRequest, factory transportFactory) (FileDownloadResult, error) {
	result := FileDownloadResult{NetworkPath: request.NetworkPath, DestinationPath: request.DestinationPath}
	if err := request.validate(); err != nil {
		return result, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	source := request.source
	if source == nil {
		var err error
		source, err = newDownloadSourceState(DownloadSource{URL: request.URL, Header: request.Header}, request.Refresh, request.MaxRefreshes)
		if err != nil {
			return result, err
		}
	}

	transport, err := factory(request.NetworkPath)
	if err != nil {
		return result, fmt.Errorf("create bound download transport: %w", err)
	}
	if closer, ok := transport.(interface{ CloseIdleConnections() }); ok {
		defer closer.CloseIdleConnections()
	}
	client := &http.Client{Transport: transport, Timeout: request.Timeout}

	artifacts, offset, err := openFileResume(request.DestinationPath, request.ResumeKey, request.ExpectedSize)
	if err != nil {
		return result, err
	}
	defer artifacts.closeOnFailure()
	result.ResumedFrom = offset
	result.BytesWritten = offset
	if artifacts.persistent && request.ExpectedSize >= 0 && offset == request.ExpectedSize {
		if err := artifacts.replaceDestination(request.DestinationPath); err != nil {
			return result, fmt.Errorf("replace resumed download destination: %w", err)
		}
		result.Refreshes = source.refreshCount()
		return result, nil
	}
	if _, err := artifacts.file.Seek(offset, io.SeekStart); err != nil {
		return result, fmt.Errorf("seek resumable download part: %w", err)
	}

	started := time.Now()
	for {
		snapshot := source.current()
		parsedURL, err := parseCDNProbeURL(snapshot.URL)
		if err != nil {
			return result, err
		}
		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
		if err != nil {
			return result, fmt.Errorf("create download request for host %q: %w", parsedURL.Hostname(), stripURLErrorURL(err))
		}
		if snapshot.Header != nil {
			httpRequest.Header = snapshot.Header.Clone()
		}
		if offset > 0 {
			httpRequest.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		} else {
			httpRequest.Header.Del("Range")
		}

		response, err := client.Do(httpRequest)
		result.Duration = time.Since(started)
		if err != nil {
			result.Refreshes = source.refreshCount()
			return result, fmt.Errorf("download from host %q: %w", parsedURL.Hostname(), markNetworkPathFailure(stripURLErrorURL(err)))
		}
		result.StatusCode = response.StatusCode
		result.FinalHost = parsedURL.Hostname()
		if response.Request != nil && response.Request.URL != nil {
			result.FinalHost = response.Request.URL.Hostname()
		}

		if isDownloadSourceExpiredStatus(response.StatusCode) {
			_ = response.Body.Close()
			_, refreshErr := source.refreshIfStale(ctx, snapshot.Version)
			if refreshErr != nil {
				result.Refreshes = source.refreshCount()
				return result, errors.Join(sourceExpiredError(response.StatusCode), refreshErr)
			}
			continue
		}

		resuming := offset > 0
		if resuming && response.StatusCode == http.StatusOK {
			// The server ignored Range. Falling back to a full rewrite is safe: the
			// old destination is still untouched and only the private part is reset.
			if err := artifacts.file.Truncate(0); err != nil {
				_ = response.Body.Close()
				return result, fmt.Errorf("restart resumable download part: %w", err)
			}
			if _, err := artifacts.file.Seek(0, io.SeekStart); err != nil {
				_ = response.Body.Close()
				return result, fmt.Errorf("seek restarted download part: %w", err)
			}
			offset = 0
			result.ResumedFrom = 0
			resuming = false
		} else if resuming {
			if response.StatusCode != http.StatusPartialContent {
				_ = response.Body.Close()
				return result, fmt.Errorf("%w: %d", ErrUnexpectedDownloadStatus, response.StatusCode)
			}
			start, end, total, rangeErr := parseContentRange(response.Header.Get("Content-Range"))
			if rangeErr != nil || request.ExpectedSize < 0 || start != offset || end != request.ExpectedSize-1 || total != request.ExpectedSize {
				_ = response.Body.Close()
				return result, fmt.Errorf("%w: resume Content-Range %q does not match offset %d and size %d", ErrDownloadSizeMismatch, response.Header.Get("Content-Range"), offset, request.ExpectedSize)
			}
		} else if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return result, fmt.Errorf("%w: %d", ErrUnexpectedDownloadStatus, response.StatusCode)
		}

		expectedRemaining := int64(UnknownFileSize)
		if request.ExpectedSize >= 0 {
			expectedRemaining = request.ExpectedSize - offset
		}
		if response.ContentLength >= 0 {
			if request.MaxBytes > 0 && (offset > request.MaxBytes || response.ContentLength > request.MaxBytes-offset) {
				_ = response.Body.Close()
				return result, fmt.Errorf("%w: resumed content would exceed %d bytes", ErrDownloadExceedsLimit, request.MaxBytes)
			}
			if expectedRemaining >= 0 && response.ContentLength != expectedRemaining {
				_ = response.Body.Close()
				return result, fmt.Errorf("%w: content length is %d bytes, expected remaining %d bytes", ErrDownloadSizeMismatch, response.ContentLength, expectedRemaining)
			}
		}

		written, copyErr := copyDownloadBodyResumable(artifacts.file, response.Body, expectedRemaining, request.MaxBytes, offset)
		_ = response.Body.Close()
		result.BytesWritten = offset + written
		if copyErr != nil {
			result.Refreshes = source.refreshCount()
			transferErr := markLikelyNetworkPathFailure(copyErr)
			if artifacts.persistent {
				if syncErr := artifacts.file.Sync(); syncErr != nil {
					return result, errors.Join(transferErr, fmt.Errorf("sync interrupted download part: %w", syncErr))
				}
			}
			return result, transferErr
		}
		offset += written
		if request.ExpectedSize >= 0 && offset != request.ExpectedSize {
			return result, fmt.Errorf("%w: wrote %d bytes, expected %d bytes", ErrDownloadSizeMismatch, offset, request.ExpectedSize)
		}
		if err := artifacts.file.Sync(); err != nil {
			return result, fmt.Errorf("sync download part file: %w", err)
		}
		if err := artifacts.replaceDestination(request.DestinationPath); err != nil {
			return result, fmt.Errorf("replace download destination: %w", err)
		}
		result.BytesWritten = offset
		result.Duration = time.Since(started)
		result.Refreshes = source.refreshCount()
		return result, nil
	}
}

func copyDownloadBodyResumable(dst io.Writer, src io.Reader, expectedRemaining, maxBytes, alreadyWritten int64) (int64, error) {
	limit := int64(0)
	limitErr := error(nil)
	if expectedRemaining >= 0 {
		if expectedRemaining == 1<<63-1 {
			limit = expectedRemaining
		} else {
			limit = expectedRemaining + 1
		}
		limitErr = ErrDownloadSizeMismatch
	}
	if maxBytes > 0 {
		remaining := maxBytes - alreadyWritten
		if remaining < 0 {
			return 0, fmt.Errorf("%w: limit is %d bytes", ErrDownloadExceedsLimit, maxBytes)
		}
		maxLimit := remaining
		if maxLimit < 1<<63-1 {
			maxLimit++
		}
		if limit == 0 || maxLimit < limit {
			limit = maxLimit
			limitErr = ErrDownloadExceedsLimit
		}
	}
	if limit == 0 {
		return io.Copy(dst, src)
	}
	limited := &io.LimitedReader{R: src, N: limit}
	written, err := io.Copy(dst, limited)
	if err != nil {
		return written, err
	}
	if limited.N == 0 {
		if errors.Is(limitErr, ErrDownloadExceedsLimit) {
			return written, fmt.Errorf("%w: limit is %d bytes", ErrDownloadExceedsLimit, maxBytes)
		}
		return written, fmt.Errorf("%w: response exceeded expected remaining size", ErrDownloadSizeMismatch)
	}
	if expectedRemaining >= 0 && written != expectedRemaining {
		return written, fmt.Errorf("%w: wrote %d bytes, expected remaining %d bytes", ErrDownloadSizeMismatch, written, expectedRemaining)
	}
	return written, nil
}
