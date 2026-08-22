package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestResolvePathsRejectsInvalidBatchBeforeClientUse(t *testing.T) {
	dt := NewDirTools(nil)
	for name, args := range map[string]ResolvePathsArgs{
		"empty":     {},
		"blank":     {Paths: []string{"/", "   "}},
		"duplicate": {Paths: []string{"/folder/", "folder"}},
		"oversized": {Paths: make([]string, maxMCPResolvePaths+1)},
	} {
		t.Run(name, func(t *testing.T) {
			result, _, err := dt.resolvePaths(context.Background(), nil, args)
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("resolve_paths invalid batch = %#v, %v", result, err)
			}
		})
	}
}

func TestResolvePathsPreservesOrderUsesReadOnlyFileLookupAndContinues(t *testing.T) {
	listCalls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatalf("resolve_paths used %s, want GET", req.Method)
		}
		if req.URL.Path == "/files/getid" {
			remotePath := req.URL.Query().Get("path")
			switch remotePath {
			case "folder":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"42"}`), nil
			case "target.bin":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
			case "bad":
				return nil, errors.New("synthetic resolver failure")
			case "after":
				return mcpResolveJSONResponse(req, `{"state":true,"id":"43"}`), nil
			default:
				t.Fatalf("unexpected getid path %q", remotePath)
			}
		}

		listCalls++
		q := req.URL.Query()
		if q.Get("cid") != "0" || q.Get("record_open_time") != "0" {
			t.Fatalf("resolve_paths file lookup lost read-only root listing contract: %s", req.URL)
		}
		body := `{"state":true,"cid":"0","count":1,"offset":0,"limit":100,"data":[{"fid":"f1","cid":"0","n":"target.bin","s":"7","pc":"pick1","sha":"ABC"}]}`
		return mcpResolveJSONResponse(req, body), nil
	})})))

	dt := NewDirTools(client)
	result, output, err := dt.resolvePaths(context.Background(), nil, ResolvePathsArgs{Paths: []string{"/", "folder", "target.bin", "bad", "after"}})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("resolve_paths result=%#v output=%#v err=%v", result, output, err)
	}
	if listCalls != 1 {
		t.Fatalf("resolve_paths file-list calls=%d, want 1", listCalls)
	}
	if output.Requested != 5 || output.Succeeded != 4 || output.Failed != 1 || len(output.Items) != 5 {
		t.Fatalf("unexpected resolve_paths summary: %#v", output)
	}
	if !output.Items[0].Success || output.Items[0].FileID != "0" || !output.Items[0].IsDirectory {
		t.Fatalf("unexpected root result: %#v", output.Items[0])
	}
	if !output.Items[1].Success || output.Items[1].FileID != "42" || !output.Items[1].IsDirectory {
		t.Fatalf("unexpected directory result: %#v", output.Items[1])
	}
	if !output.Items[2].Success || output.Items[2].FileID != "f1" || output.Items[2].IsDirectory {
		t.Fatalf("unexpected file result: %#v", output.Items[2])
	}
	if output.Items[3].Success || !strings.Contains(output.Items[3].Error, "synthetic resolver failure") {
		t.Fatalf("unexpected failed result: %#v", output.Items[3])
	}
	if !output.Items[4].Success || output.Items[4].FileID != "43" {
		t.Fatalf("resolve_paths did not continue after failure: %#v", output.Items[4])
	}

	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected resolve_paths content: %#v", result.Content[0])
	}
	var decoded ResolvePathsResult
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != output.Requested || decoded.Items[2].FileID != output.Items[2].FileID {
		t.Fatalf("text/typed resolve_paths outputs diverged: text=%#v typed=%#v", decoded, output)
	}
}

func TestResolvePathsReusesParentDirectoryPagesWithinOneBatch(t *testing.T) {
	listCalls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/files/getid" {
			return mcpResolveJSONResponse(req, `{"state":true,"id":"0"}`), nil
		}
		listCalls++
		body := `{"state":true,"cid":"0","count":2,"offset":0,"limit":100,"data":[{"fid":"f1","cid":"0","n":"a.bin","s":"1"},{"fid":"f2","cid":"0","n":"b.bin","s":"2"}]}`
		return mcpResolveJSONResponse(req, body), nil
	})})))

	dt := NewDirTools(client)
	result, output, err := dt.resolvePaths(context.Background(), nil, ResolvePathsArgs{Paths: []string{"a.bin", "b.bin"}})
	if err != nil || result == nil || result.IsError || output.Succeeded != 2 {
		t.Fatalf("resolve_paths shared-parent result=%#v output=%#v err=%v", result, output, err)
	}
	if listCalls != 1 {
		t.Fatalf("same parent directory page was fetched %d times, want 1 request-scoped snapshot", listCalls)
	}
	if output.Items[0].FileID != "f1" || output.Items[1].FileID != "f2" {
		t.Fatalf("unexpected shared-parent resolutions: %#v", output.Items)
	}
}

func mcpResolveJSONResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}
