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

type mcpMutationTestObject struct {
	id       string
	parentID string
	name     string
	isDir    bool
}

type mcpMutationTestTransport struct {
	t                *testing.T
	objects          map[string]mcpMutationTestObject
	mutationCalls    int
	failMutationCall int
	listCalls        int
}

func (transport *mcpMutationTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport.t.Helper()
	if req.Method == http.MethodGet && req.URL.Path == "/files/get_info" {
		id := req.URL.Query().Get("file_id")
		object, ok := transport.objects[id]
		if !ok {
			return mcpMutationJSONResponse(req, `{"state":true,"data":[]}`), nil
		}
		return mcpMutationJSONResponse(req, mcpMutationFileInfoJSON(object)), nil
	}
	if req.Method == http.MethodGet && (req.URL.Path == "/natsort/files.php" || req.URL.Path == "/files") {
		transport.listCalls++
		if got := req.URL.Query().Get("record_open_time"); got != "0" {
			transport.t.Fatalf("mutation preflight list record_open_time = %q, want 0", got)
		}
		parentID := req.URL.Query().Get("cid")
		data := make([]string, 0)
		for _, object := range transport.objects {
			if object.parentID == parentID {
				data = append(data, mcpMutationFileEntryJSON(object))
			}
		}
		body := fmt.Sprintf(`{"state":true,"cid":%q,"count":%d,"offset":0,"limit":1150,"data":[%s]}`, parentID, len(data), strings.Join(data, ","))
		return mcpMutationJSONResponse(req, body), nil
	}
	if req.Method == http.MethodPost {
		transport.mutationCalls++
		if transport.failMutationCall > 0 && transport.mutationCalls == transport.failMutationCall {
			return nil, errors.New("synthetic mutation failure")
		}
		if req.URL.Path == "/files/add" {
			return mcpMutationJSONResponse(req, fmt.Sprintf(`{"state":true,"cid":%q}`, fmt.Sprintf("created-%d", transport.mutationCalls))), nil
		}
		return mcpMutationJSONResponse(req, `{"state":true}`), nil
	}
	transport.t.Fatalf("unexpected mutation test request: %s %s", req.Method, req.URL)
	return nil, errors.New("unreachable")
}

func mcpMutationJSONResponse(req *http.Request, body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func mcpMutationFileInfoJSON(object mcpMutationTestObject) string {
	return fmt.Sprintf(`{"state":true,"data":[%s]}`, mcpMutationFileEntryJSON(object))
}

func mcpMutationFileEntryJSON(object mcpMutationTestObject) string {
	if object.isDir {
		return fmt.Sprintf(`{"cid":%q,"pid":%q,"n":%q,"s":"0"}`, object.id, object.parentID, object.name)
	}
	return fmt.Sprintf(`{"fid":%q,"cid":%q,"n":%q,"s":"1"}`, object.id, object.parentID, object.name)
}

func newMCPMutationTestFileTools(t *testing.T, transport *mcpMutationTestTransport) *FileTools {
	t.Helper()
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: transport})))
	return NewFileTools(client)
}

func baseMCPMutationObjects() map[string]mcpMutationTestObject {
	return map[string]mcpMutationTestObject{
		"p":    {id: "p", parentID: "0", name: "parent", isDir: true},
		"q":    {id: "q", parentID: "0", name: "other-parent", isDir: true},
		"dest": {id: "dest", parentID: "0", name: "dest", isDir: true},
		"d1":   {id: "d1", parentID: "p", name: "source-dir", isDir: true},
		"d2":   {id: "d2", parentID: "d1", name: "inside-source", isDir: true},
		"f1":   {id: "f1", parentID: "p", name: "one.bin"},
		"f2":   {id: "f2", parentID: "p", name: "two.bin"},
		"f3":   {id: "f3", parentID: "p", name: "occupied.bin"},
	}
}

func mutationBatchResultFromCall(t *testing.T, result *mcp.CallToolResult) MCPMutationBatchResult {
	t.Helper()
	if result == nil || len(result.Content) != 1 {
		t.Fatalf("unexpected mutation batch call result: %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected mutation batch content: %#v", result.Content[0])
	}
	var decoded MCPMutationBatchResult
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatalf("decode mutation batch result: %v; body=%s", err, text.Text)
	}
	return decoded
}

