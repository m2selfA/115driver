package tools

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/go-resty/resty/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGetAppVersionsReturnsStableSortedTypedShape(t *testing.T) {
	calls := 0
	client := driver.New(driver.WithRestyClient(resty.NewWithClient(&http.Client{Transport: mcpTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		body := `{"state":true,"data":{"win":{"version_code":"32.1.0.0"},"android":{"version_code":"35.2.0"}}}`
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})})))
	at := NewAccountTools(client)
	result, output, err := at.getAppVersions(context.Background(), nil, struct{}{})
	if err != nil || result == nil || result.IsError || calls != 1 || output.Count != 2 || len(output.Versions) != 2 {
		t.Fatalf("get_app_versions result=%#v output=%#v err=%v calls=%d", result, output, err, calls)
	}
	if output.Versions[0].App != "android" || output.Versions[0].Version != "35.2.0" || output.Versions[1].App != "win" {
		t.Fatalf("unexpected app-version ordering/content: %#v", output)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected app-version content: %#v", result.Content[0])
	}
	var decoded MCPAppVersionsResult
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Count != output.Count || decoded.Versions[0] != output.Versions[0] {
		t.Fatalf("text/typed app-version outputs diverged: text=%#v typed=%#v", decoded, output)
	}
}

func TestGetAppVersionsRejectsMissingClientAsToolError(t *testing.T) {
	result, output, err := NewAccountTools(nil).getAppVersions(context.Background(), nil, struct{}{})
	if err != nil || result == nil || !result.IsError || output.Count != 0 {
		t.Fatalf("missing-client app versions result=%#v output=%#v err=%v", result, output, err)
	}
}
