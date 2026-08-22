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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDownloadFileResumesPersistentPartAcrossCalls(t *testing.T) {
	data := []byte("abcdefgh")
	target := filepath.Join(t.TempDir(), "movie.bin")
	if err := os.WriteFile(target, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	request := testFileDownloadRequest(target, int64(len(data)))
	request.ResumeKey = "pick-resume-file"

	first, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Header.Get("Range") != "" {
				t.Fatalf("first request unexpectedly resumed: %q", req.Header.Get("Range"))
			}
			return &http.Response{
				StatusCode: http.StatusOK, ContentLength: -1, Header: make(http.Header),
				Body: io.NopCloser(&p9BytesThenErrorReader{data: append([]byte(nil), data[:4]...), err: io.ErrUnexpectedEOF}), Request: req,
			}, nil
		}), nil
	})
	if !errors.Is(err, ErrNetworkPathFailure) || first.BytesWritten != 4 {
		t.Fatalf("expected resumable network failure after 4 bytes: result=%#v err=%v", first, err)
	}
	gotOld, readErr := os.ReadFile(target)
	if readErr != nil || string(gotOld) != "old" {
		t.Fatalf("existing destination changed after interrupted transfer: %q err=%v", gotOld, readErr)
	}
	partPath, statePath := resumeArtifactPaths(target)
	if info, statErr := os.Stat(partPath); statErr != nil || info.Size() != 4 {
		t.Fatalf("expected 4-byte persistent part: info=%v err=%v", info, statErr)
	}
	if _, statErr := os.Stat(statePath); statErr != nil {
		t.Fatalf("resume metadata missing after interruption: %v", statErr)
	}

	second, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Range"); got != "bytes=4-" {
				t.Fatalf("unexpected resume Range: %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusPartialContent, ContentLength: 4,
				Header: http.Header{"Content-Range": []string{"bytes 4-7/8"}},
				Body:   io.NopCloser(strings.NewReader("efgh")), Request: req,
			}, nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ResumedFrom != 4 || second.BytesWritten != 8 {
		t.Fatalf("unexpected resumed result: %#v", second)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(data) {
		t.Fatalf("resumed file mismatch: %q err=%v", got, err)
	}
	assertResumeArtifactsRemoved(t, target)
}

func TestDownloadFileCentralResumeMetadataKeepsPartAtDestination(t *testing.T) {
	data := []byte("abcdefgh")
	root := t.TempDir()
	target := filepath.Join(root, "movie.bin")
	centralState := filepath.Join(t.TempDir(), "sessions", "payload.json")
	request := testFileDownloadRequest(target, int64(len(data)))
	request.ResumeKey = "pick-central-file"
	request.ResumeStatePath = centralState

	first, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, ContentLength: -1, Header: make(http.Header),
				Body: io.NopCloser(&p9BytesThenErrorReader{data: append([]byte(nil), data[:4]...), err: io.ErrUnexpectedEOF}), Request: req,
			}, nil
		}), nil
	})
	if !errors.Is(err, ErrNetworkPathFailure) || first.BytesWritten != 4 {
		t.Fatalf("expected interrupted central resume: result=%#v err=%v", first, err)
	}
	partPath, legacyState := resumeArtifactPaths(target)
	if info, statErr := os.Stat(partPath); statErr != nil || info.Size() != 4 {
		t.Fatalf("destination-side part missing: info=%v err=%v", info, statErr)
	}
	if _, statErr := os.Stat(centralState); statErr != nil {
		t.Fatalf("central resume metadata missing: %v", statErr)
	}
	if _, statErr := os.Stat(legacyState); !os.IsNotExist(statErr) {
		t.Fatalf("legacy destination metadata should not be created: %v", statErr)
	}

	second, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Range"); got != "bytes=4-" {
				t.Fatalf("unexpected central resume Range: %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusPartialContent, ContentLength: 4,
				Header: http.Header{"Content-Range": []string{"bytes 4-7/8"}},
				Body:   io.NopCloser(strings.NewReader("efgh")), Request: req,
			}, nil
		}), nil
	})
	if err != nil || second.ResumedFrom != 4 {
		t.Fatalf("central resume failed: result=%#v err=%v", second, err)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != string(data) {
		t.Fatalf("central resumed file mismatch: %q err=%v", got, readErr)
	}
	if _, statErr := os.Stat(centralState); !os.IsNotExist(statErr) {
		t.Fatalf("central metadata survived successful download: %v", statErr)
	}
}

