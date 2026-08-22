package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpUploadURLRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn mcpUploadURLRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func TestPrepareMCPURLUploadValidatesSourceTargetAndNameWithoutEchoingURL(t *testing.T) {
	item, err := prepareMCPURLUpload(UploadFromURLArgs{URL: "https://example.com/path/file.bin?token=secret", DirID: "0"})
	if err != nil {
		t.Fatal(err)
	}
	if item.fileName != "file.bin" || item.dirID != "0" {
		t.Fatalf("unexpected prepared URL upload: %#v", item)
	}
	if _, err := prepareMCPURLUpload(UploadFromURLArgs{URL: "https://example.com/%zz?token=super-secret", DirID: "0"}); err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("malformed source URL error = %v", err)
	}
	if _, err := prepareMCPURLUpload(UploadFromURLArgs{URL: "https://example.com/file", DirID: "", FileName: "file.bin"}); err == nil {
		t.Fatal("blank target directory was accepted")
	}
	if _, err := prepareMCPURLUpload(UploadFromURLArgs{URL: "https://example.com/file", DirID: "0", FileName: "bad/name"}); err == nil {
		t.Fatal("invalid remote filename was accepted")
	}
}

func TestUploadFromURLDryRunDoesNotFetchOrEchoSource(t *testing.T) {
	ft := NewFileTools(driver.New())
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{}, errors.New("dry-run reached P10")
	}
	const source = "https://no-dns-during-dry-run.invalid/path/file.bin?token=url-secret"
	result, _, err := ft.uploadFromURL(context.Background(), nil, UploadFromURLArgs{URL: source, DirID: "0", DryRun: true})
	if err != nil || result == nil || result.IsError || uploadCalls != 0 || len(result.Content) != 1 {
		t.Fatalf("URL upload dry-run = %#v, %v uploads=%d", result, err, uploadCalls)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, forbidden := range []string{source, "no-dns-during-dry-run", "url-secret", "source_url"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("URL upload dry-run echoed %q: %s", forbidden, text)
		}
	}
	var plan MCPUploadPlan
	if err := json.Unmarshal([]byte(text), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.Operation != "upload_from_url" || plan.Requested != 1 || len(plan.Items) != 1 || plan.Items[0].FileName != "file.bin" || plan.Items[0].FileSize != nil {
		t.Fatalf("unexpected URL upload plan: %#v", plan)
	}
}

func TestUploadFromURLFilesDryRunDoesNotFetchOrEchoSources(t *testing.T) {
	ft := NewFileTools(driver.New())
	uploadCalls := 0
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		uploadCalls++
		return uploadpkg.Result{}, errors.New("dry-run reached P10")
	}
	result, _, err := ft.uploadFromURLs(context.Background(), nil, UploadFromURLFilesArgs{DryRun: true, Files: []UploadFromURLFileItem{
		{URL: "https://first-no-dns.invalid/a.bin?token=first-secret", DirID: "0"},
		{URL: "https://second-no-dns.invalid/b.bin?token=second-secret", DirID: "0", FileName: "remote.bin"},
	}})
	if err != nil || result == nil || result.IsError || uploadCalls != 0 || len(result.Content) != 1 {
		t.Fatalf("URL batch dry-run = %#v, %v uploads=%d", result, err, uploadCalls)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	for _, forbidden := range []string{"https://", "first-no-dns", "second-no-dns", "first-secret", "second-secret", "source_url"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("URL batch dry-run echoed %q: %s", forbidden, text)
		}
	}
	var plan MCPUploadPlan
	if err := json.Unmarshal([]byte(text), &plan); err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.Operation != "upload_from_urls" || plan.Requested != 2 || len(plan.Items) != 2 || plan.Items[1].FileName != "remote.bin" || plan.Items[0].FileSize != nil || plan.Items[1].FileSize != nil {
		t.Fatalf("unexpected URL batch plan: %#v", plan)
	}
}

func TestUploadFromURLFilesWireDryRunPopulatesSafeStructuredContent(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "url-upload-batch-test", Version: "1"}, nil)
	NewFileTools(driver.New(), WithDestructiveTools(true)).RegisterTools(server)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "url-upload-batch-client", Version: "1"}, nil)
	clientSession, err := mcpClient.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	const source = "https://wire-no-dns.invalid/file.bin?token=wire-secret"
	result, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "upload_from_urls",
		Arguments: map[string]any{
			"dry_run": true,
			"files":   []any{map[string]any{"url": source, "dir_id": "0"}},
		},
	})
	if err != nil || result == nil || result.IsError || result.StructuredContent == nil {
		t.Fatalf("wire URL batch dry-run result=%#v err=%v", result, err)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{source, "wire-no-dns", "wire-secret", "source_url"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("structured URL batch dry-run leaked %q: %s", secret, encoded)
		}
	}
	var output UploadFromURLFilesOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.Mode != "dry_run" || output.Plan == nil || output.Result != nil || output.Plan.Requested != 1 || len(output.Plan.Items) != 1 || output.Plan.Items[0].FileName != "file.bin" {
		t.Fatalf("unexpected structured URL batch dry-run: %#v", output)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var legacyPlan MCPUploadPlan
	if err := json.Unmarshal([]byte(text), &legacyPlan); err != nil {
		t.Fatal(err)
	}
	if legacyPlan.Operation != "upload_from_urls" || legacyPlan.Requested != 1 {
		t.Fatalf("legacy TextContent changed: %#v", legacyPlan)
	}
}

