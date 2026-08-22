package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResolveTransferConfigReadsSessionStoreSection(t *testing.T) {
	t.Setenv(EnvSessionDir, "")
	root := filepath.Join(t.TempDir(), "sessions")
	configPath := writeTestConfig(t, `
[transfer.sessions]
dir = "`+filepath.ToSlash(root)+`"
auto_gc = false
gc_interval = "12h"
retention = "720h"
trash_retention = "168h"
`)
	config, err := ResolveTransferConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(config.SessionDir) != filepath.Clean(root) || config.SessionAutoGC || config.SessionGCInterval != 12*time.Hour || config.SessionRetention != 30*24*time.Hour || config.SessionTrashRetention != 7*24*time.Hour {
		t.Fatalf("unexpected session config: %#v", config)
	}
}

func TestResolveSessionStoreConfigReadsOnlySessionSection(t *testing.T) {
	t.Setenv(EnvSessionDir, "")
	root := filepath.Join(t.TempDir(), "sessions")
	configPath := writeTestConfig(t, `
[transfer]
interfaces = ""
workers_per_interface = 0
chunk_size = ""

[transfer.sessions]
dir = "`+filepath.ToSlash(root)+`"
auto_gc = false
gc_interval = "12h"
retention = "720h"
trash_retention = "168h"
`)
	config, err := ResolveSessionStoreConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Clean(config.SessionDir) != filepath.Clean(root) || config.SessionAutoGC || config.SessionGCInterval != 12*time.Hour || config.SessionRetention != 30*24*time.Hour || config.SessionTrashRetention != 7*24*time.Hour {
		t.Fatalf("unexpected session-only config: %#v", config)
	}
	if _, err := ResolveTransferConfig(configPath); err == nil {
		t.Fatal("full transfer config unexpectedly accepted intentionally invalid transfer settings")
	}
}

func TestResolveSessionStoreConfigRejectsInvalidSessionSettings(t *testing.T) {
	t.Setenv(EnvSessionDir, "")
	configPath := writeTestConfig(t, `[transfer.sessions]
retention = "not-a-duration"
`)
	if _, err := ResolveSessionStoreConfig(configPath); err == nil || !strings.Contains(err.Error(), "transfer.sessions.retention") {
		t.Fatalf("invalid session retention was accepted: %v", err)
	}
}

func TestSessionDirEnvironmentOverridesConfig(t *testing.T) {
	configured := filepath.Join(t.TempDir(), "configured")
	override := filepath.Join(t.TempDir(), "override")
	t.Setenv(EnvSessionDir, override)
	configPath := writeTestConfig(t, "[transfer.sessions]\ndir = \""+filepath.ToSlash(configured)+"\"\n")
	for name, resolve := range map[string]func(string) (string, error){
		"full": func(path string) (string, error) {
			config, err := ResolveTransferConfig(path)
			return config.SessionDir, err
		},
		"session-only": func(path string) (string, error) {
			config, err := ResolveSessionStoreConfig(path)
			return config.SessionDir, err
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := resolve(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if filepath.Clean(got) != filepath.Clean(override) {
				t.Fatalf("session dir env override was ignored: got %q want %q", got, override)
			}
		})
	}
}

func TestDefaultSessionDirUsesHome(t *testing.T) {
	t.Setenv(EnvSessionDir, "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, DefaultConfigDir, "sessions")
	if got := DefaultTransferConfig().SessionDir; filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("unexpected default session dir: got %q want %q", got, want)
	}
}
