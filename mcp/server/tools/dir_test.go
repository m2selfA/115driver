package tools

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestValidateDirectoryListPaginationPreservesOffsetWithUnpaginatedDefault(t *testing.T) {
	offset, limit, err := validateDirectoryListPagination(25, 0)
	if err != nil {
		t.Fatal(err)
	}
	if offset != 25 || limit != 0 {
		t.Fatalf("unexpected pagination: offset=%d limit=%d", offset, limit)
	}
}

func TestValidateDirectoryListPaginationRejectsInvalidValues(t *testing.T) {
	for name, values := range map[string][2]int64{
		"negative-offset": {-1, 0},
		"negative-limit":  {0, -1},
		"oversized-limit": {0, maxDirectoryListLimit + 1},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := validateDirectoryListPagination(values[0], values[1]); err == nil {
				t.Fatal("expected invalid pagination error")
			}
		})
	}
}

func TestListDirectoryRejectsInvalidPaginationBeforeNetwork(t *testing.T) {
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid listDirectory pagination reached network: %s", req.URL)
		return nil, fmt.Errorf("unreachable")
	})})))
	dt := NewDirTools(client)
	for _, args := range []ListDirectoryArgs{
		{DirID: "0", Offset: -1},
		{DirID: "0", Limit: -1},
		{DirID: "0", Limit: maxDirectoryListLimit + 1},
	} {
		result, _, err := dt.listDirectory(context.Background(), nil, args)
		if err != nil || result == nil || !result.IsError {
			t.Fatalf("invalid listDirectory pagination result=%#v err=%v args=%#v", result, err, args)
		}
	}
}

func TestListDirectoryPreservesOffsetWhenLimitIsOmitted(t *testing.T) {
	calls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		q := req.URL.Query()
		if q.Get("record_open_time") != "0" {
			t.Fatalf("unpaged offset read did not disable record_open_time: %s", req.URL)
		}
		body := `{"state":true,"cid":"0","count":3,"offset":0,"limit":1150,"data":[{"fid":"f1","cid":"0","n":"one","s":"1"},{"fid":"f2","cid":"0","n":"two","s":"2"},{"fid":"f3","cid":"0","n":"three","s":"3"}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	result, output, err := NewDirTools(client).listDirectory(context.Background(), nil, ListDirectoryArgs{DirID: "0", Offset: 1})
	if err != nil || result == nil || result.IsError || calls != 1 {
		t.Fatalf("unpaged offset list result=%#v output=%#v err=%v calls=%d", result, output, err, calls)
	}
	if len(output.Entries) != 2 || output.Entries[0].FileID != "f2" || output.Entries[1].FileID != "f3" {
		t.Fatalf("unpaged offset was not applied: %#v", output.Entries)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, `"FileID":"f1"`) || !strings.Contains(text, `"FileID":"f2"`) {
		t.Fatalf("legacy text did not preserve offset semantics: %s", text)
	}
}

func TestListDirectoryLegacyTextRedactsThumbnailURL(t *testing.T) {
	const secretURL = "https://thumb.example.invalid/image?token=thumbnail-secret"
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"state":true,"cid":"0","count":1,"offset":0,"limit":1,"data":[{"fid":"f1","cid":"0","n":"one.bin","s":"1","u":"` + secretURL + `"}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))
	dt := NewDirTools(client)
	result, output, err := dt.listDirectory(context.Background(), nil, ListDirectoryArgs{DirID: "0", Limit: 1})
	if err != nil || result == nil || result.IsError || len(output.Entries) != 1 {
		t.Fatalf("listDirectory thumbnail redaction = %#v output=%#v err=%v", result, output, err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, secretURL) || strings.Contains(text, "thumbnail-secret") {
		t.Fatalf("listDirectory legacy text leaked thumbnail URL: %s", text)
	}
	if strings.Contains(output.Entries[0].UpdateTime, "thumbnail-secret") {
		t.Fatalf("unexpected typed-output contamination: %#v", output)
	}
}

func TestListDirectoryDisablesRecordOpenTimeForPagedAndUnpagedReads(t *testing.T) {
	for _, tc := range []struct {
		name string
		args ListDirectoryArgs
	}{
		{name: "unpaged", args: ListDirectoryArgs{DirID: "0"}},
		{name: "paged", args: ListDirectoryArgs{DirID: "0", Limit: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Method != http.MethodGet {
					t.Fatalf("listDirectory used %s, want GET", req.Method)
				}
				q := req.URL.Query()
				if got := q.Get("record_open_time"); got != "0" {
					t.Fatalf("listDirectory record_open_time = %q, want 0", got)
				}
				offset := q.Get("offset")
				if offset == "" {
					offset = "0"
				}
				limit := q.Get("limit")
				if limit == "" {
					limit = "1150"
				}
				body := fmt.Sprintf(`{"state":true,"cid":"0","count":0,"offset":%s,"limit":%s,"data":[]}`, offset, limit)
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(body)),
					Request:    req,
				}, nil
			})})))

			dt := NewDirTools(client)
			result, _, err := dt.listDirectory(context.Background(), nil, tc.args)
			if err != nil || result == nil || result.IsError {
				t.Fatalf("listDirectory(%s) = %#v, %v", tc.name, result, err)
			}
			if calls != 1 {
				t.Fatalf("listDirectory(%s) calls = %d, want 1", tc.name, calls)
			}
		})
	}
}
