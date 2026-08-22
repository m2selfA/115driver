package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/internal/remoteresolver"
	"github.com/SheltonZhu/115driver/internal/remotetree"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type usageTestClient struct {
	dirIDs    map[string]string
	dirErrs   map[string]error
	filePages map[string][]driver.File
	treePages map[string][]driver.File
	files     map[string]driver.File
	dirCalls  map[string]int
	pageCalls map[string]int
	getCalls  map[string]int
}

func (c *usageTestClient) DirName2CID(path string) (*driver.APIGetDirIDResp, error) {
	if c.dirCalls == nil {
		c.dirCalls = make(map[string]int)
	}
	c.dirCalls[path]++
	if err := c.dirErrs[path]; err != nil {
		return nil, err
	}
	return &driver.APIGetDirIDResp{CategoryID: driver.IntString(c.dirIDs[path])}, nil
}

func (c *usageTestClient) ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error) {
	if c.pageCalls == nil {
		c.pageCalls = make(map[string]int)
	}
	key := dirID
	if limit == remoteresolver.FileResolvePageLimit {
		key = "resolve:" + dirID
	} else if limit == remotetree.WalkPageLimit {
		key = "tree:" + dirID
	}
	c.pageCalls[key]++
	var source []driver.File
	if limit == remoteresolver.FileResolvePageLimit {
		source = c.filePages[dirID]
	} else {
		source = c.treePages[dirID]
	}
	if offset > 0 {
		empty := []driver.File{}
		return &empty, nil
	}
	cloned := append([]driver.File(nil), source...)
	return &cloned, nil
}

func (c *usageTestClient) GetFile(fileID string) (*driver.File, error) {
	if c.getCalls == nil {
		c.getCalls = make(map[string]int)
	}
	c.getCalls[fileID]++
	file, ok := c.files[fileID]
	if !ok {
		return nil, errors.New("missing test file metadata")
	}
	copy := file
	return &copy, nil
}

func TestNormalizeMCPUsageArgsBoundsAndDefaults(t *testing.T) {
	paths, maxNodes, err := normalizeMCPUsageArgs(SummarizeUsageArgs{Paths: []string{"/a"}})
	if err != nil || len(paths) != 1 || maxNodes != defaultMCPUsageMaxNodes {
		t.Fatalf("usage defaults = paths=%v maxNodes=%d err=%v", paths, maxNodes, err)
	}
	for name, args := range map[string]SummarizeUsageArgs{
		"empty":          {},
		"negative-depth": {Paths: []string{"/a"}, MaxDepth: -1},
		"negative-nodes": {Paths: []string{"/a"}, MaxNodes: -1},
		"too-many-nodes": {Paths: []string{"/a"}, MaxNodes: maxMCPUsageMaxNodes + 1},
		"duplicate":      {Paths: []string{"/a/", "a"}},
		"too-many-paths": {Paths: make([]string, maxMCPUsagePaths+1)},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := normalizeMCPUsageArgs(args); err == nil {
				t.Fatal("expected usage preflight failure")
			}
		})
	}
}

