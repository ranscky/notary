package ledger_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/ledger"
	"notary/internal/record"
)

// ledgerContentCanary is the distinctive marker the leak test plants in a
// record's Content.Text. It is deliberately high-entropy so a partial match
// cannot hide it.
const ledgerContentCanary = "CANARY-7f4b1e9a-5c3d-4e21-9b77-leak-me-if-you-can"

// canaryRecord returns a valid record whose Content carries the canary text and
// is flagged sensitive -- the exact payload the leak test must never see in an
// error string.
func canaryRecord(t *testing.T, id record.RecordID) record.Record {
	t.Helper()
	rec := validRecord(t, id)
	rec.Content = &record.Content{Text: ledgerContentCanary, Sensitive: true}
	return rec
}

// TestAppendErrorsNeverEmbedRecordContent is the test behind a hard project
// invariant that was previously only a comment.
//
// INVARIANT: no error reachable from ledger.Append embeds a record's
// Content.Text. The interceptor's gapMarker
// (internal/interceptor/interceptor.go) interpolates that error into a
// single-line, loud log marker, trusting that every cause names only IDs,
// events, kinds, tiers, SQL parameters, or constraint names -- never content.
// .clinerules forbids logging raw sensitive memory content, so if a store or
// ledger error ever began embedding content, that marker would leak it with no
// other test to notice. This test forces every Append failure path reachable
// here and asserts the canary appears in NONE of the returned error strings.
//
// The duplicate-key path is included deliberately: it is a no-op that returns a
// nil error, so it is asserted explicitly rather than left to be skipped.
func TestAppendErrorsNeverEmbedRecordContent(t *testing.T) {
	// Each case name says which Append path it forces, so a future failure
	// names the leaking path directly.
	assertNoCanary := func(t *testing.T, err error) {
		t.Helper()
		require.Error(t, err)
		assert.NotContains(t, err.Error(), ledgerContentCanary,
			"an Append failure error must never embed record content: %v", err)
	}

	t.Run("invalid-tier", func(t *testing.T) {
		l, _, _, _ := newLedger(t)
		rec := canaryRecord(t, "rec-canary-tier")
		rec.Reason = record.Reason{} // zero value: invalid tier

		_, err := l.Append(rec)
		require.ErrorIs(t, err, ledger.ErrInvalidTier)
		assertNoCanary(t, err)
	})

	t.Run("invalid-record", func(t *testing.T) {
		l, _, _, _ := newLedger(t)
		rec := canaryRecord(t, "rec-canary-event")
		rec.Event = record.EventType("not_a_real_event")

		_, err := l.Append(rec)
		require.ErrorIs(t, err, ledger.ErrInvalidRecord)
		assertNoCanary(t, err)
	})

	t.Run("caller-supplied-seq", func(t *testing.T) {
		l, _, _, _ := newLedger(t)
		rec := canaryRecord(t, "rec-canary-seq")
		rec.Seq = 3

		_, err := l.Append(rec)
		require.ErrorIs(t, err, ledger.ErrSeqAssigned)
		assertNoCanary(t, err)
	})

	t.Run("no-signer", func(t *testing.T) {
		st := newStore(t)
		l := ledger.New(st, nil, func() time.Time { return fixedNow })
		rec := canaryRecord(t, "rec-canary-nosigner")

		_, err := l.Append(rec)
		assertNoCanary(t, err)
	})

	t.Run("closed-store", func(t *testing.T) {
		l, _, _, st := newLedger(t)
		require.NoError(t, st.Close())

		rec := canaryRecord(t, "rec-canary-closed")
		_, err := l.Append(rec)
		require.Error(t, err, "appending through a closed store must fail")
		assertNoCanary(t, err)
	})

	t.Run("duplicate-key-noop", func(t *testing.T) {
		l, _, _, st := newLedger(t)

		first := canaryRecord(t, "rec-canary-first")
		first.IdempotencyKey = "canary-key"
		firstID, err := l.Append(first)
		require.NoError(t, err)

		// The retry carries the same key with a DIFFERENT id and its own canary
		// content. This path is a no-op, so it must return a nil error and the
		// EXISTING record's ID -- and it must write nothing.
		retry := canaryRecord(t, "rec-canary-retry")
		retry.IdempotencyKey = "canary-key"
		retryID, err := l.Append(retry)
		require.NoError(t, err, "the duplicate-key path is a no-op, not an error")
		assert.Equal(t, firstID, retryID,
			"the no-op must return the existing record's ID, not the retry's")
		assert.Equal(t, 1, countRecords(t, st), "the no-op must not write a second row")
	})
}
