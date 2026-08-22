package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPrepareMCPOfflinePageBatchRejectsInvalidAndDuplicatePages(t *testing.T) {
	for name, args := range map[string]ListOfflinePagesArgs{
		"empty":     {},
		"oversized": {Pages: make([]int64, maxMCPOfflineBatchPages+1)},
		"negative":  {Pages: []int64{-1}},
		"duplicate": {Pages: []int64{0, 1}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareMCPOfflinePageBatch(args); err == nil {
				t.Fatal("expected offline page batch preflight error")
			}
		})
	}
	pages, err := prepareMCPOfflinePageBatch(ListOfflinePagesArgs{Pages: []int64{0, 2, 3}})
	if err != nil || len(pages) != 3 || pages[0] != 1 {
		t.Fatalf("offline page normalization = %#v, %v", pages, err)
	}
}

func TestListOfflinePagesPreservesOrderContinuesAndNeverEchoesSourceURL(t *testing.T) {
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
		secret := "https://example.invalid/source?token=offline-page-secret-" + page
		body := fmt.Sprintf(`{"state":true,"total":3,"count":1,"page_row":1,"page_count":3,"page":%s,"quota":0,"tasks":[{"info_hash":"H%s","name":"task-%s","size":7,"url":%q,"status":1}]}`, page, page, page, secret)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	result, output, err := NewOfflineTools(client).listOfflinePages(context.Background(), nil, ListOfflinePagesArgs{Pages: []int64{1, 2, 3}})
	if err != nil || result == nil || !result.IsError || calls != 3 {
		t.Fatalf("list_offline_pages result=%#v output=%#v err=%v calls=%d", result, output, err, calls)
	}
	if output.Requested != 3 || output.Succeeded != 2 || output.Failed != 1 || output.TasksReturned != 2 || output.BudgetExhausted {
		t.Fatalf("unexpected offline page output: %#v", output)
	}
	if !output.Items[0].Success || output.Items[0].Data == nil || output.Items[0].Data.Tasks[0].InfoHash != "H1" {
		t.Fatalf("unexpected first offline page: %#v", output.Items[0])
	}
	if output.Items[1].Success || !strings.Contains(output.Items[1].Error, "synthetic offline page failure") {
		t.Fatalf("unexpected failed offline page: %#v", output.Items[1])
	}
	if !output.Items[2].Success || output.Items[2].Data == nil || output.Items[2].Data.Tasks[0].InfoHash != "H3" {
		t.Fatalf("unexpected final offline page: %#v", output.Items[2])
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, forbidden := range []string{"offline-page-secret", "https://example.invalid", `"url"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("offline batch text leaked %q: %s", forbidden, text)
		}
	}
	var decoded ListOfflinePagesResult
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != output.Requested || decoded.Items[2].Data.Tasks[0].InfoHash != output.Items[2].Data.Tasks[0].InfoHash {
		t.Fatalf("text/typed offline outputs diverged: text=%#v typed=%#v", decoded, output)
	}
}

func TestListOfflinePagesStopsBeforeNextRequestWhenTaskBudgetIsExactlyFull(t *testing.T) {
	calls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Query().Get("page") != "1" {
			t.Fatalf("offline page requested after task budget was full: %s", req.URL)
		}
		var b strings.Builder
		b.WriteString(`{"state":true,"total":5000,"count":5000,"page_row":5000,"page_count":1,"page":1,"quota":0,"tasks":[`)
		for i := 0; i < maxMCPOfflineBatchTasks; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"info_hash":"H`)
			b.WriteString(strconv.Itoa(i))
			b.WriteString(`","status":2}`)
		}
		b.WriteString(`]}`)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(b.String())), Request: req}, nil
	})})))

	result, output, err := NewOfflineTools(client).listOfflinePages(context.Background(), nil, ListOfflinePagesArgs{Pages: []int64{1, 2}})
	if err != nil || result == nil || !result.IsError || calls != 1 {
		t.Fatalf("offline budget result=%#v output=%#v err=%v calls=%d", result, output, err, calls)
	}
	if output.TasksReturned != maxMCPOfflineBatchTasks || !output.BudgetExhausted || output.Succeeded != 1 || output.Failed != 1 {
		t.Fatalf("unexpected offline budget output: %#v", output)
	}
	if output.Items[1].Success || !strings.Contains(output.Items[1].Error, "budget exhausted") {
		t.Fatalf("later offline page was not budget-skipped: %#v", output.Items[1])
	}
}
