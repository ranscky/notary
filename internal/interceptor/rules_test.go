package interceptor_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"notary/internal/interceptor"
	"notary/internal/record"
)

// TestScopeOnlyRuleMatchesAnyMetadata verifies a rule with a scope clause and no
// metadata clause matches any metadata in the matching scope, and nothing
// outside it.
func TestScopeOnlyRuleMatchesAnyMetadata(t *testing.T) {
	rs := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "patient", Scope: record.Scope{UserID: "patient-42"}},
	})

	t.Run("any metadata in the matching scope", func(t *testing.T) {
		name, matched := rs.Match(record.Scope{UserID: "patient-42"}, nil)
		assert.True(t, matched)
		assert.Equal(t, "patient", name)

		name, matched = rs.Match(record.Scope{UserID: "patient-42"}, map[string]any{"anything": "at all"})
		assert.True(t, matched)
		assert.Equal(t, "patient", name)
	})

	t.Run("no metadata outside the scope", func(t *testing.T) {
		_, matched := rs.Match(record.Scope{UserID: "someone-else"}, map[string]any{"anything": "at all"})
		assert.False(t, matched)
	})
}

// TestMetadataOnlyRuleMatchesAnyScope verifies a rule with a metadata clause and
// no scope clause matches that key/value in any scope, and nothing else.
func TestMetadataOnlyRuleMatchesAnyScope(t *testing.T) {
	rs := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "health", MetadataKey: "category", MetadataValue: "health"},
	})

	cases := []struct {
		name     string
		scope    record.Scope
		metadata map[string]any
		want     bool
	}{
		{"empty scope, matching metadata", record.Scope{}, map[string]any{"category": "health"}, true},
		{"populated scope, matching metadata", record.Scope{UserID: "u", AgentID: "a", AppID: "p", RunID: "r"}, map[string]any{"category": "health"}, true},
		{"matching key, different value", record.Scope{UserID: "u"}, map[string]any{"category": "finance"}, false},
		{"different key", record.Scope{UserID: "u"}, map[string]any{"tier": "health"}, false},
		{"key absent", record.Scope{UserID: "u"}, map[string]any{}, false},
		{"nil metadata", record.Scope{UserID: "u"}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, matched := rs.Match(tc.scope, tc.metadata)
			assert.Equal(t, tc.want, matched)
			if tc.want {
				assert.Equal(t, "health", name)
			}
		})
	}
}

// TestRuleWithBothClausesRequiresBoth verifies a rule that specifies a scope
// clause AND a metadata clause matches only when both match.
func TestRuleWithBothClausesRequiresBoth(t *testing.T) {
	rs := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "both", Scope: record.Scope{UserID: "patient-42"}, MetadataKey: "category", MetadataValue: "health"},
	})

	cases := []struct {
		name     string
		scope    record.Scope
		metadata map[string]any
		want     bool
	}{
		{"both match", record.Scope{UserID: "patient-42"}, map[string]any{"category": "health"}, true},
		{"scope matches, metadata differs", record.Scope{UserID: "patient-42"}, map[string]any{"category": "finance"}, false},
		{"scope differs, metadata matches", record.Scope{UserID: "other"}, map[string]any{"category": "health"}, false},
		{"neither matches", record.Scope{UserID: "other"}, map[string]any{"category": "finance"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, matched := rs.Match(tc.scope, tc.metadata)
			assert.Equal(t, tc.want, matched)
			if tc.want {
				assert.Equal(t, "both", name)
			}
		})
	}
}

// TestScopeMatchesOnEverySpecifiedField verifies a scope clause matches only when
// every field it sets equals the record's, so a rule can be as narrow as two
// fields together.
func TestScopeMatchesOnEverySpecifiedField(t *testing.T) {
	rs := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "narrow", Scope: record.Scope{UserID: "u1", RunID: "r1"}},
	})

	_, matched := rs.Match(record.Scope{UserID: "u1", RunID: "r1", AppID: "anything"}, nil)
	assert.True(t, matched, "an unset rule field is any value")

	_, matched = rs.Match(record.Scope{UserID: "u1", RunID: "r2"}, nil)
	assert.False(t, matched, "run_id is set on the rule and must match")
}

