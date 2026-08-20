package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestReadDownloadTransferConfigUsesDefaultsWhenConfigMissing(t *testing.T) {
	config, err := readDownloadTransferConfig(filepath.Join(t.TempDir(), "missing.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Interfaces != "auto" || config.Strategy != "file" || config.WorkersPerInterface != 1 || config.Retries != 3 {
		t.Fatalf("unexpected default transfer config: %#v", config)
	}
	if config.ProbeCacheTTL != 15*time.Minute || config.ChunkSize != "32MiB" || config.HealthCooldown != 5*time.Second || config.HealthCooldownMax != time.Minute || !config.Resume || config.URLRefreshes != 3 {
		t.Fatalf("unexpected default transfer tuning: %#v", config)
	}
}

func TestReadDownloadTransferConfigReadsTopLevelTransferSection(t *testing.T) {
	configPath := writeTestConfig(t, `
default_profile = "main"

[profiles.main]
cookie = "UID=1;CID=2"

[transfer]
interfaces = "Ethernet 2,3"
strategy = "file"
workers_per_interface = 3
probe_cache_ttl = "7m"
retries = 5
chunk_size = "64MiB"
health_cooldown = "7s"
health_cooldown_max = "50s"
resume = false
url_refreshes = 5
`)
	config, err := readDownloadTransferConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.Interfaces != "Ethernet 2,3" || config.Strategy != "file" {
		t.Fatalf("unexpected selector/strategy: %#v", config)
	}
	if config.WorkersPerInterface != 3 || config.ProbeCacheTTL != 7*time.Minute || config.Retries != 5 || config.ChunkSize != "64MiB" || config.HealthCooldown != 7*time.Second || config.HealthCooldownMax != 50*time.Second || config.Resume || config.URLRefreshes != 5 {
		t.Fatalf("unexpected transfer config: %#v", config)
	}
}

func TestReadDownloadTransferConfigAcceptsChunkStrategy(t *testing.T) {
	configPath := writeTestConfig(t, `
[transfer]
interfaces = "auto"
strategy = "chunk"
workers_per_interface = 1
probe_cache_ttl = "15m"
retries = 3
chunk_size = "32MiB"
`)
	config, err := readDownloadTransferConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if config.Strategy != "chunk" || config.ChunkSize != "32MiB" {
		t.Fatalf("unexpected chunk config: %#v", config)
	}
}

func TestReadDownloadTransferConfigRejectsInvalidHealthCooldown(t *testing.T) {
	configPath := writeTestConfig(t, `
[transfer]
health_cooldown = "10s"
health_cooldown_max = "5s"
`)
	if _, err := readDownloadTransferConfig(configPath); err == nil {
		t.Fatal("expected invalid health cooldown range to fail before MCP startup")
	}
}

func TestReadDownloadTransferConfigRejectsNegativeURLRefreshes(t *testing.T) {
	configPath := writeTestConfig(t, `
[transfer]
url_refreshes = -1
`)
	if _, err := readDownloadTransferConfig(configPath); err == nil {
		t.Fatal("expected negative URL refresh count to fail before MCP startup")
	}
}

func TestReadDownloadTransferConfigRejectsInvalidChunkSize(t *testing.T) {
	configPath := writeTestConfig(t, `
[transfer]
interfaces = "auto"
strategy = "chunk"
workers_per_interface = 1
probe_cache_ttl = "15m"
retries = 3
chunk_size = "1.5MiB"
`)
	if _, err := readDownloadTransferConfig(configPath); err == nil {
		t.Fatal("expected invalid chunk size to fail before MCP startup")
	}
}
