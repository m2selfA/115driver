package transfer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadFileByChunksAggregatesInterfacesAndAssemblesFile(t *testing.T) {
	data := []byte("abcdefghijklmnop")
	target := filepath.Join(t.TempDir(), "movie.bin")
	paths := []NetworkPath{testNetworkPath(1, "10.0.0.1"), testNetworkPath(2, "10.0.0.2")}
	headers := http.Header{"User-Agent": []string{""}, "Cookie": []string{"session=abc"}, "Range": []string{"bytes=99-100"}}
	request := ChunkDownloadRequest{
		URL:             "https://cdn.example.invalid/file?token=secret",
		Header:          headers,
		DestinationPath: target,
		NetworkPaths:    paths,
		ExpectedSize:    int64(len(data)),
		ChunkSize:       4,
		Retries:         1,
	}

	var mu sync.Mutex
	activeByInterface := map[int]int{}
	maxByInterface := map[int]int{}
	globalActive := 0
	maxGlobal := 0
	result, err := downloadFileByChunks(context.Background(), request, func(path NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if values, ok := req.Header["User-Agent"]; !ok || len(values) != 1 || values[0] != "" {
				t.Fatalf("empty User-Agent was not preserved: %#v", req.Header["User-Agent"])
			}
			if req.Header.Get("Cookie") != "session=abc" {
				t.Fatalf("Cookie was not preserved: %q", req.Header.Get("Cookie"))
			}
			start, end := mustParseRequestRange(t, req.Header.Get("Range"))
			mu.Lock()
			activeByInterface[path.InterfaceIndex]++
			if activeByInterface[path.InterfaceIndex] > maxByInterface[path.InterfaceIndex] {
				maxByInterface[path.InterfaceIndex] = activeByInterface[path.InterfaceIndex]
			}
			globalActive++
			if globalActive > maxGlobal {
				maxGlobal = globalActive
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			activeByInterface[path.InterfaceIndex]--
			globalActive--
			mu.Unlock()
			body := string(data[start : end+1])
			return chunkHTTPResponse(req, start, end, int64(len(data)), body), nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesWritten != int64(len(data)) || result.ChunkCount != 4 {
		t.Fatalf("unexpected chunk result: %#v", result)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("assembled file mismatch: %q", got)
	}
	mu.Lock()
	if maxGlobal != 2 {
		t.Fatalf("expected both interfaces to overlap, max global=%d", maxGlobal)
	}
	for _, path := range paths {
		if maxByInterface[path.InterfaceIndex] != 1 {
			t.Fatalf("interface %d had %d simultaneous chunks", path.InterfaceIndex, maxByInterface[path.InterfaceIndex])
		}
	}
	mu.Unlock()
	if headers.Get("Range") != "bytes=99-100" {
		t.Fatalf("caller Range header mutated: %q", headers.Get("Range"))
	}
	assertNoDownloadTemps(t, filepath.Dir(target), filepath.Base(target))
}

func TestDownloadFileByChunksRetriesFailedChunkOnDifferentInterface(t *testing.T) {
	data := []byte("abcdefgh")
	target := filepath.Join(t.TempDir(), "retry.bin")
	paths := []NetworkPath{testNetworkPath(1, "10.0.0.1"), testNetworkPath(2, "10.0.0.2")}
	request := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file?token=secret", DestinationPath: target,
		NetworkPaths: paths, ExpectedSize: int64(len(data)), ChunkSize: 4, Retries: 1,
	}
	var failed atomic.Bool
	result, err := downloadFileByChunks(context.Background(), request, func(path NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			start, end := mustParseRequestRange(t, req.Header.Get("Range"))
			if path.InterfaceIndex == 1 && start == 0 && failed.CompareAndSwap(false, true) {
				return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("link failed")}
			}
			return chunkHTTPResponse(req, start, end, int64(len(data)), string(data[start:end+1])), nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	attempts := chunkAttemptsByIndex(result.Attempts)
	var chunkZero []ChunkDownloadAttempt
	for _, attempt := range attempts {
		if attempt.ChunkIndex == 0 {
			chunkZero = append(chunkZero, attempt)
		}
	}
	if len(chunkZero) != 2 {
		t.Fatalf("expected chunk 0 retry, attempts=%#v", attempts)
	}
	if chunkZero[0].NetworkPath.InterfaceIndex == chunkZero[1].NetworkPath.InterfaceIndex {
		t.Fatalf("retry did not switch interface: %#v", chunkZero)
	}
	got, _ := os.ReadFile(target)
	if string(got) != string(data) {
		t.Fatalf("unexpected retry output: %q", got)
	}
}

func TestDownloadFileByChunksRetryOverwritesPartialFailedRange(t *testing.T) {
	data := []byte("abcdefgh")
	target := filepath.Join(t.TempDir(), "partial-retry.bin")
	paths := []NetworkPath{testNetworkPath(1, "10.0.0.1"), testNetworkPath(2, "10.0.0.2")}
	request := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file?token=secret", DestinationPath: target,
		NetworkPaths: paths, ExpectedSize: int64(len(data)), ChunkSize: 4, Retries: 1,
	}
	var failed atomic.Bool
	result, err := downloadFileByChunks(context.Background(), request, func(path NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			start, end := mustParseRequestRange(t, req.Header.Get("Range"))
			if path.InterfaceIndex == 1 && start == 0 && failed.CompareAndSwap(false, true) {
				return &http.Response{
					StatusCode:    http.StatusPartialContent,
					Header:        http.Header{"Content-Range": []string{"bytes 0-3/8"}},
					ContentLength: 4,
					Body:          io.NopCloser(&bytesThenErrorReader{data: []byte("ab"), err: errors.New("mid-stream reset")}),
					Request:       req,
				}, nil
			}
			return chunkHTTPResponse(req, start, end, int64(len(data)), string(data[start:end+1])), nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	attempts := chunkAttemptsByIndex(result.Attempts)
	var chunkZero []ChunkDownloadAttempt
	for _, attempt := range attempts {
		if attempt.ChunkIndex == 0 {
			chunkZero = append(chunkZero, attempt)
		}
	}
	if len(chunkZero) != 2 || chunkZero[0].BytesWritten != 2 || chunkZero[0].Err == nil || chunkZero[1].Err != nil {
		t.Fatalf("unexpected partial retry attempts: %#v", chunkZero)
	}
	if chunkZero[0].NetworkPath.InterfaceIndex == chunkZero[1].NetworkPath.InterfaceIndex {
		t.Fatalf("partial retry did not switch interfaces: %#v", chunkZero)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("partial retry left stale bytes: got %q want %q", got, data)
	}
}

func TestDownloadFileByChunksWaitsForSinglePathCooldownAndRecovers(t *testing.T) {
	data := []byte("abcd")
	path := testNetworkPath(1, "10.0.0.1")
	health, err := NewNetworkHealthTracker(NetworkHealthOptions{Cooldown: 15 * time.Millisecond, CooldownMax: 15 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	started := time.Now()
	result, err := downloadFileByChunks(context.Background(), ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file", DestinationPath: filepath.Join(t.TempDir(), "recover.bin"),
		NetworkPaths: []NetworkPath{path}, ExpectedSize: 4, ChunkSize: 4, Retries: 1, HealthTracker: health,
	}, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("reset")}
			}
			return chunkHTTPResponse(req, 0, 3, 4, string(data)), nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesWritten != 4 || calls != 2 || time.Since(started) < 10*time.Millisecond {
		t.Fatalf("unexpected cooldown recovery: result=%#v calls=%d", result, calls)
	}
	snapshot := health.Snapshot(path)
	if snapshot.Failures != 1 || snapshot.Successes != 1 || snapshot.InCooldown || snapshot.ConsecutiveFailures != 0 {
		t.Fatalf("unexpected recovered chunk health: %#v", snapshot)
	}
}

func TestDownloadFileByChunksSharesCooldownAcrossDownloads(t *testing.T) {
	path1 := testNetworkPath(1, "10.0.0.1")
	path2 := testNetworkPath(2, "10.0.0.2")
	health, err := NewNetworkHealthTracker(NetworkHealthOptions{Cooldown: time.Second, CooldownMax: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	var firstFailed atomic.Bool
	firstTarget := filepath.Join(t.TempDir(), "first.bin")
	_, err = downloadFileByChunks(context.Background(), ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file", DestinationPath: firstTarget,
		NetworkPaths: []NetworkPath{path1, path2}, ExpectedSize: 4, ChunkSize: 4, Retries: 1, HealthTracker: health,
	}, func(path NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if path.InterfaceIndex == path1.InterfaceIndex && firstFailed.CompareAndSwap(false, true) {
				return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("reset")}
			}
			return chunkHTTPResponse(req, 0, 3, 4, "abcd"), nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !health.Snapshot(path1).InCooldown {
		t.Fatal("failed interface did not remain in cooldown for the next file")
	}

	used := 0
	secondTarget := filepath.Join(t.TempDir(), "second.bin")
	_, err = downloadFileByChunks(context.Background(), ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file2", DestinationPath: secondTarget,
		NetworkPaths: []NetworkPath{path1, path2}, ExpectedSize: 4, ChunkSize: 4, Retries: 0, HealthTracker: health,
	}, func(path NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			used = path.InterfaceIndex
			return chunkHTTPResponse(req, 0, 3, 4, "wxyz"), nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if used != path2.InterfaceIndex {
		t.Fatalf("second file used cooling interface: got %d want %d", used, path2.InterfaceIndex)
	}
}

func TestDownloadFileByChunksRangeFailureDoesNotPenalizeHealth(t *testing.T) {
	path := testNetworkPath(1, "10.0.0.1")
	health := NewDefaultNetworkHealthTracker()
	_, err := downloadFileByChunks(context.Background(), ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file", DestinationPath: filepath.Join(t.TempDir(), "bad-range.bin"),
		NetworkPaths: []NetworkPath{path}, ExpectedSize: 4, ChunkSize: 4, Retries: 0, HealthTracker: health,
	}, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("full")), Request: req, ContentLength: 4}, nil
		}), nil
	})
	if !errors.Is(err, ErrChunkRangeUnsupported) {
		t.Fatalf("expected Range failure, got %v", err)
	}
	snapshot := health.Snapshot(path)
	if snapshot.Failures != 0 || snapshot.Score != 100 || snapshot.InCooldown {
		t.Fatalf("Range failure changed NIC health: %#v", snapshot)
	}
}

func TestDownloadFileByChunksPreservesExistingDestinationOnInvalidRangeResponse(t *testing.T) {
	target := writeExistingDownloadTarget(t)
	request := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file?token=secret", DestinationPath: target,
		NetworkPaths: []NetworkPath{testNetworkPath(1, "10.0.0.1")}, ExpectedSize: 4, ChunkSize: 4,
	}
	_, err := downloadFileByChunks(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			resp := chunkHTTPResponse(req, 0, 3, 4, "new!")
			resp.Header.Set("Content-Range", "bytes 1-3/4")
			return resp, nil
		}), nil
	})
	if !errors.Is(err, ErrChunkDownloadIncomplete) || !errors.Is(err, ErrChunkRangeUnsupported) {
		t.Fatalf("expected invalid range failure, got %v", err)
	}
	assertExistingDownloadTarget(t, target)
	assertNoDownloadTemps(t, filepath.Dir(target), filepath.Base(target))
}