// TestFirstMatchWinsInSliceOrder verifies that when more than one rule matches,
// the first in slice order is reported -- deterministically, never a function of
// map iteration.
func TestFirstMatchWinsInSliceOrder(t *testing.T) {
	metadata := map[string]any{"category": "health"}
	scope := record.Scope{UserID: "u1"}

	first := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "first", MetadataKey: "category", MetadataValue: "health"},
		{Name: "second", MetadataKey: "category", MetadataValue: "health"},
	})
	name, matched := first.Match(scope, metadata)
	assert.True(t, matched)
	assert.Equal(t, "first", name)

	reversed := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "second", MetadataKey: "category", MetadataValue: "health"},
		{Name: "first", MetadataKey: "category", MetadataValue: "health"},
	})
	name, matched = reversed.Match(scope, metadata)
	assert.True(t, matched)
	assert.Equal(t, "second", name, "reversing the slice reverses which rule wins")

	// A first, non-matching rule must not shadow a later matching one, even when
	// the later one is the broader scope-only rule.
	mixed := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "no-match", Scope: record.Scope{UserID: "someone-else"}},
		{Name: "winner", MetadataKey: "category", MetadataValue: "health"},
	})
	name, matched = mixed.Match(scope, metadata)
	assert.True(t, matched)
	assert.Equal(t, "winner", name)
}

// TestNoRuleMatchesReturnsFalse verifies the no-match outcome is ("", false) for
// a rule set whose rules do not match, an empty rule set, and a nil rule set.
func TestNoRuleMatchesReturnsFalse(t *testing.T) {
	nonMatching := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "scope", Scope: record.Scope{UserID: "nobody"}},
		{Name: "metadata", MetadataKey: "category", MetadataValue: "absent"},
	})

	for _, tc := range []struct {
		name string
		rs   *interceptor.RuleSet
	}{
		{"rules do not match", nonMatching},
		{"empty rule set", interceptor.NewRuleSet(nil)},
		{"nil rule set", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, matched := tc.rs.Match(record.Scope{UserID: "u1"}, map[string]any{"category": "health"})
			assert.False(t, matched)
			assert.Empty(t, name)
		})
	}
}

// TestNonStringMetadataValueDoesNotMatch is the guard that the matcher honours
// Mem0's metadata type: values are any, and a value that is not a string must
// never match a string rule -- not a number that looks like one, not a bool, not
// a list or a nested map, and not nil.
func TestNonStringMetadataValueDoesNotMatch(t *testing.T) {
	value := "42"
	rs := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "num", MetadataKey: "count", MetadataValue: value},
	})

	t.Run("a string that equals the rule matches", func(t *testing.T) {
		name, matched := rs.Match(record.Scope{}, map[string]any{"count": "42"})
		assert.True(t, matched)
		assert.Equal(t, "num", name)
	})

	for _, tc := range []struct {
		name string
		val  any
	}{
		{"int", 42},
		{"float", 42.0},
		{"bool", true},
		{"[]string", []string{"42"}},
		{"map", map[string]any{"42": "42"}},
		{"nil", nil},
	} {
		t.Run("non-string "+tc.name, func(t *testing.T) {
			_, matched := rs.Match(record.Scope{}, map[string]any{"count": tc.val})
			assert.False(t, matched, "a non-string metadata value must not match a string rule")
		})
	}
}

// TestRuleWithNoClausesMatchesEverything pins the literal semantics: a rule that
// specifies neither clause specifies nothing, so every clause it specifies
// matches (vacuously) and it matches every record. config.LoadSensitivityRules
// rejects such a rule at load time, so this is never reachable from a rules
// file; the matcher is a pure function of the rule it is given and does not
// re-validate.
func TestRuleWithNoClausesMatchesEverything(t *testing.T) {
	rs := interceptor.NewRuleSet([]interceptor.Rule{{Name: "everything"}})

	name, matched := rs.Match(record.Scope{UserID: "anyone"}, map[string]any{"any": "thing"})
	assert.True(t, matched)
	assert.Equal(t, "everything", name)
}
