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
	assert.Equal(t, record.GenesisHash, stored.PrevHash, "the first record's PrevHash must be the genesis hash")

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
