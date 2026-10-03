package ledger_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// testSeed is a fixed 32-byte ed25519 seed, so every run signs with the same
// key and the tests stay deterministic.
var testSeed = []byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

// fixedNow is the deterministic clock the ledger is given.
var fixedNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// newSigner builds a Signer from testSeed, following the sign package's own
// test pattern (base64 seed through the env KeySource; NewSigner never
// generates a key). It also returns the matching public key so tests can verify
// signatures.
func newSigner(t *testing.T) (*sign.Signer, ed25519.PublicKey) {
	t.Helper()
	t.Setenv("NOTARY_LEDGER_TEST_KEY", base64.StdEncoding.EncodeToString(testSeed))
	s, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_LEDGER_TEST_KEY"})
	require.NoError(t, err)
	require.NotNil(t, s)
	pub := ed25519.NewKeyFromSeed(testSeed).Public().(ed25519.PublicKey)
	return s, pub
}

// newStore opens a store on a fresh temp path and closes it on cleanup.
func newStore(t *testing.T) *store.SQLiteStore {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// newLedger wires a fresh store, signer, and fixed clock into a Ledger.
func newLedger(t *testing.T) (*ledger.Ledger, *sign.Signer, ed25519.PublicKey, *store.SQLiteStore) {
	t.Helper()
	st := newStore(t)
	sg, pub := newSigner(t)
	l := ledger.New(st, sg, func() time.Time { return fixedNow })
	return l, sg, pub, st
}

// hash32 builds a distinct 32-byte hash from a seed byte.
func hash32(seed byte) record.Hash {
	var h record.Hash
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// validRecord returns a well-formed record with every caller-supplied field
// set, but no chain position (Seq, PrevHash, Hash, Signature) -- those belong
// to the ledger.
func validRecord(t *testing.T, id record.RecordID) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"ok":true}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonReturnedBySearch, ev)
	require.NoError(t, err)
	rec := record.Record{
		ID:     id,
		At:     time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC),
		Event:  record.EventMemorySurfaced,
		Reason: reason,
		Subject: record.Subject{
			MemoryID:    "mem-1",
			Scope:       record.Scope{UserID: "u1", AgentID: "a1"},
			ContentHash: hash32(0x40),
		},
	}
	require.NoError(t, rec.Validate(), "the test fixture must be a valid record")
	return rec
}

// countRecords reports how many records the store holds, by listing a range
// wide enough to include anything the tests write.
func countRecords(t *testing.T, st store.Store) int {
	t.Helper()
	from := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	recs, err := st.ListRecords(from, to)
	require.NoError(t, err)
	return len(recs)
}

// TestAppendAssignsSeqPrevHashHashAndSignature is the core contract: a valid
// record is assigned chain position 0, linked to the genesis hash, hashed over
// its final position, and signed, all provably inside the ledger.
func TestAppendAssignsSeqPrevHashHashAndSignature(t *testing.T) {
	l, sg, pub, st := newLedger(t)

	rec := validRecord(t, "rec-0001")
	id, err := l.Append(rec)
	require.NoError(t, err)
	assert.Equal(t, rec.ID, id, "Append must return the record's ID")

	stored, err := l.GetRecord(id)
	require.NoError(t, err)

	// Chain position: first record is Seq 0 and links to genesis.
	assert.Equal(t, uint64(0), stored.Seq, "the first record must be Seq 0")
	assert.Equal(t, record.GenesisHash(), stored.PrevHash, "the first record's PrevHash must be the genesis hash")

	// RecordedAt is stamped from the injected clock.
	assert.Equal(t, fixedNow, stored.RecordedAt, "RecordedAt must be stamped from the ledger's clock")

	// The hash must be a fresh ComputeHash over the stored record (Seq and
	// PrevHash already set), proving the hash covers the chain position.
	wantHash, err := record.ComputeHash(stored)
	require.NoError(t, err)
	assert.Equal(t, wantHash, stored.Hash, "Hash must be ComputeHash over the record with Seq/PrevHash set")

	// The signature must be present and name the signer's key.
	assert.NotEmpty(t, stored.Signature, "the record must carry a signature")
	assert.Equal(t, sg.KeyID(), stored.SignerKeyID, "SignerKeyID must match the signer's KeyID")

	// Round trip: the signature genuinely verifies under the signer's public
	// key, so the record is proven signed, not merely populated.
	verifier := sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})
	require.NoError(t, verifier.Verify(stored.SignerKeyID, stored.Hash[:], stored.Signature),
		"the stored signature must verify against the stored hash")

	assert.Equal(t, 1, countRecords(t, st))
}

