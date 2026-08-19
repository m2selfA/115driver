package transfer

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadFileReplacesExistingDestinationAfterCompleteSuccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "movie.bin")
	if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}

	headers := http.Header{
		"User-Agent": []string{""},
		"Cookie":     []string{"session=abc"},
		"Range":      []string{"bytes=0-0"},
	}
	request := testFileDownloadRequest(target, 3)
	request.Header = headers

	result, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method != http.MethodGet {
				t.Fatalf("expected GET, got %s", req.Method)
			}
			if _, ok := req.Header["Range"]; ok {
				t.Fatalf("whole-file request retained Range header: %#v", req.Header["Range"])
			}
			if values, ok := req.Header["User-Agent"]; !ok || len(values) != 1 || values[0] != "" {
				t.Fatalf("empty User-Agent was not preserved: %#v", req.Header["User-Agent"])
			}
			if got := req.Header.Get("Cookie"); got != "session=abc" {
				t.Fatalf("unexpected Cookie header: %q", got)
			}
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: 3,
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader("new")),
				Request:       req,
			}, nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesWritten != 3 || result.StatusCode != http.StatusOK || result.FinalHost != "cdn.example.invalid" {
		t.Fatalf("unexpected result: %#v", result)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("expected replacement contents, got %q", got)
	}
	if got := headers.Get("Range"); got != "bytes=0-0" {
		t.Fatalf("caller headers were mutated: Range=%q", got)
	}
	assertNoDownloadTemps(t, dir, filepath.Base(target))
}

func TestDownloadFilePreservesExistingDestinationOnHTTPFailure(t *testing.T) {
	target := writeExistingDownloadTarget(t)
	request := testFileDownloadRequest(target, 3)

	_, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusForbidden,
				ContentLength: 0,
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader("")),
				Request:       req,
			}, nil
		}), nil
	})
	if !errors.Is(err, ErrUnexpectedDownloadStatus) {
		t.Fatalf("expected HTTP status error, got %v", err)
	}
	assertExistingDownloadTarget(t, target)
	assertNoDownloadTemps(t, filepath.Dir(target), filepath.Base(target))
}

func TestDownloadFilePreservesExistingDestinationOnContentLengthMismatch(t *testing.T) {
	target := writeExistingDownloadTarget(t)
	request := testFileDownloadRequest(target, 4)
	body := &trackingReadCloser{Reader: strings.NewReader("new")}

	_, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: 3,
				Header:        make(http.Header),
				Body:          body,
				Request:       req,
			}, nil
		}), nil
	})
	if !errors.Is(err, ErrDownloadSizeMismatch) {
		t.Fatalf("expected size mismatch, got %v", err)
	}
	if !body.closed.Load() {
		t.Fatal("response body was not closed")
	}
	assertExistingDownloadTarget(t, target)
}

func TestDownloadFilePreservesExistingDestinationOnStreamSizeMismatch(t *testing.T) {
	target := writeExistingDownloadTarget(t)
	request := testFileDownloadRequest(target, 4)

	_, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: -1,
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader("new")),
				Request:       req,
			}, nil
		}), nil
	})
	if !errors.Is(err, ErrDownloadSizeMismatch) {
		t.Fatalf("expected size mismatch, got %v", err)
	}
	assertExistingDownloadTarget(t, target)
	assertNoDownloadTemps(t, filepath.Dir(target), filepath.Base(target))
}

func TestDownloadFilePreservesExistingDestinationWhenBodyExceedsLimit(t *testing.T) {
	target := writeExistingDownloadTarget(t)
	request := testFileDownloadRequest(target, UnknownFileSize)
	request.MaxBytes = 3

	result, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: -1,
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader("1234")),
				Request:       req,
			}, nil
		}), nil
	})
	if !errors.Is(err, ErrDownloadExceedsLimit) {
		t.Fatalf("expected size-limit error, got %v", err)
	}
	if result.BytesWritten != 4 {
		t.Fatalf("expected detection after fourth byte, wrote %d", result.BytesWritten)
	}
	assertExistingDownloadTarget(t, target)
	assertNoDownloadTemps(t, filepath.Dir(target), filepath.Base(target))
}

