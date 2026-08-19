package auth

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const (
	DefaultTransferInterfaces          = "auto"
	DefaultTransferStrategy            = "file"
	DefaultTransferWorkersPerInterface = 1
	DefaultTransferProbeCacheTTL       = 15 * time.Minute
	DefaultTransferRetries             = 3
	DefaultTransferChunkSize           = "32MiB"
	DefaultTransferHealthCooldown      = 5 * time.Second
	DefaultTransferHealthCooldownMax   = time.Minute
	DefaultTransferResume              = true
	DefaultTransferURLRefreshes        = 3
)

// TransferConfig is machine-specific transfer configuration. It intentionally
// lives at the top-level [transfer] section instead of under an auth profile.
type TransferConfig struct {
	Interfaces          string
	Strategy            string
	WorkersPerInterface int
	ProbeCacheTTL       time.Duration
	Retries             int
	ChunkSize           string
	HealthCooldown      time.Duration
	HealthCooldownMax   time.Duration
	Resume              bool
	URLRefreshes        int
}

func DefaultTransferConfig() TransferConfig {
	return TransferConfig{
		Interfaces:          DefaultTransferInterfaces,
		Strategy:            DefaultTransferStrategy,
		WorkersPerInterface: DefaultTransferWorkersPerInterface,
		ProbeCacheTTL:       DefaultTransferProbeCacheTTL,
		Retries:             DefaultTransferRetries,
		ChunkSize:           DefaultTransferChunkSize,
		HealthCooldown:      DefaultTransferHealthCooldown,
		HealthCooldownMax:   DefaultTransferHealthCooldownMax,
		Resume:              DefaultTransferResume,
		URLRefreshes:        DefaultTransferURLRefreshes,
	}
}

// ResolveTransferConfig reads the machine-wide [transfer] section. Missing
// configuration is not an error: the automatic file-level strategy is the
// zero-configuration default.
func ResolveTransferConfig(configPath string) (TransferConfig, error) {
	config := DefaultTransferConfig()
	path := resolveConfigFilePath(configPath)

	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		if os.IsNotExist(err) {
			return config, nil
		}
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			return config, nil
		}
		return TransferConfig{}, fmt.Errorf("failed to read transfer config: %w", err)
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
		rawTTL := strings.TrimSpace(v.GetString("transfer.probe_cache_ttl"))
		ttl, err := time.ParseDuration(rawTTL)
		if err != nil {
			return TransferConfig{}, fmt.Errorf("invalid transfer.probe_cache_ttl %q: %w", rawTTL, err)
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
			return TransferConfig{}, fmt.Errorf("invalid transfer.health_cooldown %q: %w", raw, err)
		}
		config.HealthCooldown = value
	}
	if v.IsSet("transfer.health_cooldown_max") {
		raw := strings.TrimSpace(v.GetString("transfer.health_cooldown_max"))
		value, err := time.ParseDuration(raw)
		if err != nil {
			return TransferConfig{}, fmt.Errorf("invalid transfer.health_cooldown_max %q: %w", raw, err)
		}
		config.HealthCooldownMax = value
	}
	if v.IsSet("transfer.resume") {
		config.Resume = v.GetBool("transfer.resume")
	}
	if v.IsSet("transfer.url_refreshes") {
		config.URLRefreshes = v.GetInt("transfer.url_refreshes")
	}

	if config.Interfaces == "" {
		return TransferConfig{}, fmt.Errorf("transfer.interfaces must not be empty")
	}
	if config.Strategy == "" {
		return TransferConfig{}, fmt.Errorf("transfer.strategy must not be empty")
	}
	if config.WorkersPerInterface <= 0 {
		return TransferConfig{}, fmt.Errorf("transfer.workers_per_interface must be > 0")
	}
	if config.ProbeCacheTTL <= 0 {
		return TransferConfig{}, fmt.Errorf("transfer.probe_cache_ttl must be > 0")
	}
	if config.Retries < 0 {
		return TransferConfig{}, fmt.Errorf("transfer.retries must be >= 0")
	}
	if config.ChunkSize == "" {
		return TransferConfig{}, fmt.Errorf("transfer.chunk_size must not be empty")
	}
	if config.HealthCooldown <= 0 {
		return TransferConfig{}, fmt.Errorf("transfer.health_cooldown must be > 0")
	}
	if config.HealthCooldownMax < config.HealthCooldown {
		return TransferConfig{}, fmt.Errorf("transfer.health_cooldown_max must be >= transfer.health_cooldown")
	}
	if config.URLRefreshes < 0 {
		return TransferConfig{}, fmt.Errorf("transfer.url_refreshes must be >= 0")
	}
	return config, nil
}

func resolveConfigFilePath(configPath string) string {
	if configPath != "" {
		return configPath
	}
	if envPath := os.Getenv(EnvConfig); envPath != "" {
		return envPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, DefaultConfigDir, DefaultConfigFile)
}
