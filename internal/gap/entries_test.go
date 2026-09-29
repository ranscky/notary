package gap_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
)

// TestLogEntriesReadsChainInFileOrder pins the additive reader added for Task
// 19: (*gap.Log).Entries returns every decodable entry in the OPEN log, in
// file order, with exactly the semantics of the package-level Read -- so a
// caller that holds only a *gap.Log (and not its unexported path) can read it
// back.
func TestLogEntriesReadsChainInFileOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	for i := 0; i < 3; i++ {
		require.NoError(t, g.Record(gapEntry(fmt.Sprintf("c%d", i), "u", "a")))
	}

	got, err := g.Entries()
	require.NoError(t, err)
	require.Len(t, got, 3)

	// Identical to the package-level Read over the same path, same order.
	want, err := gap.Read(path)
	require.NoError(t, err)
	assert.Equal(t, want, got, "Entries must match the package-level Read")

	for i, e := range got {
		assert.Equal(t, uint64(i), e.Counter, "file order is chain order")
		assert.Equal(t, fmt.Sprintf("c%d", i), e.CorrelationID)
	}
}

// TestLogEntriesEmptyLog pins that a fresh (empty) log yields no entries and a
// nil error, mirroring Read.
func TestLogEntriesEmptyLog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })

	got, err := g.Entries()
	require.NoError(t, err)
	assert.Empty(t, got)
}

// TestLogEntriesSkipsUndecodableLines pins that an undecodable line is skipped
// rather than fatal -- Verify is what reports it -- so Entries keeps Read's
// semantics.
func TestLogEntriesSkipsUndecodableLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(path)
	require.NoError(t, err)
	require.NoError(t, g.Record(gapEntry("c0", "u", "a")))
	require.NoError(t, g.Close())

	// Inject a non-JSON line directly, then reopen the log.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("not json\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	g2, err := gap.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g2.Close() })

	got, err := g2.Entries()
	require.NoError(t, err)
	require.Len(t, got, 1, "the undecodable line is skipped")
	assert.Equal(t, "c0", got[0].CorrelationID)
}
