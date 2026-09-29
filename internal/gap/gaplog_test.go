package gap_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
	"notary/internal/ledger"
	"notary/internal/record"
)

// gapTestTime carries sub-second precision so hashing a whole-second instant
// and a fractional one are both exercised, and so the fixed-width instant
// layout is actually tested.
var gapTestTime = time.Date(2026, 9, 28, 12, 0, 0, 123456789, time.UTC)

// gapEntry builds a well-formed gap entry for corr with the given user and
// agent, shared by every test so the fixtures stay identical except where a
// test deliberately varies them.
func gapEntry(corr, userID, agentID string) gap.Entry {
	return gap.Entry{
		At:            gapTestTime,
		Kind:          record.EventAuditGap,
		Scope:         record.Scope{UserID: userID, AgentID: agentID},
		CorrelationID: corr,
		Detail:        "memory store unreachable",
	}
}

// recordFirstHash records e as the first entry of a fresh log at path, closes
// it, and returns the assigned hash.
func recordFirstHash(t *testing.T, path string, e gap.Entry) record.Hash {
	t.Helper()
	g, err := gap.Open(path)
	require.NoError(t, err)
	require.NoError(t, g.Record(e))
	require.NoError(t, g.Close())

	entries, err := gap.Read(path)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	return entries[0].Hash
}

// writeThree records counters 0, 1, 2 into a fresh log and returns its path.
func writeThree(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(path)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		require.NoError(t, g.Record(gapEntry(fmt.Sprintf("c%d", i), "u", "a")))
	}
	require.NoError(t, g.Close())
	return path
}

// readLines returns the file's non-empty, newline-split lines.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// writeLines writes lines back, newline-terminated.
func writeLines(t *testing.T, path string, lines []string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

// assertHasField requires that breaks contains at least one break naming field.
func assertHasField(t *testing.T, breaks []gap.Break, field string) {
	t.Helper()
	for _, b := range breaks {
		if b.Field == field {
			return
		}
	}
	require.Failf(t, "missing break field", "no break with Field %q in %+v", field, breaks)
}

// TestRecordBuildsChain locks the core chain contract: three appends produce
// counters 0, 1, 2, each PrevHash equals the previous Hash, the first is the
// all-zero genesis hash, and Verify on a clean log is silent.
func TestRecordBuildsChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(path)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		require.NoError(t, g.Record(gapEntry(fmt.Sprintf("c%d", i), "u", "a")))
	}
	require.NoError(t, g.Close())

	entries, err := gap.Read(path)
	require.NoError(t, err)
	require.Len(t, entries, 3)

	var zero record.Hash
	for i, e := range entries {
		assert.Equal(t, uint64(i), e.Counter, "entry %d counter", i)
	}
	assert.Equal(t, zero, entries[0].PrevHash, "the first entry links to the all-zero genesis hash")
	assert.Equal(t, entries[0].Hash, entries[1].PrevHash)
	assert.Equal(t, entries[1].Hash, entries[2].PrevHash)

	breaks, err := g.Verify()
	require.NoError(t, err)
	assert.Empty(t, breaks, "a clean log has no breaks")
}

// TestVerifyDetectsDeletedMiddleLine proves a gap cannot be silently erased:
// deleting the middle line of three leaves counters 0 and 2, whose broken link
// surfaces as a "chain" break. The chain check -- not the counter check -- is
// what must catch it.
func TestVerifyDetectsDeletedMiddleLine(t *testing.T) {
	path := writeThree(t)
	lines := readLines(t, path)
	require.Len(t, lines, 3)

	kept := append([]string{lines[0]}, lines[2:]...)
	writeLines(t, path, kept)

	breaks, err := gap.Verify(path)
	require.NoError(t, err)
	assertHasField(t, breaks, "chain")
}

// TestVerifyDetectsEditedDetail proves editing an entry's payload breaks its
// digest: changing Detail leaves the stored Hash stale, reported as "hash".
func TestVerifyDetectsEditedDetail(t *testing.T) {
	path := writeThree(t)
	lines := readLines(t, path)

	edited := strings.Replace(lines[1], "memory store unreachable", "tampered detail", 1)
	require.NotEqual(t, lines[1], edited, "fixture must actually change")
	lines[1] = edited
	writeLines(t, path, lines)

	breaks, err := gap.Verify(path)
	require.NoError(t, err)
	assertHasField(t, breaks, "hash")
}

