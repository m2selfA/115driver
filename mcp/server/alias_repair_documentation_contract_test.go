package server

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAliasRepairDocumentationFreezesCrashAndAccountScopeContract(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve alias repair documentation contract source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	for _, rel := range []string{"README.md", filepath.Join("mcp", "mcp_example.md")} {
		body, err := os.ReadFile(filepath.Join(repoRoot, rel))
		if err != nil {
			t.Fatalf("read %s: %v", filepath.ToSlash(rel), err)
		}
		text := string(body)
		for _, needle := range []string{
			"not power-loss atomic",
			"monotonic convergence",
			"Requested=0",
			"items=[]",
			"fresh `plan_sync_journal_alias_repair`",
			"Profile-only CLI",
			"authenticated MCP",
			"foreign-account alias binding",
		} {
			if !strings.Contains(text, needle) {
				t.Errorf("%s does not freeze alias repair contract %q", filepath.ToSlash(rel), needle)
			}
		}
	}
}

func TestAliasRepairReleaseCertificationIsPinnedInRepository(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve alias repair release contract source path")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))

	makefile, err := os.ReadFile(filepath.Join(repoRoot, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"ALIAS_REPAIR_CERT_COUNT ?= 10",
		"test-sync-journal-alias-cert:",
		"-count=$(ALIAS_REPAIR_CERT_COUNT)",
		"test-sync-journal-alias-race:",
		"CGO_ENABLED=1 $(GO) test -race",
		"./internal/syncjournal",
		"./cli/cmd",
		"./mcp/server/tools",
	} {
		if !strings.Contains(string(makefile), needle) {
			t.Errorf("Makefile does not pin alias repair release gate %q", needle)
		}
	}

	workflow, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "test.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"sync-session-certification:",
		"runs-on: ubuntu-latest",
		"CGO_ENABLED: \"1\"",
		"make release-ready-race",
	} {
		if !strings.Contains(string(workflow), needle) {
			t.Errorf("test workflow does not pin unified sync/session release gate %q", needle)
		}
	}

	releaseWorkflow, err := os.ReadFile(filepath.Join(repoRoot, ".github", "workflows", "changelog.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"Run complete release readiness gate",
		"CGO_ENABLED: \"1\"",
		"make release-ready-race",
	} {
		if !strings.Contains(string(releaseWorkflow), needle) {
			t.Errorf("release workflow does not pin unified sync/session gate %q", needle)
		}
	}
}

func TestAliasRepairMCPDescriptionsFreezeReviewAndAccountScope(t *testing.T) {
	tools := registeredTools(t, true, false, true)
	byName := make(map[string]string, len(tools))
	for _, tool := range tools {
		byName[tool.Name] = tool.Description
	}
	checks := map[string][]string{
		"diagnose_sync_journal_aliases":     {"exact-account", "foreign-account alias bindings", "fail"},
		"plan_sync_journal_alias_repair":    {"exact-account", "complete currently diagnosed orphan set", "fresh plan"},
		"execute_sync_journal_alias_repair": {"fresh plan", "power-loss atomic", "crash-convergent"},
	}
	for toolName, needles := range checks {
		description := byName[toolName]
		if description == "" {
			t.Fatalf("registered tool %s has no description", toolName)
		}
		for _, needle := range needles {
			if !strings.Contains(description, needle) {
				t.Errorf("%s description does not freeze %q: %s", toolName, needle, description)
			}
		}
	}
}
