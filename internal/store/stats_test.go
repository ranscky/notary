package store

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
)

// statsRecord builds a storable record with the given seq, event time and
// reason. Only the columns the aggregate reads vary between rows -- seq, at, and
// the tier the reason carries -- because this fixture exists to be counted
// rather than to be read back. Everything else is fixed and valid so the insert
// succeeds.
func statsRecord(t *testing.T, seq uint64, at time.Time, reason record.Reason) record.Record {
	t.Helper()
	n := strconv.FormatUint(seq, 10)
	rec := record.Record{
		ID:             record.RecordID("stats-" + n),
		Seq:            seq,
		At:             at,
		RecordedAt:     at.Add(time.Minute),
		Event:          record.EventMemorySurfaced,
		Reason:         reason,
		Subject:        record.Subject{MemoryID: "mem-stats", ContentHash: hash32(byte(seq))},
		IdempotencyKey: record.IdemKey("idem-stats-" + n),
		PrevHash:       hash32(byte(seq + 10)),
		Signature:      []byte{0x01, 0x02},
		SignerKeyID:    "key-stats",
	}
	h, err := record.ComputeHash(rec)
	require.NoError(t, err)
	rec.Hash = h
	return rec
}

// TestLedgerStatsCountsSpanAndTiers pins the one read the Overview's facts
// section is built on: how many records, when the ledger starts and ends, and how
// those records split across the three tiers.
//
// The rows are given out-of-order event times on purpose. "Earliest" and
// "latest" mean earliest and latest BY EVENT TIME, not by seq, so the aggregate
// cannot be satisfied by reading the head and the tail of the chain.
func TestLedgerStatsCountsSpanAndTiers(t *testing.T) {
	s := newOpenStore(t)

	observed := observedReason(t, record.ReasonReturnedBySearch, `{"score":0.9}`)
	reconstructed := reconstructedReason(t, record.ReasonKeptByContentMatch)
	note, err := record.NewInternalNote("mem0 removed it and gave no reason")
	require.NoError(t, err)
	internal, err := record.NewInternalReason(record.ReasonRemovedByMem0, note)
	require.NoError(t, err)

	first := time.Date(2026, 5, 2, 9, 14, 0, 0, time.UTC)
	last := time.Date(2026, 10, 21, 18, 2, 0, 0, time.UTC)

	for _, r := range []record.Record{
		statsRecord(t, 0, first, observed),
		statsRecord(t, 1, last, internal), // the newest row is not the last one inserted
		statsRecord(t, 2, first.Add(time.Hour), observed),
		statsRecord(t, 3, last.Add(-time.Hour), reconstructed),
	} {
		require.NoError(t, s.PutRecord(r))
	}

	got, err := s.LedgerStats()
	require.NoError(t, err)

	assert.Equal(t, 4, got.Count, "every stored row is counted")
	assert.True(t, got.Earliest.Equal(first), "earliest is the smallest event time, got %s", got.Earliest)
	assert.True(t, got.Latest.Equal(last), "latest is the largest event time, got %s", got.Latest)
	assert.Equal(t, map[record.VisibilityTier]int{
		record.Observed:      2,
		record.Reconstructed: 1,
		record.Internal:      1,
	}, got.ByTier, "the tier split is by the stored tier column")
}

// TestLedgerStatsOnAnEmptyLedgerIsZeroAndNoError pins the first-run case: an
// empty ledger is not a failure to read, and it must not produce a bound that a
// page could render as "records from year 1".
func TestLedgerStatsOnAnEmptyLedgerIsZeroAndNoError(t *testing.T) {
	s := newOpenStore(t)

	got, err := s.LedgerStats()
	require.NoError(t, err, "an empty ledger is a readable ledger")

	assert.Equal(t, 0, got.Count)
	assert.True(t, got.Earliest.IsZero(), "no earliest record, got %s", got.Earliest)
	assert.True(t, got.Latest.IsZero(), "no latest record, got %s", got.Latest)
	assert.Empty(t, got.ByTier, "no tiers are present in an empty ledger")
}
