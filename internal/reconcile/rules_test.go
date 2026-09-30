package reconcile_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/reconcile"
	"notary/internal/record"
)

// TestRulesAreWellFormed checks the registry's own invariants. The load-bearing
// one is the tier check: a rule may only justify a claim Notary inferred or
// produced itself, never an Observed claim, which carries direct evidence and
// has no rule at all. A rule whose Kind.AllowedTier() were Observed would be a
// category error — an inference pretending to be a witnessed fact.
func TestRulesAreWellFormed(t *testing.T) {
	rules := reconcile.Rules()
	require.NotEmpty(t, rules, "the registry must not be empty")

	seen := make(map[string]bool, len(rules))
	for _, r := range rules {
		t.Run(r.Name, func(t *testing.T) {
			assert.NotEmpty(t, r.Name, "every rule needs a name; it is recorded in signed records")
			assert.NotEmpty(t, r.Version, "every rule needs a version; it is recorded in signed records")
			assert.NotEmpty(t, r.Summary, "every rule needs a summary for a reader years later")

			assert.False(t, seen[r.Name], "rule name %q is not unique", r.Name)
			seen[r.Name] = true

			tier, ok := r.Kind.AllowedTier()
			require.True(t, ok, "rule %q carries unknown reason kind %q", r.Name, r.Kind)
			assert.NotEqual(t, record.Observed, tier,
				"rule %q claims an Observed tier; an observed claim has direct evidence and no rule", r.Name)
			assert.True(t, tier == record.Reconstructed || tier == record.Internal,
				"rule %q must justify a Reconstructed or Internal claim, got %s", r.Name, tier)
		})
	}
}

// TestRegistryCoversEveryDerivedKind pins the registry against the reason
// vocabulary: each of the four derived kinds must be reachable through exactly
// one rule. Exactly one — a second rule for the same kind would mean two
// different inferences share a name, and no rule for a kind would mean a
// producer could write a claim the registry cannot explain.
func TestRegistryCoversEveryDerivedKind(t *testing.T) {
	derived := []record.ReasonKind{
		record.ReasonKeptByContentMatch,
		record.ReasonAbsentFromSearch,
		record.ReasonNoFactsExtracted,
		record.ReasonRemovedByMem0,
	}

	counts := make(map[record.ReasonKind]int)
	for _, r := range reconcile.Rules() {
		counts[r.Kind]++
	}

	for _, kind := range derived {
		assert.Equal(t, 1, counts[kind], "reason kind %q must be reachable through exactly one rule", kind)
	}

	// And no rule may exist for a kind outside the derived set: the registry
	// covers observations too, which have no rule.
	for kind := range counts {
		assert.Contains(t, derived, kind, "rule registered for non-derived kind %q", kind)
	}
}

// TestLookupRule exercises the lookup path: a registered name resolves to its
// rule, and an unknown name reports false rather than the zero rule.
func TestLookupRule(t *testing.T) {
	got, ok := reconcile.LookupRule(reconcile.RuleAbsentFromSearch)
	require.True(t, ok)
	assert.Equal(t, reconcile.RuleAbsentFromSearch, got.Name)
	assert.Equal(t, record.ReasonAbsentFromSearch, got.Kind)

	_, ok = reconcile.LookupRule("not_a_rule")
	assert.False(t, ok)
}

// TestRulesReturnsCopy proves callers cannot mutate the registry through the
// slice Rules hands back: writing to a returned element must not change what a
// later call reports.
func TestRulesReturnsCopy(t *testing.T) {
	first := reconcile.Rules()
	require.NotEmpty(t, first)

	original := first[0].Name
	first[0].Name = "mutated"
	first[0].Version = "mutated"
	first[0].Summary = "mutated"

	second := reconcile.Rules()
	assert.Equal(t, original, second[0].Name, "Rules must return a copy, not its backing store")
	got, ok := reconcile.LookupRule(original)
	require.True(t, ok)
	assert.Equal(t, original, got.Name)
}