func TestDownloadFileByChunksRejectsServerIgnoringRange(t *testing.T) {
	target := writeExistingDownloadTarget(t)
	request := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file", DestinationPath: target,
		NetworkPaths: []NetworkPath{testNetworkPath(1, "10.0.0.1")}, ExpectedSize: 4, ChunkSize: 4,
	}
	_, err := downloadFileByChunks(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("full")), Request: req, ContentLength: 4}, nil
		}), nil
	})
	if !errors.Is(err, ErrChunkRangeUnsupported) {
		t.Fatalf("expected ignored Range to fail, got %v", err)
	}
	assertExistingDownloadTarget(t, target)
}

func TestDownloadFileByChunksRejectsKnownOversizeAndUnknownSizeBeforeTransport(t *testing.T) {
	base := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file", DestinationPath: filepath.Join(t.TempDir(), "file.bin"),
		NetworkPaths: []NetworkPath{testNetworkPath(1, "10.0.0.1")}, ExpectedSize: 5, ChunkSize: 2,
	}
	var called atomic.Bool
	factory := func(NetworkPath) (http.RoundTripper, error) {
		called.Store(true)
		return nil, errors.New("must not run")
	}
	over := base
	over.MaxBytes = 4
	if _, err := downloadFileByChunks(context.Background(), over, factory); !errors.Is(err, ErrDownloadExceedsLimit) {
		t.Fatalf("expected oversize error, got %v", err)
	}
	unknown := base
	unknown.ExpectedSize = UnknownFileSize
	if _, err := downloadFileByChunks(context.Background(), unknown, factory); !errors.Is(err, ErrChunkRequiresKnownSize) {
		t.Fatalf("expected known-size requirement, got %v", err)
	}
	if called.Load() {
		t.Fatal("transport created before preflight validation")
	}
}

