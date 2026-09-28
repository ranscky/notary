package ledger_test

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// truncFixture builds a fresh SQLite-backed ledger with n appended records, the
// signer that signed them, a verifier trusting that signer, and the database
// path so a test can tamper with the raw file out of band. It is the checkpoint
// analogue of verifyFixture, which does not expose the signer.
func truncFixture(t *testing.T, n int) (*ledger.Ledger, *sign.Signer, *sign.Verifier, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	st, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	sg, pub := newSigner(t)
	l := ledger.New(st, sg, func() time.Time { return fixedNow })
	for i := 0; i < n; i++ {
		_, err := l.Append(validRecord(t, record.RecordID(fmt.Sprintf("rec-%04d", i+1))))
		require.NoError(t, err)
	}
	v := sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})
	return l, sg, v, path
}

// truncationBreak reports the single truncation break VerifyAgainstCheckpoint
// returned, failing the test when the result is not exactly one truncation
// break.
func truncationBreak(t *testing.T, breaks []ledger.Break) ledger.Break {
	t.Helper()
	require.Len(t, breaks, 1, "a truncated chain must yield exactly one break")
	require.Equal(t, "truncation", breaks[0].Field, "the break must name the truncation field")
	return breaks[0]
}

// TestTruncationIsOnlyCaughtByCheckpoint is the core case (Review Focus #2):
// deleting the tail of a hash chain leaves a perfectly self-consistent
// remainder, so plain Verify reports success. Only a signed head checkpoint can
// tell that the incriminating tail was removed -- the single most dangerous
// tamper for this product.
func TestTruncationIsOnlyCaughtByCheckpoint(t *testing.T) {
	l, sg, v, path := truncFixture(t, 6)

	cp, err := l.Checkpoint(sg, fixedNow)
	require.NoError(t, err)
	require.Equal(t, uint64(5), cp.Seq, "the checkpoint must attest to the head, seq 5")

	// Remove the tail: records at seq 4 and 5 (the last two) simply vanish.
	execRaw(t, path, `DELETE FROM records WHERE seq >= 4`)

	// (a) The hazard: the remainder is self-consistent, so plain Verify is
	// clean. This is exactly why a hash chain alone cannot detect truncation.
	breaks, err := l.Verify(v)
	require.NoError(t, err)
	assert.Empty(t, breaks, "plain Verify cannot see a removed tail: it must stay clean")

	// (b) The checkpoint catches it.
	trunc, terr := l.VerifyAgainstCheckpoint(cp, v)
	require.Error(t, terr, "a shortened chain must be reported as an error")
	assert.ErrorIs(t, terr, ledger.ErrTruncated, "the error must wrap ErrTruncated")
	b := truncationBreak(t, trunc)
	assert.Contains(t, b.Detail, "seq", "the detail must explain which rule fired")
}

// TestTruncationToNothingIsStillTruncation proves that deleting every record
// reports truncation rather than a vacuous "ok: 0 records": an empty store must
// not look like an untampered ledger when a checkpoint attests to a non-empty
// history.
func TestTruncationToNothingIsStillTruncation(t *testing.T) {
	l, sg, v, path := truncFixture(t, 6)

	cp, err := l.Checkpoint(sg, fixedNow)
	require.NoError(t, err)

	execRaw(t, path, `DELETE FROM records`)

	// Plain Verify on an empty store reports no breaks -- so "ok: 0 records"
	// is precisely the misleading reading the checkpoint must override.
	breaks, err := l.Verify(v)
	require.NoError(t, err)
	assert.Empty(t, breaks)

	trunc, terr := l.VerifyAgainstCheckpoint(cp, v)
	require.Error(t, terr)
	assert.ErrorIs(t, terr, ledger.ErrTruncated, "an emptied ledger must still be reported as truncated")
	b := truncationBreak(t, trunc)
	assert.Contains(t, b.Detail, "empty", "the detail must name the empty-store rule")
}

// TestAppendingAfterCheckpointStaysClean is the property most likely to be got
// wrong: a checkpoint is a lower bound on history, not a fixed head. Appending
// records after the checkpoint was taken must leave VerifyAgainstCheckpoint
// clean, because the chain has only grown, not been shortened.
func TestAppendingAfterCheckpointStaysClean(t *testing.T) {
	l, sg, v, _ := truncFixture(t, 3)

	cp, err := l.Checkpoint(sg, fixedNow)
	require.NoError(t, err)
	require.Equal(t, uint64(2), cp.Seq)

	// The store advances past the checkpoint.
	_, err = l.Append(validRecord(t, "rec-0004"))
	require.NoError(t, err)
	_, err = l.Append(validRecord(t, "rec-0005"))
	require.NoError(t, err)

	trunc, terr := l.VerifyAgainstCheckpoint(cp, v)
	require.NoError(t, terr, "appending after a checkpoint must not be reported as truncation")
	assert.Empty(t, trunc)

	// The plain walk stays clean too.
	breaks, err := l.Verify(v)
	require.NoError(t, err)
	assert.Empty(t, breaks)
}

