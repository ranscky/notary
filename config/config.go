// Package config defines Notary's runtime configuration and how it is loaded
// from the process environment.
package config

import (
	"os"
	"strings"
)

// Default values applied when the corresponding environment variable is unset.
const (
	// DefaultMem0BaseURL is the hosted Mem0 API endpoint.
	DefaultMem0BaseURL = "https://api.mem0.ai"
	// DefaultDBPath is the SQLite ledger file, relative to the working directory.
	DefaultDBPath = "notary.db"
	// DefaultGapLogPath is the gap-log file, relative to the working directory.
	DefaultGapLogPath = "notary-gaps.log"
)

// Environment variable names that override the defaults.
const (
	EnvMem0APIKey  = "NOTARY_MEM0_API_KEY"
	EnvMem0BaseURL = "NOTARY_MEM0_BASE_URL"
	EnvDBPath      = "NOTARY_DB_PATH"
	EnvGapLogPath  = "NOTARY_GAP_LOG_PATH"
)

// Config is the fully resolved runtime configuration. A *Config is passed
// explicitly to the components that need it; there is no global state.
type Config struct {
	// Mem0APIKey authenticates calls to Mem0. It is never defaulted.
	Mem0APIKey string
	// Mem0BaseURL is the base URL of the Mem0 API.
	Mem0BaseURL string
	// DBPath is the path to the SQLite ledger file.
	DBPath string
	// GapLogPath is the path to the gap log.
	GapLogPath string
	// Verbose enables verbose output.
	Verbose bool
}

// Load reads configuration from the process environment.
func Load() (*Config, error) {
	return LoadFrom(environ())
}

// LoadFrom resolves configuration from the supplied environment map. A missing
// key means "unset": documented defaults are applied and Mem0APIKey is left
// empty, which is not an error — it is only required once a Mem0 call is made.
func LoadFrom(env map[string]string) (*Config, error) {
	cfg := &Config{
		Mem0APIKey:  env[EnvMem0APIKey],
		Mem0BaseURL: valueOr(env, EnvMem0BaseURL, DefaultMem0BaseURL),
		DBPath:      valueOr(env, EnvDBPath, DefaultDBPath),
		GapLogPath:  valueOr(env, EnvGapLogPath, DefaultGapLogPath),
	}
	return cfg, nil
}

// valueOr returns env[key] when it is set and non-empty, otherwise fallback.
func valueOr(env map[string]string, key, fallback string) string {
	if v := env[key]; v != "" {
		return v
	}
	return fallback
}

// environ converts os.Environ() into a map, keeping the last value for any
// duplicated key.
func environ() map[string]string {
	pairs := os.Environ()
	env := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	return env
}
