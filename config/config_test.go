package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
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
