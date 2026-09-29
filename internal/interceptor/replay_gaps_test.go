package interceptor_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
	"notary/internal/interceptor"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/store"
)

// replayAt is the fixed ledger clock for the replay harness, chosen to sit
// inside the ListRecords window replayList uses.
var replayAt = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// replayList returns every record the ledger holds, over a window wide enough
// to cover the fixture clock.
func replayList(t *testing.T, l *ledger.Ledger) []record.Record {
	t.Helper()
	recs, err := l.ListRecords(replayAt.Add(-48*time.Hour), replayAt.Add(48*time.Hour))
	require.NoError(t, err)
	return recs
}

// startReplayLedger opens a store and a ledger over dbPath, so a test can
// recover over the SAME database file a previous (broken) store used.
func startReplayLedger(t *testing.T, dbPath string) (*ledger.Ledger, *store.SQLiteStore) {
	t.Helper()
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return ledger.New(st, newSigner(t), func() time.Time { return replayAt }), st
}

// newReplayWriter wires a healthy ledger, gap log, and AuditWriter over a fresh
// temp dir. It is the nil-safety harness: real components, no breakage.
func newReplayWriter(t *testing.T) (*ledger.Ledger, *gap.Log, *interceptor.AuditWriter) {
	t.Helper()
	dir := t.TempDir()
	l, _ := startReplayLedger(t, filepath.Join(dir, "ledger.db"))
	g := openGap(t, filepath.Join(dir, "gaps.log"))
	return l, g, interceptor.NewAuditWriter(l, g, nil)
}

// recordGapWhileBroken breaks the store, writes a keyed record for corr (which
// fails open and logs exactly one gap entry), then recovers by opening a FRESH
// ledger and gap log over the SAME database and gap-log files -- the only way
// to reopen a closed store in this codebase. It returns the recovered writer
// and ledger, the original record, and the gap-log path. The recovered store is
// asserted empty: a broken write leaves no record.
func recordGapWhileBroken(t *testing.T, corr record.RecordID) (
	*interceptor.AuditWriter, *ledger.Ledger, record.Record, string) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	gapPath := filepath.Join(dir, "gaps.log")
	sg := newSigner(t)

	st, err := store.Open(dbPath)
	require.NoError(t, err)
	l := ledger.New(st, sg, func() time.Time { return replayAt })
	g := openGap(t, gapPath)
	w := interceptor.NewAuditWriter(l, g, nil)

	rec := validRecord(t, corr, nil)
	rec.IdempotencyKey = record.IdemKey("idem-" + string(corr))

	require.NoError(t, st.Close(), "break the store")
	require.NoError(t, w.Write(rec), "a broken ledger must fail open")
	require.NoError(t, g.Close())

	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the broken write logs exactly one gap entry")
	require.Equal(t, string(corr), entries[0].CorrelationID)

	l2, _ := startReplayLedger(t, dbPath)
	g2 := openGap(t, gapPath)
	w2 := interceptor.NewAuditWriter(l2, g2, nil)
	require.Empty(t, replayList(t, l2), "the broken write stored no record")
	return w2, l2, rec, gapPath
}

// TestReplayGapsReconcilesAfterRecovery is Task 19's core contract and the
// assertion deferred from Task 14 (Review Focus #4, matched case): a gap
// recorded while the store was broken is replayed into the ledger once the
// store recovers, with its ORIGINAL idempotency key, and ledger.GapBreaks then
// reports NO break for it. A second run is a no-op.
func TestReplayGapsReconcilesAfterRecovery(t *testing.T) {
	const corr = record.RecordID("corr-replay-1")
	w, l, orig, gapPath := recordGapWhileBroken(t, corr)

	lookup := func(c string) (record.Record, bool) {
		if c == string(corr) {
			return orig, true
		}
		return record.Record{}, false
	}

	n, err := w.ReplayGaps(context.Background(), lookup)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "exactly one record is reconciled")

	// Exactly one record, carrying the ORIGINAL idempotency key.
	stored, err := l.GetRecord(corr)
	require.NoError(t, err)
	assert.Equal(t, orig.IdempotencyKey, stored.IdempotencyKey,
		"the replayed record must carry its ORIGINAL idempotency key")
	assert.Len(t, replayList(t, l), 1, "exactly one record is stored")

	// The gap log gained a matching marker, and its own chain still validates.
	after, err := gap.Read(gapPath)
	require.NoError(t, err)
	require.Len(t, after, 2, "a reconciliation marker is appended")
	assert.Equal(t, string(corr), after[1].CorrelationID,
		"the marker keeps the original correlation ID")
	assert.Equal(t, after[0].Kind, after[1].Kind, "the marker keeps the original Kind")
	assert.Equal(t, after[0].Scope, after[1].Scope, "the marker keeps the original Scope")
	assert.Contains(t, after[1].Detail, "reconciled")
	assert.Contains(t, after[1].Detail, string(corr), "the marker names the reconciled record")

	gapBreaks, err := gap.Verify(gapPath)
	require.NoError(t, err)
	assert.Empty(t, gapBreaks, "the gap log's own chain stays valid")

	// DEFERRED (Task 14, Review Focus #4, matched case): the gap entry is now
	// accounted for, so the cross-check `notary verify` runs reports no break.
	assert.Empty(t, ledger.GapBreaks(after, []record.Record{stored}),
		"a reconciled gap entry must no longer surface as a gap break")

	// Idempotency: a second run reconciles nothing and appends nothing.
	n2, err := w.ReplayGaps(context.Background(), lookup)
	require.NoError(t, err)
	assert.Equal(t, 0, n2, "a second run reconciles nothing")

	finalEntries, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Len(t, finalEntries, 2, "a second run appends no gap entry")
	assert.Len(t, replayList(t, l), 1, "a second run stores no new record")
}