// TestVerifyDetectsReordering proves swapping two lines is caught: the moved
// entry no longer links to its predecessor, reported as "chain".
func TestVerifyDetectsReordering(t *testing.T) {
	path := writeThree(t)
	lines := readLines(t, path)
	require.Len(t, lines, 3)

	lines[0], lines[1] = lines[1], lines[0]
	writeLines(t, path, lines)

	breaks, err := gap.Verify(path)
	require.NoError(t, err)
	assertHasField(t, breaks, "chain")
}

// TestVerifyCannotDetectDeletedFinalLine documents a real limitation rather
// than hiding it: truncating the tail leaves a shorter but still self-consistent
// chain, which a hash chain cannot see. Detecting it needs an external signed
// checkpoint, which the gap log does not have in v1. We assert the absence of a
// break so the limitation is explicit.
func TestVerifyCannotDetectDeletedFinalLine(t *testing.T) {
	path := writeThree(t)
	lines := readLines(t, path)

	writeLines(t, path, lines[:2])

	breaks, err := gap.Verify(path)
	require.NoError(t, err)
	assert.Empty(t, breaks, "a hash chain cannot detect its own removed tail without a checkpoint")
}

// TestOpenContinuesCounter proves reopening an existing log continues the chain
// rather than restarting at 0, and keeps the reopened log verifiable.
func TestOpenContinuesCounter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(path)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		require.NoError(t, g.Record(gapEntry(fmt.Sprintf("c%d", i), "u", "a")))
	}
	require.NoError(t, g.Close())

	g2, err := gap.Open(path)
	require.NoError(t, err)
	require.NoError(t, g2.Record(gapEntry("c2", "u", "a")))
	require.NoError(t, g2.Close())

	entries, err := gap.Read(path)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, uint64(2), entries[2].Counter, "the reopened log continues from the last counter")
	assert.Equal(t, entries[1].Hash, entries[2].PrevHash, "the new entry links to the recovered head")

	breaks, err := gap.Verify(path)
	require.NoError(t, err)
	assert.Empty(t, breaks)
}

// TestVerifyReportsTornTail covers the crash-mid-write hazard: a partial final
// line must be reported as a "decode" break, and reopening must not treat it as
// the last good entry (which would resurrect a bogus counter).
func TestVerifyReportsTornTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gaps.log")
	g, err := gap.Open(path)
	require.NoError(t, err)
	require.NoError(t, g.Record(gapEntry("c0", "u", "a")))
	require.NoError(t, g.Close())

	// Simulate a crash mid-write: a partial JSON fragment with no newline.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"counter":999,"detail":"torn`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	breaks, err := gap.Verify(path)
	require.NoError(t, err)
	require.Len(t, breaks, 1)
	assert.Equal(t, "decode", breaks[0].Field)
	assert.Equal(t, 2, breaks[0].Line)

	// Reopen: the torn tail must not resurrect counter 999; the next entry is
	// counter 1, chained to the last good entry.
	g2, err := gap.Open(path)
	require.NoError(t, err)
	require.NoError(t, g2.Record(gapEntry("c1", "u", "a")))
	require.NoError(t, g2.Close())

	entries, err := gap.Read(path)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, uint64(0), entries[0].Counter)
	assert.Equal(t, uint64(1), entries[1].Counter, "counter continues from the last good entry, not the torn one")
	assert.Equal(t, entries[0].Hash, entries[1].PrevHash)
}

// TestOpenAdoptsCompleteTornTail covers the crash-at-boundary case: a write
// that finished every byte of an entry but lost only its terminating newline.
// The fragment is a complete JSON entry with a correct hash and link, so it must
// be adopted as the head -- not just excluded -- or the next append would reuse
// its counter and manufacture a spurious break.
func TestOpenAdoptsCompleteTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(path)
	require.NoError(t, err)
	require.NoError(t, g.Record(gapEntry("c0", "u", "a")))
	require.NoError(t, g.Record(gapEntry("c1", "u", "a")))
	require.NoError(t, g.Close())

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(string(data), "\n"))
	// Drop the final newline: the last entry is now a complete JSON object
	// whose terminator was lost.
	require.NoError(t, os.WriteFile(path, data[:len(data)-1], 0o600))

	g2, err := gap.Open(path)
	require.NoError(t, err)
	require.NoError(t, g2.Record(gapEntry("c2", "u", "a")))
	require.NoError(t, g2.Close())

	entries, err := gap.Read(path)
	require.NoError(t, err)
	require.Len(t, entries, 3)
	assert.Equal(t, uint64(2), entries[2].Counter, "the chain continues past the recovered tail")

	breaks, err := gap.Verify(path)
	require.NoError(t, err)
	assert.Empty(t, breaks, "a recovered complete tail must not produce a spurious break")
}

