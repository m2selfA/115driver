package tools

import "testing"

func TestDestructiveToolsDisabledByDefault(t *testing.T) {
	ft := NewFileTools(nil)
	if ft.allowDestructive {
		t.Fatal("expected destructive MCP tools to be disabled by default")
	}
}

func TestSensitiveToolsDisabledByDefault(t *testing.T) {
	ft := NewFileTools(nil)
	if ft.allowSensitive {
		t.Fatal("expected sensitive MCP tools to be disabled by default")
	}
}

func TestWithDestructiveToolsAllowsDestructiveTools(t *testing.T) {
	ft := NewFileTools(nil, WithDestructiveTools(true))
	if !ft.allowDestructive {
		t.Fatal("expected destructive MCP tools to be enabled")
	}
}

func TestWithSensitiveToolsAllowsSensitiveTools(t *testing.T) {
	ft := NewFileTools(nil, WithSensitiveTools(true))
	if !ft.allowSensitive {
		t.Fatal("expected sensitive MCP tools to be enabled")
	}
}
