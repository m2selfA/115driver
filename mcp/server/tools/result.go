package tools

import (
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpTypedTextResult centralizes the dual-output MCP contract: legacy
// TextContent remains available for older clients while the generic handler
// return value becomes StructuredContent for schema-aware clients.
func mcpTypedTextResult[T any](text string, structured T, isError bool) (*mcp.CallToolResult, T, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: isError,
	}, structured, nil
}

// mcpTypedJSONResult is the common case where legacy TextContent is JSON. The
// legacy payload may intentionally differ from the typed payload, allowing old
// wire shapes to stay stable while StructuredContent evolves independently.
func mcpTypedJSONResult[T any](label string, legacyPayload any, structured T, isError bool) (*mcp.CallToolResult, T, error) {
	encoded, err := json.Marshal(legacyPayload)
	if err != nil {
		var zero T
		return toolError(fmt.Sprintf("Failed to serialize %s result: %v", label, err)), zero, nil
	}
	return mcpTypedTextResult(string(encoded), structured, isError)
}