// TestAppendSecondRecordLinksToFirst proves Seq increments and the chain links:
// the second record's PrevHash is the first record's Hash.
func TestAppendSecondRecordLinksToFirst(t *testing.T) {
	l, _, _, st := newLedger(t)

	firstID, err := l.Append(validRecord(t, "rec-0001"))
	require.NoError(t, err)
	first, err := l.GetRecord(firstID)
	require.NoError(t, err)

	secondID, err := l.Append(validRecord(t, "rec-0002"))
	require.NoError(t, err)
	second, err := l.GetRecord(secondID)
	require.NoError(t, err)

	assert.Equal(t, uint64(1), second.Seq, "the second record must be Seq 1")
	assert.Equal(t, first.Hash, second.PrevHash, "the second record must link to the first record's hash")
	assert.Equal(t, 2, countRecords(t, st))
}

// TestAppendRejectsInvalidTierAndLeavesStoreUnchanged proves a record whose
// Reason is the zero value (whose tier is invalid) is refused with
// ErrInvalidTier and that the store is provably untouched.
func TestAppendRejectsInvalidTierAndLeavesStoreUnchanged(t *testing.T) {
	l, _, _, st := newLedger(t)

	rec := validRecord(t, "rec-bad")
	rec.Reason = record.Reason{} // zero value: invalid tier

	id, err := l.Append(rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ledger.ErrInvalidTier)
	assert.Empty(t, id, "a rejected append must not return an ID")

	assert.Equal(t, 0, countRecords(t, st), "the store must be unchanged after a rejected append")
	_, ok, err := l.Head()
	require.NoError(t, err)
	assert.False(t, ok, "the store must still be empty after a rejected append")
}

// TestAppendRejectsOtherInvalidRecord proves a non-reason validation failure is
// reported as ErrInvalidRecord, and the store stays empty.
func TestAppendRejectsOtherInvalidRecord(t *testing.T) {
	l, _, _, st := newLedger(t)

	rec := validRecord(t, "rec-bad")
	rec.Event = record.EventType("not_a_real_event")

	_, err := l.Append(rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ledger.ErrInvalidRecord)
	assert.NotErrorIs(t, err, ledger.ErrInvalidTier)
	assert.Equal(t, 0, countRecords(t, st), "the store must be unchanged after a rejected append")
}

// TestAppendRejectsCallerSuppliedSeq proves no caller may choose a chain
// position, and that the store is left untouched when one tries.
func TestAppendRejectsCallerSuppliedSeq(t *testing.T) {
	l, _, _, st := newLedger(t)

	rec := validRecord(t, "rec-seq")
	rec.Seq = 5

	id, err := l.Append(rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ledger.ErrSeqAssigned)
	assert.Empty(t, id)
	assert.Equal(t, 0, countRecords(t, st), "the store must be unchanged after a rejected append")
}

// TestHeadOnFreshLedger proves Head reports absence (false) with no error on an
// empty ledger.
func TestHeadOnFreshLedger(t *testing.T) {
	l, _, _, _ := newLedger(t)

	got, ok, err := l.Head()
	require.NoError(t, err, "Head on a fresh ledger must not error")
	assert.False(t, ok, "Head on a fresh ledger must report false")
	assert.Equal(t, record.Record{}, got)
}

