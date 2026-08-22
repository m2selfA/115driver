package tools

import (
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPTypedJSONResultKeepsLegacyAndStructuredPayloadsIndependent(t *testing.T) {
	type structuredResult struct {
		Safe string `json:"safe"`
	}
	result, output, err := mcpTypedJSONResult("test", map[string]any{"legacy": "value"}, structuredResult{Safe: "typed"}, true)
	if err != nil || result == nil || !result.IsError || output.Safe != "typed" {
		t.Fatalf("mcpTypedJSONResult = %#v output=%#v err=%v", result, output, err)
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("unexpected legacy content: %#v", result.Content)
	}
	var legacy map[string]any
	if err := json.Unmarshal([]byte(text.Text), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy["legacy"] != "value" {
		t.Fatalf("legacy payload changed: %#v", legacy)
	}
}
