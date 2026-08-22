package driver

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDownloadInfoGetRejectsInvalidMetadata(t *testing.T) {
	for name, info := range map[string]*DownloadInfo{
		"nil":         nil,
		"invalid-url": {Url: FileDownloadUrl{Url: "https://cdn.example.invalid/file"}},
		"empty-url":   {Url: FileDownloadUrl{Valid: true}},
	} {
		t.Run(name, func(t *testing.T) {
			reader, err := info.Get()
			if reader != nil || !errors.Is(err, ErrUnexpected) {
				t.Fatalf("DownloadInfo.Get invalid metadata = %#v, %v; want nil, ErrUnexpected", reader, err)
			}
		})
	}
}

func TestDownloadInfoGetRejectsHTTPFailureWithoutReturningBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not a file", http.StatusNotFound)
	}))
	defer server.Close()

	reader, err := (&DownloadInfo{Url: FileDownloadUrl{Valid: true, Url: server.URL + "/missing"}}).Get()
	if reader != nil || !errors.Is(err, ErrUnexpected) || !strings.Contains(err.Error(), "404") {
		t.Fatalf("DownloadInfo.Get HTTP failure = %#v, %v; want nil, ErrUnexpected with status", reader, err)
	}
}

func TestDownloadInfoGetSanitizesSignedURLNetworkFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	baseURL := server.URL
	server.Close()

	reader, err := (&DownloadInfo{Url: FileDownloadUrl{Valid: true, Url: baseURL + "/file?download_token=secret"}}).Get()
	if reader != nil || err == nil {
		t.Fatalf("DownloadInfo.Get network failure = %#v, %v; want nil error result", reader, err)
	}
	if strings.Contains(err.Error(), "download_token=secret") {
		t.Fatalf("DownloadInfo.Get leaked signed URL query: %v", err)
	}
}

func TestDownloadInfoGetReturnsSuccessfulBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "payload")
	}))
	defer server.Close()

	reader, err := (&DownloadInfo{Url: FileDownloadUrl{Valid: true, Url: server.URL + "/file"}}).Get()
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "payload" {
		t.Fatalf("DownloadInfo.Get body = %q", body)
	}
}
