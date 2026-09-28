package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
)

// newOpenStore opens a store on a fresh temp path and closes it when the test
// finishes.
func newOpenStore(t *testing.T) *SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	s, err := Open(path)
	require.NoError(t, err, "Open on a fresh path must succeed")
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

// observedReason builds a valid Observed reason of the given kind with a
// canonical JSON payload.
func observedReason(t *testing.T, kind record.ReasonKind, payload string) record.Reason {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(payload))
	require.NoError(t, err)
	r, err := record.NewObservedReason(kind, ev)
	require.NoError(t, err)
	return r
}

// reconstructedReason builds a valid Reconstructed reason.
func reconstructedReason(t *testing.T, kind record.ReasonKind) record.Reason {
	t.Helper()
	ev, err := record.NewReconstructedEvidence(
		[]record.RecordID{"basis-1", "basis-2"}, "rule-x", "v1", 0.75)
	require.NoError(t, err)
	r, err := record.NewReconstructedReason(kind, ev)
	require.NoError(t, err)
	return r
}

// hash32 builds a distinct 32-byte hash from a seed byte.
func hash32(seed byte) record.Hash {
	var h record.Hash
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// fullRecord returns a record with every field populated, so a round trip
// exercises every column.
func fullRecord(t *testing.T) record.Record {
	t.Helper()
	rec := record.Record{
		ID:         "rec-0001",
		Seq:        7,
		At:         time.Date(2026, 3, 4, 5, 6, 7, 891234567, time.UTC),
		RecordedAt: time.Date(2026, 3, 4, 5, 6, 8, 123456789, time.UTC),
		Event:      record.EventMemorySurfaced,
		Reason:     observedReason(t, record.ReasonReturnedBySearch, `{"score":0.9}`),
		Subject: record.Subject{
			MemoryID:    "mem-42",
			Scope:       record.Scope{UserID: "user-1", AgentID: "agent-1", AppID: "app-1", RunID: "run-1"},
			ContentHash: hash32(0x10),
		},
		Content:        &record.Content{Text: "the memory text", Sensitive: true},
		IdempotencyKey: "idem-0001",
		PrevHash:       hash32(0x20),
		Signature:      []byte{0xde, 0xad, 0xbe, 0xef},
		SignerKeyID:    "key-abc",
	}
	// A real record's Hash is ComputeHash of itself, so a read back out of the
	// store must recompute to the same digest.
	h, err := record.ComputeHash(rec)
	require.NoError(t, err)
	rec.Hash = h
	return rec
}

func TestOpenFreshCreatesSchemaAndHeadEmpty(t *testing.T) {
	s := newOpenStore(t)

	// Review Focus #1: an empty table must yield no error and a false bool.
	got, ok, err := s.Head()
	require.NoError(t, err, "Head on an empty table must not error")
	assert.False(t, ok, "Head on an empty table must report false")
	assert.Equal(t, record.Record{}, got, "Head on an empty table must return the zero record")

	// The schema must actually exist.
	var name string
	err = s.db.QueryRow(
		`SELECT name FROM sqlite_master WHERE type='table' AND name='records'`).Scan(&name)
	require.NoError(t, err, "records table must exist after Open")
	assert.Equal(t, "records", name)

	// WAL must be enabled.
	var mode string
	require.NoError(t, s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode))
	assert.Equal(t, "wal", mode, "journal_mode must be WAL")

	// Open must be idempotent against an existing file.
	path := filepath.Join(t.TempDir(), "again.db")
	s1, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s1.Close())
	s2, err := Open(path)
	require.NoError(t, err, "re-opening an existing database must succeed")
	require.NoError(t, s2.Close())
}

func TestOpenCreatesParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "ledger.db")
	s, err := Open(path)
	require.NoError(t, err, "Open must create missing parent directories")
	t.Cleanup(func() { require.NoError(t, s.Close()) })

	_, ok, err := s.Head()
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestPutGetRoundTripEveryField(t *testing.T) {
	s := newOpenStore(t)
	want := fullRecord(t)

	require.NoError(t, s.PutRecord(want))

	got, err := s.GetRecord(want.ID)
	require.NoError(t, err)

	// CanonicalBytes covers ID, Event, Reason (tier + evidence payload),
	// Subject, Content, IdempotencyKey, At, RecordedAt and SignerKeyID, so
	// equal encodings prove every one of those survived byte-for-byte.
	wantCanon, err := record.CanonicalBytes(want)
	require.NoError(t, err)
	gotCanon, err := record.CanonicalBytes(got)
	require.NoError(t, err)
	assert.Equal(t, wantCanon, gotCanon, "canonical encoding must round-trip byte-for-byte")

	// The chain columns are excluded from CanonicalBytes, so assert them too.
	assert.Equal(t, want.Seq, got.Seq)
	assert.Equal(t, want.PrevHash, got.PrevHash)
	assert.Equal(t, want.Hash, got.Hash)
	assert.Equal(t, want.Signature, got.Signature)
	assert.Equal(t, want.SignerKeyID, got.SignerKeyID)

	// The strong end-to-end form of the same check: recomputing the hash from
	// the read-back record must reproduce the stored hash.
	recomputed, err := record.ComputeHash(got)
	require.NoError(t, err)
	assert.Equal(t, want.Hash, recomputed, "hash recomputed from the read-back record must match")

	// The reason's tier and kind must be identifiable from the rebuilt value.
	assert.Equal(t, record.Observed, got.Reason.Tier())
	assert.Equal(t, record.ReasonReturnedBySearch, got.Reason.Kind())
	ev, ok := got.Reason.Observed()
	require.True(t, ok)
	assert.NotEmpty(t, ev)
}