func TestDownloadFileMigratesLegacyDestinationResumeMetadata(t *testing.T) {
	data := []byte("abcdefgh")
	target := filepath.Join(t.TempDir(), "legacy.bin")
	legacyRequest := testFileDownloadRequest(target, int64(len(data)))
	legacyRequest.ResumeKey = "pick-legacy-file"
	_, err := downloadFile(context.Background(), legacyRequest, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, ContentLength: -1, Header: make(http.Header),
				Body: io.NopCloser(&p9BytesThenErrorReader{data: []byte("abcd"), err: io.ErrUnexpectedEOF}), Request: req,
			}, nil
		}), nil
	})
	if err == nil {
		t.Fatal("expected interrupted legacy download")
	}
	_, legacyState := resumeArtifactPaths(target)
	if _, statErr := os.Stat(legacyState); statErr != nil {
		t.Fatalf("legacy metadata missing before migration: %v", statErr)
	}

	centralState := filepath.Join(t.TempDir(), "managed", "payload.json")
	request := legacyRequest
	request.ResumeStatePath = centralState
	result, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Range"); got != "bytes=4-" {
				t.Fatalf("legacy metadata was not migrated for resume: %q", got)
			}
			if _, statErr := os.Stat(centralState); statErr != nil {
				t.Fatalf("central metadata not published before resumed request: %v", statErr)
			}
			if _, statErr := os.Stat(legacyState); !os.IsNotExist(statErr) {
				t.Fatalf("legacy metadata still present after migration: %v", statErr)
			}
			return &http.Response{
				StatusCode: http.StatusPartialContent, ContentLength: 4,
				Header: http.Header{"Content-Range": []string{"bytes 4-7/8"}},
				Body:   io.NopCloser(strings.NewReader("efgh")), Request: req,
			}, nil
		}), nil
	})
	if err != nil || result.ResumedFrom != 4 {
		t.Fatalf("migrated legacy resume failed: result=%#v err=%v", result, err)
	}
}

func TestDownloadFileResumeFallsBackWhenRangeIgnored(t *testing.T) {
	data := []byte("abcdefgh")
	target := filepath.Join(t.TempDir(), "fallback.bin")
	request := testFileDownloadRequest(target, int64(len(data)))
	request.ResumeKey = "pick-range-fallback"

	_, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK, ContentLength: -1, Header: make(http.Header),
				Body: io.NopCloser(&p9BytesThenErrorReader{data: []byte("abcd"), err: io.ErrUnexpectedEOF}), Request: req,
			}, nil
		}), nil
	})
	if err == nil {
		t.Fatal("expected interrupted first transfer")
	}

	result, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Range"); got != "bytes=4-" {
				t.Fatalf("expected resume Range before fallback, got %q", got)
			}
			return &http.Response{
				StatusCode: http.StatusOK, ContentLength: int64(len(data)), Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(string(data))), Request: req,
			}, nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ResumedFrom != 0 || result.BytesWritten != int64(len(data)) {
		t.Fatalf("ignored Range did not safely restart from zero: %#v", result)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(data) {
		t.Fatalf("fallback output mismatch: %q err=%v", got, err)
	}
	assertResumeArtifactsRemoved(t, target)
}