// TestReplayGapsLookupMissLeavesGapUnreconciled pins the miss contract: a lookup
// that cannot produce the record leaves the entry unreconciled, stores no
// record, appends no marker, and is NOT an error (a miss is "nothing to
// reconcile", not "reconciliation failed").
func TestReplayGapsLookupMissLeavesGapUnreconciled(t *testing.T) {
	const corr = record.RecordID("corr-miss-1")
	w, l, _, gapPath := recordGapWhileBroken(t, corr)

	lookup := func(string) (record.Record, bool) { return record.Record{}, false }

	n, err := w.ReplayGaps(context.Background(), lookup)
	require.NoError(t, err, "a lookup miss is not an error")
	assert.Equal(t, 0, n, "a miss is not counted as reconciled")
	assert.Empty(t, replayList(t, l), "a miss stores no record")

	after, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Len(t, after, 1, "a miss appends no marker")
}

// TestReplayGapsHonoursContextCancellation pins that a cancelled context stops
// the run before it reconciles anything and surfaces the wrapped error.
func TestReplayGapsHonoursContextCancellation(t *testing.T) {
	const corr = record.RecordID("corr-cancel-1")
	w, l, orig, gapPath := recordGapWhileBroken(t, corr)

	lookup := func(string) (record.Record, bool) { return orig, true }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	n, err := w.ReplayGaps(ctx, lookup)
	require.Error(t, err, "a cancelled context is surfaced, not swallowed")
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, n, "nothing is reconciled under a cancelled context")
	assert.Empty(t, replayList(t, l), "a cancelled run stores no record")

	after, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Len(t, after, 1, "a cancelled run appends no marker")
}

// TestReplayGapsNilSafety pins the documented meaning of a missing component:
// a nil gap log, nil ledger, or nil lookup makes reconciliation impossible, so
// each is reported as an error rather than a silent no-op -- and none panics.
func TestReplayGapsNilSafety(t *testing.T) {
	noLookup := func(string) (record.Record, bool) { return record.Record{}, false }

	t.Run("nil gap log is an error", func(t *testing.T) {
		l, _, _ := newReplayWriter(t)
		w := interceptor.NewAuditWriter(l, nil, nil)
		var n int
		var err error
		require.NotPanics(t, func() { n, err = w.ReplayGaps(context.Background(), noLookup) })
		require.Error(t, err)
		assert.Equal(t, 0, n)
	})

	t.Run("nil ledger is an error", func(t *testing.T) {
		_, g, _ := newReplayWriter(t)
		w := interceptor.NewAuditWriter(nil, g, nil)
		var n int
		var err error
		require.NotPanics(t, func() { n, err = w.ReplayGaps(context.Background(), noLookup) })
		require.Error(t, err)
		assert.Equal(t, 0, n)
	})

	t.Run("nil lookup is an error", func(t *testing.T) {
		_, _, w := newReplayWriter(t)
		var n int
		var err error
		require.NotPanics(t, func() { n, err = w.ReplayGaps(context.Background(), nil) })
		require.Error(t, err)
		assert.Equal(t, 0, n)
	})
}

// TestReplayGapsAppendFailureWritesNoMarker forces the append-failure branch --
// the one earlier reported as unreachable without a test seam.
//
// That claim conflated a broken STORE with a failed APPEND. Append also fails
// VALIDATION, which a healthy store exercises perfectly well, so a lookup
// returning a record the ledger will refuse forces the branch with the store
// fully working.
//
// The contract under test is "the marker must not lie": if the record could not
// be stored, the run must report the error, count nothing, and append no
// reconciliation marker -- because a marker claiming reconciliation that never
// happened would be worse than no marker at all.
func TestReplayGapsAppendFailureWritesNoMarker(t *testing.T) {
	const corr = record.RecordID("corr-append-fail")
	w, l, _, gapPath := recordGapWhileBroken(t, corr)

	before, err := gap.Read(gapPath)
	require.NoError(t, err)
	require.Len(t, before, 1, "the broken write logged exactly one gap entry")

	// A record the ledger will refuse: Validate rejects an unknown event, so the
	// append fails without the store being broken at all.
	bad := record.Record{ID: corr, Event: record.EventType("not-a-real-event")}
	lookup := func(string) (record.Record, bool) { return bad, true }

	n, err := w.ReplayGaps(context.Background(), lookup)
	require.Error(t, err, "an append failure must be surfaced, not swallowed away")
	assert.Equal(t, 0, n, "a failed append reconciles nothing")
	assert.Contains(t, err.Error(), string(corr), "the error must name the offending record")

	assert.Empty(t, replayList(t, l), "no record may be stored when the append failed")

	after, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Len(t, after, 1,
		"no reconciliation marker may be appended when the append failed")

	breaks, err := gap.Verify(gapPath)
	require.NoError(t, err)
	assert.Empty(t, breaks, "the gap log's own chain must stay intact")
}
