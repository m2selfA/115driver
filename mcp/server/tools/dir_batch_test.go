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

func TestPrepareMCPDirectoryBatchRejectsInvalidRequestsBeforeClientUse(t *testing.T) {
	for name, args := range map[string]ListDirectoriesArgs{
		"empty":                     {},
		"oversized":                 {Directories: make([]ListDirectoriesItem, maxMCPDirectoryBatchItems+1)},
		"negative-offset":           {Directories: []ListDirectoriesItem{{DirID: "1", Offset: -1}}},
		"oversized-page":            {Directories: []ListDirectoriesItem{{DirID: "1", Limit: maxDirectoryListLimit + 1}}},
		"duplicate-normalized-root": {Directories: []ListDirectoriesItem{{DirID: "", Limit: 20}, {DirID: " 0 ", Limit: 20}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareMCPDirectoryBatch(args); err == nil {
				t.Fatal("expected batch preflight error")
			}
		})
	}

	tooLarge := ListDirectoriesArgs{Directories: make([]ListDirectoriesItem, 51)}
	for i := range tooLarge.Directories {
		tooLarge.Directories[i] = ListDirectoriesItem{DirID: fmt.Sprintf("d-%d", i), Limit: 100}
	}
	tooLarge.Directories = append(tooLarge.Directories, ListDirectoriesItem{DirID: "overflow", Limit: 1})
	if _, err := prepareMCPDirectoryBatch(tooLarge); err == nil || !strings.Contains(err.Error(), "page budget") {
		t.Fatalf("expected aggregate page-budget error, got %v", err)
	}
}

func TestPrepareMCPDirectoryBatchAllowsDifferentPagesForSameDirectory(t *testing.T) {
	prepared, err := prepareMCPDirectoryBatch(ListDirectoriesArgs{Directories: []ListDirectoriesItem{
		{DirID: "same", Offset: 0, Limit: 10},
		{DirID: "same", Offset: 10, Limit: 10},
		{DirID: "root-default"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared) != 3 || prepared[2].limit != defaultMCPDirectoryBatchLimit {
		t.Fatalf("unexpected prepared batch: %#v", prepared)
	}
}

func TestListDirectoriesUsesReadOnlyPaginationPreservesOrderAndContinues(t *testing.T) {
	calls := 0
	badCalls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet {
			t.Fatalf("list_directories used %s, want GET", req.Method)
		}
		q := req.URL.Query()
		if got := q.Get("record_open_time"); got != "0" {
			t.Fatalf("list_directories record_open_time = %q, want 0", got)
		}
		cid := q.Get("cid")
		if cid == "bad" {
			badCalls++
			return nil, errors.New("synthetic directory failure")
		}
		offset := q.Get("offset")
		limit := q.Get("limit")
		body := fmt.Sprintf(`{"state":true,"cid":%q,"count":3,"offset":%s,"limit":%s,"data":[{"fid":%q,"cid":%q,"n":%q,"s":"7","pc":%q,"sha":"ABC"}]}`, cid, offset, limit, "f-"+cid, cid, "file-"+cid, "pick-"+cid)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	dt := NewDirTools(client)
	result, output, err := dt.listDirectories(context.Background(), nil, ListDirectoriesArgs{Directories: []ListDirectoriesItem{
		{DirID: "a", Offset: 0, Limit: 2},
		{DirID: "bad", Offset: 0, Limit: 2},
		{DirID: "c", Offset: 1, Limit: 2},
	}})
	if err != nil || result == nil || !result.IsError || calls != 4 || badCalls != 2 {
		t.Fatalf("list_directories result=%#v output=%#v err=%v calls=%d badCalls=%d", result, output, err, calls, badCalls)
	}
	if output.Requested != 3 || output.Succeeded != 2 || output.Failed != 1 || len(output.Items) != 3 {
		t.Fatalf("unexpected typed output: %#v", output)
	}
	if !output.Items[0].Success || output.Items[0].DirID != "a" || len(output.Items[0].Entries) != 1 || output.Items[0].Entries[0].Name != "file-a" {
		t.Fatalf("unexpected first item: %#v", output.Items[0])
	}
	if output.Items[1].Success || !strings.Contains(output.Items[1].Error, "synthetic directory failure") {
		t.Fatalf("unexpected failed item: %#v", output.Items[1])
	}
	if !output.Items[2].Success || output.Items[2].Offset != 1 || output.Items[2].Entries[0].PickCode != "pick-c" {
		t.Fatalf("unexpected final item: %#v", output.Items[2])
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected content: %#v", result.Content[0])
	}
	var decoded ListDirectoriesResult
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != output.Requested || decoded.Items[2].DirID != output.Items[2].DirID {
		t.Fatalf("text/typed outputs diverged: text=%#v typed=%#v", decoded, output)
	}
}

func TestListDirectoriesWirePopulatesStructuredContent(t *testing.T) {
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		q := req.URL.Query()
		if q.Get("record_open_time") != "0" {
			t.Fatalf("wire list_directories lost read-only option: %s", req.URL)
		}
		body := `{"state":true,"cid":"0","count":1,"offset":0,"limit":1,"data":[{"fid":"f1","cid":"0","n":"one.bin","s":"1","pc":"pick1","sha":"ABC"}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	server := mcp.NewServer(&mcp.Implementation{Name: "dir-batch-test", Version: "1"}, nil)
	NewDirTools(client).RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "dir-batch-client", Version: "1"}, nil)
	clientSession, err := mcpClient.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_directories",
		Arguments: map[string]any{"directories": []any{map[string]any{"dir_id": "0", "limit": 1}}},
	})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil {
		t.Fatalf("wire list_directories result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ListDirectoriesResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != 1 || decoded.Succeeded != 1 || len(decoded.Items) != 1 || decoded.Items[0].Entries[0].Name != "one.bin" {
		t.Fatalf("unexpected structured content: %#v", decoded)
	}
}
