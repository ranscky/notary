package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/interceptor"
)

// TestLoadFrom verifies LoadFrom applies documented defaults and that every
// value can be overridden through the supplied environment map.
func TestLoadFrom(t *testing.T) {
	t.Run("empty env yields non-empty DBPath default", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{})
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.NotEmpty(t, cfg.DBPath)
	})

	t.Run("NOTARY_DB_PATH overrides the default", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{"NOTARY_DB_PATH": "/tmp/x.db"})
		require.NoError(t, err)
		assert.Equal(t, "/tmp/x.db", cfg.DBPath)
	})

	t.Run("missing MEM0_API_KEY is not an error", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{})
		require.NoError(t, err)
		assert.Empty(t, cfg.Mem0APIKey)
	})

	t.Run("documented defaults", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{})
		require.NoError(t, err)
		assert.Equal(t, "https://api.mem0.ai", cfg.Mem0BaseURL)
		assert.Equal(t, "notary.db", cfg.DBPath)
		assert.Equal(t, "notary-gaps.log", cfg.GapLogPath)
	})

	t.Run("every value is overridable by env", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{
			"NOTARY_MEM0_API_KEY":  "sk-test",
			"NOTARY_MEM0_BASE_URL": "https://example.test",
			"NOTARY_DB_PATH":       "/tmp/other.db",
			"NOTARY_GAP_LOG_PATH":  "/tmp/other.log",
		})
		require.NoError(t, err)
		assert.Equal(t, "sk-test", cfg.Mem0APIKey)
		assert.Equal(t, "https://example.test", cfg.Mem0BaseURL)
		assert.Equal(t, "/tmp/other.db", cfg.DBPath)
		assert.Equal(t, "/tmp/other.log", cfg.GapLogPath)
	})
}

// TestLoadFromSigningKeys verifies the signing-key fields: SigningKeyEnv
// defaults to NOTARY_SIGNING_KEY and TrustedKeysPath defaults to empty, and
// both can be overridden through the environment.
func TestLoadFromSigningKeys(t *testing.T) {
	t.Run("signing key env defaults to NOTARY_SIGNING_KEY", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{})
		require.NoError(t, err)
		assert.Equal(t, "NOTARY_SIGNING_KEY", cfg.SigningKeyEnv)
		assert.Equal(t, config.DefaultSigningKeyEnv, cfg.SigningKeyEnv)
	})

	t.Run("trusted keys path defaults to empty", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{})
		require.NoError(t, err)
		assert.Empty(t, cfg.TrustedKeysPath)
	})

	t.Run("both fields are overridable by env", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{
			"NOTARY_SIGNING_KEY":       "NOTARY_OTHER_KEY_VAR",
			"NOTARY_TRUSTED_KEYS_PATH": "/tmp/trusted.keys",
		})
		require.NoError(t, err)
		assert.Equal(t, "NOTARY_OTHER_KEY_VAR", cfg.SigningKeyEnv)
		assert.Equal(t, "/tmp/trusted.keys", cfg.TrustedKeysPath)
	})

	t.Run("empty env value falls back to the default", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{"NOTARY_SIGNING_KEY": ""})
		require.NoError(t, err)
		assert.Equal(t, config.DefaultSigningKeyEnv, cfg.SigningKeyEnv)
	})
}

// TestLoadFromFailMode verifies Config.FailMode defaults to the only mode
// implemented in v1 -- interceptor.FailOpenLoud -- and that it is settable on
// the struct. There is deliberately no environment variable for it, so an
// environment that names an unknown key leaves the default intact.
func TestLoadFromFailMode(t *testing.T) {
	t.Run("defaults to FailOpenLoud", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{})
		require.NoError(t, err)
		assert.Equal(t, interceptor.FailOpenLoud, cfg.FailMode)
		require.NoError(t, cfg.FailMode.Validate(), "the default must be a valid mode")
	})

	t.Run("no env var overrides it", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{
			"NOTARY_FAIL_MODE": "fail_closed",
		})
		require.NoError(t, err)
		assert.Equal(t, interceptor.FailOpenLoud, cfg.FailMode,
			"there is no env override for FailMode; the default must stand")
	})
}