func TestMutationDryRunsUseOnlyReadOnlyPreflight(t *testing.T) {
	transport := &mcpMutationTestTransport{t: t, objects: baseMCPMutationObjects()}
	ft := newMCPMutationTestFileTools(t, transport)
	ctx := context.Background()

	calls := []struct {
		name string
		call func() (*mcp.CallToolResult, any, error)
	}{
		{"mkdir", func() (*mcp.CallToolResult, any, error) {
			return ft.mkdir(ctx, nil, MkdirArgs{ParentID: "p", Name: "new-dir", DryRun: true})
		}},
		{"delete", func() (*mcp.CallToolResult, any, error) {
			return ft.delete(ctx, nil, DeleteArgs{FileIDs: []string{"f1", "d1"}, DryRun: true})
		}},
		{"rename", func() (*mcp.CallToolResult, any, error) {
			return ft.rename(ctx, nil, RenameArgs{FileID: "f1", NewName: "renamed.bin", DryRun: true})
		}},
		{"move", func() (*mcp.CallToolResult, any, error) {
			return ft.move(ctx, nil, MoveArgs{DirID: "dest", FileIDs: []string{"f1", "d1"}, DryRun: true})
		}},
		{"copy", func() (*mcp.CallToolResult, any, error) {
			return ft.copy(ctx, nil, CopyArgs{DirID: "dest", FileIDs: []string{"f2"}, DryRun: true})
		}},
		{"mkdir_many", func() (*mcp.CallToolResult, any, error) {
			return ft.mkdirMany(ctx, nil, MkdirManyArgs{Directories: []MkdirManyItem{{ParentID: "p", Name: "a"}, {ParentID: "q", Name: "b"}}, DryRun: true})
		}},
		{"rename_many", func() (*mcp.CallToolResult, any, error) {
			return ft.renameMany(ctx, nil, RenameManyArgs{Files: []RenameManyItem{{FileID: "f1", NewName: "a.bin"}, {FileID: "f2", NewName: "b.bin"}}, DryRun: true})
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			before := transport.mutationCalls
			result, _, err := tc.call()
			if err != nil || result == nil || result.IsError {
				t.Fatalf("dry-run %s = %#v, %v", tc.name, result, err)
			}
			if transport.mutationCalls != before {
				t.Fatalf("dry-run %s issued %d mutation POST(s)", tc.name, transport.mutationCalls-before)
			}
		})
	}
	if transport.listCalls == 0 {
		t.Fatal("name-aware mutation dry-runs did not use read-only directory listings")
	}
}

func TestDeletePreflightsAllSourcesBeforeMutation(t *testing.T) {
	transport := &mcpMutationTestTransport{t: t, objects: baseMCPMutationObjects()}
	ft := newMCPMutationTestFileTools(t, transport)
	result, _, err := ft.delete(context.Background(), nil, DeleteArgs{FileIDs: []string{"f1", "missing"}})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("delete missing-source preflight = %#v, %v", result, err)
	}
	if transport.mutationCalls != 0 {
		t.Fatalf("delete submitted %d mutation POST(s) before all sources resolved", transport.mutationCalls)
	}
}

func TestMoveAndCopyRejectTargetInsideSourceDirectoryBeforeMutation(t *testing.T) {
	for _, operation := range []string{"move", "copy"} {
		t.Run(operation, func(t *testing.T) {
			transport := &mcpMutationTestTransport{t: t, objects: baseMCPMutationObjects()}
			ft := newMCPMutationTestFileTools(t, transport)
			var result *mcp.CallToolResult
			var err error
			if operation == "move" {
				result, _, err = ft.move(context.Background(), nil, MoveArgs{DirID: "d2", FileIDs: []string{"d1"}})
			} else {
				result, _, err = ft.copy(context.Background(), nil, CopyArgs{DirID: "d2", FileIDs: []string{"d1"}})
			}
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("%s descendant-target preflight = %#v, %v", operation, result, err)
			}
			if transport.mutationCalls != 0 {
				t.Fatalf("%s submitted %d mutation POST(s) for descendant target", operation, transport.mutationCalls)
			}
		})
	}
}

func TestRenameManyRejectsOccupiedSiblingBeforeFirstMutation(t *testing.T) {
	transport := &mcpMutationTestTransport{t: t, objects: baseMCPMutationObjects()}
	ft := newMCPMutationTestFileTools(t, transport)
	result, _, err := ft.renameMany(context.Background(), nil, RenameManyArgs{Files: []RenameManyItem{
		{FileID: "f1", NewName: "safe.bin"},
		{FileID: "f2", NewName: "occupied.bin"},
	}})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("rename_many occupied target = %#v, %v", result, err)
	}
	if transport.mutationCalls != 0 {
		t.Fatalf("rename_many submitted %d mutation POST(s) before sibling-name preflight completed", transport.mutationCalls)
	}
}

func TestMkdirManyRejectsOccupiedSiblingBeforeFirstMutation(t *testing.T) {
	transport := &mcpMutationTestTransport{t: t, objects: baseMCPMutationObjects()}
	ft := newMCPMutationTestFileTools(t, transport)
	result, _, err := ft.mkdirMany(context.Background(), nil, MkdirManyArgs{Directories: []MkdirManyItem{
		{ParentID: "q", Name: "safe"},
		{ParentID: "p", Name: "occupied.bin"},
	}})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("mkdir_many occupied target = %#v, %v", result, err)
	}
	if transport.mutationCalls != 0 {
		t.Fatalf("mkdir_many submitted %d mutation POST(s) before sibling-name preflight completed", transport.mutationCalls)
	}
}