func TestRoundTripReconstructedReason(t *testing.T) {
	s := newOpenStore(t)
	rec := fullRecord(t)
	rec.ID = "rec-recon"
	rec.IdempotencyKey = ""
	rec.Reason = reconstructedReason(t, record.ReasonKeptByContentMatch)

	require.NoError(t, s.PutRecord(rec))
	got, err := s.GetRecord(rec.ID)
	require.NoError(t, err)

	assert.Equal(t, record.Reconstructed, got.Reason.Tier())
	assert.Equal(t, record.ReasonKeptByContentMatch, got.Reason.Kind())
	_, ok := got.Reason.Reconstructed()
	assert.True(t, ok, "reconstructed evidence must survive the round trip")
}

func TestNilContentRoundTripsAsNil(t *testing.T) {
	s := newOpenStore(t)
	rec := fullRecord(t)
	rec.ID = "rec-nocontent"
	rec.IdempotencyKey = ""
	rec.Content = nil

	require.NoError(t, s.PutRecord(rec))
	got, err := s.GetRecord(rec.ID)
	require.NoError(t, err)
	assert.Nil(t, got.Content, "a NULL content_text must read back as nil Content")

	// A non-nil but empty Content is a different claim and must not collapse
	// to nil.
	rec2 := fullRecord(t)
	rec2.ID = "rec-emptycontent"
	rec2.Seq = 8
	rec2.IdempotencyKey = ""
	rec2.Content = &record.Content{Text: "", Sensitive: false}
	require.NoError(t, s.PutRecord(rec2))
	got2, err := s.GetRecord(rec2.ID)
	require.NoError(t, err)
	require.NotNil(t, got2.Content)
	assert.Equal(t, "", got2.Content.Text)
	assert.False(t, got2.Content.Sensitive)
}

func TestGetRecordNotFound(t *testing.T) {
	s := newOpenStore(t)
	_, err := s.GetRecord("does-not-exist")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotFound), "unknown ID must wrap ErrNotFound, got %v", err)
}

func TestListRecordsOrderedByAtThenSeq(t *testing.T) {
	s := newOpenStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	mk := func(id string, seq uint64, at time.Time) record.Record {
		r := fullRecord(t)
		r.ID = record.RecordID(id)
		r.Seq = seq
		r.At = at
		r.IdempotencyKey = ""
		return r
	}

	// Insert deliberately out of order, including a tie on At broken by Seq.
	require.NoError(t, s.PutRecord(mk("c", 5, base.Add(2*time.Hour))))
	require.NoError(t, s.PutRecord(mk("a", 3, base.Add(1*time.Hour))))
	require.NoError(t, s.PutRecord(mk("b", 9, base.Add(1*time.Hour))))

	got, err := s.ListRecords(base, base.Add(10*time.Hour))
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, []record.RecordID{"a", "b", "c"},
		[]record.RecordID{got[0].ID, got[1].ID, got[2].ID},
		"records must order by At ascending, ties broken by Seq ascending")
}

func TestListRecordsInclusiveBounds(t *testing.T) {
	s := newOpenStore(t)
	base := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)

	mk := func(id string, seq uint64, at time.Time) record.Record {
		r := fullRecord(t)
		r.ID = record.RecordID(id)
		r.Seq = seq
		r.At = at
		r.IdempotencyKey = ""
		return r
	}

	require.NoError(t, s.PutRecord(mk("before", 1, base.Add(-time.Minute))))
	require.NoError(t, s.PutRecord(mk("from", 2, base)))
	require.NoError(t, s.PutRecord(mk("mid", 3, base.Add(time.Minute))))
	require.NoError(t, s.PutRecord(mk("to", 4, base.Add(2*time.Minute))))
	require.NoError(t, s.PutRecord(mk("after", 5, base.Add(3*time.Minute))))

	got, err := s.ListRecords(base, base.Add(2*time.Minute))
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, []record.RecordID{"from", "mid", "to"},
		[]record.RecordID{got[0].ID, got[1].ID, got[2].ID},
		"both range bounds must be inclusive")
}

