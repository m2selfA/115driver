package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/mcp/server"
	"github.com/spf13/viper"
)

func readDownloadTransferConfig(configPath string) (server.DownloadTransferConfig, error) {
	config := server.DefaultDownloadTransferConfig()
	path := resolveMCPConfigPath(configPath)

	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		if os.IsNotExist(err) {
			return config, nil
		}
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			return config, nil
		}
		return server.DownloadTransferConfig{}, fmt.Errorf("failed to read transfer config: %w", err)
	}

	if v.IsSet("transfer.interfaces") {
		config.Interfaces = strings.TrimSpace(v.GetString("transfer.interfaces"))
	}
	if v.IsSet("transfer.strategy") {
		config.Strategy = strings.ToLower(strings.TrimSpace(v.GetString("transfer.strategy")))
	}
	if v.IsSet("transfer.workers_per_interface") {
		config.WorkersPerInterface = v.GetInt("transfer.workers_per_interface")
	}
	if v.IsSet("transfer.probe_cache_ttl") {
		raw := strings.TrimSpace(v.GetString("transfer.probe_cache_ttl"))
		ttl, err := time.ParseDuration(raw)
		if err != nil {
			return server.DownloadTransferConfig{}, fmt.Errorf("invalid transfer.probe_cache_ttl %q: %w", raw, err)
		}
		config.ProbeCacheTTL = ttl
	}
	if v.IsSet("transfer.retries") {
		config.Retries = v.GetInt("transfer.retries")
	}
	if v.IsSet("transfer.chunk_size") {
		config.ChunkSize = strings.TrimSpace(v.GetString("transfer.chunk_size"))
	}
	if v.IsSet("transfer.health_cooldown") {
		raw := strings.TrimSpace(v.GetString("transfer.health_cooldown"))
		value, err := time.ParseDuration(raw)
		if err != nil {
			return server.DownloadTransferConfig{}, fmt.Errorf("invalid transfer.health_cooldown %q: %w", raw, err)
		}
		config.HealthCooldown = value
	}
	if v.IsSet("transfer.health_cooldown_max") {
		raw := strings.TrimSpace(v.GetString("transfer.health_cooldown_max"))
		value, err := time.ParseDuration(raw)
		if err != nil {
			return server.DownloadTransferConfig{}, fmt.Errorf("invalid transfer.health_cooldown_max %q: %w", raw, err)
		}
		config.HealthCooldownMax = value
	}
	if v.IsSet("transfer.resume") {
		config.Resume = v.GetBool("transfer.resume")
	}
	if v.IsSet("transfer.url_refreshes") {
		config.URLRefreshes = v.GetInt("transfer.url_refreshes")
	}
	if err := config.Validate(); err != nil {
		return server.DownloadTransferConfig{}, err
	}
	return config, nil
}

func resolveMCPConfigPath(configPath string) string {
	if configPath != "" {
		return configPath
	}
	if envPath := os.Getenv("DRIVER115_CONFIG"); envPath != "" {
		return envPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".115driver", "config.toml")
}
