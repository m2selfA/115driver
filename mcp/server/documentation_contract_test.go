package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMCPDocumentationCoversRegisteredToolSurface(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve documentation contract source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	docPaths := []string{
		filepath.Join(repoRoot, "README.md"),
		filepath.Join(repoRoot, "mcp", "mcp_example.md"),
	}

	docs := make(map[string]string, len(docPaths))
	for _, docPath := range docPaths {
		body, err := os.ReadFile(docPath)
		if err != nil {
			t.Fatalf("read MCP documentation %s: %v", filepath.Base(docPath), err)
		}
		docs[docPath] = string(body)
	}

	for _, tool := range registeredTools(t, true, true, true) {
		needle := "`" + tool.Name + "`"
		for docPath, body := range docs {
			if !strings.Contains(body, needle) {
				t.Errorf("registered MCP tool %q is missing from %s", tool.Name, filepath.ToSlash(docPath))
			}
		}
	}
}
