package auth

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/SheltonZhu/115driver/internal/sessionconfig"
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
	DefaultSessionAutoGC               = sessionconfig.DefaultAutoGC
	DefaultSessionGCInterval           = sessionconfig.DefaultGCInterval
	DefaultSessionRetention            = sessionconfig.DefaultRetention
	DefaultSessionTrashRetention       = sessionconfig.DefaultTrashRetention
	EnvSessionDir                      = sessionconfig.EnvSessionDir
)

// TransferConfig is machine-specific transfer configuration. It intentionally
// lives at the top-level [transfer] section instead of under an auth profile.
type TransferConfig struct {
	Interfaces            string
	Strategy              string
	WorkersPerInterface   int
	ProbeCacheTTL         time.Duration
	Retries               int
	ChunkSize             string
	HealthCooldown        time.Duration
	HealthCooldownMax     time.Duration
	Resume                bool
	URLRefreshes          int
	SessionDir            string
	SessionAutoGC         bool
	SessionGCInterval     time.Duration
	SessionRetention      time.Duration
	SessionTrashRetention time.Duration
}

// SessionStoreConfig is the storage/retention subset of transfer configuration.
// Session inspection, sync journaling, and directory-only sync execution use
// this resolver so unrelated file-transfer tuning cannot block metadata work.
type SessionStoreConfig = sessionconfig.Config

func DefaultSessionStoreConfig() SessionStoreConfig {
	return sessionconfig.Default()
}

// ResolveSessionStoreConfig reads only [transfer.sessions]. Invalid unrelated
// transfer settings are intentionally ignored; invalid session-store settings
// still fail closed.
func ResolveSessionStoreConfig(configPath string) (SessionStoreConfig, error) {
	return sessionconfig.Resolve(configPath)
}

func DefaultTransferConfig() TransferConfig {
	return TransferConfig{
		Interfaces:            DefaultTransferInterfaces,
		Strategy:              DefaultTransferStrategy,
		WorkersPerInterface:   DefaultTransferWorkersPerInterface,
		ProbeCacheTTL:         DefaultTransferProbeCacheTTL,
		Retries:               DefaultTransferRetries,
		ChunkSize:             DefaultTransferChunkSize,
		HealthCooldown:        DefaultTransferHealthCooldown,
		HealthCooldownMax:     DefaultTransferHealthCooldownMax,
		Resume:                DefaultTransferResume,
		URLRefreshes:          DefaultTransferURLRefreshes,
		SessionDir:            sessionconfig.DefaultDir(),
		SessionAutoGC:         DefaultSessionAutoGC,
		SessionGCInterval:     DefaultSessionGCInterval,
		SessionRetention:      DefaultSessionRetention,
		SessionTrashRetention: DefaultSessionTrashRetention,
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
		raw := strings.TrimSpace(v.GetString("transfer.probe_cache_ttl"))
		value, err := time.ParseDuration(raw)
		if err != nil {
			return TransferConfig{}, fmt.Errorf("invalid transfer.probe_cache_ttl %q: %w", raw, err)
		}
		config.ProbeCacheTTL = value
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
	if v.IsSet("transfer.sessions.dir") {
		config.SessionDir = expandUserPath(strings.TrimSpace(v.GetString("transfer.sessions.dir")))
	}
	if v.IsSet("transfer.sessions.auto_gc") {
		config.SessionAutoGC = v.GetBool("transfer.sessions.auto_gc")
	}
	if v.IsSet("transfer.sessions.gc_interval") {
		raw := strings.TrimSpace(v.GetString("transfer.sessions.gc_interval"))
		value, err := time.ParseDuration(raw)
		if err != nil {
			return TransferConfig{}, fmt.Errorf("invalid transfer.sessions.gc_interval %q: %w", raw, err)
		}
		config.SessionGCInterval = value
	}
	if v.IsSet("transfer.sessions.retention") {
		raw := strings.TrimSpace(v.GetString("transfer.sessions.retention"))
		value, err := time.ParseDuration(raw)
		if err != nil {
			return TransferConfig{}, fmt.Errorf("invalid transfer.sessions.retention %q: %w", raw, err)
		}
		config.SessionRetention = value
	}
	if v.IsSet("transfer.sessions.trash_retention") {
		raw := strings.TrimSpace(v.GetString("transfer.sessions.trash_retention"))
		value, err := time.ParseDuration(raw)
		if err != nil {
			return TransferConfig{}, fmt.Errorf("invalid transfer.sessions.trash_retention %q: %w", raw, err)
		}
		config.SessionTrashRetention = value
	}
	if envDir := strings.TrimSpace(os.Getenv(EnvSessionDir)); envDir != "" {
		config.SessionDir = expandUserPath(envDir)
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
	if strings.TrimSpace(config.SessionDir) == "" {
		return TransferConfig{}, fmt.Errorf("transfer.sessions.dir must not be empty")
	}
	if config.SessionGCInterval <= 0 {
		return TransferConfig{}, fmt.Errorf("transfer.sessions.gc_interval must be > 0")
	}
	if config.SessionRetention <= 0 {
		return TransferConfig{}, fmt.Errorf("transfer.sessions.retention must be > 0")
	}
	if config.SessionTrashRetention <= 0 {
		return TransferConfig{}, fmt.Errorf("transfer.sessions.trash_retention must be > 0")
	}
	return config, nil
}

func resolveConfigFilePath(configPath string) string {
	return ResolveConfigFilePath(configPath)
}

func defaultSessionDir() string {
	return sessionconfig.DefaultDir()
}

func expandUserPath(value string) string {
	return sessionconfig.ExpandUserPath(value)
}
