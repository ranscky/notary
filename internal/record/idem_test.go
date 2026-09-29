package record_test

import (
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
)

// digest returns a deterministic, distinct Hash for a byte tag so tests can
// vary requestDigest without depending on any other package.
func digest(tag byte) record.Hash {
	var h record.Hash
	for i := range h {
		h[i] = tag
	}
	return h
}

// baseScope is a fully populated Scope reused by most tests.
func baseScope() record.Scope {
	return record.Scope{UserID: "user-1", AgentID: "agent-1", AppID: "app-1", RunID: "run-1"}
}

// TestDeriveIdemKeyStable verifies the key is a pure function of its
// arguments: repeated calls with identical inputs return byte-identical keys,
// and the key does not change across wall-clock time. A second call after an
// actual time.Sleep must be identical, which a clock- or randomness-derived
// key would fail. This is hazard 2.
func TestDeriveIdemKeyStable(t *testing.T) {
	first, err := record.DeriveIdemKey(record.EventAddRequested, baseScope(), "event-1", "", digest(0xab))
	require.NoError(t, err)

	time.Sleep(2 * time.Millisecond)

	second, err := record.DeriveIdemKey(record.EventAddRequested, baseScope(), "event-1", "", digest(0xab))
	require.NoError(t, err)

	assert.Equal(t, first, second, "key changed across wall-clock time")

	// Pure function: same arguments, repeatedly, same key.
	for i := 0; i < 100; i++ {
		got, err := record.DeriveIdemKey(record.EventAddRequested, baseScope(), "event-1", "", digest(0xab))
		require.NoError(t, err)
		assert.Equal(t, first, got, "key not a pure function of its arguments (iteration %d)", i)
	}
}

// TestDeriveIdemKeyIsLowercaseHex verifies the key is rendered as lowercase
// hex of the 32-byte digest, matching the project's hash rendering elsewhere.
func TestDeriveIdemKeyIsLowercaseHex(t *testing.T) {
	k, err := record.DeriveIdemKey(record.EventAddRequested, baseScope(), "event-1", "", digest(0x01))
	require.NoError(t, err)

	s := string(k)
	assert.Len(t, s, 64, "a SHA-256 hex digest is 64 characters")
	assert.Equal(t, strings.ToLower(s), s, "key must be lowercase hex")
	_, decErr := hex.DecodeString(s)
	require.NoError(t, decErr, "key must be valid hex")
}

// TestDeriveIdemKeyLengthPrefixPreventsCollision is hazard 1. Naive
// concatenation would make these two distinct logical events hash identically:
//
//	scope{UserID:"ab", AgentID:"c"} and scope{UserID:"a", AgentID:"bc"}
//
// with every other field equal. They must derive different keys, or one
// event would silently deduplicate the other and a record would never be
// written.
func TestDeriveIdemKeyLengthPrefixPreventsCollision(t *testing.T) {
	left := record.Scope{UserID: "ab", AgentID: "c", AppID: "app", RunID: "run"}
	right := record.Scope{UserID: "a", AgentID: "bc", AppID: "app", RunID: "run"}

	// Sanity: the boundary-shifted concatenations are equal as raw strings.
	require.Equal(t, left.UserID+left.AgentID, right.UserID+right.AgentID,
		"test setup: the two scopes must concatenate to the same raw string")

	kl, err := record.DeriveIdemKey(record.EventAddRequested, left, "event-1", "", digest(0x07))
	require.NoError(t, err)
	kr, err := record.DeriveIdemKey(record.EventAddRequested, right, "event-1", "", digest(0x07))
	require.NoError(t, err)

	assert.NotEqual(t, kl, kr, "length-prefix collision: distinct events share a key")
}

// TestDeriveIdemKeySensitivity is hazard 3: the key must vary when any single
// input changes. A key that ignores an argument would silently collide
// unrelated events.
func TestDeriveIdemKeySensitivity(t *testing.T) {
	baseKind := record.EventAddRequested
	baseSc := baseScope()
	baseEvent := "event-1"
	baseCorr := ""
	baseDigest := digest(0x10)

	base, err := record.DeriveIdemKey(baseKind, baseSc, baseEvent, baseCorr, baseDigest)
	require.NoError(t, err)

	cases := []struct {
		name string
		kind record.EventType
		sc   record.Scope
		ev   string
		co   string
		dg   record.Hash
	}{
		{"kind", record.EventSearchPerformed, baseSc, baseEvent, baseCorr, baseDigest},
		{"scope.UserID", baseKind, record.Scope{"other", baseSc.AgentID, baseSc.AppID, baseSc.RunID}, baseEvent, baseCorr, baseDigest},
		{"scope.AgentID", baseKind, record.Scope{baseSc.UserID, "other", baseSc.AppID, baseSc.RunID}, baseEvent, baseCorr, baseDigest},
		{"scope.AppID", baseKind, record.Scope{baseSc.UserID, baseSc.AgentID, "other", baseSc.RunID}, baseEvent, baseCorr, baseDigest},
		{"scope.RunID", baseKind, record.Scope{baseSc.UserID, baseSc.AgentID, baseSc.AppID, "other"}, baseEvent, baseCorr, baseDigest},
		{"eventID", baseKind, baseSc, "event-2", baseCorr, baseDigest},
		{"correlationID", baseKind, baseSc, "", "corr-1", baseDigest},
		{"requestDigest", baseKind, baseSc, baseEvent, baseCorr, digest(0x11)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := record.DeriveIdemKey(tc.kind, tc.sc, tc.ev, tc.co, tc.dg)
			require.NoError(t, err)
			assert.NotEqual(t, base, got, "key did not vary when %s changed", tc.name)
		})
	}
}

// TestDeriveIdemKeyPrefersEventID is hazard 4: with both identifiers present the
// eventID form must be used, so an implementation that accidentally preferred
// correlationID would fail this test.
func TestDeriveIdemKeyPrefersEventID(t *testing.T) {
	kind := record.EventAddRequested
	sc := baseScope()
	dg := digest(0x22)

	both, err := record.DeriveIdemKey(kind, sc, "event-1", "corr-1", dg)
	require.NoError(t, err)
	eventOnly, err := record.DeriveIdemKey(kind, sc, "event-1", "", dg)
	require.NoError(t, err)
	corrOnly, err := record.DeriveIdemKey(kind, sc, "", "corr-1", dg)
	require.NoError(t, err)

	assert.Equal(t, eventOnly, both, "eventID must take precedence over correlationID")
	assert.NotEqual(t, corrOnly, both, "correlationID form must not have been used")
}

// TestDeriveIdemKeyUnavailable is hazard 5: with both identifiers empty the
// derivation is unavailable, returning ErrIdemKeyUnavailable and a zero key.
func TestDeriveIdemKeyUnavailable(t *testing.T) {
	k, err := record.DeriveIdemKey(record.EventSearchPerformed, baseScope(), "", "", digest(0x33))
	require.ErrorIs(t, err, record.ErrIdemKeyUnavailable)
	assert.Equal(t, record.IdemKey(""), k, "unavailable derivation must return an empty key")
}
