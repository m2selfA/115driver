package mcpapp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SheltonZhu/115driver/internal/sessionconfig"
	"github.com/SheltonZhu/115driver/internal/transfer"
)

func writeMCPScopeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveSessionScopeMatchesSharedProfileAndTransferScope(t *testing.T) {
	path := writeMCPScopeConfig(t, `default_profile = " configured "
[profiles.configured]
cookie = "configured-cookie"
[profiles.env]
cookie = "env-cookie"
[profiles.explicit]
cookie = "explicit-cookie"
`)
	t.Setenv(sessionconfig.EnvProfile, " env ")
	name, scope, err := ResolveSessionScope(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "env" {
		t.Fatalf("resolved MCP profile = %q, want env", name)
	}
	wantScope, err := transfer.SessionProfileScope(sessionconfig.ResolveConfigFilePath(path), name)
	if err != nil {
		t.Fatal(err)
	}
	if scope != wantScope {
		t.Fatalf("resolved MCP scope = %q, want %q", scope, wantScope)
	}
	if got := ReadConfigValue(path, "", "cookie"); got != "env-cookie" {
		t.Fatalf("ReadConfigValue did not share profile resolution: %q", got)
	}

	name, scope, err = ResolveSessionScope(path, " explicit ")
	if err != nil || name != "explicit" {
		t.Fatalf("explicit MCP profile = %q scope=%q err=%v", name, scope, err)
	}
	if got := ReadConfigValue(path, " explicit ", "cookie"); got != "explicit-cookie" {
		t.Fatalf("explicit ReadConfigValue profile = %q", got)
	}
}

func TestResolveSessionScopeFallsBackToMain(t *testing.T) {
	path := writeMCPScopeConfig(t, `[profiles.main]
cookie = "main-cookie"
`)
	t.Setenv(sessionconfig.EnvProfile, "")
	name, scope, err := ResolveSessionScope(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if name != sessionconfig.DefaultProfile || scope == "" {
		t.Fatalf("fallback MCP profile/scope = %q/%q", name, scope)
	}
	if got := ReadConfigValue(path, "", "cookie"); got != "main-cookie" {
		t.Fatalf("fallback ReadConfigValue = %q", got)
	}
}
