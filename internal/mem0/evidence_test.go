package mem0_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/mem0"
)

// TestEvidencePayloadGoldenBytes pins the exact JSON bytes of the three
// Observed evidence payloads. It is the byte-identity guard for the move of
// these types out of internal/interceptor/library: the expected strings here
// are the same ones the characterisation test used against the old, unexported
// declarations, so their being unchanged is the proof the refactor did not
// alter a single hashed byte.
//
// These bytes are not incidental. ObservedEvidence.Payload is embedded in
// Reason.Encode's output, and CanonicalBytes writes that output straight into
// the canonical record hash, so the encoded form of each payload is part of
// every add_requested, search_performed and memory_surfaced record ever
// signed. What actually enters that hash is the payload AFTER
// NewObservedEvidence canonicalises it, decoding the JSON and re-marshalling
// it with sorted keys (internal/record/reason.go). So a field's NAME and JSON
// TAG are hashed; declaration ORDER is not, because canonicalisation sorts the
// keys regardless. This test pins BOTH: it asserts the direct-marshal bytes,
// which additionally pin declaration order as a deliberately stricter (and
// harmless) extra on top of the hashed contract.
//
// Each struct is exercised twice: once with every field set to a non-zero
// value, and once with every field left at its zero value. The all-zero cases
// are what catch a field gaining an ",omitempty" tag -- that would silently
// drop the key for real records where the field is zero (Threshold 0, Rerank
// false, Count 0, Score 0, an empty Query), changing the hashed key set while
// the fully-populated cases still passed. mem0.Filters has ",omitempty" on all
// four of its fields, so an all-zero SearchPerformedPayload legitimately emits
// an empty "filters" object.
func TestEvidencePayloadGoldenBytes(t *testing.T) {
	add, err := json.Marshal(mem0.AddPayload{EventID: "evt-1", Status: "PENDING"})
	require.NoError(t, err)
	assert.Equal(t, `{"event_id":"evt-1","status":"PENDING"}`, string(add))

	search, err := json.Marshal(mem0.SearchPerformedPayload{
		Query:     "what did the user ask",
		Filters:   mem0.Filters{UserID: "u1", AgentID: "a1", AppID: "app1", RunID: "r1"},
		TopK:      10,
		Threshold: 0.5,
		Rerank:    true,
		Count:     3,
	})
	require.NoError(t, err)
	assert.Equal(t,
		`{"query":"what did the user ask","filters":{"user_id":"u1","agent_id":"a1","app_id":"app1","run_id":"r1"},"top_k":10,"threshold":0.5,"rerank":true,"count":3}`,
		string(search))

	surfaced, err := json.Marshal(mem0.MemorySurfacedPayload{Score: 0.87, Rank: 2})
	require.NoError(t, err)
	assert.Equal(t, `{"score":0.87,"rank":2}`, string(surfaced))

	// All-zero cases: no field may silently disappear behind an ",omitempty",
	// which would change the hashed key set for real records carrying a zero
	// value. The empty "filters" object is expected: all four Filters fields
	// carry ",omitempty".
	addZero, err := json.Marshal(mem0.AddPayload{})
	require.NoError(t, err)
	assert.Equal(t, `{"event_id":"","status":""}`, string(addZero))

	searchZero, err := json.Marshal(mem0.SearchPerformedPayload{})
	require.NoError(t, err)
	assert.Equal(t,
		`{"query":"","filters":{},"top_k":0,"threshold":0,"rerank":false,"count":0}`,
		string(searchZero))

	surfacedZero, err := json.Marshal(mem0.MemorySurfacedPayload{})
	require.NoError(t, err)
	assert.Equal(t, `{"score":0,"rank":0}`, string(surfacedZero))
}
