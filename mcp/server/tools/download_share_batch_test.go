package tools

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestSharedDownloadInfoForRequestValidatesIdentityAndMetadata(t *testing.T) {
	valid := &driver.SharedDownloadRequest{SharedDownloadInfo: driver.SharedDownloadInfo{FileID: "file-1", FileName: "a.bin", FileSize: 10}}
	valid.URL.URL = "https://cdn.example.invalid/a"
	info, err := sharedDownloadInfoForRequest(valid, "file-1", "stable")
	if err != nil || info == nil || info.PickCode != "stable" || info.FileName != "a.bin" {
		t.Fatalf("valid shared metadata = %#v, %v", info, err)
	}
	if _, err := sharedDownloadInfoForRequest(valid, "file-2", "stable"); err == nil {
		t.Fatal("mismatched share file ID was accepted")
	}
	missingURL := *valid
	missingURL.URL.URL = ""
	if _, err := sharedDownloadInfoForRequest(&missingURL, "file-1", "stable"); err == nil {
		t.Fatal("share metadata without CDN URL was accepted")
	}
}

func TestDownloadShareFilesRejectsOutsideRootBeforeUsingClient(t *testing.T) {
	root := t.TempDir()
	ft := NewFileTools(nil, WithLocalRoot(root))
	result, _, err := ft.downloadShareFiles(context.Background(), nil, DownloadShareFilesArgs{
		ShareCode: "share-code",
		Files: []DownloadShareFilesItem{
			{FileID: "file-1", LocalPath: filepath.Join(root, "inside.bin")},
			{FileID: "file-2", LocalPath: filepath.Join(filepath.Dir(root), "outside.bin")},
		},
	})
	if err != nil {
		t.Fatalf("handler should return MCP tool error, got Go error: %v", err)
	}
	if result == nil || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("outside-root share batch did not fail closed: %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "local target denied") || strings.Contains(text.Text, "client is unavailable") {
		t.Fatalf("share batch did not validate all local targets before client use: %#v", result.Content[0])
	}
}

func TestDownloadShareFilesRedactsReceiveCodeFromMetadataErrors(t *testing.T) {
	const receiveCode = "secret-share-batch-password"
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("upstream rejected receive code " + receiveCode)
	})})))
	root := t.TempDir()
	ft := NewFileTools(client, WithLocalRoot(root))
	result, _, err := ft.downloadShareFiles(context.Background(), nil, DownloadShareFilesArgs{
		ShareCode:   "share-code",
		ReceiveCode: receiveCode,
		Files:       []DownloadShareFilesItem{{FileID: "file-1", LocalPath: filepath.Join(root, "file.bin")}},
	})
	if err != nil {
		t.Fatalf("handler should return MCP tool error, got Go error: %v", err)
	}
	if result == nil || !result.IsError || len(result.Content) != 1 {
		t.Fatalf("expected share batch tool error, got %#v", result)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected MCP content: %#v", result.Content[0])
	}
	if strings.Contains(text.Text, receiveCode) || !strings.Contains(text.Text, "[REDACTED]") {
		t.Fatalf("share batch receive code was not redacted: %s", text.Text)
	}
}
