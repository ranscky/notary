package interceptor_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
	"notary/internal/interceptor"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// testSeed is a fixed 32-byte ed25519 seed, so every run signs with the same
// key and the tests stay deterministic. It mirrors the ledger package's own
// fixture so the two suites share a signing identity shape.
var testSeed = []byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

// newSigner builds a Signer from testSeed through an environment key source.
func newSigner(t *testing.T) *sign.Signer {
	t.Helper()
	t.Setenv("NOTARY_INTERCEPTOR_TEST_KEY", base64.StdEncoding.EncodeToString(testSeed))
	s, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_INTERCEPTOR_TEST_KEY"})
	require.NoError(t, err)
	require.NotNil(t, s)
	return s
}

// hash32 builds a distinct 32-byte hash from a seed byte.
func hash32(seed byte) record.Hash {
	var h record.Hash
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// validRecord returns a well-formed record with no chain position (Seq,
// PrevHash, Hash, Signature) -- those belong to the ledger. content may be nil.
func validRecord(t *testing.T, id record.RecordID, content *record.Content) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"ok":true}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonSearchPerformed, ev)
	require.NoError(t, err)
	rec := record.Record{
		ID:     id,
		At:     time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC),
		Event:  record.EventSearchPerformed,
		Reason: reason,
		Subject: record.Subject{
			MemoryID:    "mem-1",
			Scope:       record.Scope{UserID: "u1", AgentID: "a1", AppID: "app1", RunID: "run1"},
			ContentHash: hash32(0x40),
		},
		Content: content,
	}
	require.NoError(t, rec.Validate(), "the test fixture must be a valid record")
	return rec
}

// newLedgerHarness wires a fresh store, signer, and ledger over a temp dir and
// returns them together with the temp dir so a test can place a gap log beside
// the database.
func newLedgerHarness(t *testing.T) (*ledger.Ledger, *store.SQLiteStore, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	l := ledger.New(st, newSigner(t), func() time.Time { return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC) })
	return l, st, dir
}

