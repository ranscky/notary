package ledger_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
	"notary/internal/ledger"
	"notary/internal/record"
)

// writeGapEntries writes entries to a gap log at path, in order, using the same
// gap.Open + Record path the package's other gap fixtures use, and closes it
// before returning. The caller never supplies chain fields: Record assigns
// Counter, PrevHash, and Hash.
func writeGapEntries(t *testing.T, path string, entries ...gap.Entry) {
	t.Helper()
	g, err := gap.Open(path)
	require.NoError(t, err)
	for _, e := range entries {
		require.NoError(t, g.Record(e))
	}
	require.NoError(t, g.Close())
}

// TestOutstandingGapsIsEmptyOnACleanLedger pins the clean answer: no records, no
// gap log, so nothing is unreconciled, the log has no integrity breaks, and the
// call does not error. A missing gap log is an empty log, not an error.
func TestOutstandingGapsIsEmptyOnACleanLedger(t *testing.T) {
	_, st, _, _ := breaksFixture(t, 0)

	report, err := ledger.OutstandingGaps(st, filepath.Join(t.TempDir(), "absent-gaps.log"))

	require.NoError(t, err, "a clean ledger with no gap log must not error")
	assert.Empty(t, report.Unreconciled, "a clean ledger has nothing unreconciled")
	assert.Empty(t, report.Integrity, "a missing gap log has no integrity breaks")
}

// TestOutstandingGapsIgnoresAMatchingEntry covers the healthy logged case: a gap
// entry whose (Kind, Scope, CorrelationID) a stored record accounts for is not
// outstanding, and a well-formed log has no integrity breaks.
func TestOutstandingGapsIgnoresAMatchingEntry(t *testing.T) {
	_, st, _, _ := breaksFixture(t, 2)

	gapPath := filepath.Join(t.TempDir(), "gaps.log")
	writeGapEntries(t, gapPath, gap.Entry{
		At:            fixedNow,
		Kind:          record.EventMemorySurfaced,
		Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
		CorrelationID: "rec-0001",
		Detail:        "reconciled",
	})

	report, err := ledger.OutstandingGaps(st, gapPath)

	require.NoError(t, err)
	assert.Empty(t, report.Unreconciled,
		"a gap entry a stored record accounts for must not be outstanding")
	assert.Empty(t, report.Integrity, "a well-formed gap log has no integrity breaks")
}

// TestOutstandingGapsReportsAnUnmatchedEntry is the load-bearing case (Review
// Focus 1): a ledger whose records are all intact, and whose gap log holds an
// entry naming a correlation ID no record accounts for. The chain walk alone
// cannot see it -- asserted below -- so OutstandingGaps reporting it is what
// proves the report covers the gap cross-check and not merely the chain walk.
func TestOutstandingGapsReportsAnUnmatchedEntry(t *testing.T) {
	l, st, v, _ := breaksFixture(t, 3)

	gapPath := filepath.Join(t.TempDir(), "gaps.log")
	// Same kind and scope as the stored records, so the ONLY thing keeping this
	// entry from being accounted for is its correlation ID.
	writeGapEntries(t, gapPath, gap.Entry{
		At:            fixedNow,
		Kind:          record.EventMemorySurfaced,
		Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
		CorrelationID: "rec-missing",
		Detail:        "memory store unreachable",
	})

	// Every record in the store is intact, so the chain walk on its own finds
	// nothing: this is the case ledger.Verify cannot answer.
	chainBreaks, cerr := l.Verify(v)
	require.NoError(t, cerr)
	require.Empty(t, chainBreaks, "the records are intact, so the chain walk must report nothing")

	report, err := ledger.OutstandingGaps(st, gapPath)

	require.NoError(t, err)
	require.Len(t, report.Unreconciled, 1, "a gap entry matching no record must be outstanding")
	assert.Equal(t, uint64(0), report.Unreconciled[0].Counter,
		"the report must carry the entry's own chain position")
	assert.Equal(t, "rec-missing", report.Unreconciled[0].CorrelationID,
		"the report must carry the entry's correlation ID")
	assert.Empty(t, report.Integrity, "the gap log itself is well-formed")
}

// TestOutstandingGapsReportsACorruptLog proves the report carries the gap log's
// own integrity breaks: gap.Read silently skips a line it cannot decode, so a
// rewritten line could otherwise look healthy. Here the line is rewritten so it
// still decodes and still matches a stored record -- so nothing is unreconciled
// -- but its stored hash no longer matches, which gap.Verify must surface.
func TestOutstandingGapsReportsACorruptLog(t *testing.T) {
	_, st, _, _ := breaksFixture(t, 1)

	gapPath := filepath.Join(t.TempDir(), "gaps.log")
	writeGapEntries(t, gapPath, gap.Entry{
		At:            fixedNow,
		Kind:          record.EventMemorySurfaced,
		Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
		CorrelationID: "rec-0001",
		Detail:        "ok",
	})

	// Rewrite the entry's Detail without recomputing its hash: the line still
	// decodes, so gap.Read reads it back, but its stored hash no longer matches
	// -- exactly the kind of rewrite the cross-check cannot see.
	raw, err := os.ReadFile(gapPath)
	require.NoError(t, err)
	tampered := bytes.Replace(raw, []byte(`"detail":"ok"`), []byte(`"detail":"tampered"`), 1)
	require.NotEqual(t, raw, tampered, "the fixture detail must appear verbatim for the rewrite to bite")
	require.NoError(t, os.WriteFile(gapPath, tampered, 0o600))

	report, err := ledger.OutstandingGaps(st, gapPath)

	require.NoError(t, err)
	assert.Empty(t, report.Unreconciled,
		"the entry still matches a stored record, so nothing is outstanding")
	require.Len(t, report.Integrity, 1, "a rewritten gap-log line must surface as one integrity break")
	assert.Equal(t, "hash", report.Integrity[0].Field, "the break must name the hash")
}
