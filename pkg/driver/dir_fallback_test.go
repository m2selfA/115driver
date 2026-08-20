package driver

import (
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestDefaultListOptionsUseHTTPSAPSFallbacks(t *testing.T) {
	got := DefaultListOptions().ApiURLs
	want := []string{ApiFileListByName, ApiFileList}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default list endpoints=%v want=%v", got, want)
	}
	multi := DefaultListOptions()
	WithMultiUrls()(multi)
	if !reflect.DeepEqual(multi.ApiURLs, want) {
		t.Fatalf("multi list endpoints=%v want=%v", multi.ApiURLs, want)
	}
	for _, endpoint := range got {
		if !strings.HasPrefix(endpoint, "https://") {
			t.Fatalf("default list endpoint must be HTTPS: %q", endpoint)
		}
		if endpoint == ApiFileList1 {
			t.Fatalf("default list endpoints must not expose credentials over legacy HTTP: %q", endpoint)
		}
	}
}

type listRoundTripFunc func(*http.Request) (*http.Response, error)

func (f listRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestGetFilesUsesAPSCompatibleOrdering(t *testing.T) {
	transport := listRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasPrefix(req.URL.String(), ApiFileListByName) {
			t.Fatalf("unexpected URL: %s", req.URL)
		}
		q := req.URL.Query()
		if q.Get("cid") != "42" || q.Get("o") != FileOrderByName || q.Get("natsort") != "1" {
			t.Fatalf("unexpected APS query: %s", req.URL.RawQuery)
		}
		body := `{"state":true,"cid":"42","count":1,"offset":0,"limit":20,"data":[{"fid":"7","cid":"42","n":"file.bin","s":"3","sha":"ABC","pc":"pick"}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	client := New(UA(UADefault), WithClient(&http.Client{Transport: transport}))
	result, err := GetFiles(client.NewRequest(), "42", WithApiURL(ApiFileListByName), WithLimit(20), WithOffset(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0].Name != "file.bin" {
		t.Fatalf("unexpected APS result: %#v", result.Files)
	}
}

func TestListPageFallsBackToNextEndpoint(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("<html><title>405</title></html>"))
	}))
	defer primary.Close()

	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":true,"cid":"42","count":1,"offset":0,"limit":100,"data":[{"fid":"7","cid":"42","n":"file.bin","s":"3","sha":"ABC","pc":"pick"}]}`))
	}))
	defer fallback.Close()

	client := New(UA(UADefault))
	files, err := client.ListPage("42", 0, 100, WithApiURLs(primary.URL, fallback.URL))
	if err != nil {
		t.Fatal(err)
	}
	if len(*files) != 1 || (*files)[0].Name != "file.bin" || (*files)[0].Sha1 != "ABC" {
		t.Fatalf("unexpected fallback files: %#v", *files)
	}
}

func TestListWithLimitRestartsWholeListingOnEndpointFailure(t *testing.T) {
	var primaryMu sync.Mutex
	primaryOffsets := make([]string, 0)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		primaryMu.Lock()
		primaryOffsets = append(primaryOffsets, offset)
		primaryMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if offset == "0" {
			_, _ = w.Write([]byte(`{"state":true,"cid":"42","count":3,"offset":0,"limit":2,"data":[{"fid":"old1","cid":"42","n":"old-a"},{"fid":"old2","cid":"42","n":"old-b"}]}`))
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte("blocked"))
	}))
	defer primary.Close()

	var fallbackMu sync.Mutex
	fallbackOffsets := make([]string, 0)
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		fallbackMu.Lock()
		fallbackOffsets = append(fallbackOffsets, offset)
		fallbackMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch offset {
		case "0":
			_, _ = w.Write([]byte(`{"state":true,"cid":"42","count":3,"offset":0,"limit":2,"data":[{"fid":"1","cid":"42","n":"a"},{"fid":"2","cid":"42","n":"b"}]}`))
		case "2":
			_, _ = w.Write([]byte(`{"state":true,"cid":"42","count":3,"offset":2,"limit":2,"data":[{"fid":"3","cid":"42","n":"c"}]}`))
		default:
			t.Fatalf("unexpected fallback offset %q", offset)
		}
	}))
	defer fallback.Close()

	client := New(UA(UADefault))
	files, err := client.ListWithLimit("42", 2, WithApiURLs(primary.URL, fallback.URL))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, file := range *files {
		names = append(names, file.Name)
	}
	if !reflect.DeepEqual(names, []string{"a", "b", "c"}) {
		t.Fatalf("listing mixed endpoints instead of restarting: %v", names)
	}
	if !reflect.DeepEqual(primaryOffsets, []string{"0", "2"}) {
		t.Fatalf("primary offsets=%v", primaryOffsets)
	}
	if !reflect.DeepEqual(fallbackOffsets, []string{"0", "2"}) {
		t.Fatalf("fallback offsets=%v", fallbackOffsets)
	}
}

func TestGetFilesRejectsDirectoryMismatch(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":true,"cid":"99","count":0,"offset":0,"data":[]}`))
	}))
	defer server.Close()

	client := New(UA(UADefault))
	_, err := GetFiles(client.NewRequest(), "42", WithApiURL(server.URL), WithLimit(10), WithOffset(0))
	if err == nil || !strings.Contains(err.Error(), "directory mismatch") {
		t.Fatalf("expected directory mismatch, got %v", err)
	}
}
