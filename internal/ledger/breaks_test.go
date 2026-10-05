package ledger_test

import (
	"crypto/ed25519"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// breaksFixture builds a fresh SQLite-backed ledger holding n valid records and
// returns it together with its store, a verifier trusting its signer, and the
// database path. It mirrors verifyFixture, which the package's other verify
// tests use, and differs only in returning the store: CollectBreaks takes the
// store separately because *Ledger deliberately does not expose its own.
func breaksFixture(t *testing.T, n int) (*ledger.Ledger, store.Store, *sign.Verifier, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	sg, pub := newSigner(t)
	l := ledger.New(st, sg, func() time.Time { return fixedNow })
	for i := 0; i < n; i++ {
		_, err := l.Append(validRecord(t, record.RecordID(fmt.Sprintf("rec-%04d", i+1))))
		require.NoError(t, err)
	}
	v := sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})
	return l, st, v, dbPath
}

// TestCollectBreaksIsCleanOnAnIntactLedger pins the clean answer: an intact
// chain and a gap log that does not exist are no breaks and no error, so every
// caller reads this ledger as intact.
func TestCollectBreaksIsCleanOnAnIntactLedger(t *testing.T) {
	l, st, v, _ := breaksFixture(t, 3)

	breaks, err := ledger.CollectBreaks(l, st, filepath.Join(t.TempDir(), "absent-gaps.log"), v)

	require.NoError(t, err, "an intact ledger must not error")
	assert.Empty(t, breaks, "an intact chain with no gap log must report no breaks")
}

// TestCollectBreaksReportsAnEditedRecord proves the chain walk's breaks reach
// the shared collection unchanged: a record edited out of band is named by its
// exact ID, its chain position, and the field that broke.
func TestCollectBreaksReportsAnEditedRecord(t *testing.T) {
	l, st, v, dbPath := breaksFixture(t, 5)

	execRaw(t, dbPath, `UPDATE records SET event = 'memory_kept' WHERE seq = 3`)

	breaks, err := ledger.CollectBreaks(l, st, filepath.Join(t.TempDir(), "absent-gaps.log"), v)

	require.NoError(t, err)
	require.Len(t, breaks, 1, "editing one record's event must yield exactly one break")
	assert.Equal(t, record.RecordID("rec-0004"), breaks[0].RecordID,
		"the break must name the exact record ID")
	assert.Equal(t, uint64(3), breaks[0].Seq)
	assert.Equal(t, "hash", breaks[0].Field)
}

// TestCollectBreaksReportsAGapEntryMatchingNoRecord is the load-bearing case
// (Review Focus 1): a ledger whose records are all intact, and whose gap log
// holds an entry naming a correlation ID no record accounts for. The chain walk
// alone cannot see it -- asserted below -- so CollectBreaks reporting it is what
// proves the shared collection covers all three of `notary verify`'s checks and
// not merely the first one.
func TestCollectBreaksReportsAGapEntryMatchingNoRecord(t *testing.T) {
	l, st, v, _ := breaksFixture(t, 3)

	gapPath := filepath.Join(t.TempDir(), "gaps.log")
	g, err := gap.Open(gapPath)
	require.NoError(t, err)
	require.NoError(t, g.Record(gap.Entry{
		At:            fixedNow,
		Kind:          record.EventAuditGap,
		Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
		CorrelationID: "rec-missing",
		Detail:        "memory store unreachable",
	}))
	require.NoError(t, g.Close())

	// Every record in the store is intact, so the chain walk on its own finds
	// nothing: this is the case ledger.Verify cannot answer.
	chainBreaks, cerr := l.Verify(v)
	require.NoError(t, cerr)
	require.Empty(t, chainBreaks,
		"the records are intact, so the chain walk must report nothing")

	breaks, err := ledger.CollectBreaks(l, st, gapPath, v)

	require.NoError(t, err)
	require.Len(t, breaks, 1, "a gap entry matching no record must be reported")
	assert.Equal(t, record.RecordID("rec-missing"), breaks[0].RecordID,
		"the break must name the correlation ID no record accounts for")
	assert.Equal(t, "gap", breaks[0].Field)
}
