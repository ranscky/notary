// Package config defines Notary's runtime configuration and how it is loaded
// from the process environment.
package config

import (
	"os"
	"strings"

	"notary/internal/interceptor"
	"notary/internal/reconcile"
)

// Default values applied when the corresponding environment variable is unset.
const (
	// DefaultMem0BaseURL is the hosted Mem0 API endpoint.
	DefaultMem0BaseURL = "https://api.mem0.ai"
	// DefaultDBPath is the SQLite ledger file, relative to the working directory.
	DefaultDBPath = "notary.db"
	// DefaultGapLogPath is the gap-log file, relative to the working directory.
	DefaultGapLogPath = "notary-gaps.log"
	// DefaultSigningKeyEnv is the name of the environment variable that holds
	// the base64 signing key material, and the default value of
	// Config.SigningKeyEnv.
	DefaultSigningKeyEnv = "NOTARY_SIGNING_KEY"
)

// Environment variable names that override the defaults.
const (
	EnvMem0APIKey  = "NOTARY_MEM0_API_KEY"
	EnvMem0BaseURL = "NOTARY_MEM0_BASE_URL"
	EnvDBPath      = "NOTARY_DB_PATH"
	EnvGapLogPath  = "NOTARY_GAP_LOG_PATH"
	// EnvTrustedKeysPath is the environment variable holding the path to a file
	// of trusted public keys.
	EnvTrustedKeysPath = "NOTARY_TRUSTED_KEYS_PATH"
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
	// SigningKeyEnv is the name of the environment variable holding the base64
	// signing key material. LoadFrom always sets it to DefaultSigningKeyEnv; a
	// caller may override it programmatically. There is deliberately no
	// environment variable that sets it, because the variable it names is where
	// the key material itself lives -- reading that value here would copy the key
	// into the config. It holds a variable name, not the key itself, so config
	// stays free of cryptographic material.
	SigningKeyEnv string
	// TrustedKeysPath is the path to a file of trusted public keys; an empty
	// string means none is configured.
	TrustedKeysPath string
	// FailMode selects how the interceptor behaves when the audit ledger cannot
	// accept a record. It defaults to interceptor.FailOpenLoud, the only mode
	// implemented in v1.
	//
	// There is deliberately NO environment variable for this field.
	// interceptor.FailClosed is defined but unimplemented, so exposing the mode
	// via the environment would invite an operator to configure a mode that can
	// only fail. When FailClosed is implemented, an env override can be added
	// and validated with interceptor.FailMode.Validate.
	FailMode interceptor.FailMode
	// ReconcileMode selects how reconciliation is driven. It defaults to
	// reconcile.ReconcileCommand, the only mode implemented in v1: a one-shot
	// pass commanded by the CLI.
	//
	// There is deliberately NO environment variable for this field.
	// reconcile.ReconcileInProcess is defined but reserved -- no code path
	// implements it -- so exposing the mode via the environment would invite an
	// operator to select a mode this build cannot honour. When InProcess is
	// implemented, an env override can be added and validated with
	// reconcile.ReconcileMode.Validate.
	ReconcileMode reconcile.ReconcileMode
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
		// SigningKeyEnv is the NAME of the variable holding key material, not the
		// material itself. It is never read from the environment: the variable it
		// names (NOTARY_SIGNING_KEY) holds the key's VALUE, so copying that value
		// here would move cryptographic material into the config -- and from there
		// into any error that renders cfg.SigningKeyEnv.
		SigningKeyEnv:   DefaultSigningKeyEnv,
		TrustedKeysPath: env[EnvTrustedKeysPath],
		FailMode:        interceptor.FailOpenLoud,
		ReconcileMode:   reconcile.ReconcileCommand,
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