func TestDownloadFileRefreshesExpiredSourceAndReplacesHeaders(t *testing.T) {
	target := filepath.Join(t.TempDir(), "refresh.bin")
	request := testFileDownloadRequest(target, 4)
	request.URL = "https://cdn.example.invalid/file?token=old"
	request.Header = http.Header{"User-Agent": []string{""}, "Cookie": []string{"old-cookie"}}
	request.MaxRefreshes = 1
	var refreshCalls atomic.Int32
	request.Refresh = func(context.Context) (DownloadSource, error) {
		refreshCalls.Add(1)
		return DownloadSource{
			URL:    "https://cdn.example.invalid/file?token=new",
			Header: http.Header{"User-Agent": []string{""}, "Cookie": []string{"fresh-cookie"}},
		}, nil
	}

	result, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Query().Get("token") {
			case "old":
				if req.Header.Get("Cookie") != "old-cookie" {
					t.Fatalf("old source header changed: %#v", req.Header)
				}
				return p9StatusResponse(req, http.StatusForbidden), nil
			case "new":
				if req.Header.Get("Cookie") != "fresh-cookie" {
					t.Fatalf("refreshed headers were not replaced: %#v", req.Header)
				}
				if values, ok := req.Header["User-Agent"]; !ok || len(values) != 1 || values[0] != "" {
					t.Fatalf("refreshed empty User-Agent was not preserved: %#v", req.Header["User-Agent"])
				}
				return &http.Response{StatusCode: http.StatusOK, ContentLength: 4, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data")), Request: req}, nil
			default:
				t.Fatalf("unexpected signed source: %s", req.URL)
				return nil, nil
			}
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if refreshCalls.Load() != 1 || result.Refreshes != 1 || result.BytesWritten != 4 {
		t.Fatalf("unexpected refresh result: calls=%d result=%#v", refreshCalls.Load(), result)
	}
}

func TestDownloadFileStopsAtURLRefreshLimitWithoutHealthPenalty(t *testing.T) {
	request := testFileDownloadRequest(filepath.Join(t.TempDir(), "refresh-limit.bin"), 4)
	request.URL = "https://cdn.example.invalid/file?token=old"
	request.MaxRefreshes = 1
	var refreshCalls atomic.Int32
	request.Refresh = func(context.Context) (DownloadSource, error) {
		refreshCalls.Add(1)
		return DownloadSource{URL: "https://cdn.example.invalid/file?token=still-expired"}, nil
	}
	_, err := downloadFile(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return p9StatusResponse(req, http.StatusForbidden), nil
		}), nil
	})
	if !errors.Is(err, ErrDownloadSourceExpired) || !errors.Is(err, ErrDownloadSourceRefresh) {
		t.Fatalf("expected terminal source refresh error, got %v", err)
	}
	if errors.Is(err, ErrNetworkPathFailure) {
		t.Fatalf("source expiration was incorrectly classified as NIC failure: %v", err)
	}
	if refreshCalls.Load() != 1 {
		t.Fatalf("refresh limit was not enforced: calls=%d", refreshCalls.Load())
	}
}

