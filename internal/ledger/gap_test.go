package ledger

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
)

// TestGapIntegrityBreaksMapsCounterFieldAndLine pins the mapping
// internal/ledger performs on gap.Verify's breaks: the gap Counter becomes
// Seq (not a ledger sequence), the gap Field is carried across, and the 1-based
// Line is folded into Detail so an operator can find the break in the file.
func TestGapIntegrityBreaksMapsCounterFieldAndLine(t *testing.T) {
	in := []gap.Break{
		{Line: 2, Counter: 1, Field: "chain", Detail: "prev_hash aa does not match predecessor hash bb"},
	}

	out := GapIntegrityBreaks(in)

	require.Len(t, out, 1)
	assert.Empty(t, out[0].RecordID, "a gap-integrity break identifies no ledger record")
	assert.Equal(t, uint64(1), out[0].Seq, "the gap counter goes in Seq")
	assert.Equal(t, "chain", out[0].Field, "the gap field is carried across unchanged")
	assert.Contains(t, out[0].Detail, "gap log line 2", "the 1-based line number must be folded into Detail")
	assert.Contains(t, out[0].Detail, "prev_hash", "the original detail must be preserved")
}

// TestGapIntegrityBreaksTruncationOmitsLineNumber pins the truncation case: a
// break whose Line is 0 -- a truncation removes a line, so it has no position in
// the surviving file -- must not render a nonsense "gap log line 0" prefix,
// while a real break carrying a genuine 1-based line still reports it. This is
// the auditor-facing text; "gap log line 0" reads as nonsense.
func TestGapIntegrityBreaksTruncationOmitsLineNumber(t *testing.T) {
	in := []gap.Break{
		{Line: 0, Counter: 3, Field: "truncation",
			Detail: "checkpoint attests to counter 3, but the gap log is now empty or missing"},
		{Line: 2, Counter: 1, Field: "chain",
			Detail: "prev_hash aa does not match predecessor hash bb"},
	}

	out := GapIntegrityBreaks(in)

	require.Len(t, out, 2)
	assert.NotContains(t, out[0].Detail, "line 0",
		"a truncation break has no file line and must not claim one")
	assert.Contains(t, out[0].Detail, "checkpoint attests to counter 3",
		"the original truncation detail must be preserved")
	assert.Equal(t, uint64(3), out[0].Seq, "the checkpoint counter still goes in Seq")
	assert.Equal(t, "truncation", out[0].Field, "the truncation field is carried across unchanged")

	assert.Contains(t, out[1].Detail, "gap log line 2",
		"a real break with a genuine line must still report it")
	assert.Contains(t, out[1].Detail, "prev_hash",
		"the original detail must be preserved")
}

// TestGapIntegrityBreaksEmpty proves the helper is a pure, no-op mapping for an
// empty input, so a clean or missing gap log adds no breaks.
func TestGapIntegrityBreaksEmpty(t *testing.T) {
	assert.Empty(t, GapIntegrityBreaks(nil))
}