// TestGetRecordNotFound proves GetRecord surfaces the store's not-found error.
func TestGetRecordNotFound(t *testing.T) {
	l, _, _, _ := newLedger(t)

	_, err := l.GetRecord("does-not-exist")
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// TestAppendWithoutSignerErrorsNotPanics proves Append returns an error rather
// than dereferencing a nil signer, and leaves the store untouched.
func TestAppendWithoutSignerErrorsNotPanics(t *testing.T) {
	st := newStore(t)
	l := ledger.New(st, nil, func() time.Time { return fixedNow })

	var id record.RecordID
	require.NotPanics(t, func() {
		var err error
		id, err = l.Append(validRecord(t, "rec-nosigner"))
		require.Error(t, err)
	})
	assert.Empty(t, id)
	assert.Equal(t, 0, countRecords(t, st), "the store must be unchanged")
}

// TestAppendRoundTripsContentAndHash covers the *Content path: a record carrying
// non-nil content must survive the store faithfully, and its stored hash must
// still recompute to itself -- Task 10's exact operation (recompute the digest
// over the stored record) applied to a record with content. The nil-content case
// is covered alongside it, so CanonicalBytes' content presence flag is exercised
// on both sides.
func TestAppendRoundTripsContentAndHash(t *testing.T) {
	t.Run("non-nil content", func(t *testing.T) {
		l, _, _, _ := newLedger(t)

		rec := validRecord(t, "rec-content")
		rec.Content = &record.Content{Text: "sensitive note", Sensitive: true}

		id, err := l.Append(rec)
		require.NoError(t, err)
		stored, err := l.GetRecord(id)
		require.NoError(t, err)

		require.NotNil(t, stored.Content, "content must survive as a non-nil pointer")
		assert.Equal(t, "sensitive note", stored.Content.Text, "the content text must survive")
		assert.True(t, stored.Content.Sensitive, "the sensitive flag must survive")

		wantHash, err := record.ComputeHash(stored)
		require.NoError(t, err)
		assert.Equal(t, wantHash, stored.Hash,
			"the stored hash must recompute over the stored content")
	})

	t.Run("nil content", func(t *testing.T) {
		l, _, _, _ := newLedger(t)

		rec := validRecord(t, "rec-nocontent")
		require.Nil(t, rec.Content, "the fixture carries no content")

		id, err := l.Append(rec)
		require.NoError(t, err)
		stored, err := l.GetRecord(id)
		require.NoError(t, err)

		assert.Nil(t, stored.Content, "absent content must round-trip as nil, not as empty content")

		wantHash, err := record.ComputeHash(stored)
		require.NoError(t, err)
		assert.Equal(t, wantHash, stored.Hash,
			"the stored hash must recompute over the stored record")
	})
}

// appendRecordedAt appends a fresh valid record stamped with the given
// RecordedAt, so a test can drive the as-of read's filter directly. RecordedAt
// is set on the record before Append; Append only stamps the field when it is
// zero, so the value the test sets survives into the store.
func appendRecordedAt(t *testing.T, l *ledger.Ledger, id string, recordedAt time.Time) {
	t.Helper()
	rec := validRecord(t, record.RecordID(id))
	rec.RecordedAt = recordedAt
	_, err := l.Append(rec)
	require.NoError(t, err)
}

// TestReplayAsOfReturnsCleanPrefixWithNoBreaks covers the ordinary case: a
// contiguous run of the chain whose RecordedAt is at or before T is returned in
// Seq order and verifies with zero breaks.
func TestReplayAsOfReturnsCleanPrefixWithNoBreaks(t *testing.T) {
	l, sg, pub, _ := newLedger(t)
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	appendRecordedAt(t, l, "rec-0001", t0)
	appendRecordedAt(t, l, "rec-0002", t0.Add(1*time.Minute))
	appendRecordedAt(t, l, "rec-0003", t0.Add(2*time.Minute))
	appendRecordedAt(t, l, "rec-0004", t0.Add(3*time.Minute))

	v := sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})

	// T equals rec-0002's RecordedAt and falls before rec-0003's, so the as-of
	// view is the contiguous prefix {seq 0, seq 1} -- inclusive at the bound.
	records, breaks, err := l.ReplayAsOf(t0.Add(1*time.Minute), v)
	require.NoError(t, err)
	assert.Empty(t, breaks, "a contiguous prefix must verify with no breaks")
	require.Len(t, records, 2)
	assert.Equal(t, []record.RecordID{"rec-0001", "rec-0002"},
		[]record.RecordID{records[0].ID, records[1].ID})
}