func TestDownloadFileByChunksResumesOnlyIncompleteChunksAcrossCalls(t *testing.T) {
	data := []byte("abcdefgh")
	target := filepath.Join(t.TempDir(), "chunks.bin")
	path := testNetworkPath(1, "10.0.0.1")
	request := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file?token=one", DestinationPath: target,
		NetworkPaths: []NetworkPath{path}, ExpectedSize: int64(len(data)), ChunkSize: 4, Retries: 0,
		ResumeKey: "pick-resume-chunks",
	}

	_, err := downloadFileByChunks(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			start, end := mustParseRequestRange(t, req.Header.Get("Range"))
			if start == 0 {
				return chunkHTTPResponse(req, start, end, int64(len(data)), string(data[start:end+1])), nil
			}
			return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("link interrupted")}
		}), nil
	})
	if !errors.Is(err, ErrChunkDownloadIncomplete) {
		t.Fatalf("expected interrupted chunk transfer, got %v", err)
	}
	partPath, statePath := resumeArtifactPaths(target)
	if info, statErr := os.Stat(partPath); statErr != nil || info.Size() != int64(len(data)) {
		t.Fatalf("chunk part was not retained/preallocated: info=%v err=%v", info, statErr)
	}
	metadata, metaErr := readResumeMetadata(statePath)
	if metaErr != nil || len(metadata.Completed) != 1 || metadata.Completed[0] != 0 {
		t.Fatalf("completed chunk metadata mismatch: %#v err=%v", metadata, metaErr)
	}

	result, err := downloadFileByChunks(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			start, end := mustParseRequestRange(t, req.Header.Get("Range"))
			if start == 0 {
				t.Fatal("completed chunk 0 was downloaded again")
			}
			return chunkHTTPResponse(req, start, end, int64(len(data)), string(data[start:end+1])), nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ResumedChunks != 1 || result.BytesWritten != int64(len(data)) {
		t.Fatalf("unexpected chunk resume result: %#v", result)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(data) {
		t.Fatalf("chunk resumed output mismatch: %q err=%v", got, err)
	}
	assertResumeArtifactsRemoved(t, target)
}

func TestDownloadFileByChunksUsesCentralResumeMetadata(t *testing.T) {
	data := []byte("abcdefgh")
	target := filepath.Join(t.TempDir(), "central-chunks.bin")
	centralState := filepath.Join(t.TempDir(), "managed", "chunk.json")
	path := testNetworkPath(1, "10.0.0.1")
	request := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file", DestinationPath: target,
		NetworkPaths: []NetworkPath{path}, ExpectedSize: int64(len(data)), ChunkSize: 4, Retries: 0,
		ResumeKey: "pick-central-chunks", ResumeStatePath: centralState,
	}
	_, err := downloadFileByChunks(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			start, end := mustParseRequestRange(t, req.Header.Get("Range"))
			if start == 0 {
				return chunkHTTPResponse(req, start, end, int64(len(data)), string(data[start:end+1])), nil
			}
			return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("interrupted")}
		}), nil
	})
	if !errors.Is(err, ErrChunkDownloadIncomplete) {
		t.Fatalf("expected interrupted central chunk transfer, got %v", err)
	}
	partPath, legacyState := resumeArtifactPaths(target)
	if info, statErr := os.Stat(partPath); statErr != nil || info.Size() != int64(len(data)) {
		t.Fatalf("central chunk part not destination-side: info=%v err=%v", info, statErr)
	}
	if metadata, metaErr := readResumeMetadata(centralState); metaErr != nil || len(metadata.Completed) != 1 || metadata.Completed[0] != 0 {
		t.Fatalf("central chunk metadata mismatch: %#v err=%v", metadata, metaErr)
	}
	if _, statErr := os.Stat(legacyState); !os.IsNotExist(statErr) {
		t.Fatalf("legacy chunk metadata should not exist: %v", statErr)
	}

	result, err := downloadFileByChunks(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			start, end := mustParseRequestRange(t, req.Header.Get("Range"))
			if start == 0 {
				t.Fatal("completed central chunk was downloaded again")
			}
			return chunkHTTPResponse(req, start, end, int64(len(data)), string(data[start:end+1])), nil
		}), nil
	})
	if err != nil || result.ResumedChunks != 1 {
		t.Fatalf("central chunk resume failed: result=%#v err=%v", result, err)
	}
	if _, statErr := os.Stat(centralState); !os.IsNotExist(statErr) {
		t.Fatalf("central chunk metadata survived success: %v", statErr)
	}
}

func TestDownloadFileByChunksRecoveryContinuesPersistedIncompleteChunks(t *testing.T) {
	data := []byte("abcdefgh")
	target := filepath.Join(t.TempDir(), "same-process-recovery.bin")
	path := testNetworkPath(1, "10.0.0.1")
	request := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file", DestinationPath: target,
		NetworkPaths: []NetworkPath{path}, ExpectedSize: int64(len(data)), ChunkSize: 4,
		Retries: 0, RecoveryRetries: 1, ResumeKey: "pick-same-process-recovery",
	}
	var failed atomic.Bool
	var firstChunkRequests atomic.Int32
	var secondChunkRequests atomic.Int32
	result, err := downloadFileByChunksWithRecovery(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			start, end := mustParseRequestRange(t, req.Header.Get("Range"))
			if start == 0 {
				firstChunkRequests.Add(1)
			} else {
				secondChunkRequests.Add(1)
				if failed.CompareAndSwap(false, true) {
					return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("interface disappeared")}
				}
			}
			return chunkHTTPResponse(req, start, end, int64(len(data)), string(data[start:end+1])), nil
		}), nil
	}, func(context.Context, int) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesWritten != int64(len(data)) || firstChunkRequests.Load() != 1 || secondChunkRequests.Load() != 2 {
		t.Fatalf("outer recovery did not resume only missing chunks: first=%d second=%d result=%#v", firstChunkRequests.Load(), secondChunkRequests.Load(), result)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != string(data) {
		t.Fatalf("recovered chunk output mismatch: %q err=%v", got, readErr)
	}
}

