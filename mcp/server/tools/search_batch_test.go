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

func TestPrepareMCPSearchBatchRejectsInvalidRequestsBeforeClientUse(t *testing.T) {
	for name, args := range map[string]SearchManyArgs{
		"empty":           {},
		"oversized":       {Queries: make([]SearchManyItem, maxMCPSearchBatchItems+1)},
		"negative-offset": {Queries: []SearchManyItem{{SearchValue: "a", Offset: -1}}},
		"negative-limit":  {Queries: []SearchManyItem{{SearchValue: "a", Limit: -1}}},
		"oversized-page":  {Queries: []SearchManyItem{{SearchValue: "a", Limit: maxMCPSearchBatchLimit + 1}}},
		"invalid-type":    {Queries: []SearchManyItem{{SearchValue: "a", Type: 7}}},
		"invalid-asc":     {Queries: []SearchManyItem{{SearchValue: "a", Asc: 2}}},
		"duplicate-defaults": {Queries: []SearchManyItem{
			{SearchValue: "same", Limit: 0, Order: ""},
			{SearchValue: "same", Limit: defaultMCPSearchLimit, Order: "file_name"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareMCPSearchBatch(args); err == nil {
				t.Fatal("expected search batch preflight error")
			}
		})
	}

	tooLarge := SearchManyArgs{Queries: make([]SearchManyItem, 51)}
	for i := range tooLarge.Queries {
		tooLarge.Queries[i] = SearchManyItem{SearchValue: fmt.Sprintf("q-%d", i), Limit: 100}
	}
	if _, err := prepareMCPSearchBatch(tooLarge); err == nil || !strings.Contains(err.Error(), "page budget") {
		t.Fatalf("expected aggregate page-budget error, got %v", err)
	}
}

func TestPrepareMCPSearchBatchAllowsDifferentPagesForSameSearch(t *testing.T) {
	prepared, err := prepareMCPSearchBatch(SearchManyArgs{Queries: []SearchManyItem{
		{SearchValue: "same", Offset: 0, Limit: 10},
		{SearchValue: "same", Offset: 10, Limit: 10},
		{SearchValue: "default-limit"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 3 || prepared[2].limit != defaultMCPSearchLimit {
		t.Fatalf("unexpected prepared search batch: %#v", prepared)
	}
}

func TestSearchManyPreservesOrderContinuesAfterFailureAndReportsContinuation(t *testing.T) {
	calls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet {
			t.Fatalf("search_many used %s, want GET", req.Method)
		}
		q := req.URL.Query()
		keyword := q.Get("search_value")
		if keyword == "bad" {
			return nil, errors.New("synthetic search failure")
		}
		offset := q.Get("offset")
		limit := q.Get("limit")
		count := "3"
		if keyword == "tail" {
			count = "2"
		}
		body := fmt.Sprintf(`{"state":true,"count":%s,"offset":%s,"page_size":%s,"order":"file_name","is_asc":0,"data":[{"fid":%q,"cid":"0","n":%q,"s":"7","pc":%q,"sha":"ABC"}]}`, count, offset, limit, "f-"+keyword, "file-"+keyword, "pick-"+keyword)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	st := NewSearchTools(client)
	result, output, err := st.searchMany(context.Background(), nil, SearchManyArgs{Queries: []SearchManyItem{
		{SearchValue: "head", Offset: 0, Limit: 1},
		{SearchValue: "bad", Offset: 0, Limit: 2},
		{SearchValue: "tail", Offset: 1, Limit: 2},
	}})
	if err != nil || result == nil || !result.IsError || calls != 3 {
		t.Fatalf("search_many result=%#v output=%#v err=%v calls=%d", result, output, err, calls)
	}
	if output.Requested != 3 || output.Succeeded != 2 || output.Failed != 1 || len(output.Items) != 3 {
		t.Fatalf("unexpected search_many output: %#v", output)
	}
	if !output.Items[0].Success || output.Items[0].Data == nil || output.Items[0].Data.Files[0].Name != "file-head" || output.Items[0].NextOffset == nil || *output.Items[0].NextOffset != 1 {
		t.Fatalf("unexpected first search item: %#v", output.Items[0])
	}
	if output.Items[1].Success || !strings.Contains(output.Items[1].Error, "synthetic search failure") {
		t.Fatalf("unexpected failed search item: %#v", output.Items[1])
	}
	if !output.Items[2].Success || output.Items[2].Data == nil || output.Items[2].Data.Offset != 1 || output.Items[2].NextOffset != nil {
		t.Fatalf("unexpected final search item: %#v", output.Items[2])
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var decoded SearchManyResult
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != output.Requested || decoded.Items[0].Data.Files[0].FileID != output.Items[0].Data.Files[0].FileID {
		t.Fatalf("text/typed search_many outputs diverged: text=%#v typed=%#v", decoded, output)
	}
}

func TestSearchManyReturnsContinuationForShortNonTerminalPage(t *testing.T) {
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"state":true,"count":10,"offset":3,"page_size":5,"order":"file_name","is_asc":0,"data":[{"fid":"f1","cid":"0","n":"one.bin","s":"1","pc":"pick1","sha":"ABC"}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	result, output, err := NewSearchTools(client).searchMany(context.Background(), nil, SearchManyArgs{Queries: []SearchManyItem{{SearchValue: "one", Offset: 3, Limit: 5}}})
	if err != nil || result == nil || result.IsError || len(output.Items) != 1 {
		t.Fatalf("short search page result=%#v output=%#v err=%v", result, output, err)
	}
	item := output.Items[0]
	if item.Returned != 1 || item.NextOffset == nil || *item.NextOffset != 4 {
		t.Fatalf("short non-terminal search page lost continuation: %#v", item)
	}
}

func TestSearchManyWirePopulatesStructuredContent(t *testing.T) {
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"state":true,"count":1,"offset":0,"page_size":1,"order":"file_name","is_asc":0,"data":[{"fid":"f1","cid":"0","n":"one.bin","s":"1","pc":"pick1","sha":"ABC"}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	server := mcp.NewServer(&mcp.Implementation{Name: "search-batch-test", Version: "1"}, nil)
	NewSearchTools(client).RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "search-batch-client", Version: "1"}, nil)
	clientSession, err := mcpClient.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "search_many",
		Arguments: map[string]any{"queries": []any{map[string]any{"search_value": "one", "limit": 1}}},
	})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil {
		t.Fatalf("wire search_many result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SearchManyResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != 1 || decoded.Succeeded != 1 || len(decoded.Items) != 1 || decoded.Items[0].Data == nil || decoded.Items[0].Data.Files[0].Name != "one.bin" {
		t.Fatalf("unexpected structured search_many content: %#v", decoded)
	}
}
