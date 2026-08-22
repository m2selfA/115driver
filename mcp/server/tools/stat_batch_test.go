package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStatManyRejectsInvalidBatchBeforeClientUse(t *testing.T) {
	ft := NewFileTools(nil)
	for name, args := range map[string]StatManyArgs{
		"empty":     {},
		"duplicate": {FileIDs: []string{"1", " 1 "}},
		"blank":     {FileIDs: []string{"1", "   "}},
		"oversized": {FileIDs: make([]string, maxMCPFileBatchItems+1)},
	} {
		t.Run(name, func(t *testing.T) {
			result, _, err := ft.statMany(context.Background(), nil, args)
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("stat_many invalid batch = %#v, %v", result, err)
			}
		})
	}
}

func TestStatManyPreservesOrderAndContinuesAfterLookupFailure(t *testing.T) {
	calls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet || req.URL.Path != "/category/get" {
			t.Fatalf("unexpected stat_many request: %s %s", req.Method, req.URL)
		}
		id := req.URL.Query().Get("cid")
		if id == "bad" {
			return nil, errors.New("synthetic stat failure")
		}
		body := fmt.Sprintf(`{"file_name":%q,"pick_code":%q,"sha1":%q,"file_category":"1","count":"0","folder_count":"0","ptime":"1","utime":"2","paths":[{"file_id":"0","file_name":"root"}]}`, "file-"+id, "pick-"+id, "sha-"+id)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})})))
	ft := NewFileTools(client)
	result, _, err := ft.statMany(context.Background(), nil, StatManyArgs{FileIDs: []string{"a", "bad", "c"}})
	if err != nil || result == nil || !result.IsError || len(result.Content) != 1 || calls != 3 {
		t.Fatalf("stat_many result=%#v err=%v calls=%d", result, err, calls)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected stat_many content: %#v", result.Content[0])
	}
	var decoded StatManyResult
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != 3 || decoded.Succeeded != 2 || decoded.Failed != 1 || len(decoded.Items) != 3 {
		t.Fatalf("unexpected stat_many summary: %#v", decoded)
	}
	if decoded.Items[0].FileID != "a" || !decoded.Items[0].Success || decoded.Items[0].Data == nil || decoded.Items[0].Data.Name != "file-a" {
		t.Fatalf("unexpected first stat_many item: %#v", decoded.Items[0])
	}
	if decoded.Items[1].FileID != "bad" || decoded.Items[1].Success || !strings.Contains(decoded.Items[1].Error, "synthetic stat failure") {
		t.Fatalf("unexpected failed stat_many item: %#v", decoded.Items[1])
	}
	if decoded.Items[2].FileID != "c" || !decoded.Items[2].Success || decoded.Items[2].Data == nil || decoded.Items[2].Data.PickCode != "pick-c" {
		t.Fatalf("unexpected final stat_many item: %#v", decoded.Items[2])
	}
}
