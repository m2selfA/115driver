package tools

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNormalizeOfflinePageDefaultsAndRejectsNegativeValues(t *testing.T) {
	page, err := normalizeOfflinePage(0)
	if err != nil || page != 1 {
		t.Fatalf("default page = %d, %v; want 1, nil", page, err)
	}
	page, err = normalizeOfflinePage(7)
	if err != nil || page != 7 {
		t.Fatalf("explicit page = %d, %v; want 7, nil", page, err)
	}
	if _, err := normalizeOfflinePage(-1); err == nil {
		t.Fatal("negative page was accepted")
	}
}

func TestListOfflineTasksOmitsSourceURLFromTextAndTypedOutput(t *testing.T) {
	const secretURL = "https://downloads.example.invalid/archive?token=offline-list-secret"
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPost {
			t.Fatalf("offline list used %s, want POST", req.Method)
		}
		body := `{"state":true,"total":1,"count":1,"page_row":1,"page_count":1,"page":1,"quota":0,"tasks":[{"info_hash":"HASH1","name":"archive","size":7,"url":"` + secretURL + `","add_time":1,"peers":2,"rateDownload":3.5,"status":1,"percentDone":0.5,"last_update":2,"left_time":3,"file_id":"f1","delete_file_id":"df1","wp_path_id":"d1","move":0}]}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))
	ot := NewOfflineTools(client)
	result, output, err := ot.listOfflineTasks(context.Background(), nil, ListOfflineTaskArgs{Page: 1})
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 {
		t.Fatalf("offline list result=%#v output=%#v err=%v", result, output, err)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, forbidden := range []string{secretURL, "offline-list-secret", `"url"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("offline list leaked %q: %s", forbidden, text)
		}
	}
	if output.Total != 1 || len(output.Tasks) != 1 || output.Tasks[0].InfoHash != "HASH1" || output.Tasks[0].Name != "archive" || output.Tasks[0].StatusText == "" {
		t.Fatalf("unexpected typed offline list output: %#v", output)
	}
}

func TestAddOfflineTaskPreservesDefaultDirectoryResolutionFailure(t *testing.T) {
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})})))
	ot := NewOfflineTools(client, WithOfflineDefaultSaveDir("downloads"))
	result, _, err := ot.addOfflineTaskURIs(context.Background(), nil, AddOfflineTaskURIsArgs{URIs: []string{"https://example.invalid/file"}})
	if err != nil {
		t.Fatalf("MCP handler should return tool error, got Go error: %v", err)
	}
	if result == nil || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("expected one MCP tool error, got %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected MCP error content: %#v", result.Content[0])
	}
	if !strings.Contains(text.Text, "network down") || !strings.Contains(text.Text, "resolve default offline save directory") {
		t.Fatalf("default directory resolution cause was lost: %s", text.Text)
	}
	if strings.Contains(strings.ToLower(text.Text), "not found") {
		t.Fatalf("network failure was misclassified as not found: %s", text.Text)
	}
}