// TestConcurrentRecordAssignsDistinctDenseCounters is the concurrency guard:
// many goroutines appending at once must produce exactly counters 0..N-1 with
// no duplicates or lost writes, and a clean Verify. Run under -race.
func TestConcurrentRecordAssignsDistinctDenseCounters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	const n = 32
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := g.Record(gapEntry(fmt.Sprintf("c%d", i), "u", "a")); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	entries, err := gap.Read(path)
	require.NoError(t, err)
	require.Len(t, entries, n, "every concurrent append must survive")

	got := make([]uint64, 0, n)
	for _, e := range entries {
		got = append(got, e.Counter)
	}
	sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
	for i := 0; i < n; i++ {
		require.Equal(t, uint64(i), got[i], "counters must be exactly 0..N-1 with no duplicates")
	}

	breaks, err := g.Verify()
	require.NoError(t, err)
	assert.Empty(t, breaks)
}

// TestHashIsUnambiguousUnderNaiveConcatenation is the hash-ambiguity guard: two
// entries that differ only in where the UserID/AgentID boundary falls collide
// under naive concatenation, so the real hash must distinguish them.
func TestHashIsUnambiguousUnderNaiveConcatenation(t *testing.T) {
	a := gapEntry("corr", "ab", "c")
	b := gapEntry("corr", "a", "bc")

	// Premise: the naive encoding (no length prefixes) collides.
	require.Equal(t, naiveConcat(a), naiveConcat(b),
		"premise: naive concatenation of Scope fields collides")

	dir := t.TempDir()
	ha := recordFirstHash(t, filepath.Join(dir, "a.log"), a)
	hb := recordFirstHash(t, filepath.Join(dir, "b.log"), b)
	require.NotEqual(t, ha, hb, "the length-prefixed hash must distinguish ab|c from a|bc")
}

// naiveConcat reproduces the ambiguous encoding the real hash must not use:
// fields concatenated raw, with no length prefixes. It exists only so the test
// can prove its collision premise.
func naiveConcat(e gap.Entry) string {
	var b bytes.Buffer
	b.WriteString("notary/gap/v1")
	var c [8]byte
	binary.BigEndian.PutUint64(c[:], e.Counter)
	b.Write(c[:])
	b.WriteString(e.At.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"))
	b.WriteString(string(e.Kind))
	b.WriteString(e.Scope.UserID)
	b.WriteString(e.Scope.AgentID)
	b.WriteString(e.Scope.AppID)
	b.WriteString(e.Scope.RunID)
	b.WriteString(e.CorrelationID)
	b.WriteString(e.Detail)
	b.Write(e.PrevHash[:])
	return b.String()
}

// TestGapBreaksReportsUnmatchedGap covers Step 5's unmatched case: a gap entry
// whose (Kind, Scope, CorrelationID) matches no stored record must surface as a
// "gap" break.
//
// NOTE: the matched case cannot be exercised until Task 15 gives gaps a
// corresponding record, so it is deferred to Task 19.
func TestGapBreaksReportsUnmatchedGap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(path)
	require.NoError(t, err)
	require.NoError(t, g.Record(gapEntry("missing-corr", "u", "a")))
	require.NoError(t, g.Close())

	entries, err := gap.Read(path)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	records := []record.Record{{
		ID:      "some-other-record",
		Event:   record.EventMemorySurfaced,
		Subject: record.Subject{Scope: record.Scope{UserID: "u", AgentID: "a"}},
	}}

	breaks := ledger.GapBreaks(entries, records)
	require.Len(t, breaks, 1)
	assert.Equal(t, "gap", breaks[0].Field)
	assert.Equal(t, "missing-corr", string(breaks[0].RecordID))
}
