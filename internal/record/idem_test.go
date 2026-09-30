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
	first, err := record.DeriveIdemKey(record.EventAddRequested, record.ReasonAddAcknowledged, baseScope(), "event-1", "", digest(0xab))
	require.NoError(t, err)

	time.Sleep(2 * time.Millisecond)

	second, err := record.DeriveIdemKey(record.EventAddRequested, record.ReasonAddAcknowledged, baseScope(), "event-1", "", digest(0xab))
	require.NoError(t, err)

	assert.Equal(t, first, second, "key changed across wall-clock time")

	// Pure function: same arguments, repeatedly, same key.
	for i := 0; i < 100; i++ {
		got, err := record.DeriveIdemKey(record.EventAddRequested, record.ReasonAddAcknowledged, baseScope(), "event-1", "", digest(0xab))
		require.NoError(t, err)
		assert.Equal(t, first, got, "key not a pure function of its arguments (iteration %d)", i)
	}
}

// TestDeriveIdemKeyIsLowercaseHex verifies the key is rendered as lowercase
// hex of the 32-byte digest, matching the project's hash rendering elsewhere.
func TestDeriveIdemKeyIsLowercaseHex(t *testing.T) {
	k, err := record.DeriveIdemKey(record.EventAddRequested, record.ReasonAddAcknowledged, baseScope(), "event-1", "", digest(0x01))
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
	// Each case shifts one character across an ADJACENT boundary of the hash
	// input, so the two argument sets concatenate to the same raw bytes while
	// describing DIFFERENT logical events. Every variable-length boundary is
	// covered -- not just UserID|AgentID -- because a future change that
	// un-prefixed only AppID, RunID, or the identifier would otherwise slip
	// past this test while still silently sharing a key.
	//
	// kind and reasonKind are written into the hash as opaque strings
	// (DeriveIdemKey does not validate them), so the kind|reasonKind case
	// below uses non-canonical values.
	dg := digest(0x07)

	type args struct {
		kind record.EventType
		rk   record.ReasonKind
		sc   record.Scope
		ev   string
	}
	cases := []struct {
		name                  string
		left, right           args
		naiveLeft, naiveRight string
	}{
		{
			name:       "kind|reasonKind",
			left:       args{record.EventType("ab"), record.ReasonKind("c"), record.Scope{UserID: "u", AgentID: "a", AppID: "p", RunID: "r"}, "e"},
			right:      args{record.EventType("a"), record.ReasonKind("bc"), record.Scope{UserID: "u", AgentID: "a", AppID: "p", RunID: "r"}, "e"},
			naiveLeft:  "ab" + "c",
			naiveRight: "a" + "bc",
		},
		{
			name:       "reasonKind|UserID",
			left:       args{record.EventAddRequested, record.ReasonKind("ab"), record.Scope{UserID: "c", AgentID: "a", AppID: "p", RunID: "r"}, "e"},
			right:      args{record.EventAddRequested, record.ReasonKind("a"), record.Scope{UserID: "bc", AgentID: "a", AppID: "p", RunID: "r"}, "e"},
			naiveLeft:  "ab" + "c",
			naiveRight: "a" + "bc",
		},
		{
			name:       "UserID|AgentID",
			left:       args{record.EventAddRequested, record.ReasonAddAcknowledged, record.Scope{UserID: "ab", AgentID: "c", AppID: "p", RunID: "r"}, "e"},
			right:      args{record.EventAddRequested, record.ReasonAddAcknowledged, record.Scope{UserID: "a", AgentID: "bc", AppID: "p", RunID: "r"}, "e"},
			naiveLeft:  "ab" + "c",
			naiveRight: "a" + "bc",
		},
		{
			name:       "AgentID|AppID",
			left:       args{record.EventAddRequested, record.ReasonAddAcknowledged, record.Scope{UserID: "u", AgentID: "ab", AppID: "c", RunID: "r"}, "e"},
			right:      args{record.EventAddRequested, record.ReasonAddAcknowledged, record.Scope{UserID: "u", AgentID: "a", AppID: "bc", RunID: "r"}, "e"},
			naiveLeft:  "ab" + "c",
			naiveRight: "a" + "bc",
		},
		{
			name:       "AppID|RunID",
			left:       args{record.EventAddRequested, record.ReasonAddAcknowledged, record.Scope{UserID: "u", AgentID: "a", AppID: "ab", RunID: "c"}, "e"},
			right:      args{record.EventAddRequested, record.ReasonAddAcknowledged, record.Scope{UserID: "u", AgentID: "a", AppID: "a", RunID: "bc"}, "e"},
			naiveLeft:  "ab" + "c",
			naiveRight: "a" + "bc",
		},
		{
			name:       "RunID|identifier",
			left:       args{record.EventAddRequested, record.ReasonAddAcknowledged, record.Scope{UserID: "u", AgentID: "a", AppID: "p", RunID: "ab"}, "c"},
			right:      args{record.EventAddRequested, record.ReasonAddAcknowledged, record.Scope{UserID: "u", AgentID: "a", AppID: "p", RunID: "a"}, "bc"},
			naiveLeft:  "ab" + "c",
			naiveRight: "a" + "bc",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.naiveLeft, tc.naiveRight,
				"test setup: the two scopes must concatenate to the same raw string")

			kl, err := record.DeriveIdemKey(tc.left.kind, tc.left.rk, tc.left.sc, tc.left.ev, "", dg)
			require.NoError(t, err)
			kr, err := record.DeriveIdemKey(tc.right.kind, tc.right.rk, tc.right.sc, tc.right.ev, "", dg)
			require.NoError(t, err)

			assert.NotEqual(t, kl, kr,
				"length-prefix collision at boundary %s: distinct events share a key", tc.name)
		})
	}
}

