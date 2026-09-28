package ledger_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
)

// TestConcurrentAppendsSerialize proves appends through the single write path
// serialize rather than fail. With N goroutines appending at once, every append
// must succeed, every chain position must be distinct, and the positions must be
// exactly 0..N-1 -- contiguous, with no gaps and no duplicates.
//
// This is the property spec section 7 states for a ledger file ("one writer at a
// time, serialized by SQLite"). A busy timeout is what delivers it: with a zero
// busy timeout a losing writer returns SQLITE_BUSY immediately and the append is
// dropped, i.e. an audit gap. The test fails loudly on any error rather than
// tallying, so a regression in serialization is a failure, not a number in a log.
func TestConcurrentAppendsSerialize(t *testing.T) {
	l, _, _, st := newLedger(t)

	const n = 12

	// Build the records here: validRecord calls require, which must not run on a
	// non-test goroutine.
	recs := make([]record.Record, n)
	for i := 0; i < n; i++ {
		recs[i] = validRecord(t, record.RecordID(fmt.Sprintf("rec-%02d", i)))
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

	// Every append must succeed. A failure here is a dropped audit record.
	for i, err := range errs {
		require.NoErrorf(t, err, "concurrent append %d must succeed, not be dropped", i)
	}

	// Every chain position must be distinct.
	seen := make(map[uint64]bool, n)
	for i, id := range ids {
		stored, err := l.GetRecord(id)
		require.NoErrorf(t, err, "reading appended record %d", i)
		require.Falsef(t, seen[stored.Seq], "chain position %d was assigned twice", stored.Seq)
		seen[stored.Seq] = true
	}

	// The positions must be exactly 0..N-1: contiguous, no gaps.
	for seq := uint64(0); seq < uint64(n); seq++ {
		assert.Truef(t, seen[seq], "chain position %d is missing", seq)
	}
	assert.Len(t, seen, n)
	assert.Equal(t, n, countRecords(t, st))
	t.Logf("all %d concurrent appends succeeded with distinct, contiguous Seq 0..%d", n, n-1)
}