// openGap opens a gap log at path and closes it on cleanup.
func openGap(t *testing.T, path string) *gap.Log {
	t.Helper()
	g, err := gap.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// TestFailModeValidate pins the fail-mode vocabulary and its validation: the
// zero value and any unknown value are invalid, FailOpenLoud validates, and
// FailClosed is defined but unimplemented.
func TestFailModeValidate(t *testing.T) {
	t.Run("FailOpenLoud is valid", func(t *testing.T) {
		require.NoError(t, interceptor.FailOpenLoud.Validate())
	})

	t.Run("FailClosed is defined but unimplemented", func(t *testing.T) {
		err := interceptor.FailClosed.Validate()
		require.Error(t, err)
		assert.True(t, errors.Is(err, interceptor.ErrFailClosedUnimplemented),
			"FailClosed must report ErrFailClosedUnimplemented, got %v", err)
	})

	t.Run("the zero value is invalid", func(t *testing.T) {
		assert.Error(t, interceptor.FailMode(0).Validate(),
			"FailMode(0) must not be a valid mode")
	})

	t.Run("an unknown value is invalid", func(t *testing.T) {
		assert.Error(t, interceptor.FailMode(200).Validate(),
			"an unrecognised FailMode must be rejected")
	})
}

// TestAuditWriterSatisfiesInterceptor proves the compile-time and behavioural
// contract: *AuditWriter is an Interceptor whose fail mode is FailOpenLoud.
func TestAuditWriterSatisfiesInterceptor(t *testing.T) {
	var i interceptor.Interceptor = interceptor.NewAuditWriter(nil, nil, nil)
	require.NotNil(t, i)
	assert.Equal(t, interceptor.FailOpenLoud, i.FailMode())
}

// TestWriteHealthyLedgerStoresOneRecord is the happy path: a writable ledger
// stores exactly one record, no gap is logged, and no marker is emitted.
func TestWriteHealthyLedgerStoresOneRecord(t *testing.T) {
	l, _, dir := newLedgerHarness(t)
	gapPath := filepath.Join(dir, "gaps.log")
	g := openGap(t, gapPath)
	var a, b bytes.Buffer
	w := interceptor.NewAuditWriter(l, g, []io.Writer{&a, &b})

	rec := validRecord(t, "rec-healthy", nil)
	require.NoError(t, w.Write(rec))

	stored, err := l.GetRecord(rec.ID)
	require.NoError(t, err, "the healthy write must be stored")
	assert.Equal(t, rec.ID, stored.ID)
	assert.Equal(t, uint64(0), stored.Seq)

	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Empty(t, entries, "a healthy write must not log a gap")
	assert.Empty(t, a.String(), "a healthy write must not emit a marker")
	assert.Empty(t, b.String(), "a healthy write must not emit a marker")
}

// TestWriteFailOpenLoudOnLedgerFailure is the core fail-open-loud contract: a
// ledger failure returns nil, logs exactly one reconcilable gap entry, writes a
// marker to EVERY channel, and leaves the gap log's own chain valid.
func TestWriteFailOpenLoudOnLedgerFailure(t *testing.T) {
	l, st, dir := newLedgerHarness(t)
	gapPath := filepath.Join(dir, "gaps.log")
	g := openGap(t, gapPath)
	var a, b bytes.Buffer
	w := interceptor.NewAuditWriter(l, g, []io.Writer{&a, &b})

	// Close the store so the ledger's append fails at the store boundary.
	require.NoError(t, st.Close())

	rec := validRecord(t, "rec-gap", nil)
	err := w.Write(rec)
	assert.NoError(t, err, "a ledger failure must NOT become a caller error (fail-open)")

	// Exactly one gap entry, reconcilable by (Kind, Scope, CorrelationID).
	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, rec.Event, entries[0].Kind, "the gap Kind is the event the gap concerns")
	assert.Equal(t, rec.Subject.Scope, entries[0].Scope)
	assert.Equal(t, string(rec.ID), entries[0].CorrelationID, "CorrelationID is the missing record's ID")

	// HAZARD 1: the gap log's own hash chain must still validate. A raw,
	// non-JSON marker written straight to the log file would break this.
	breaks, err := gap.Verify(gapPath)
	require.NoError(t, err)
	assert.Empty(t, breaks, "the gap log's own chain must validate after a fail-open-loud write")

	// HAZARD 3: every channel receives the marker, not just the first.
	assert.NotEmpty(t, a.String(), "channel a must receive the marker")
	assert.NotEmpty(t, b.String(), "channel b must receive the marker")
	assert.Contains(t, a.String(), string(rec.ID), "the marker must name the correlation ID")
	assert.Equal(t, a.String(), b.String(), "every channel must receive the same marker")
}

// TestWriteMarkerNeverCarriesSensitiveContent is HAZARD 2: the loud marker must
// name only non-sensitive metadata, never raw memory text.
func TestWriteMarkerNeverCarriesSensitiveContent(t *testing.T) {
	const secret = "SUPER_SECRET_MEMORY_TEXT_9f3a2b"

	l, st, dir := newLedgerHarness(t)
	gapPath := filepath.Join(dir, "gaps.log")
	g := openGap(t, gapPath)
	var a, b bytes.Buffer
	w := interceptor.NewAuditWriter(l, g, []io.Writer{&a, &b})
	require.NoError(t, st.Close())

	rec := validRecord(t, "rec-secret", &record.Content{Text: secret, Sensitive: true})
	require.NoError(t, w.Write(rec))

	assert.NotContains(t, a.String(), secret, "channel a must never leak raw memory text")
	assert.NotContains(t, b.String(), secret, "channel b must never leak raw memory text")

	raw, err := os.ReadFile(gapPath)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), secret, "the gap log must never leak raw memory text")
}

