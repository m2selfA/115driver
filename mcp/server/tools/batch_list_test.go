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

func TestPrepareMCPOfflinePageBatchRejectsInvalidLogicalPages(t *testing.T) {
	for name, args := range map[string]ListOfflinePagesArgs{
		"empty":     {},
		"duplicate": {Pages: []int64{0, 1}},
		"negative":  {Pages: []int64{-1}},
		"oversized": {Pages: make([]int64, maxMCPOfflineBatchPages+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareMCPOfflinePageBatch(args); err == nil {
				t.Fatal("expected offline page-batch preflight error")
			}
		})
	}
}

func TestListOfflinePagesPreservesOrderContinuesAndOmitsSourceURLs(t *testing.T) {
	const secretURL = "https://downloads.invalid/archive?token=offline-batch-secret"
	calls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodPost {
			t.Fatalf("list_offline_pages used %s, want POST", req.Method)
		}
		page := req.URL.Query().Get("page")
		if page == "2" {
			return nil, errors.New("synthetic offline page failure")
		}
		body := fmt.Sprintf(`{"state":true,"total":2,"count":1,"page_row":1,"page_count":3,"page":%s,"quota":0,"tasks":[{"info_hash":"HASH%s","name":"archive%s","size":7,"url":%q,"add_time":1,"peers":2,"rateDownload":3.5,"status":1,"percentDone":0.5,"last_update":2,"left_time":3,"file_id":"f%s","delete_file_id":"df%s","wp_path_id":"d1","move":0}]}`, page, page, page, secretURL, page, page)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	result, output, err := NewOfflineTools(client).listOfflinePages(context.Background(), nil, ListOfflinePagesArgs{Pages: []int64{1, 2, 3}})
	if err != nil || result == nil || !result.IsError || calls != 3 {
		t.Fatalf("list_offline_pages result=%#v output=%#v err=%v calls=%d", result, output, err, calls)
	}
	if output.Requested != 3 || output.Succeeded != 2 || output.Failed != 1 || output.TasksReturned != 2 || output.BudgetExhausted {
		t.Fatalf("unexpected offline page batch output: %#v", output)
	}
	if output.Items[0].Data == nil || output.Items[0].Data.Tasks[0].InfoHash != "HASH1" || output.Items[0].NextPage == nil || *output.Items[0].NextPage != 2 || output.Items[1].Success || output.Items[2].Data == nil || output.Items[2].Data.Tasks[0].InfoHash != "HASH3" || output.Items[2].NextPage != nil {
		t.Fatalf("offline page batch lost order/continuation: %#v", output.Items)
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, forbidden := range []string{secretURL, "offline-batch-secret", `"url"`} {
		if strings.Contains(string(encoded), forbidden) || strings.Contains(text, forbidden) {
			t.Fatalf("offline page batch leaked %q: typed=%s text=%s", forbidden, encoded, text)
		}
	}
}

func TestPrepareMCPRecyclePageBatchRejectsDuplicatesAndBudgetOverflow(t *testing.T) {
	for name, args := range map[string]ListRecyclePagesArgs{
		"empty":     {},
		"duplicate": {Pages: []ListRecyclePagesItem{{Offset: 0, Limit: 0}, {Offset: 0, Limit: defaultRecycleLimit}}},
		"negative":  {Pages: []ListRecyclePagesItem{{Offset: -1, Limit: 10}}},
		"oversized": {Pages: make([]ListRecyclePagesItem, maxMCPRecycleBatchPages+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareMCPRecyclePageBatch(args); err == nil {
				t.Fatal("expected recycle page-batch preflight error")
			}
		})
	}

	pages := make([]ListRecyclePagesItem, 51)
	for i := range pages {
		pages[i] = ListRecyclePagesItem{Offset: i * maxRecycleLimit, Limit: maxRecycleLimit}
	}
	if _, err := prepareMCPRecyclePageBatch(ListRecyclePagesArgs{Pages: pages}); err == nil || !strings.Contains(err.Error(), "page budget") {
		t.Fatalf("expected recycle page-budget error, got %v", err)
	}
}

func TestListRecyclePagesPreservesOrderContinuesAndReportsCandidateOffset(t *testing.T) {
	calls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet {
			t.Fatalf("list_recycle_pages used %s, want GET", req.Method)
		}
		offset := req.URL.Query().Get("offset")
		limit := req.URL.Query().Get("limit")
		if offset == "2" {
			return nil, errors.New("synthetic recycle page failure")
		}
		items := `[{"id":"r1","file_name":"one.bin","file_size":"1","cid":"0","parent_name":"root","dtime":"1"},{"id":"r2","file_name":"two.bin","file_size":"2","cid":"0","parent_name":"root","dtime":"2"}]`
		if offset == "4" {
			items = `[{"id":"r3","file_name":"three.bin","file_size":"3","cid":"0","parent_name":"root","dtime":"3"}]`
		}
		body := fmt.Sprintf(`{"state":true,"data":%s,"offset":%s,"limit":%s}`, items, offset, limit)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	result, output, err := NewRecycleTools(client).listRecyclePages(context.Background(), nil, ListRecyclePagesArgs{Pages: []ListRecyclePagesItem{{Offset: 0, Limit: 2}, {Offset: 2, Limit: 2}, {Offset: 4, Limit: 2}}})
	if err != nil || result == nil || !result.IsError || calls != 3 {
		t.Fatalf("list_recycle_pages result=%#v output=%#v err=%v calls=%d", result, output, err, calls)
	}
	if output.Requested != 3 || output.Succeeded != 2 || output.Failed != 1 || len(output.Items) != 3 {
		t.Fatalf("unexpected recycle page batch output: %#v", output)
	}
	if output.Items[0].NextOffset == nil || *output.Items[0].NextOffset != 2 || output.Items[1].Success || output.Items[2].Returned != 1 || output.Items[2].NextOffset != nil {
		t.Fatalf("unexpected recycle page continuation/order: %#v", output.Items)
	}
}
