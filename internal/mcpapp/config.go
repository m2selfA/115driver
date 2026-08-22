package mcpapp

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/internal/sessionconfig"
	"github.com/SheltonZhu/115driver/internal/transfer"
	"github.com/SheltonZhu/115driver/mcp/server"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/viper"
)

// ReadConfigValue reads a profile value from the 115driver config. Missing
// config files, profiles, or keys return an empty string for compatibility with
// the historical MCP entry point.
func ReadConfigValue(configPath, profile, key string) string {
	path := sessionconfig.ResolveConfigFilePath(configPath)

	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return ""
	}

	prof := sessionconfig.ResolveProfileName(configPath, profile)
	return v.GetString("profiles." + prof + "." + key)
}

// ResolveSessionScope returns the exact profile name/scope pair used by CLI
// transfer and sync journals for the same config/profile selection.
func ResolveSessionScope(configPath, profile string) (string, string, error) {
	name := sessionconfig.ResolveProfileName(configPath, profile)
	scope, err := transfer.SessionProfileScope(sessionconfig.ResolveConfigFilePath(configPath), name)
	if err != nil {
		return "", "", err
	}
	return name, scope, nil
}

// CredentialFromCookie parses the cookie form accepted by the MCP CLI.
func CredentialFromCookie(cookie string) (*driver.Credential, error) {
	cr := &driver.Credential{}
	if err := cr.FromCookie(cookie); err != nil {
		return nil, err
	}
	return cr, nil
}

// ValidateOptions checks MCP command-line transfer limits before any network
// operation is attempted.
func ValidateOptions(urlUploadMaxBytes, downloadMaxBytes int64, downloadTimeout time.Duration) error {
	if urlUploadMaxBytes < 0 {
		return fmt.Errorf("url-upload-max-bytes must be >= 0")
	}
	if downloadMaxBytes < 0 {
		return fmt.Errorf("download-max-bytes must be >= 0")
	}
	if downloadTimeout < 0 {
		return fmt.Errorf("download-timeout must be >= 0")
	}
	return nil
}

// ReadDownloadTransferConfig loads the machine-wide transfer section used by
// MCP download_file.
func ReadDownloadTransferConfig(configPath string) (server.DownloadTransferConfig, error) {
	config := server.DefaultDownloadTransferConfig()
	path := resolveConfigPath(configPath)

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

// ReadSessionStoreConfig loads the shared [transfer.sessions] subset without
// validating unrelated transfer tuning. MCP calls this only when a feature
// actually needs persistent session or journal state.
func ReadSessionStoreConfig(configPath string) (sessionconfig.Config, error) {
	return sessionconfig.Resolve(configPath)
}

func resolveConfigPath(configPath string) string {
	return sessionconfig.ResolveConfigFilePath(configPath)
}
