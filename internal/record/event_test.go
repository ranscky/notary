package record_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
)

// validReason builds a well-formed Reason for use in Record fixtures.
func validReason(t *testing.T) record.Reason {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"a":1}`))
	require.NoError(t, err)
	r, err := record.NewObservedReason(record.ReasonReturnedBySearch, ev)
	require.NoError(t, err)
	return r
}

// nonZeroHash returns a Hash that is not the all-zero value.
func nonZeroHash() record.Hash {
	var h record.Hash
	h[0] = 1
	return h
}

// populatedRecord returns a Record that is valid in every field Validate
// checks, so a test can perturb exactly one field and attribute the failure.
func populatedRecord(t *testing.T) record.Record {
	t.Helper()
	return record.Record{
		ID:             record.RecordID("rec-1"),
		Seq:            7,
		Event:          record.EventMemorySurfaced,
		Reason:         validReason(t),
		Subject:        record.Subject{MemoryID: "mem-1", ContentHash: nonZeroHash()},
		Content:        &record.Content{Text: "hello", Sensitive: true},
		IdempotencyKey: record.IdemKey("idem-1"),
		SignerKeyID:    "key-1",
	}
}

func TestRecordValidate(t *testing.T) {
	t.Run("fully populated validates", func(t *testing.T) {
		r := populatedRecord(t)
		require.NoError(t, r.Validate())
	})

	t.Run("nil content validates", func(t *testing.T) {
		r := populatedRecord(t)
		r.Content = nil
		require.NoError(t, r.Validate())
	})

	t.Run("non-nil content with empty text validates", func(t *testing.T) {
		r := populatedRecord(t)
		r.Content = &record.Content{}
		require.NoError(t, r.Validate())
	})

	t.Run("zero reason is rejected", func(t *testing.T) {
		r := populatedRecord(t)
		r.Reason = record.Reason{}
		require.Error(t, r.Validate())
	})

	t.Run("empty ID is rejected", func(t *testing.T) {
		r := populatedRecord(t)
		r.ID = ""
		require.Error(t, r.Validate())
	})

	t.Run("zero subject content hash is rejected", func(t *testing.T) {
		r := populatedRecord(t)
		r.Subject.ContentHash = record.Hash{}
		require.Error(t, r.Validate())
	})

	t.Run("unknown event type is rejected", func(t *testing.T) {
		r := populatedRecord(t)
		r.Event = record.EventType("nonsense")
		require.Error(t, r.Validate())
	})

	t.Run("empty event type is rejected", func(t *testing.T) {
		r := populatedRecord(t)
		r.Event = ""
		require.Error(t, r.Validate())
	})

	// Validate must not reject a record whose Seq, Hash, PrevHash, or
	// Signature are unset: those are assigned by Ledger.Append, and rejecting
	// a zero Seq would make the genesis record unconstructible.
	t.Run("unassigned ledger fields are accepted", func(t *testing.T) {
		r := populatedRecord(t)
		r.Seq = 0
		r.Hash = record.Hash{}
		r.PrevHash = record.Hash{}
		r.Signature = nil
		require.NoError(t, r.Validate())
	})
}

func TestEventTypeValid(t *testing.T) {
	known := []record.EventType{
		record.EventAddRequested,
		record.EventAddResolved,
		record.EventSearchPerformed,
		record.EventMemorySurfaced,
		record.EventMemoryKept,
		record.EventMemoryDropped,
		record.EventAuditGap,
	}
	for _, et := range known {
		t.Run(string(et), func(t *testing.T) {
			assert.True(t, et.Valid(), "known event type %q must be valid", et)
		})
	}

	t.Run("zero value is invalid", func(t *testing.T) {
		assert.False(t, record.EventType("").Valid())
	})

	t.Run("unknown value is invalid", func(t *testing.T) {
		assert.False(t, record.EventType("nonsense").Valid())
	})
}

func TestEventTypeString(t *testing.T) {
	t.Run("known values return their wire name", func(t *testing.T) {
		assert.Equal(t, "add_requested", record.EventAddRequested.String())
		assert.Equal(t, "search_performed", record.EventSearchPerformed.String())
		assert.Equal(t, "audit_gap", record.EventAuditGap.String())
	})

	t.Run("unknown values are returned unchanged", func(t *testing.T) {
		assert.Equal(t, "nonsense", record.EventType("nonsense").String())
		assert.Equal(t, "", record.EventType("").String())
	})
}
