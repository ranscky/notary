// This file is deliberately NOT behind the mem0live build tag.
//
// It holds the one safety check the live test's README paragraph leans on. If
// that check lived only in the tagged file, no gate would ever compile it -- and
// a safety check no gate compiles is a claim, not a guarantee. Here it is
// compiled and exercised by the default suite and by CI, offline, with no key.
package reconcile_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"notary/internal/record"
)

// liveTestScopePrefix is the shape every scope the live test is willing to write
// to must have.
const liveTestScopePrefix = "notary-live-test-"

// testScopeIsSafe reports whether the live test may write to scope. It is pure
// so that it can be tested directly: the asserting wrapper below fails the test
// via t.FailNow, which no test can observe.
//
// GENERATION is what actually makes the live test unable to reach a real user's
// memories -- it mints its scope from a random suffix and hands no other scope to
// Mem0. This predicate is the belt to that braces, and it takes the scope as an
// argument rather than re-checking the value just built, so it still stands if
// that value's origin ever changes.
func testScopeIsSafe(scope record.Scope) bool {
	if !strings.HasPrefix(scope.UserID, liveTestScopePrefix) {
		return false
	}
	// A non-empty suffix, because the test's scopes are always minted as prefix +
	// random hex. Without this, a hand-written scope that merely starts with the
	// prefix -- the bare prefix itself, say -- would be accepted as generated.
	return len(scope.UserID) > len(liveTestScopePrefix)
}

// requireTestScope refuses any scope the live test must not write to.
func requireTestScope(t *testing.T, scope record.Scope) record.Scope {
	t.Helper()
	if !testScopeIsSafe(scope) {
		t.Fatalf("refusing to run against scope %q: the live test may only write to a %q scope",
			scope.UserID, liveTestScopePrefix)
	}
	return scope
}

// TestTestScopeIsSafe pins the guard itself, because the README's safety claim
// rests on it and the live test cannot be run by any gate.
func TestTestScopeIsSafe(t *testing.T) {
	assert.True(t, testScopeIsSafe(record.Scope{UserID: liveTestScopePrefix + "9f1c2d3e4b5a6c7d"}),
		"a generated scope must be accepted")

	for _, bad := range []struct {
		name  string
		scope record.Scope
	}{
		{"empty", record.Scope{}},
		{"a plain user id", record.Scope{UserID: "u1"}},
		{"the bare prefix", record.Scope{UserID: liveTestScopePrefix}},
		{"the prefix not at the start", record.Scope{UserID: "real-" + liveTestScopePrefix + "x"}},
		{"the prefix misspelled", record.Scope{UserID: "notary-live-tests-x"}},
		{"the prefix upper case", record.Scope{UserID: strings.ToUpper(liveTestScopePrefix) + "x"}},
		{"an agent-scoped lookalike", record.Scope{AgentID: liveTestScopePrefix + "x"}},
	} {
		t.Run(bad.name, func(t *testing.T) {
			assert.False(t, testScopeIsSafe(bad.scope),
				"%q must be refused: the live test may only write to a generated %q scope",
				bad.scope.UserID, liveTestScopePrefix)
		})
	}
}