func TestUploadPreparedURLSanitizesTransportErrorAndNeverCallsP10(t *testing.T) {
	ft := NewFileTools(driver.New())
	ft.uploadTransfer.deps.uploadFile = func(context.Context, *driver.Pan115Client, string, string, int64, *os.File, uploadpkg.Options) (uploadpkg.Result, error) {
		t.Fatal("P10 uploader ran after external transport failure")
		return uploadpkg.Result{}, nil
	}
	item, err := prepareMCPURLUpload(UploadFromURLArgs{
		URL:      "https://user:password@example.com/private/file?token=super-secret#fragment",
		DirID:    "0",
		FileName: "file.bin",
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: mcpUploadURLRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})}
	_, err = ft.uploadPreparedURLWithClient(context.Background(), item, client)
	if err == nil {
		t.Fatal("expected external transport failure")
	}
	text := err.Error()
	for _, secret := range []string{"user", "password", "/private", "token", "super-secret", "fragment"} {
		if strings.Contains(text, secret) {
			t.Fatalf("URL upload transport error leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "example.com") || !strings.Contains(text, "network down") {
		t.Fatalf("sanitized transport error lost diagnostics: %s", text)
	}
}

func TestUploadPreparedURLFeedsExternalBodyIntoExistingP10Uploader(t *testing.T) {
	ft := NewFileTools(driver.New())
	calls := 0
	ft.uploadTransfer.deps.uploadFile = func(_ context.Context, _ *driver.Pan115Client, dirID, name string, size int64, file *os.File, _ uploadpkg.Options) (uploadpkg.Result, error) {
		calls++
		if dirID != "0" || name != "file.bin" || size != 7 {
			t.Fatalf("unexpected P10 arguments: dir=%q name=%q size=%d", dirID, name, size)
		}
		body, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "payload" {
			t.Fatalf("unexpected P10 body: %q", body)
		}
		return uploadpkg.Result{BytesUploaded: size, Rapid: true}, nil
	}
	item, err := prepareMCPURLUpload(UploadFromURLArgs{URL: "https://example.com/file.bin", DirID: "0"})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: mcpUploadURLRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("payload")), Request: req}, nil
	})}
	result, err := ft.uploadPreparedURLWithClient(context.Background(), item, client)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.BytesUploaded != 7 || !result.Rapid {
		t.Fatalf("unexpected URL upload result/calls: %#v calls=%d", result, calls)
	}
}

func TestUploadFromURLFilesPreflightsAllTargetsBeforeExternalFetch(t *testing.T) {
	lookups := 0
	client := mcpUploadTargetTestClient(t, map[string]bool{"dir-1": true, "file-2": false}, &lookups)
	ft := NewFileTools(client)
	result, _, err := ft.uploadFromURLs(context.Background(), nil, UploadFromURLFilesArgs{Files: []UploadFromURLFileItem{
		{URL: "https://example.com/a.bin", DirID: "dir-1"},
		{URL: "https://example.com/b.bin", DirID: "file-2"},
	}})
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	if result == nil || !result.IsError || lookups != 2 {
		t.Fatalf("URL batch target preflight = %#v lookups=%d", result, lookups)
	}
}

func TestUploadFromURLFilesResultDoesNotEchoSourceURLs(t *testing.T) {
	response := UploadFromURLFilesResult{
		Requested: 2,
		Succeeded: 1,
		Failed:    1,
		Items: []UploadFromURLFilesItemResult{
			{Index: 0, FileName: "a.bin", BytesUploaded: 3, Success: true},
			{Index: 1, FileName: "b.bin", Error: "network down"},
		},
	}
	result, _, err := uploadFromURLFilesCallResult(response)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("unexpected URL batch call result: %#v", result)
	}
	text := result.Content[0].(*mcp.TextContent).Text
	var decoded UploadFromURLFilesResult
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Requested != 2 || decoded.Succeeded != 1 || decoded.Failed != 1 {
		t.Fatalf("unexpected decoded URL batch result: %#v", decoded)
	}
	for _, forbidden := range []string{"https://", "token=", "source_url"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("URL batch result leaked %q: %s", forbidden, text)
		}
	}
}

func TestSanitizeExternalURLHelperHandlesDirectURLError(t *testing.T) {
	err := &url.Error{Op: "Get", URL: "https://user:pass@example.com/private?token=secret", Err: errors.New("boom")}
	text := sanitizeMCPExternalURLError(err).Error()
	if strings.Contains(text, "token") || strings.Contains(text, "user") || !strings.Contains(text, "example.com") {
		t.Fatalf("unexpected sanitized direct URL error: %s", text)
	}
}
