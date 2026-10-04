package config_test

import (
	"encoding/base64"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/interceptor"
	"notary/internal/reconcile"
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

// TestLoadFromSigningKeys verifies the signing-key fields. SigningKeyEnv is the
// NAME of the environment variable that holds key material: it defaults to
// DefaultSigningKeyEnv and is NEVER populated from the value of
// NOTARY_SIGNING_KEY, so the key's material can never end up in the config --
// and so can never be echoed by an error that renders the config. TrustedKeysPath
// defaults to empty and is overridable through its own environment variable.
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

	// The material lives IN NOTARY_SIGNING_KEY; the config carries the variable's
	// NAME. LoadFrom must therefore leave SigningKeyEnv at its default and must
	// never copy the material's value into it. Before the fix this case asserted
	// the opposite (SigningKeyEnv == the value), which is exactly the assertion
	// that encoded the leak.
	t.Run("NOTARY_SIGNING_KEY holding material is not read as a variable name", func(t *testing.T) {
		material := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
		cfg, err := config.LoadFrom(map[string]string{"NOTARY_SIGNING_KEY": material})
		require.NoError(t, err)
		assert.Equal(t, config.DefaultSigningKeyEnv, cfg.SigningKeyEnv,
			"SigningKeyEnv must remain the NAME of the variable, not the key's value")
		assert.NotEqual(t, material, cfg.SigningKeyEnv,
			"the key material must never become the signing-key variable name")
		assert.NotContains(t, cfg.SigningKeyEnv, material,
			"no fragment of the key material may appear in SigningKeyEnv")
	})

	t.Run("trusted keys path is overridable by env", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{
			"NOTARY_TRUSTED_KEYS_PATH": "/tmp/trusted.keys",
		})
		require.NoError(t, err)
		assert.Equal(t, "/tmp/trusted.keys", cfg.TrustedKeysPath)
		assert.Equal(t, config.DefaultSigningKeyEnv, cfg.SigningKeyEnv,
			"no environment variable changes SigningKeyEnv")
	})

	t.Run("empty env value falls back to the default", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{"NOTARY_SIGNING_KEY": ""})
		require.NoError(t, err)
		assert.Equal(t, config.DefaultSigningKeyEnv, cfg.SigningKeyEnv)
	})

	t.Run("SigningKeyEnv remains settable by the caller", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{})
		require.NoError(t, err)
		cfg.SigningKeyEnv = "NOTARY_OTHER_KEY_VAR"
		assert.Equal(t, "NOTARY_OTHER_KEY_VAR", cfg.SigningKeyEnv,
			"a caller may still override SigningKeyEnv programmatically")
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

// TestEnvSensitivityRulesIsDocumentedName pins the environment variable that
// names the declarative sensitivity-rules file. It is the ONLY way an operator
// points a write path at the rules: there is deliberately no CLI flag, because
// rules mark content at write time, so they concern the paths whose records
// CARRY content -- an application constructing an interceptor, and `notary
// proxy` -- not the reading commands. The name is a documented interface and
// not an implementation detail.
func TestEnvSensitivityRulesIsDocumentedName(t *testing.T) {
	assert.Equal(t, "NOTARY_SENSITIVITY_RULES", config.EnvSensitivityRules)
}

// TestEnvPhraseNamesAreDocumentedNames pins the three environment variables that
// configure the optional phrasing provider. They are env-only by design (D10):
// there is deliberately no config-file path for the key, so the names are a
// documented interface and not an implementation detail.
func TestEnvPhraseNamesAreDocumentedNames(t *testing.T) {
	assert.Equal(t, "NOTARY_PHRASE_API_KEY", config.EnvPhraseAPIKey)
	assert.Equal(t, "NOTARY_PHRASE_MODEL", config.EnvPhraseModel)
	assert.Equal(t, "NOTARY_PHRASE_BASE_URL", config.EnvPhraseBaseURL)
}

// TestConfigDefaultsToReconcileCommand verifies Config.ReconcileMode defaults to
// the only mode implemented in v1 -- reconcile.ReconcileCommand -- and that the
// default is one this build can actually run. There is deliberately no
// environment variable for it, so an environment that names an unknown key
// leaves the default intact. It is the direct peer of TestLoadFromFailMode.
func TestConfigDefaultsToReconcileCommand(t *testing.T) {
	t.Run("defaults to ReconcileCommand", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{})
		require.NoError(t, err)
		assert.Equal(t, reconcile.ReconcileCommand, cfg.ReconcileMode)
		require.NoError(t, cfg.ReconcileMode.Validate(),
			"the default must be a mode this build can run")
	})

	t.Run("no env var overrides it", func(t *testing.T) {
		cfg, err := config.LoadFrom(map[string]string{
			"NOTARY_RECONCILE_MODE": "in_process",
		})
		require.NoError(t, err)
		assert.Equal(t, reconcile.ReconcileCommand, cfg.ReconcileMode,
			"there is no env override for ReconcileMode; the default must stand")
	})
}