func TestDownloadFileByChunksRecoveryKeepsRefreshedSourceAndBudget(t *testing.T) {
	data := []byte("abcdefgh")
	target := filepath.Join(t.TempDir(), "recovery-refresh.bin")
	path := testNetworkPath(1, "10.0.0.1")
	request := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file?token=old", DestinationPath: target,
		NetworkPaths: []NetworkPath{path}, ExpectedSize: int64(len(data)), ChunkSize: 4,
		Retries: 0, RecoveryRetries: 1, ResumeKey: "pick-recovery-refresh", MaxRefreshes: 1,
	}
	var refreshCalls atomic.Int32
	request.Refresh = func(context.Context) (DownloadSource, error) {
		refreshCalls.Add(1)
		return DownloadSource{URL: "https://cdn.example.invalid/file?token=new"}, nil
	}
	var failed atomic.Bool
	var oldRequests atomic.Int32
	result, err := downloadFileByChunksWithRecovery(context.Background(), request, func(NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get("token") == "old" {
				oldRequests.Add(1)
				return p9StatusResponse(req, http.StatusForbidden), nil
			}
			start, end := mustParseRequestRange(t, req.Header.Get("Range"))
			if start > 0 && failed.CompareAndSwap(false, true) {
				return nil, &url.Error{Op: "Get", URL: req.URL.String(), Err: errors.New("interface dropped after refresh")}
			}
			return chunkHTTPResponse(req, start, end, int64(len(data)), string(data[start:end+1])), nil
		}), nil
	}, func(context.Context, int) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.BytesWritten != int64(len(data)) || result.Refreshes != 1 || refreshCalls.Load() != 1 || oldRequests.Load() != 1 {
		t.Fatalf("outer recovery reset refreshed source state: result=%#v refreshes=%d old=%d", result, refreshCalls.Load(), oldRequests.Load())
	}
}

func TestDownloadFileByChunksRefreshesExpiredSourceOnceAcrossWorkers(t *testing.T) {
	data := []byte("abcdefgh")
	paths := []NetworkPath{testNetworkPath(1, "10.0.0.1"), testNetworkPath(2, "10.0.0.2")}
	health := NewDefaultNetworkHealthTracker()
	request := ChunkDownloadRequest{
		URL: "https://cdn.example.invalid/file?token=old", Header: http.Header{"Cookie": []string{"old"}},
		DestinationPath: filepath.Join(t.TempDir(), "parallel-refresh.bin"), NetworkPaths: paths,
		ExpectedSize: int64(len(data)), ChunkSize: 4, Retries: 0, HealthTracker: health, MaxRefreshes: 1,
	}
	var refreshCalls atomic.Int32
	request.Refresh = func(context.Context) (DownloadSource, error) {
		refreshCalls.Add(1)
		return DownloadSource{URL: "https://cdn.example.invalid/file?token=new", Header: http.Header{"Cookie": []string{"fresh"}}}, nil
	}

	var oldRequests atomic.Int32
	releaseOld := make(chan struct{})
	var releaseOnce sync.Once
	result, err := downloadFileByChunks(context.Background(), request, func(path NetworkPath) (http.RoundTripper, error) {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get("token") == "old" {
				if oldRequests.Add(1) == int32(len(paths)) {
					releaseOnce.Do(func() { close(releaseOld) })
				}
				select {
				case <-releaseOld:
				case <-time.After(time.Second):
					t.Fatal("chunk workers did not overlap on the stale source")
				}
				return p9StatusResponse(req, http.StatusForbidden), nil
			}
			if req.Header.Get("Cookie") != "fresh" {
				t.Fatalf("worker %d did not switch to refreshed headers: %#v", path.InterfaceIndex, req.Header)
			}
			start, end := mustParseRequestRange(t, req.Header.Get("Range"))
			return chunkHTTPResponse(req, start, end, int64(len(data)), string(data[start:end+1])), nil
		}), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if oldRequests.Load() != 2 || refreshCalls.Load() != 1 || result.Refreshes != 1 {
		t.Fatalf("unexpected concurrent refresh behavior: old=%d refresh=%d result=%#v", oldRequests.Load(), refreshCalls.Load(), result)
	}
	for _, path := range paths {
		snapshot := health.Snapshot(path)
		if snapshot.Failures != 0 || snapshot.InCooldown {
			t.Fatalf("source expiration polluted interface health for %s: %#v", path, snapshot)
		}
	}
}

