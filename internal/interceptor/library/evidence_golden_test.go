package library

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/mem0"
)

// TestEvidencePayloadGoldenBytes pins the exact JSON bytes of the three
// Observed evidence payloads this interceptor writes.
//
// These bytes are not incidental. ObservedEvidence.Payload is embedded in
// Reason.Encode's output, and CanonicalBytes writes that output straight into
// the canonical record hash, so the encoded form of each payload is part of
// every add_requested, search_performed and memory_surfaced record ever
// signed. Go's encoding/json emits struct fields in declaration order, which
// is why field ORDER is hashed too, not just field names. Changing a field's
// order, name or tag would make every historical record of that kind fail
// verify.
//
// Every field of every struct is set to a non-zero value so the marshaler
// cannot drop one: a zero-valued field is still emitted for these structs
// (they carry no omitempty except mem0.Filters), but a fully populated case
// leaves no room for a field to go missing unnoticed.
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
}