func TestOfflineAddDryRunValidatesSaveDirectoryWithoutEchoingURI(t *testing.T) {
	calls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet || req.URL.Path != "/files/get_info" || req.URL.Query().Get("file_id") != "dir-1" {
			t.Fatalf("offline add dry-run unexpectedly issued request: %s %s", req.Method, req.URL)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"state":true,"data":[{"cid":"dir-1","pid":"0","n":"downloads","s":"0"}]}`)),
			Request:    req,
		}, nil
	})})))
	ot := NewOfflineTools(client, WithOfflineDestructiveTools(true))
	const secretURI = "https://example.invalid/archive?token=offline-secret"
	result, _, err := ot.addOfflineTaskURIs(context.Background(), nil, AddOfflineTaskURIsArgs{URIs: []string{secretURI}, SaveDirID: "dir-1", DryRun: true})
	if err != nil || result == nil || result.IsError || len(result.Content) != 1 || calls != 1 {
		t.Fatalf("offline add dry-run = %#v, %v calls=%d", result, err, calls)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected offline add dry-run content: %#v", result.Content[0])
	}
	if strings.Contains(text.Text, secretURI) || strings.Contains(text.Text, "offline-secret") {
		t.Fatalf("offline add dry-run echoed source URI: %s", text.Text)
	}
	if !strings.Contains(text.Text, `"requested":1`) || !strings.Contains(text.Text, `"save_dir_id":"dir-1"`) {
		t.Fatalf("offline add dry-run lost safe plan metadata: %s", text.Text)
	}
}

func TestOfflineMutationDryRunsAndStaticPreflightAvoidMutationNetwork(t *testing.T) {
	ot := NewOfflineTools(nil, WithOfflineDestructiveTools(true))
	if result, _, err := ot.deleteOfflineTasks(context.Background(), nil, DeleteOfflineTasksArgs{Hashes: []string{"hash-a", "hash-b"}, DeleteFiles: true, DryRun: true}); err != nil || result == nil || result.IsError {
		t.Fatalf("offline delete dry-run = %#v, %v", result, err)
	}
	if result, _, err := ot.clearOfflineTasks(context.Background(), nil, ClearOfflineTasksArgs{Scope: "all", DryRun: true}); err != nil || result == nil || result.IsError {
		t.Fatalf("offline clear dry-run = %#v, %v", result, err)
	}
	if result, _, err := ot.deleteOfflineTasks(context.Background(), nil, DeleteOfflineTasksArgs{Hashes: []string{"same", " same "}}); err != nil || result == nil || !result.IsError {
		t.Fatalf("duplicate offline hashes = %#v, %v", result, err)
	}
}

func TestOfflineAddRejectsDuplicateURIsBeforeNetwork(t *testing.T) {

	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("duplicate offline URI unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})})))
	ot := NewOfflineTools(client, WithOfflineDestructiveTools(true))
	result, _, err := ot.addOfflineTaskURIs(context.Background(), nil, AddOfflineTaskURIsArgs{URIs: []string{"magnet:?xt=urn:btih:ABC", " magnet:?xt=urn:btih:ABC "}, SaveDirID: "0"})
	if err != nil || result == nil || !result.IsError {
		t.Fatalf("duplicate offline URI preflight = %#v, %v", result, err)
	}
}

func TestOfflineAddRejectsUnsupportedOrMalformedURIsBeforeNetworkWithoutEcho(t *testing.T) {
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("invalid offline URI unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})})))
	ot := NewOfflineTools(client, WithOfflineDestructiveTools(true))
	for name, rawURI := range map[string]string{
		"unsupported": "ftp://example.invalid/archive?token=offline-secret",
		"http-host":   "https:///archive?token=offline-secret",
		"magnet":      "magnet:offline-secret",
		"ed2k":        "ed2k:offline-secret",
		"no-scheme":   "offline-secret",
	} {
		t.Run(name, func(t *testing.T) {
			result, _, err := ot.addOfflineTaskURIs(context.Background(), nil, AddOfflineTaskURIsArgs{URIs: []string{rawURI}, SaveDirID: "0", DryRun: true})
			if err != nil || result == nil || !result.IsError || len(result.Content) != 1 {
				t.Fatalf("invalid offline URI result = %#v, %v", result, err)
			}
			text := result.Content[0].(*mcp.TextContent).Text
			if strings.Contains(text, rawURI) || strings.Contains(text, "offline-secret") {
				t.Fatalf("invalid offline URI error echoed source URI: %s", text)
			}
		})
	}
}

func TestNormalizeMCPOfflineURIsAcceptsDocumentedSchemes(t *testing.T) {
	inputs := []string{
		"https://example.invalid/file.bin",
		"http://example.invalid/file.bin",
		"magnet:?xt=urn:btih:ABC",
		"ed2k://|file|name.bin|1|ABC|/",
	}
	got, err := normalizeMCPOfflineURIs(inputs)
	if err != nil || len(got) != len(inputs) {
		t.Fatalf("documented offline URIs = %#v, %v", got, err)
	}
}

func offlineClearFlag(value int64) *int64 {

	return &value
}

func TestClearOfflineTasksNamedScopesSubmitDocumentedTaskOnlyFlags(t *testing.T) {
	for scope, wantFlag := range map[string]string{"completed": "0", "all": "1", "failed": "2", "active": "3"} {
		t.Run(scope, func(t *testing.T) {
			calls := 0
			client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if err := req.ParseForm(); err != nil {
					t.Fatalf("parse clear form: %v", err)
				}
				if got := req.Form.Get("flag"); got != wantFlag {
					t.Fatalf("scope %q submitted flag %q, want %q", scope, got, wantFlag)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"state":true}`)),
					Request:    req,
				}, nil
			})})))
			ot := NewOfflineTools(client, WithOfflineDestructiveTools(true))
			result, _, err := ot.clearOfflineTasks(context.Background(), nil, ClearOfflineTasksArgs{Scope: scope})
			if err != nil {
				t.Fatalf("handler returned Go error: %v", err)
			}
			if result == nil || result.IsError || calls != 1 {
				t.Fatalf("scope %q result=%#v calls=%d", scope, result, calls)
			}
		})
	}
}