func TestDownloadFileRejectsKnownOversizeBeforeNetworkRequest(t *testing.T) {
	request := testFileDownloadRequest(filepath.Join(t.TempDir(), "file.bin"), 5)
	request.MaxBytes = 4
	var called atomic.Bool

	_, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		called.Store(true)
		return nil, errors.New("must not be called")
	})
	if !errors.Is(err, ErrDownloadExceedsLimit) {
		t.Fatalf("expected preflight size-limit error, got %v", err)
	}
	if called.Load() {
		t.Fatal("transport factory called for a known oversize file")
	}
}

func TestDownloadFileAllowsKnownEmptyFile(t *testing.T) {
	target := filepath.Join(t.TempDir(), "empty.bin")
	request := testFileDownloadRequest(target, 0)

	result, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode:    http.StatusOK,
				ContentLength: 0,
				Header:        make(http.Header),
				Body:          io.NopCloser(strings.NewReader("")),
				Request:       req,
			}, nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesWritten != 0 {
		t.Fatalf("expected zero bytes, got %d", result.BytesWritten)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected empty file, size=%d", info.Size())
	}
}

func TestDownloadFileSanitizesSignedURLFromNetworkErrors(t *testing.T) {
	request := testFileDownloadRequest(filepath.Join(t.TempDir(), "file.bin"), UnknownFileSize)
	request.URL = "https://cdn.example.invalid/file?token=super-secret"

	_, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("network down")}
		}), nil
	})
	if err == nil {
		t.Fatal("expected network error")
	}
	if !errors.Is(err, ErrNetworkPathFailure) {
		t.Fatalf("network error was not classified for P8 health: %v", err)
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "/file") {
		t.Fatalf("signed URL leaked through error: %v", err)
	}
	if !strings.Contains(err.Error(), "cdn.example.invalid") || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("expected host and root network error, got %v", err)
	}
}

func TestDownloadFileTimeoutCancelsRequest(t *testing.T) {
	request := testFileDownloadRequest(filepath.Join(t.TempDir(), "file.bin"), UnknownFileSize)
	request.Timeout = 20 * time.Millisecond

	started := time.Now()
	_, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}), nil
	})
	if err == nil || !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("download timeout did not cancel promptly")
	}
}

func TestFileDownloadRequestValidation(t *testing.T) {
	base := testFileDownloadRequest(filepath.Join(t.TempDir(), "file.bin"), UnknownFileSize)
	tests := []FileDownloadRequest{
		func() FileDownloadRequest { r := base; r.DestinationPath = ""; return r }(),
		func() FileDownloadRequest { r := base; r.ExpectedSize = -2; return r }(),
		func() FileDownloadRequest { r := base; r.MaxBytes = -1; return r }(),
		func() FileDownloadRequest { r := base; r.Timeout = -time.Second; return r }(),
		func() FileDownloadRequest { r := base; r.NetworkPath.InterfaceIndex = 0; return r }(),
	}
	for _, request := range tests {
		if err := request.validate(); err == nil {
			t.Fatalf("expected invalid request to fail: %#v", request)
		}
	}
}

func testFileDownloadRequest(target string, expectedSize int64) FileDownloadRequest {
	return FileDownloadRequest{
		URL:             "https://cdn.example.invalid/file?token=secret",
		DestinationPath: target,
		NetworkPath:     testNetworkPath(7, "10.0.0.7"),
		ExpectedSize:    expectedSize,
	}
}

func writeExistingDownloadTarget(t *testing.T) string {
	t.Helper()
	target := filepath.Join(t.TempDir(), "target.bin")
	if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	return target
}

func assertExistingDownloadTarget(t *testing.T, target string) {
	t.Helper()
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("expected existing target to survive failure, got %q", got)
	}
}

func assertNoDownloadTemps(t *testing.T, dir, targetBase string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	prefix := "." + targetBase + "."
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), prefix) {
			t.Fatalf("temporary download file was not cleaned up: %s", entry.Name())
		}
	}
}

type trackingReadCloser struct {
	io.Reader
	closed atomic.Bool
}

func (body *trackingReadCloser) Close() error {
	body.closed.Store(true)
	return nil
}
