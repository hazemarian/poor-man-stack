// Package config loads the pmcluster runtime configuration from a YAML file
// and PMCLUSTER_* environment variables, in that priority order.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/viper"
)

// Config is the resolved runtime configuration for the pmcluster daemon
// and CLI commands. It is constructed via Load.
type Config struct {
	// ListenAddr is the bind address for `pmcluster serve`.
	// Default: 127.0.0.1:9090. Traefik (in the swarm) reaches the daemon
	// via host.docker.internal:9090, so binding to localhost is sufficient.
	ListenAddr string `mapstructure:"listen_addr"`

	// DataDir is the directory holding ~/.pmcluster state (db, encryption key,
	// rendered configs). Default: $HOME/.pmcluster.
	DataDir string `mapstructure:"data_dir"`

	// LogLevel controls daemon log verbosity. One of: debug, info, warn, error.
	LogLevel string `mapstructure:"log_level"`
}

// DBPath returns the SQLite database file path derived from DataDir.
// Stable across platforms; do not change without a migration story.
func (c *Config) DBPath() string {
	return filepath.Join(c.DataDir, "data.db")
}

// EncryptionKeyPath returns the AES-GCM key file path derived from DataDir.
// Created with mode 0600 by `pmcluster init`; absent until then.
func (c *Config) EncryptionKeyPath() string {
	return filepath.Join(c.DataDir, ".encryption_key")
}

// ConfigPath returns the YAML config file path derived from DataDir.
// Loaded at startup, env vars override.
func (c *Config) ConfigPath() string {
	return filepath.Join(c.DataDir, "config.yaml")
}

// LogsDir returns the directory where pmcluster writes its rotating JSON
// log files (one per UTC day). Created on demand by the logger package.
func (c *Config) LogsDir() string {
	return filepath.Join(c.DataDir, "logs")
}

// defaultConfig returns a Config populated with safe defaults — DataDir
// resolved against the current user's $HOME. Returns an error only if $HOME
// cannot be determined (very unusual; typically a misconfigured environment).
func defaultConfig() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home dir: %w", err)
	}
	return &Config{
		ListenAddr: "127.0.0.1:9090",
		DataDir:    filepath.Join(home, ".pmcluster"),
		LogLevel:   "info",
	}, nil
}

// Load resolves configuration in this order: defaults → optional YAML file → env.
// Later sources override earlier ones.
//
// configPath: explicit path from --config; if empty, falls back to
//
//	$DataDir/config.yaml. Missing files are not an error (defaults apply);
//	a present-but-malformed file is.
//
// env: any PMCLUSTER_<UPPER_FIELD> overrides (e.g. PMCLUSTER_LISTEN_ADDR).
func Load(configPath string) (*Config, error) {
	cfg, err := defaultConfig()
	if err != nil {
		return nil, err
	}

	v := viper.New()
	v.SetEnvPrefix("PMCLUSTER")
	v.AutomaticEnv()
	// SetDefault is required so AutomaticEnv binds the keys; without these
	// calls viper has no idea PMCLUSTER_LISTEN_ADDR maps to listen_addr.
	v.SetDefault("listen_addr", cfg.ListenAddr)
	v.SetDefault("data_dir", cfg.DataDir)
	v.SetDefault("log_level", cfg.LogLevel)

	resolved := configPath
	if resolved == "" {
		resolved = cfg.ConfigPath()
	}
	v.SetConfigFile(resolved)
	v.SetConfigType("yaml")

	if err := v.ReadInConfig(); err != nil {
		// Missing file is fine — caller may not have run `pmcluster init` yet,
		// or env vars cover everything. Anything else (parse error, permission
		// denied) is surfaced.
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) {
			return nil, fmt.Errorf("read config %s: %w", resolved, err)
		}
		if !os.IsNotExist(pathErr) {
			return nil, fmt.Errorf("read config %s: %w", resolved, err)
		}
	}

	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.ListenAddr == "" {
		return errors.New("listen_addr is required")
	}
	if c.DataDir == "" {
		return errors.New("data_dir is required")
	}
	switch c.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log_level must be one of debug|info|warn|error, got %q", c.LogLevel)
	}
	return nil
}