func TestClearOfflineTasksRejectsExplicitScopeFlagConflictBeforeNetwork(t *testing.T) {
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatalf("conflicting clear request unexpectedly reached network: %s", req.URL)
		return nil, errors.New("unreachable")
	})})))
	ot := NewOfflineTools(client, WithOfflineDestructiveTools(true))
	result, _, err := ot.clearOfflineTasks(context.Background(), nil, ClearOfflineTasksArgs{Scope: "all", ClearFlag: offlineClearFlag(0)})
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	if result == nil || !result.IsError {
		t.Fatalf("expected conflicting clear request to fail, got %#v", result)
	}
}

func TestResolveMCPOfflineClearScopeSupportsNamedAndLegacyTaskOnlyModes(t *testing.T) {
	gotScope, gotFlag, err := resolveMCPOfflineClearScope("", nil)
	if err != nil || gotScope != "completed" || gotFlag != 0 {
		t.Fatalf("default scope = %q/%d/%v, want completed/0/nil", gotScope, gotFlag, err)
	}
	for scope, wantFlag := range map[string]int64{"completed": 0, "all": 1, "failed": 2, "active": 3} {
		gotScope, gotFlag, err := resolveMCPOfflineClearScope(scope, nil)
		if err != nil || gotScope != scope || gotFlag != wantFlag {
			t.Fatalf("scope %q = %q/%d/%v, want %q/%d/nil", scope, gotScope, gotFlag, err, scope, wantFlag)
		}
		gotScope, gotFlag, err = resolveMCPOfflineClearScope(scope, offlineClearFlag(wantFlag))
		if err != nil || gotScope != scope || gotFlag != wantFlag {
			t.Fatalf("matching scope+flag %q/%d = %q/%d/%v", scope, wantFlag, gotScope, gotFlag, err)
		}
	}
	for flag, wantScope := range map[int64]string{0: "completed", 1: "all", 2: "failed", 3: "active"} {
		gotScope, gotFlag, err := resolveMCPOfflineClearScope("", offlineClearFlag(flag))
		if err != nil || gotScope != wantScope || gotFlag != flag {
			t.Fatalf("legacy flag %d = %q/%d/%v, want %q/%d/nil", flag, gotScope, gotFlag, err, wantScope, flag)
		}
	}
}

func TestResolveMCPOfflineClearScopeRejectsUnsafeOrConflictingModes(t *testing.T) {
	for _, flag := range []int64{-1, 4, 5, 99} {
		if _, _, err := resolveMCPOfflineClearScope("", offlineClearFlag(flag)); err == nil {
			t.Fatalf("unsupported clear flag %d was accepted", flag)
		}
	}
	if _, _, err := resolveMCPOfflineClearScope("future", nil); err == nil {
		t.Fatal("unsupported named scope was accepted")
	}
	for _, tc := range []struct {
		scope string
		flag  int64
	}{
		{scope: "all", flag: 0},
		{scope: "completed", flag: 1},
		{scope: "failed", flag: 3},
	} {
		if _, _, err := resolveMCPOfflineClearScope(tc.scope, offlineClearFlag(tc.flag)); err == nil {
			t.Fatalf("conflicting scope %q and explicit legacy flag %d were accepted", tc.scope, tc.flag)
		}
	}
}
