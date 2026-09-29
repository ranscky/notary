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

// TestGapIntegrityBreaksEmpty proves the helper is a pure, no-op mapping for an
// empty input, so a clean or missing gap log adds no breaks.
func TestGapIntegrityBreaksEmpty(t *testing.T) {
	assert.Empty(t, GapIntegrityBreaks(nil))
}
