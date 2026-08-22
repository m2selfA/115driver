package sessionconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveConfigFilePathPrecedence(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvConfig, "")
	if got, want := ResolveConfigFilePath(""), filepath.Join(home, DefaultConfigDir, DefaultConfigFile); filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("default config path = %q, want %q", got, want)
	}
	env := filepath.Join(t.TempDir(), "env.toml")
	t.Setenv(EnvConfig, env)
	if got := ResolveConfigFilePath(""); filepath.Clean(got) != filepath.Clean(env) {
		t.Fatalf("environment config path ignored: %q", got)
	}
	explicit := filepath.Join(t.TempDir(), "explicit.toml")
	if got := ResolveConfigFilePath("  " + explicit + "  "); filepath.Clean(got) != filepath.Clean(explicit) {
		t.Fatalf("explicit config path ignored: %q", got)
	}
}

func TestResolveProfileNamePrecedenceAndTrimming(t *testing.T) {
	path := writeConfig(t, `default_profile = " configured "`)
	t.Setenv(EnvProfile, "")
	if got := ResolveProfileName(path, ""); got != "configured" {
		t.Fatalf("configured profile = %q", got)
	}
	t.Setenv(EnvProfile, " env-profile ")
	if got := ResolveProfileName(path, ""); got != "env-profile" {
		t.Fatalf("environment profile = %q", got)
	}
	if got := ResolveProfileName(path, " explicit "); got != "explicit" {
		t.Fatalf("explicit profile = %q", got)
	}
	t.Setenv(EnvProfile, "")
	if got := ResolveProfileName(filepath.Join(t.TempDir(), "missing.toml"), ""); got != DefaultProfile {
		t.Fatalf("fallback profile = %q, want %q", got, DefaultProfile)
	}
}

func TestResolveReadsOnlySessionSectionAndEnvironmentWins(t *testing.T) {
	t.Setenv(EnvSessionDir, "")
	configured := filepath.Join(t.TempDir(), "configured")
	path := writeConfig(t, `[transfer]
interfaces = ""
workers_per_interface = 0

[transfer.sessions]
dir = "`+filepath.ToSlash(configured)+`"
auto_gc = false
gc_interval = "12h"
retention = "720h"
trash_retention = "168h"
`)
	config, err := Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(config.SessionDir) != filepath.Clean(configured) || config.SessionAutoGC || config.SessionGCInterval != 12*time.Hour || config.SessionRetention != 30*24*time.Hour || config.SessionTrashRetention != 7*24*time.Hour {
		t.Fatalf("unexpected session config: %#v", config)
	}

	override := filepath.Join(t.TempDir(), "override")
	t.Setenv(EnvSessionDir, override)
	config, err = Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(config.SessionDir) != filepath.Clean(override) {
		t.Fatalf("session environment override ignored: %#v", config)
	}
}

func TestResolveMissingUsesDefaultsAndInvalidSettingsFailClosed(t *testing.T) {
	t.Setenv(EnvSessionDir, "")
	config, err := Resolve(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if config != Default() {
		t.Fatalf("missing config defaults = %#v, want %#v", config, Default())
	}
	path := writeConfig(t, `[transfer.sessions]
retention = "not-a-duration"
`)
	if _, err := Resolve(path); err == nil || !strings.Contains(err.Error(), "transfer.sessions.retention") {
		t.Fatalf("invalid session retention accepted: %v", err)
	}
}

func TestExpandUserPathAndDefaultDir(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvSessionDir, "")
	if got, want := DefaultDir(), filepath.Join(home, DefaultConfigDir, "sessions"); filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("default session dir = %q, want %q", got, want)
	}
	if got := ExpandUserPath("~/nested"); filepath.Clean(got) != filepath.Join(home, "nested") {
		t.Fatalf("home expansion = %q", got)
	}
}
