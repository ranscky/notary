package export_test

import (
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/export"
	"notary/internal/record"
)

// sampleRecord builds a fully-populated, valid record so a test can assert the
// rendered line carries every structured field. Its tier is Observed.
func sampleRecord(t *testing.T) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"x":1}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonStoredByMem0, ev)
	require.NoError(t, err)

	var contentHash record.Hash
	for i := range contentHash {
		contentHash[i] = 0xab
	}
	var prevHash record.Hash
	prevHash[0] = 0x01

	return record.Record{
		ID:             record.RecordID("rec-1"),
		Seq:            7,
		At:             time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		RecordedAt:     time.Date(2024, 1, 2, 3, 4, 6, 0, time.UTC),
		Event:          record.EventMemoryKept,
		Reason:         reason,
		Subject:        record.Subject{MemoryID: "mem-1", Scope: record.Scope{UserID: "u1", AgentID: "a1"}, ContentHash: contentHash},
		Content:        &record.Content{Text: "the text", Sensitive: false},
		IdempotencyKey: record.IdemKey("idem-1"),
		PrevHash:       prevHash,
		Hash:           contentHash,
		Signature:      []byte{0xde, 0xad, 0xbe, 0xef},
		SignerKeyID:    "key-1",
	}
}

// renderJSON renders rec and returns the decoded JSON object, so a test can
// assert both the presence and the absence of fields.
func renderJSON(t *testing.T, rec record.Record, includeSensitive bool) map[string]any {
	t.Helper()
	line, err := export.Render(rec, includeSensitive)
	require.NoError(t, err)
	data, err := json.Marshal(line)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(data, &got))
	return got
}

func TestLineCarriesEveryStructuredField(t *testing.T) {
	rec := sampleRecord(t)
	got := renderJSON(t, rec, false)

	assert.Equal(t, float64(7), got["seq"])
	assert.Equal(t, "rec-1", got["id"])
	assert.Equal(t, "2024-01-02T03:04:05Z", got["at"])
	assert.Equal(t, "2024-01-02T03:04:06Z", got["recorded_at"])
	assert.Equal(t, "memory_kept", got["event"])
	assert.Equal(t, "observed", got["tier"])
	assert.Equal(t, "stored_by_mem0", got["reason_kind"])
	assert.Equal(t, "mem-1", got["memory_id"])

	scope, ok := got["scope"].(map[string]any)
	require.True(t, ok, "scope must be a nested object")
	assert.Equal(t, "u1", scope["user_id"])
	assert.Equal(t, "a1", scope["agent_id"])

	assert.Equal(t, hex.EncodeToString(rec.Subject.ContentHash[:]), got["content_hash"])
	assert.Equal(t, "the text", got["content"])
	assert.Equal(t, hex.EncodeToString(rec.PrevHash[:]), got["prev_hash"])
	assert.Equal(t, hex.EncodeToString(rec.Hash[:]), got["hash"])
	assert.Equal(t, hex.EncodeToString(rec.Signature), got["signature"])
	assert.Equal(t, "key-1", got["signer_key_id"])

	_, hasRedacted := got["redacted"]
	assert.False(t, hasRedacted, "redacted must be absent when content was shown")
	_, hasParaphrase := got["paraphrase"]
	assert.False(t, hasParaphrase, "no paraphrase object exists in this task")
}

func TestLineOrderIsStable(t *testing.T) {
	rec := sampleRecord(t)

	first, err := export.Render(rec, false)
	require.NoError(t, err)
	second, err := export.Render(rec, false)
	require.NoError(t, err)

	a, err := json.Marshal(first)
	require.NoError(t, err)
	b, err := json.Marshal(second)
	require.NoError(t, err)

	assert.Equal(t, string(a), string(b))
}

// TestRenderRejectsARecordWithoutATier covers Render's error path: a record
// whose reason carries no valid tier cannot be rendered at all.
func TestRenderRejectsARecordWithoutATier(t *testing.T) {
	rec := sampleRecord(t)
	rec.Reason = record.Reason{}

	_, err := export.Render(rec, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tier")
}