func TestDownloadFileByChunksAllowsEmptyFileWithoutNetworkPath(t *testing.T) {
	target := filepath.Join(t.TempDir(), "empty.bin")
	result, err := downloadFileByChunks(context.Background(), ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/empty", DestinationPath: target, ExpectedSize: 0, ChunkSize: 4,
	}, func(NetworkPath) (http.RoundTripper, error) {
		t.Fatal("empty file should not create a transport")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesWritten != 0 || result.ChunkCount != 0 {
		t.Fatalf("unexpected empty result: %#v", result)
	}
	info, err := os.Stat(target)
	if err != nil || info.Size() != 0 {
		t.Fatalf("empty target mismatch: info=%v err=%v", info, err)
	}
}

func TestDownloadFileByChunksCanceledContextDoesNotReplaceEmptyTarget(t *testing.T) {
	target := writeExistingDownloadTarget(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := downloadFileByChunks(ctx, ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/empty", DestinationPath: target, ExpectedSize: 0, ChunkSize: 4,
	}, func(NetworkPath) (http.RoundTripper, error) {
		t.Fatal("canceled empty download should not create a transport")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	assertExistingDownloadTarget(t, target)
	assertNoDownloadTemps(t, filepath.Dir(target), filepath.Base(target))
}

func TestBuildByteChunksAvoidsInt64Overflow(t *testing.T) {
	chunks := buildByteChunks(math.MaxInt64, math.MaxInt64)
	if len(chunks) != 1 || chunks[0].start != 0 || chunks[0].end != math.MaxInt64-1 {
		t.Fatalf("unexpected max-int chunk: %#v", chunks)
	}
	chunks = buildByteChunks(5, math.MaxInt64)
	if len(chunks) != 1 || chunks[0].start != 0 || chunks[0].end != 4 {
		t.Fatalf("unexpected oversized chunk-size result: %#v", chunks)
	}
	chunks = buildByteChunks(math.MaxInt64, math.MaxInt64-1)
	if len(chunks) != 2 || chunks[0].end != math.MaxInt64-2 || chunks[1].start != math.MaxInt64-1 || chunks[1].end != math.MaxInt64-1 {
		t.Fatalf("unexpected near-max chunks: %#v", chunks)
	}
}

func TestDownloadFileByChunksTimeoutCancelsWholeFile(t *testing.T) {
	target := writeExistingDownloadTarget(t)
	request := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file", DestinationPath: target,
		NetworkPaths: []NetworkPath{testNetworkPath(1, "10.0.0.1")}, ExpectedSize: 4, ChunkSize: 4, Timeout: 20 * time.Millisecond,
	}
	_, err := downloadFileByChunks(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		}), nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrChunkDownloadIncomplete) {
		t.Fatalf("expected file timeout, got %v", err)
	}
	assertExistingDownloadTarget(t, target)
}

