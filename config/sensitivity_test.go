package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/interceptor"
	"notary/internal/record"
)

// writeRulesFile writes content to a fresh file named rules.yaml under a
// per-test temporary directory and returns its path. Writing to a temp file
// rather than an in-memory document is deliberate: LoadSensitivityRules takes a
// path, and the path is part of what its errors must name.
func writeRulesFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// TestLoadSensitivityRules verifies a well-formed two-rule document maps onto
// interceptor.Rule values field for field, including the distinction between a
// rule that carries a scope clause and one that carries only a metadata clause.
func TestLoadSensitivityRules(t *testing.T) {
	path := writeRulesFile(t, `rules:
  - name: health-data
    match:
      scope:
        user_id: patient-42
      metadata:
        category: health
  - name: by-run
    match:
      metadata:
        retention: permanent
`)

	rules, err := config.LoadSensitivityRules(path)
	require.NoError(t, err)
	require.Len(t, rules, 2)

	assert.Equal(t, "health-data", rules[0].Name)
	assert.Equal(t, record.Scope{UserID: "patient-42"}, rules[0].Scope)
	assert.Equal(t, "category", rules[0].MetadataKey)
	assert.Equal(t, "health", rules[0].MetadataValue)

	assert.Equal(t, "by-run", rules[1].Name)
	assert.Equal(t, record.Scope{}, rules[1].Scope,
		"a rule with no scope clause must leave Scope at its zero value (any scope)")
	assert.Equal(t, "retention", rules[1].MetadataKey)
	assert.Equal(t, "permanent", rules[1].MetadataValue)
}

// TestLoadedRulesDriveTheMatcher pairs the loader with the matcher so the two
// halves of the rules feature are exercised together: what LoadSensitivityRules
// produces is what interceptor.NewRuleSet consumes.
func TestLoadedRulesDriveTheMatcher(t *testing.T) {
	path := writeRulesFile(t, `rules:
  - name: health-data
    match:
      scope:
        user_id: patient-42
      metadata:
        category: health
`)

	rules, err := config.LoadSensitivityRules(path)
	require.NoError(t, err)
	rs := interceptor.NewRuleSet(rules)

	name, matched := rs.Match(record.Scope{UserID: "patient-42"}, map[string]any{"category": "health"})
	assert.True(t, matched)
	assert.Equal(t, "health-data", name)

	_, matched = rs.Match(record.Scope{UserID: "someone-else"}, map[string]any{"category": "health"})
	assert.False(t, matched, "the scope clause must still be enforced after loading")
}

// TestRuleWithNoClausesIsRejected verifies a rule specifying neither a scope
// clause nor a metadata clause fails to load, with an error naming the rule. It
// would vacuously match every record and mark an entire ledger sensitive, so the
// failure must be loud at startup rather than discovered in an export.
func TestRuleWithNoClausesIsRejected(t *testing.T) {
	t.Run("empty match mapping", func(t *testing.T) {
		path := writeRulesFile(t, `rules:
  - name: everything
    match: {}
`)
		_, err := config.LoadSensitivityRules(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "everything", "the error must name the offending rule")
	})

	t.Run("match mapping absent", func(t *testing.T) {
		path := writeRulesFile(t, `rules:
  - name: everything
`)
		_, err := config.LoadSensitivityRules(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "everything")
	})

	t.Run("scope present but all fields empty", func(t *testing.T) {
		path := writeRulesFile(t, `rules:
  - name: blank-scope
    match:
      scope:
        user_id: ""
`)
		_, err := config.LoadSensitivityRules(path)
		require.Error(t, err,
			"a scope clause whose every field is empty specifies no clause")
		assert.Contains(t, err.Error(), "blank-scope")
	})
}

// TestRuleWithMultipleMetadataKeysIsRejected documents and enforces the single
// key/value the Rule type can carry: a metadata clause naming more than one key
// cannot be represented, and silently keeping whichever key a map iteration
// happened to yield first would make the rule's effect depend on map order.
func TestRuleWithMultipleMetadataKeysIsRejected(t *testing.T) {
	path := writeRulesFile(t, `rules:
  - name: two-keys
    match:
      metadata:
        category: health
        retention: permanent
`)
	_, err := config.LoadSensitivityRules(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "two-keys")
}

// TestMalformedYAMLNamesTheFile verifies a document that is not valid YAML fails
// with an error that names the file, so an operator with several rules files
// knows which one to fix.
func TestMalformedYAMLNamesTheFile(t *testing.T) {
	path := writeRulesFile(t, "rules:\n  - name: broken\n   match: oops\n\tbad: tab\n")

	_, err := config.LoadSensitivityRules(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Base(path),
		"a malformed document must name the file that holds it")
}

// TestLoadSensitivityRulesMissingFile verifies a path that does not exist is an
// error that names the path, rather than a silently empty rule set.
func TestLoadSensitivityRulesMissingFile(t *testing.T) {
	_, err := config.LoadSensitivityRules(filepath.Join(t.TempDir(), "absent.yaml"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "absent.yaml")
}

// TestLoadSensitivityRulesEmptyPath verifies an empty path is rejected rather
// than read as the current directory.
func TestLoadSensitivityRulesEmptyPath(t *testing.T) {
	_, err := config.LoadSensitivityRules("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), config.EnvSensitivityRules)
}
