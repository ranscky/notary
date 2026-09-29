package ledger_test

import (
	"crypto/ed25519"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
	"notary/internal/sign"
)

// verifierFor builds a verifier trusting the signer's key, so a test can check
// the chain verifies after a no-op append.
func verifierFor(sg *sign.Signer, pub ed25519.PublicKey) *sign.Verifier {
	return sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})
}

// keyedRecord returns validRecord's fixture (an Observed memory_surfaced
// record) with the given idempotency key set. The ledger does not validate the
// key's format, so any non-empty string is a legal key.
func keyedRecord(t *testing.T, id record.RecordID, key record.IdemKey) record.Record {
	t.Helper()
	rec := validRecord(t, id)
	rec.IdempotencyKey = key
	return rec
}

// searchRecord builds a second, distinctly-shaped valid claim -- a
// search_performed record -- so a test can prove a DIFFERENT claim with its own
// key appends as a separate record rather than deduplicating.
func searchRecord(t *testing.T, id record.RecordID, key record.IdemKey) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"count":0}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonSearchPerformed, ev)
	require.NoError(t, err)
	rec := record.Record{
		ID:             id,
		At:             time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC),
		Event:          record.EventSearchPerformed,
		Reason:         reason,
		Subject:        record.Subject{Scope: record.Scope{UserID: "u1"}, ContentHash: hash32(0x50)},
		IdempotencyKey: key,
	}
	require.NoError(t, rec.Validate(), "the test fixture must be a valid record")
	return rec
}

// TestRetriedEventProducesExactlyOneRecord is Task 18's core contract: an
// append carrying a key already present is a no-op that returns the EXISTING
// record's ID, writes no row, does not advance Seq, and does not change the
// head hash. It also proves a different claim with its own key appends as a
// second record, and that keyless records are never deduplicated (Ruling 2).
func TestRetriedEventProducesExactlyOneRecord(t *testing.T) {
	l, sg, pub, st := newLedger(t)
	v := verifierFor(sg, pub)

	rec := keyedRecord(t, "rec-keyed", "idem-1")
	firstID, err := l.Append(rec)
	require.NoError(t, err)
	assert.Equal(t, record.RecordID("rec-keyed"), firstID)

	head1, ok, err := l.Head()
	require.NoError(t, err)
	require.True(t, ok)

	// Hazard 1: the retry carries a DIFFERENT record ID but the SAME key. The
	// ledger must return the STORED record's ID, not the caller's.
	retry := keyedRecord(t, "rec-different-id", "idem-1")
	secondID, err := l.Append(retry)
	require.NoError(t, err, "a retry with an existing key is a no-op, not an error")
	assert.Equal(t, firstID, secondID,
		"a retry must return the EXISTING record's ID, never the caller's")

	// Hazard 3: no row added, Seq not advanced, head hash unchanged.
	assert.Equal(t, 1, countRecords(t, st), "a retry must not add a second row")

	head2, ok, err := l.Head()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, uint64(0), head2.Seq, "a no-op must not advance Seq")
	assert.Equal(t, head1.Hash, head2.Hash, "a no-op must not change the head hash")

	breaks, err := l.Verify(v)
	require.NoError(t, err)
	assert.Empty(t, breaks, "the chain must verify with no breaks after a no-op")

	// A different claim, with its own key, appends as a SECOND record.
	other := searchRecord(t, "rec-other", "idem-2")
	otherID, err := l.Append(other)
	require.NoError(t, err)
	assert.Equal(t, record.RecordID("rec-other"), otherID)
	assert.Equal(t, 2, countRecords(t, st), "a different key appends a new record")

	// Hazard 4: two keyless records are never deduplicated against each other.
	_, err = l.Append(validRecord(t, "rec-keyless-a"))
	require.NoError(t, err)
	_, err = l.Append(validRecord(t, "rec-keyless-b"))
	require.NoError(t, err)
	assert.Equal(t, 4, countRecords(t, st), "two keyless records must both be stored")

	breaks, err = l.Verify(v)
	require.NoError(t, err)
	assert.Empty(t, breaks, "the full chain must verify with no breaks")
}

// TestRetryReturnsExistingRecordIDOnly checks the returned identity is exactly
// the stored record's: GetRecord on the returned ID must yield the first
// observation's record, not the retry's.
func TestRetryReturnsExistingRecordIDOnly(t *testing.T) {
	l, _, _, st := newLedger(t)

	first, err := l.Append(keyedRecord(t, "rec-first", "idem-shared"))
	require.NoError(t, err)

	retryID, err := l.Append(keyedRecord(t, "rec-late", "idem-shared"))
	require.NoError(t, err)
	assert.Equal(t, first, retryID)

	stored, err := l.GetRecord(retryID)
	require.NoError(t, err)
	assert.Equal(t, record.RecordID("rec-first"), stored.ID,
		"the stored record is the FIRST observation, not the retry")
	assert.Equal(t, record.IdemKey("idem-shared"), stored.IdempotencyKey)
	assert.Equal(t, 1, countRecords(t, st), "only the first observation exists")
}

// TestConcurrentAppendsSameKeyProduceOneRecord is hazard 2. N goroutines append
// records carrying the SAME key at once. Exactly one record may exist and every
// caller must receive that one record's ID. A lookup performed outside the
// append transaction -- or a missing second guard -- would let two writers both
// insert and produce two rows or two different IDs.
func TestConcurrentAppendsSameKeyProduceOneRecord(t *testing.T) {
	l, _, _, st := newLedger(t)

	const n = 12

	// Build the records on the test goroutine: validRecord calls require.
	recs := make([]record.Record, n)
	for i := 0; i < n; i++ {
		rec := validRecord(t, record.RecordID(fmt.Sprintf("rec-conc-%02d", i)))
		rec.IdempotencyKey = "idem-concurrent"
		recs[i] = rec
	}

	var wg sync.WaitGroup
	ids := make([]record.RecordID, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ids[i], errs[i] = l.Append(recs[i])
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		require.NoErrorf(t, err, "concurrent append %d must succeed, not be dropped", i)
	}
	assert.Equal(t, 1, countRecords(t, st), "exactly one record may exist for one key")
	for i, id := range ids {
		assert.Equalf(t, ids[0], id, "caller %d must receive the same existing record ID", i)
	}

	stored, err := l.GetRecord(ids[0])
	require.NoError(t, err)
	assert.Equal(t, record.IdemKey("idem-concurrent"), stored.IdempotencyKey)
	assert.Equal(t, uint64(0), stored.Seq, "the sole record sits at Seq 0")
}
