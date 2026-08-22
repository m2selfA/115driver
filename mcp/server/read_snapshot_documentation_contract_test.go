package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMCPDocumentationExplainsRequestScopedReadSnapshotBounds(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve documentation contract source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	for _, rel := range []string{"README.md", filepath.Join("mcp", "mcp_example.md")} {
		body, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", filepath.ToSlash(rel), err)
		}
		text := string(body)
		for _, needle := range []string{
			"request-scoped",
			"50,000",
			"100,000",
			"never crosses tool calls",
			"compare_directories",
		} {
			if !strings.Contains(text, needle) {
				t.Errorf("%s does not explain read snapshot contract %q", filepath.ToSlash(rel), needle)
			}
		}
	}
}
