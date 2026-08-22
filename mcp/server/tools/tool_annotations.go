package tools

import "github.com/modelcontextprotocol/go-sdk/mcp"

func mcpBoolHint(value bool) *bool {
	return &value
}

func mcpReadOnlyToolAnnotations() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:   true,
		IdempotentHint: true,
		OpenWorldHint:  mcpBoolHint(true),
	}
}

func mcpMutationToolAnnotations(destructive bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		ReadOnlyHint:    false,
		DestructiveHint: mcpBoolHint(destructive),
		OpenWorldHint:   mcpBoolHint(true),
	}
}

func mcpDestructiveToolAnnotations() *mcp.ToolAnnotations {
	return mcpMutationToolAnnotations(true)
}