func TestRenameManyRuntimeFailureHonorsContinueOnError(t *testing.T) {
	for _, tc := range []struct {
		name       string
		continueOn bool
		wantCalls  int
		wantRemain int
		wantThird  string
	}{
		{name: "stop", wantCalls: 2, wantRemain: 1, wantThird: "not_run"},
		{name: "continue", continueOn: true, wantCalls: 3, wantRemain: 0, wantThird: "succeeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := baseMCPMutationObjects()
			objects["f4"] = mcpMutationTestObject{id: "f4", parentID: "p", name: "four.bin"}
			transport := &mcpMutationTestTransport{t: t, objects: objects, failMutationCall: 2}
			ft := newMCPMutationTestFileTools(t, transport)
			result, _, err := ft.renameMany(context.Background(), nil, RenameManyArgs{
				Files:           []RenameManyItem{{FileID: "f1", NewName: "a.bin"}, {FileID: "f2", NewName: "b.bin"}, {FileID: "f4", NewName: "c.bin"}},
				ContinueOnError: tc.continueOn,
			})
			if err != nil || result == nil || !result.IsError {
				t.Fatalf("rename_many runtime failure = %#v, %v", result, err)
			}
			decoded := mutationBatchResultFromCall(t, result)
			if transport.mutationCalls != tc.wantCalls || decoded.Failed != 1 || decoded.Remaining != tc.wantRemain || decoded.Items[2].Status != tc.wantThird {
				t.Fatalf("rename_many runtime semantics: calls=%d result=%#v", transport.mutationCalls, decoded)
			}
		})
	}
}

func TestRemoteNamePreflightScansBeyondFirstDirectoryPage(t *testing.T) {
	listCalls := 0
	pageLimit := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatalf("name preflight used %s, want GET", req.Method)
		}
		if req.URL.Path == "/files/get_info" {
			return mcpMutationJSONResponse(req, `{"state":true,"data":[{"cid":"p","pid":"0","n":"parent","s":"0"}]}`), nil
		}
		listCalls++
		q := req.URL.Query()
		if got := q.Get("record_open_time"); got != "0" {
			t.Fatalf("name preflight page %d record_open_time = %q, want 0", listCalls, got)
		}
		offset, err := strconv.Atoi(q.Get("offset"))
		if err != nil {
			t.Fatalf("parse list offset: %v", err)
		}
		limit, err := strconv.Atoi(q.Get("limit"))
		if err != nil {
			t.Fatalf("parse list limit: %v", err)
		}
		if limit <= 0 {
			t.Fatalf("name preflight page limit = %d, want positive", limit)
		}
		if pageLimit == 0 {
			pageLimit = limit
		} else if limit != pageLimit {
			t.Fatalf("name preflight page limit changed from %d to %d", pageLimit, limit)
		}

		total := pageLimit + 1
		entries := make([]string, 0, limit)
		if offset == 0 {
			for i := 0; i < pageLimit; i++ {
				entries = append(entries, fmt.Sprintf(`{"fid":"f-%d","cid":"p","n":"file-%d.bin","s":"1"}`, i, i))
			}
		} else if offset == pageLimit {
			entries = append(entries, `{"fid":"tail","cid":"p","n":"occupied.bin","s":"1"}`)
		} else {
			t.Fatalf("unexpected list offset %d", offset)
		}
		body := fmt.Sprintf(`{"state":true,"cid":"p","count":%d,"offset":%d,"limit":%d,"data":[%s]}`, total, offset, limit, strings.Join(entries, ","))
		return mcpMutationJSONResponse(req, body), nil
	})})))

	err := preflightMCPRemoteTargetNames(client, []mcpRemoteNameTarget{{Index: 0, ParentID: "p", Name: "occupied.bin"}})
	if err == nil || !strings.Contains(err.Error(), "occupied.bin") {
		t.Fatalf("tail-page sibling conflict was not detected: %v", err)
	}
	if listCalls != 2 {
		t.Fatalf("name preflight list calls = %d, want 2", listCalls)
	}
}

func TestMutationBatchBoundariesFailBeforeClientUse(t *testing.T) {
	ft := NewFileTools(nil)
	if result, _, err := ft.mkdirMany(context.Background(), nil, MkdirManyArgs{}); err != nil || result == nil || !result.IsError {
		t.Fatalf("empty mkdir_many = %#v, %v", result, err)
	}
	if result, _, err := ft.renameMany(context.Background(), nil, RenameManyArgs{}); err != nil || result == nil || !result.IsError {
		t.Fatalf("empty rename_many = %#v, %v", result, err)
	}
	if result, _, err := ft.mkdirMany(context.Background(), nil, MkdirManyArgs{Directories: make([]MkdirManyItem, maxMCPMutationBatchItems+1)}); err != nil || result == nil || !result.IsError {
		t.Fatalf("oversized mkdir_many = %#v, %v", result, err)
	}
	if result, _, err := ft.renameMany(context.Background(), nil, RenameManyArgs{Files: make([]RenameManyItem, maxMCPMutationBatchItems+1)}); err != nil || result == nil || !result.IsError {
		t.Fatalf("oversized rename_many = %#v, %v", result, err)
	}
}