// TestCheckpointAgainstUnchangedHeadIsClean proves the simple positive case: a
// checkpoint taken against a chain that has not moved verifies cleanly.
func TestCheckpointAgainstUnchangedHeadIsClean(t *testing.T) {
	l, sg, v, _ := truncFixture(t, 4)

	cp, err := l.Checkpoint(sg, fixedNow)
	require.NoError(t, err)

	trunc, terr := l.VerifyAgainstCheckpoint(cp, v)
	require.NoError(t, terr)
	assert.Empty(t, trunc)
}

// TestRewriteToSameLengthIsDetected proves a chain rewritten to the same length
// -- the same head Seq but a different head Hash -- is detected: the checkpoint
// pins (Seq, Hash), so a substitute chain of equal length no longer matches.
func TestRewriteToSameLengthIsDetected(t *testing.T) {
	l, sg, v, path := truncFixture(t, 6)

	cp, err := l.Checkpoint(sg, fixedNow)
	require.NoError(t, err)

	// Same head Seq (5), different head Hash: a rewritten chain of identical
	// length.
	execRaw(t, path, `UPDATE records SET hash = ? WHERE seq = 5`, bytes.Repeat([]byte{0xCD}, 32))

	trunc, terr := l.VerifyAgainstCheckpoint(cp, v)
	require.Error(t, terr, "a same-length rewrite must be caught")
	assert.ErrorIs(t, terr, ledger.ErrTruncated, "a rewrite is reported as a truncation of the attested history")
	b := truncationBreak(t, trunc)
	assert.Contains(t, b.Detail, "rewritten", "the detail must name the rewrite rule")
}

// TestTamperedCheckpointSignatureIsHardError proves an unverifiable checkpoint
// is not evidence: a checkpoint whose signature has been mangled must produce a
// hard error, never a "tamper detected" truncation reading -- otherwise an
// attacker who corrupts a checkpoint could fake a truncation alarm, or worse,
// mask a real one behind noise.
func TestTamperedCheckpointSignatureIsHardError(t *testing.T) {
	l, sg, v, _ := truncFixture(t, 6)

	cp, err := l.Checkpoint(sg, fixedNow)
	require.NoError(t, err)
	cp.Signature[0] ^= 0xFF

	trunc, terr := l.VerifyAgainstCheckpoint(cp, v)
	require.Error(t, terr)
	assert.NotErrorIs(t, terr, ledger.ErrTruncated, "a bad checkpoint must not read as truncation")
	assert.ErrorIs(t, terr, sign.ErrInvalidSignature)
	assert.Empty(t, trunc, "an unverifiable checkpoint must not produce a break")
}

// TestCheckpointSignedByUntrustedKeyIsHardError proves a checkpoint attributed
// to a key the verifier does not trust is a hard error, not a truncation break.
func TestCheckpointSignedByUntrustedKeyIsHardError(t *testing.T) {
	l, sg, _, _ := truncFixture(t, 6)

	cp, err := l.Checkpoint(sg, fixedNow)
	require.NoError(t, err)

	otherPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	untrusting := sign.NewVerifier(map[string]ed25519.PublicKey{"not-the-signer": otherPub})

	trunc, terr := l.VerifyAgainstCheckpoint(cp, untrusting)
	require.Error(t, terr)
	assert.NotErrorIs(t, terr, ledger.ErrTruncated)
	assert.ErrorIs(t, terr, sign.ErrUnknownKey)
	assert.Empty(t, trunc)
}

// TestCheckpointOnEmptyLedgerErrors proves the ledger refuses to sign a
// checkpoint for a nonexistent head: there is nothing to attest to.
func TestCheckpointOnEmptyLedgerErrors(t *testing.T) {
	l, sg, _, _ := truncFixture(t, 0)

	cp, err := l.Checkpoint(sg, fixedNow)
	require.Error(t, err, "an empty ledger has no head to attest to")
	assert.Equal(t, sign.Checkpoint{}, cp, "a failed checkpoint must be the zero value")
}
