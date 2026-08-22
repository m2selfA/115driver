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

func TestPrepareMCPRecyclePageBatchRejectsInvalidRequestsBeforeClientUse(t *testing.T) {
	for name, args := range map[string]ListRecyclePagesArgs{
		"empty":      {},
		"oversized":  {Pages: make([]ListRecyclePagesItem, maxMCPRecycleBatchPages+1)},
		"negative":   {Pages: []ListRecyclePagesItem{{Offset: -1}}},
		"over-limit": {Pages: []ListRecyclePagesItem{{Limit: maxRecycleLimit + 1}}},
		"duplicate":  {Pages: []ListRecyclePagesItem{{Offset: 0, Limit: 0}, {Offset: 0, Limit: defaultRecycleLimit}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareMCPRecyclePageBatch(args); err == nil {
				t.Fatal("expected recycle batch preflight error")
			}
		})
	}

	tooLarge := ListRecyclePagesArgs{Pages: make([]ListRecyclePagesItem, 51)}
	for i := range tooLarge.Pages {
		tooLarge.Pages[i] = ListRecyclePagesItem{Offset: i * 100, Limit: 100}
	}
	tooLarge.Pages = append(tooLarge.Pages, ListRecyclePagesItem{Offset: 5100, Limit: 1})
	if _, err := prepareMCPRecyclePageBatch(tooLarge); err == nil || !strings.Contains(err.Error(), "page budget") {
		t.Fatalf("expected recycle page-budget error, got %v", err)
	}
}

func TestListRecyclePagesPreservesOrderContinuesAndReportsConservativeNextOffset(t *testing.T) {
	calls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet {
			t.Fatalf("list_recycle_pages used %s, want GET", req.Method)
		}
		offset := req.URL.Query().Get("offset")
		limit := req.URL.Query().Get("limit")
		if offset == "40" {
			return nil, errors.New("synthetic recycle page failure")
		}
		count := 1
		if limit == "2" && offset == "0" {
			count = 2
		}
		items := make([]string, count)
		for i := range items {
			items[i] = fmt.Sprintf(`{"id":"r-%s-%d","file_name":"f%d","file_size":"1","cid":"0","parent_name":"root","dtime":"2"}`, offset, i, i)
		}
		body := `{"state":true,"data":[` + strings.Join(items, ",") + `]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	result, output, err := NewRecycleTools(client).listRecyclePages(context.Background(), nil, ListRecyclePagesArgs{Pages: []ListRecyclePagesItem{
		{Offset: 0, Limit: 2},
		{Offset: 40, Limit: 1},
		{Offset: 80, Limit: 3},
	}})
	if err != nil || result == nil || !result.IsError || calls != 3 {
		t.Fatalf("list_recycle_pages result=%#v output=%#v err=%v calls=%d", result, output, err, calls)
	}
	if output.Requested != 3 || output.Succeeded != 2 || output.Failed != 1 || len(output.Items) != 3 {
		t.Fatalf("unexpected recycle batch output: %#v", output)
	}
	if !output.Items[0].Success || output.Items[0].Returned != 2 || output.Items[0].NextOffset == nil || *output.Items[0].NextOffset != 2 {
		t.Fatalf("unexpected first recycle page: %#v", output.Items[0])
	}
	if output.Items[1].Success || !strings.Contains(output.Items[1].Error, "synthetic recycle page failure") {
		t.Fatalf("unexpected failed recycle page: %#v", output.Items[1])
	}
	if !output.Items[2].Success || output.Items[2].Returned != 1 || output.Items[2].NextOffset != nil {
		t.Fatalf("unexpected final recycle page: %#v", output.Items[2])
	}
	var decoded ListRecyclePagesResult
	if err := json.Unmarshal([]byte(result.Content[0].(*mcp.TextContent).Text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != output.Requested || decoded.Items[0].Items[0].ID != output.Items[0].Items[0].ID {
		t.Fatalf("text/typed recycle batch outputs diverged: text=%#v typed=%#v", decoded, output)
	}
}