func TestHeadReturnsGreatestSeq(t *testing.T) {
	s := newOpenStore(t)
	for i, id := range []string{"one", "two", "three"} {
		r := fullRecord(t)
		r.ID = record.RecordID(id)
		r.Seq = uint64(i + 1)
		r.IdempotencyKey = ""
		require.NoError(t, s.PutRecord(r))
	}

	got, ok, err := s.Head()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, record.RecordID("three"), got.ID)
	assert.Equal(t, uint64(3), got.Seq)
}

func TestDuplicateIdemKeyRejected(t *testing.T) {
	s := newOpenStore(t)

	first := fullRecord(t)
	require.NoError(t, s.PutRecord(first))

	second := fullRecord(t)
	second.ID = "rec-0002"
	second.Seq = 8
	second.Hash = hash32(0x40)

	err := s.PutRecord(second)
	require.Error(t, err, "a duplicate non-empty idempotency key must be rejected")
	assert.True(t, errors.Is(err, ErrDuplicateIdemKey),
		"error must wrap ErrDuplicateIdemKey, got %v", err)
}

func TestTwoKeylessRecordsAllowed(t *testing.T) {
	s := newOpenStore(t)

	// The idempotency index is partial (WHERE idempotency_key <> ''), so two
	// records with no key must both persist.
	a := fullRecord(t)
	a.ID = "keyless-a"
	a.Seq = 1
	a.IdempotencyKey = ""
	require.NoError(t, s.PutRecord(a))

	b := fullRecord(t)
	b.ID = "keyless-b"
	b.Seq = 2
	b.IdempotencyKey = ""
	require.NoError(t, s.PutRecord(b), "the second keyless record must be accepted")

	all, err := s.ListRecords(a.At.Add(-time.Hour), a.At.Add(time.Hour))
	require.NoError(t, err)
	assert.Len(t, all, 2)
}

func TestByIdemKey(t *testing.T) {
	s := newOpenStore(t)
	rec := fullRecord(t)
	require.NoError(t, s.PutRecord(rec))

	got, ok, err := s.ByIdemKey(rec.IdempotencyKey)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, rec.ID, got.ID)

	// The empty key is never a match, even against a keyless record.
	keyless := fullRecord(t)
	keyless.ID = "keyless"
	keyless.Seq = 99
	keyless.IdempotencyKey = ""
	require.NoError(t, s.PutRecord(keyless))

	zero, ok, err := s.ByIdemKey("")
	require.NoError(t, err)
	assert.False(t, ok, "ByIdemKey(\"\") must report not-found")
	assert.Equal(t, record.Record{}, zero)

	// An unknown non-empty key is not a match either.
	_, ok, err = s.ByIdemKey("no-such-key")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestHandEditedTierColumnSurfacesAsReadError(t *testing.T) {
	s := newOpenStore(t)
	rec := fullRecord(t) // tier is "observed"
	require.NoError(t, s.PutRecord(rec))

	// Simulate a person editing the row directly in SQLite so the readability
	// column disagrees with the encoded payload. The read must fail rather
	// than return a record whose tier column says something else.
	_, err := s.db.Exec(`UPDATE records SET tier = ? WHERE id = ?`, "internal", string(rec.ID))
	require.NoError(t, err)

	_, err = s.GetRecord(rec.ID)
	require.Error(t, err, "a tier column that disagrees with its payload must fail the read")

	// Put the tier back and confirm the row is readable again, proving the
	// failure was caused by the tamper and nothing else.
	_, err = s.db.Exec(`UPDATE records SET tier = ? WHERE id = ?`, "observed", string(rec.ID))
	require.NoError(t, err)
	_, err = s.GetRecord(rec.ID)
	require.NoError(t, err)
}

func TestCorruptedReasonPayloadSurfacesAsReadError(t *testing.T) {
	s := newOpenStore(t)
	rec := fullRecord(t)
	require.NoError(t, s.PutRecord(rec))

	// Overwrite the encoded reason with bytes that cannot parse.
	_, err := s.db.Exec(`UPDATE records SET reason_payload = ? WHERE id = ?`,
		[]byte("{not json"), string(rec.ID))
	require.NoError(t, err)

	_, err = s.GetRecord(rec.ID)
	require.Error(t, err, "an undecodable reason payload must fail the read")
}

func TestListRecordsOnEmptyTableReturnsNil(t *testing.T) {
	s := newOpenStore(t)
	got, err := s.ListRecords(time.Time{}, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestPutRecordRejectsInvalidReason(t *testing.T) {
	s := newOpenStore(t)
	rec := fullRecord(t)
	rec.Reason = record.Reason{} // zero value: cannot be encoded

	err := s.PutRecord(rec)
	require.Error(t, err, "a record with an unencodable reason must be rejected")
}

// ensure the concrete type satisfies the interface at compile time.
var _ Store = (*SQLiteStore)(nil)