// TestWriteNilSafety is HAZARD 4: no input may panic. A nil ledger is a
// failure (not a healthy ledger); a nil gap log is a gap-write failure; a nil
// or empty channel slice is simply silent.
func TestWriteNilSafety(t *testing.T) {
	t.Run("nil ledger is treated as a ledger failure", func(t *testing.T) {
		dir := t.TempDir()
		gapPath := filepath.Join(dir, "gaps.log")
		g := openGap(t, gapPath)
		var buf bytes.Buffer
		w := interceptor.NewAuditWriter(nil, g, []io.Writer{&buf})

		rec := validRecord(t, "rec-nil-ledger", nil)
		var err error
		require.NotPanics(t, func() { err = w.Write(rec) })
		require.NoError(t, err, "a nil ledger must fail open")
		entries, rerr := gap.Read(gapPath)
		require.NoError(t, rerr)
		assert.Len(t, entries, 1, "a nil ledger must still log a gap")
		assert.NotEmpty(t, buf.String(), "a nil ledger must still mark the channel")
	})

	t.Run("nil gap log surfaces the gap error without panicking", func(t *testing.T) {
		var buf bytes.Buffer
		w := interceptor.NewAuditWriter(nil, nil, []io.Writer{&buf})

		rec := validRecord(t, "rec-nil-gap", nil)
		var err error
		require.NotPanics(t, func() { err = w.Write(rec) })
		require.Error(t, err, "a doubly-failed write is a real error worth surfacing")
		assert.NotEmpty(t, buf.String(), "the channel marker must still be written")
	})

	t.Run("a closed gap log surfaces its error after marking", func(t *testing.T) {
		dir := t.TempDir()
		gapPath := filepath.Join(dir, "gaps.log")
		g, err := gap.Open(gapPath)
		require.NoError(t, err)
		require.NoError(t, g.Close())
		var buf bytes.Buffer
		w := interceptor.NewAuditWriter(nil, g, []io.Writer{&buf})

		rec := validRecord(t, "rec-closed-gap", nil)
		require.NotPanics(t, func() { err = w.Write(rec) })
		require.Error(t, err, "g.Record on a closed log must surface")
		assert.NotEmpty(t, buf.String(), "the channel marker must still be written")
	})

	t.Run("nil and empty channel slices do not panic", func(t *testing.T) {
		l, st, dir := newLedgerHarness(t)
		gapPath := filepath.Join(dir, "gaps.log")
		g := openGap(t, gapPath)
		require.NoError(t, st.Close())
		rec := validRecord(t, "rec-no-chan", nil)

		var err error
		require.NotPanics(t, func() { err = interceptor.NewAuditWriter(l, g, nil).Write(rec) })
		require.NoError(t, err)
		require.NotPanics(t, func() {
			err = interceptor.NewAuditWriter(l, g, []io.Writer{}).Write(validRecord(t, "rec-no-chan-2", nil))
		})
		require.NoError(t, err)

		breaks, verr := gap.Verify(gapPath)
		require.NoError(t, verr)
		assert.Empty(t, breaks)
	})

	t.Run("a nil element inside the channel slice is skipped", func(t *testing.T) {
		l, st, dir := newLedgerHarness(t)
		gapPath := filepath.Join(dir, "gaps.log")
		g := openGap(t, gapPath)
		var a, b bytes.Buffer
		w := interceptor.NewAuditWriter(l, g, []io.Writer{&a, nil, &b})
		require.NoError(t, st.Close())

		rec := validRecord(t, "rec-nil-elem", nil)
		var err error
		require.NotPanics(t, func() { err = w.Write(rec) })
		require.NoError(t, err)
		assert.NotEmpty(t, a.String())
		assert.NotEmpty(t, b.String())
	})
}

// TestAuditWriterClose verifies Close closes the gap log, is safe on a nil gap
// log, and is idempotent. The ledger is the caller's to close, so Close must
// not touch it.
func TestAuditWriterClose(t *testing.T) {
	t.Run("closes the gap log", func(t *testing.T) {
		dir := t.TempDir()
		g := openGap(t, filepath.Join(dir, "gaps.log"))
		w := interceptor.NewAuditWriter(nil, g, nil)

		require.NoError(t, w.Close())
		// The log is now closed: recording through it must fail.
		require.Error(t, g.Record(gap.Entry{Kind: record.EventAuditGap, CorrelationID: "x", Detail: "y"}),
			"Close must have closed the underlying gap log")
		require.NoError(t, w.Close(), "Close must be idempotent")
	})

	t.Run("nil gap log is safe", func(t *testing.T) {
		w := interceptor.NewAuditWriter(nil, nil, nil)
		require.NoError(t, w.Close())
	})
}