func TestDownloadFileByChunksSanitizesSignedURLFromErrors(t *testing.T) {
	request := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file?token=super-secret", DestinationPath: filepath.Join(t.TempDir(), "file.bin"),
		NetworkPaths: []NetworkPath{testNetworkPath(1, "10.0.0.1")}, ExpectedSize: 4, ChunkSize: 4,
	}
	_, err := downloadFileByChunks(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("network down")}
		}), nil
	})
	if err == nil {
		t.Fatal("expected network error")
	}
	if !errors.Is(err, ErrNetworkPathFailure) {
		t.Fatalf("chunk network error was not classified for P8 health: %v", err)
	}
	if strings.Contains(err.Error(), "super-secret") || strings.Contains(err.Error(), "/file") {
		t.Fatalf("signed URL leaked: %v", err)
	}
	if !strings.Contains(err.Error(), "cdn.example.invalid") || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("sanitized error lost useful diagnostics: %v", err)
	}
}

func TestParseContentRange(t *testing.T) {
	start, end, total, err := parseContentRange("bytes 4-7/16")
	if err != nil || start != 4 || end != 7 || total != 16 {
		t.Fatalf("unexpected parse: %d %d %d %v", start, end, total, err)
	}
	for _, value := range []string{"", "items 0-1/2", "bytes */2", "bytes 2-1/4", "bytes 0-4/4"} {
		if _, _, _, err := parseContentRange(value); err == nil {
			t.Fatalf("expected invalid Content-Range %q", value)
		}
	}
}

func mustParseRequestRange(t *testing.T, value string) (int64, int64) {
	t.Helper()
	if !strings.HasPrefix(value, "bytes=") {
		t.Fatalf("invalid Range header %q", value)
	}
	parts := strings.Split(strings.TrimPrefix(value, "bytes="), "-")
	if len(parts) != 2 {
		t.Fatalf("invalid Range header %q", value)
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	end, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return start, end
}

type bytesThenErrorReader struct {
	data []byte
	err  error
}

func (reader *bytesThenErrorReader) Read(p []byte) (int, error) {
	if len(reader.data) == 0 {
		return 0, reader.err
	}
	n := copy(p, reader.data)
	reader.data = reader.data[n:]
	if len(reader.data) == 0 {
		return n, reader.err
	}
	return n, nil
}

func chunkHTTPResponse(req *http.Request, start, end, total int64, body string) *http.Response {
	return &http.Response{
		StatusCode:    http.StatusPartialContent,
		Header:        http.Header{"Content-Range": []string{fmt.Sprintf("bytes %d-%d/%d", start, end, total)}},
		ContentLength: int64(len(body)),
		Body:          io.NopCloser(strings.NewReader(body)),
		Request:       req,
	}
}