func TestSummarizeMCPUsageMatchesDUCountsAndContinuesAfterFailure(t *testing.T) {
	client := &usageTestClient{
		dirIDs: map[string]string{"dir": "d1", "after": "d3"},
		dirErrs: map[string]error{
			"file.bin": driver.ErrNotExist,
			"bad":      errors.New("synthetic usage resolver failure"),
		},
		filePages: map[string][]driver.File{
			"0": {{FileID: "f1", Name: "file.bin", Size: 7}},
		},
		treePages: map[string][]driver.File{
			"d1": {
				{FileID: "fa", Name: "a.bin", Size: 2},
				{FileID: "d2", Name: "sub", IsDirectory: true},
			},
			"d2": {{FileID: "fb", Name: "b.bin", Size: 3}},
			"d3": {},
		},
		files: map[string]driver.File{"f1": {FileID: "f1", Name: "file.bin", Size: 7}},
	}

	response, err := summarizeMCPUsage(context.Background(), client, SummarizeUsageArgs{
		Paths: []string{"dir", "file.bin", "bad", "after"}, MaxNodes: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Requested != 4 || response.Succeeded != 3 || response.Failed != 1 || response.NodesVisited != 4 || response.BudgetExhausted {
		t.Fatalf("unexpected usage summary: %#v", response)
	}
	first := response.Items[0]
	if !first.Success || first.Data == nil || first.Data.Size != 5 || first.Data.Files != 2 || first.Data.Directories != 1 || first.Data.NodesVisited != 3 || !first.Data.Complete {
		t.Fatalf("directory usage does not match CLI du semantics: %#v", first)
	}
	second := response.Items[1]
	if !second.Success || second.Data == nil || second.Data.Size != 7 || second.Data.Files != 1 || second.Data.Directories != 0 || second.Data.NodesVisited != 1 || !second.Data.Complete {
		t.Fatalf("file usage mismatch: %#v", second)
	}
	if response.Items[2].Success || response.Items[2].Error == "" {
		t.Fatalf("failed path lost error: %#v", response.Items[2])
	}
	if !response.Items[3].Success || response.Items[3].Data == nil || response.Items[3].Data.NodesVisited != 0 || !response.Items[3].Data.Complete {
		t.Fatalf("usage did not continue after failure: %#v", response.Items[3])
	}
}

func TestSummarizeMCPUsageNodeBudgetReturnsPartialThenSkipsLaterPaths(t *testing.T) {
	client := &usageTestClient{
		dirIDs: map[string]string{"limited": "d1", "after": "d2"},
		treePages: map[string][]driver.File{
			"d1": {
				{FileID: "f1", Name: "one.bin", Size: 1},
				{FileID: "f2", Name: "two.bin", Size: 2},
				{FileID: "f3", Name: "three.bin", Size: 3},
			},
			"d2": {},
		},
	}
	response, err := summarizeMCPUsage(context.Background(), client, SummarizeUsageArgs{Paths: []string{"limited", "after"}, MaxNodes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if response.Succeeded != 1 || response.Failed != 1 || response.NodesVisited != 2 || !response.BudgetExhausted {
		t.Fatalf("unexpected node-budget summary: %#v", response)
	}
	limited := response.Items[0]
	if !limited.Success || limited.Data == nil || limited.Data.Size != 3 || limited.Data.Files != 2 || limited.Data.NodesVisited != 2 || limited.Data.Complete || !limited.Data.NodeLimited {
		t.Fatalf("unexpected partial usage summary: %#v", limited)
	}
	if response.Items[1].Success || response.Items[1].Error == "" {
		t.Fatalf("later path was not skipped after budget exhaustion: %#v", response.Items[1])
	}
	if client.dirCalls["after"] != 0 {
		t.Fatalf("budget-exhausted later path reached resolver %d times", client.dirCalls["after"])
	}
}

func TestSummarizeUsageCallResultKeepsTextAndTypedOutputEquivalent(t *testing.T) {
	response := SummarizeUsageResult{
		Requested: 1,
		Succeeded: 1,
		MaxNodes:  10,
		Items:     []SummarizeUsageItemResult{{Index: 0, Path: "/a", Success: true, Data: &MCPUsageSummary{Path: "/a", Complete: true}}},
	}
	result, output, err := summarizeUsageCallResult(response)
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("summarize usage call result=%#v output=%#v err=%v", result, output, err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected summarize usage content: %#v", result.Content[0])
	}
	var decoded SummarizeUsageResult
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != output.Requested || decoded.Items[0].Data.Path != output.Items[0].Data.Path {
		t.Fatalf("text/typed usage outputs diverged: text=%#v typed=%#v", decoded, output)
	}
}

func TestSummarizeUsageWirePopulatesStructuredContentAndReadOnlyPaging(t *testing.T) {
	calls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet {
			t.Fatalf("summarize_usage used %s, want GET", req.Method)
		}
		q := req.URL.Query()
		if q.Get("cid") != "0" || q.Get("limit") != "500" || q.Get("record_open_time") != "0" {
			t.Fatalf("summarize_usage lost bounded read-only paging: %s", req.URL)
		}
		body := `{"state":true,"cid":"0","count":1,"offset":0,"limit":500,"data":[{"fid":"f1","cid":"0","n":"one.bin","s":"9","pc":"pick1","sha":"ABC"}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))

	server := mcp.NewServer(&mcp.Implementation{Name: "usage-test", Version: "1"}, nil)
	NewDirTools(client).RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "usage-client", Version: "1"}, nil)
	clientSession, err := mcpClient.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "summarize_usage",
		Arguments: map[string]any{"paths": []any{"/"}, "max_nodes": 10},
	})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil || calls != 1 {
		t.Fatalf("wire summarize_usage result=%#v err=%v calls=%d", result, err, calls)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SummarizeUsageResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != 1 || decoded.Succeeded != 1 || decoded.NodesVisited != 1 || len(decoded.Items) != 1 || decoded.Items[0].Data == nil || decoded.Items[0].Data.Size != 9 || !decoded.Items[0].Data.Complete {
		t.Fatalf("unexpected wire summarize_usage structured content: %#v", decoded)
	}
}