func TestResumeMetadataHashesStableKeyAndRejectsStaleKey(t *testing.T) {
	target := filepath.Join(t.TempDir(), "secret.bin")
	artifacts, offset, err := openFileResume(target, "pick-super-secret", 8)
	if err != nil || offset != 0 {
		t.Fatalf("open initial resume state: offset=%d err=%v", offset, err)
	}
	if _, err := artifacts.file.Write([]byte("abcd")); err != nil {
		t.Fatal(err)
	}
	artifacts.closeOnFailure()
	_, statePath := resumeArtifactPaths(target)
	stateBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stateBytes), "pick-super-secret") {
		t.Fatalf("resume metadata leaked stable key: %s", stateBytes)
	}

	fresh, offset, err := openFileResume(target, "different-pick", 8)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.closeOnFailure()
	if offset != 0 {
		t.Fatalf("stale resume state from another key was reused: offset=%d", offset)
	}
	if info, err := fresh.file.Stat(); err != nil || info.Size() != 0 {
		t.Fatalf("stale part was not reset: info=%v err=%v", info, err)
	}
}

func TestScheduleFileDownloadsResumesAcrossNetworkInterfaces(t *testing.T) {
	data := []byte("abcdefgh")
	target := filepath.Join(t.TempDir(), "scheduled-resume.bin")
	paths := []NetworkPath{testNetworkPath(1, "10.0.0.1"), testNetworkPath(2, "10.0.0.2")}
	job := FileTransferJob{
		ID: "scheduled-resume", URL: "https://cdn.example.invalid/file?token=one",
		DestinationPath: target, ExpectedSize: int64(len(data)), ResumeKey: "pick-scheduled-resume",
	}

	download := func(ctx context.Context, request FileDownloadRequest) (FileDownloadResult, error) {
		return downloadFile(ctx, request, func(path NetworkPath) (http.RoundTripper, error) {
			return roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch path.InterfaceIndex {
				case 1:
					if got := req.Header.Get("Range"); got != "" {
						t.Fatalf("first interface unexpectedly resumed: %q", got)
					}
					return &http.Response{
						StatusCode: http.StatusOK, ContentLength: -1, Header: make(http.Header),
						Body: io.NopCloser(&p9BytesThenErrorReader{data: []byte("abcd"), err: io.ErrUnexpectedEOF}), Request: req,
					}, nil
				case 2:
					if got := req.Header.Get("Range"); got != "bytes=4-" {
						t.Fatalf("retry did not resume on second interface: %q", got)
					}
					return &http.Response{
						StatusCode: http.StatusPartialContent, ContentLength: 4,
						Header: http.Header{"Content-Range": []string{"bytes 4-7/8"}},
						Body:   io.NopCloser(strings.NewReader("efgh")), Request: req,
					}, nil
				default:
					t.Fatalf("unexpected interface %d", path.InterfaceIndex)
					return nil, nil
				}
			}), nil
		})
	}

	report, err := scheduleFileDownloads(context.Background(), paths, []FileTransferJob{job}, download, WithFileScheduleRetries(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 1 || len(report.Results[0].Attempts) != 2 {
		t.Fatalf("unexpected scheduler resume report: %#v", report)
	}
	attempts := report.Results[0].Attempts
	if attempts[0].NetworkPath.InterfaceIndex != 1 || attempts[1].NetworkPath.InterfaceIndex != 2 || attempts[1].Result.ResumedFrom != 4 {
		t.Fatalf("scheduler did not carry resume state across interfaces: %#v", attempts)
	}
	got, err := os.ReadFile(target)
	if err != nil || string(got) != string(data) {
		t.Fatalf("scheduled resumed output mismatch: %q err=%v", got, err)
	}
	assertResumeArtifactsRemoved(t, target)
}

func p9StatusResponse(req *http.Request, status int) *http.Response {
	return &http.Response{StatusCode: status, ContentLength: 0, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}
}

type p9BytesThenErrorReader struct {
	data []byte
	err  error
}

func (reader *p9BytesThenErrorReader) Read(p []byte) (int, error) {
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

func assertResumeArtifactsRemoved(t *testing.T, target string) {
	t.Helper()
	partPath, statePath := resumeArtifactPaths(target)
	for _, path := range []string{partPath, statePath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("resume artifact was not removed after success: %s err=%v", filepath.Base(path), err)
		}
	}
}