// TestDeriveIdemKeySensitivity is hazard 3: the key must vary when any single
// input changes. A key that ignores an argument would silently collide
// unrelated events.
func TestDeriveIdemKeySensitivity(t *testing.T) {
	baseKind := record.EventAddRequested
	baseReason := record.ReasonAddAcknowledged
	baseSc := baseScope()
	baseEvent := "event-1"
	baseCorr := ""
	baseDigest := digest(0x10)

	base, err := record.DeriveIdemKey(baseKind, baseReason, baseSc, baseEvent, baseCorr, baseDigest)
	require.NoError(t, err)

	cases := []struct {
		name string
		kind record.EventType
		rk   record.ReasonKind
		sc   record.Scope
		ev   string
		co   string
		dg   record.Hash
	}{
		{"kind", record.EventSearchPerformed, baseReason, baseSc, baseEvent, baseCorr, baseDigest},
		// reasonKind is the same event at a different tier: keeping it out of
		// the key would make the second claim silently deduplicate the first
		// (Ruling 1). The kind below is Reconstructed where baseReason is
		// Observed.
		{"reasonKind", baseKind, record.ReasonKeptByContentMatch, baseSc, baseEvent, baseCorr, baseDigest},
		{"scope.UserID", baseKind, baseReason, record.Scope{"other", baseSc.AgentID, baseSc.AppID, baseSc.RunID}, baseEvent, baseCorr, baseDigest},
		{"scope.AgentID", baseKind, baseReason, record.Scope{baseSc.UserID, "other", baseSc.AppID, baseSc.RunID}, baseEvent, baseCorr, baseDigest},
		{"scope.AppID", baseKind, baseReason, record.Scope{baseSc.UserID, baseSc.AgentID, "other", baseSc.RunID}, baseEvent, baseCorr, baseDigest},
		{"scope.RunID", baseKind, baseReason, record.Scope{baseSc.UserID, baseSc.AgentID, baseSc.AppID, "other"}, baseEvent, baseCorr, baseDigest},
		{"eventID", baseKind, baseReason, baseSc, "event-2", baseCorr, baseDigest},
		{"correlationID", baseKind, baseReason, baseSc, "", "corr-1", baseDigest},
		{"requestDigest", baseKind, baseReason, baseSc, baseEvent, baseCorr, digest(0x11)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := record.DeriveIdemKey(tc.kind, tc.rk, tc.sc, tc.ev, tc.co, tc.dg)
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
	rk := record.ReasonAddAcknowledged
	sc := baseScope()
	dg := digest(0x22)

	both, err := record.DeriveIdemKey(kind, rk, sc, "event-1", "corr-1", dg)
	require.NoError(t, err)
	eventOnly, err := record.DeriveIdemKey(kind, rk, sc, "event-1", "", dg)
	require.NoError(t, err)
	corrOnly, err := record.DeriveIdemKey(kind, rk, sc, "", "corr-1", dg)
	require.NoError(t, err)

	assert.Equal(t, eventOnly, both, "eventID must take precedence over correlationID")
	assert.NotEqual(t, corrOnly, both, "correlationID form must not have been used")
}

// TestDeriveIdemKeyUnavailable is hazard 5: with both identifiers empty the
// derivation is unavailable, returning ErrIdemKeyUnavailable and a zero key.
func TestDeriveIdemKeyUnavailable(t *testing.T) {
	k, err := record.DeriveIdemKey(record.EventSearchPerformed, record.ReasonSearchPerformed, baseScope(), "", "", digest(0x33))
	require.ErrorIs(t, err, record.ErrIdemKeyUnavailable)
	assert.Equal(t, record.IdemKey(""), k, "unavailable derivation must return an empty key")
}

// TestDeriveClaimIdemKeyIsStableAndSubjectSpecific pins requirement 1 of the
// reconciler's claim key: identical inputs produce an identical key across
// calls, a change to ANY input produces a different key, and a derived-claim
// key never equals a request-path key from DeriveIdemKey.
func TestDeriveClaimIdemKeyIsStableAndSubjectSpecific(t *testing.T) {
	const (
		kind = record.EventAddResolved
		rk   = record.ReasonStoredByMem0
	)

	base, err := record.DeriveClaimIdemKey(kind, rk, "mem-1", "content-a", "1")
	require.NoError(t, err)

	// Stable across wall-clock time: a second call after a real sleep is
	// identical, which a clock- or randomness-derived key would fail.
	time.Sleep(2 * time.Millisecond)
	again, err := record.DeriveClaimIdemKey(kind, rk, "mem-1", "content-a", "1")
	require.NoError(t, err)
	assert.Equal(t, base, again, "claim key changed across wall-clock time")

	// Pure function: same arguments, repeatedly, same key.
	for i := 0; i < 100; i++ {
		got, err := record.DeriveClaimIdemKey(kind, rk, "mem-1", "content-a", "1")
		require.NoError(t, err)
		assert.Equal(t, base, got, "claim key is not a pure function of its arguments (iteration %d)", i)
	}

	// Lowercase hex of the 32-byte digest, matching DeriveIdemKey.
	s := string(base)
	assert.Len(t, s, 64, "a SHA-256 hex digest is 64 characters")
	assert.Equal(t, strings.ToLower(s), s, "claim key must be lowercase hex")
	_, decErr := hex.DecodeString(s)
	require.NoError(t, decErr, "claim key must be valid hex")

	// Subject-specific: a change to ANY single input yields a different key.
	variants := []struct {
		name        string
		kind        record.EventType
		rk          record.ReasonKind
		mem, ch, rv string
	}{
		{"kind", record.EventMemoryKept, rk, "mem-1", "content-a", "1"},
		{"reasonKind", kind, record.ReasonKeptByContentMatch, "mem-1", "content-a", "1"},
		{"memoryID", kind, rk, "mem-2", "content-a", "1"},
		{"contentHash", kind, rk, "mem-1", "content-b", "1"},
		{"ruleVersion", kind, rk, "mem-1", "content-a", "2"},
	}
	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			got, err := record.DeriveClaimIdemKey(v.kind, v.rk, v.mem, v.ch, v.rv)
			require.NoError(t, err)
			assert.NotEqual(t, base, got, "a change to %s must derive a different claim key", v.name)
		})
	}

	// Cross-domain: a derived-claim key never equals a request-path key, even
	// when the identifying bytes are the same. The distinct domain constant is
	// what makes the separation structural.
	request, err := record.DeriveIdemKey(kind, rk, record.Scope{UserID: "mem-1"}, "content-a", "", digest(0x01))
	require.NoError(t, err)
	assert.NotEqual(t, base, request, "a claim key must never equal a request-path key")
}

// TestDeriveClaimIdemKeyUnavailableWithoutASubject pins that a claim with no
// subject (neither a memory id nor a content hash) cannot be keyed, mirroring
// DeriveIdemKey's "no identifier" rule.
func TestDeriveClaimIdemKeyUnavailableWithoutASubject(t *testing.T) {
	k, err := record.DeriveClaimIdemKey(record.EventAddResolved, record.ReasonStoredByMem0, "", "", "1")
	require.ErrorIs(t, err, record.ErrIdemKeyUnavailable)
	assert.Equal(t, record.IdemKey(""), k, "unavailable derivation must return an empty key")
}
