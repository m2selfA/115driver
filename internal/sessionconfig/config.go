package sessionconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/viper"
)

const (
	DefaultConfigDir  = ".115driver"
	DefaultConfigFile = "config.toml"
	EnvConfig         = "DRIVER115_CONFIG"
	EnvProfile        = "DRIVER115_PROFILE"
	EnvSessionDir     = "115DRIVER_SESSION_DIR"
	DefaultProfile    = "main"

	DefaultAutoGC         = true
	DefaultGCInterval     = 24 * time.Hour
	DefaultRetention      = 30 * 24 * time.Hour
	DefaultTrashRetention = 7 * 24 * time.Hour
)

// Config is the storage/retention subset of [transfer.sessions]. It is shared
// by CLI session management and any non-CLI frontend that persists resumable
// transfer/sync state.
type Config struct {
	SessionDir            string
	SessionAutoGC         bool
	SessionGCInterval     time.Duration
	SessionRetention      time.Duration
	SessionTrashRetention time.Duration
}

// ResolveConfigFilePath applies the process-wide config path precedence shared
// by CLI and MCP: explicit flag, DRIVER115_CONFIG, then ~/.115driver/config.toml.
func ResolveConfigFilePath(configPath string) string {
	if value := strings.TrimSpace(configPath); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv(EnvConfig)); value != "" {
		return value
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, DefaultConfigDir, DefaultConfigFile)
}

// ResolveProfileName applies the profile-name precedence shared by CLI and MCP:
// explicit request, DRIVER115_PROFILE, config default_profile, then "main".
// All sources are trimmed so the derived session profile scope is stable.
func ResolveProfileName(configPath, requested string) string {
	if value := strings.TrimSpace(requested); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv(EnvProfile)); value != "" {
		return value
	}
	v := viper.New()
	v.SetConfigFile(ResolveConfigFilePath(configPath))
	if err := v.ReadInConfig(); err == nil {
		if value := strings.TrimSpace(v.GetString("default_profile")); value != "" {
			return value
		}
	}
	return DefaultProfile
}

func ExpandUserPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(value, "~/") || strings.HasPrefix(value, "~\\") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, value[2:])
	}
	return value
}

func DefaultDir() string {
	if value := strings.TrimSpace(os.Getenv(EnvSessionDir)); value != "" {
		return ExpandUserPath(value)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, DefaultConfigDir, "sessions")
}

func Default() Config {
	return Config{
		SessionDir:            DefaultDir(),
		SessionAutoGC:         DefaultAutoGC,
		SessionGCInterval:     DefaultGCInterval,
		SessionRetention:      DefaultRetention,
		SessionTrashRetention: DefaultTrashRetention,
	}
}

// Resolve reads only [transfer.sessions]. Invalid unrelated transfer settings
// are intentionally ignored; invalid session-store settings fail closed.
func Resolve(configPath string) (Config, error) {
	config := Default()
	v := viper.New()
	v.SetConfigFile(ResolveConfigFilePath(configPath))
	if err := v.ReadInConfig(); err != nil {
		if os.IsNotExist(err) {
			return config, nil
		}
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			return config, nil
		}
		return Config{}, fmt.Errorf("failed to read session store config: %w", err)
	}

	if v.IsSet("transfer.sessions.dir") {
		config.SessionDir = ExpandUserPath(strings.TrimSpace(v.GetString("transfer.sessions.dir")))
	}
	if v.IsSet("transfer.sessions.auto_gc") {
		config.SessionAutoGC = v.GetBool("transfer.sessions.auto_gc")
	}
	for key, target := range map[string]*time.Duration{
		"transfer.sessions.gc_interval":     &config.SessionGCInterval,
		"transfer.sessions.retention":       &config.SessionRetention,
		"transfer.sessions.trash_retention": &config.SessionTrashRetention,
	} {
		if !v.IsSet(key) {
			continue
		}
		raw := strings.TrimSpace(v.GetString(key))
		value, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid %s %q: %w", key, raw, err)
		}
		*target = value
	}
	if value := strings.TrimSpace(os.Getenv(EnvSessionDir)); value != "" {
		config.SessionDir = ExpandUserPath(value)
	}
	if err := config.Validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (config Config) Validate() error {
	if strings.TrimSpace(config.SessionDir) == "" {
		return fmt.Errorf("transfer.sessions.dir must not be empty")
	}
	if config.SessionGCInterval <= 0 {
		return fmt.Errorf("transfer.sessions.gc_interval must be > 0")
	}
	if config.SessionRetention <= 0 {
		return fmt.Errorf("transfer.sessions.retention must be > 0")
	}
	if config.SessionTrashRetention <= 0 {
		return fmt.Errorf("transfer.sessions.trash_retention must be > 0")
	}
	return nil
}