// TestReplayAsOfReportsABreakForAHoleInThePrefix is the phase's falsifier (spec
// §4). A record stamped with an earlier RecordedAt than its predecessor (what a
// backwards NTP step does) puts that predecessor OUTSIDE T while its successor
// is inside: the as-of set is not a prefix of the chain but a set with a hole.
// Replay must report a break naming the missing seq, not print a
// plausible-looking prefix.
func TestReplayAsOfReportsABreakForAHoleInThePrefix(t *testing.T) {
	l, sg, pub, _ := newLedger(t)
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	appendRecordedAt(t, l, "rec-0001", t0)                    // seq 0
	appendRecordedAt(t, l, "rec-0002", t0.Add(1*time.Minute)) // seq 1
	appendRecordedAt(t, l, "rec-0003", t0.Add(5*time.Minute)) // seq 2 -- recorded AFTER T
	appendRecordedAt(t, l, "rec-0004", t0.Add(2*time.Minute)) // seq 3 -- recorded before its predecessor

	v := sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})

	// T is after seq 3's time but before seq 2's, so the as-of set is
	// {seq 0, 1, 3} -- the hole is seq 2.
	records, breaks, err := l.ReplayAsOf(t0.Add(3*time.Minute), v)
	require.NoError(t, err)
	require.Len(t, records, 3, "the as-of view must be exactly {seq 0,1,3}")
	assert.Equal(t, []record.RecordID{"rec-0001", "rec-0002", "rec-0004"},
		[]record.RecordID{records[0].ID, records[1].ID, records[2].ID})

	// The hole must be named, not silently printed as "what Notary knew at T".
	require.NotEmpty(t, breaks, "a hole in the prefix must produce a break")
	var seqBreak bool
	for _, b := range breaks {
		if b.Field == "seq" {
			seqBreak = true
			assert.Equal(t, uint64(3), b.Seq, "the break names the record found out of place")
			assert.Contains(t, b.Detail, "expected seq 2",
				"the break must name the missing seq (2)")
		}
	}
	assert.True(t, seqBreak, "the hole must be reported as a seq break, got %+v", breaks)
}

// TestReplayAsOfEmptyLedgerReturnsNothing covers Review Focus 2: an empty
// ledger yields no records, no breaks, and no error.
func TestReplayAsOfEmptyLedgerReturnsNothing(t *testing.T) {
	l, sg, pub, _ := newLedger(t)
	v := sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})

	records, breaks, err := l.ReplayAsOf(fixedNow, v)
	require.NoError(t, err)
	assert.Empty(t, records, "an empty ledger must replay no records")
	assert.Empty(t, breaks, "an empty ledger must replay no breaks")
}

// TestListRecordsByMemoryDelegatesAndFilters proves the ledger pass-through
// reaches the store's memory-filtered read rather than returning everything.
// Two memories' records are appended -- they interleave on the single chain --
// and only the requested memory's records must come back, in Seq order. A
// pass-through that ignored memoryID, or delegated to ListRecords, would
// return both memories' records here.
func TestListRecordsByMemoryDelegatesAndFilters(t *testing.T) {
	l, _, _, _ := newLedger(t)

	appendForMemory := func(id record.RecordID, memoryID string) {
		rec := validRecord(t, id)
		rec.Subject.MemoryID = memoryID
		_, err := l.Append(rec)
		require.NoError(t, err)
	}
	appendForMemory("a-1", "mem-A")
	appendForMemory("b-1", "mem-B")
	appendForMemory("a-2", "mem-A")

	got, err := l.ListRecordsByMemory("mem-A")
	require.NoError(t, err)
	require.Len(t, got, 2, "only the requested memory's records must come back")
	assert.Equal(t, []record.RecordID{"a-1", "a-2"},
		[]record.RecordID{got[0].ID, got[1].ID},
		"the ledger read must filter on memory and order by Seq ascending")
	for _, r := range got {
		assert.Equal(t, "mem-A", r.Subject.MemoryID,
			"every returned record must belong to the requested memory")
	}
}
