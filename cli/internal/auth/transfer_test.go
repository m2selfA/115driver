package auth

import (
	"path/filepath"
	"testing"
	"time"
)

func TestResolveTransferConfigDefaultsWhenMissing(t *testing.T) {
	config, err := ResolveTransferConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultTransferConfig()
	if config != want {
		t.Fatalf("unexpected defaults: got %#v want %#v", config, want)
	}
}

func TestResolveTransferConfigReadsTopLevelTransferSection(t *testing.T) {
	configPath := writeTestConfig(t, `
default_profile = "main"

[profiles.main]
cookie = "UID=123"

[transfer]
interfaces = "Ethernet,7"
strategy = "file"
workers_per_interface = 1
probe_cache_ttl = "27m"
retries = 5
chunk_size = "64MiB"
health_cooldown = "8s"
health_cooldown_max = "45s"
resume = false
url_refreshes = 7
`)
	config, err := ResolveTransferConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.Interfaces != "Ethernet,7" || config.Strategy != "file" || config.WorkersPerInterface != 1 || config.ProbeCacheTTL != 27*time.Minute || config.Retries != 5 || config.ChunkSize != "64MiB" || config.HealthCooldown != 8*time.Second || config.HealthCooldownMax != 45*time.Second || config.Resume || config.URLRefreshes != 7 {
		t.Fatalf("unexpected transfer config: %#v", config)
	}
}

func TestResolveTransferConfigRejectsInvalidValues(t *testing.T) {
	tests := []string{
		"[transfer]\ninterfaces = \"\"\n",
		"[transfer]\nworkers_per_interface = 0\n",
		"[transfer]\nprobe_cache_ttl = \"0s\"\n",
		"[transfer]\nretries = -1\n",
		"[transfer]\nhealth_cooldown = \"0s\"\n",
		"[transfer]\nurl_refreshes = -1\n",
		"[transfer]\nhealth_cooldown = \"10s\"\nhealth_cooldown_max = \"5s\"\n",
	}
	for _, content := range tests {
		if _, err := ResolveTransferConfig(writeTestConfig(t, content)); err == nil {
			t.Fatalf("expected invalid transfer config to fail: %q", content)
		}
	}
}
