package gap_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
	"notary/internal/sign"
)

// gapTestSeed is a fixed 32-byte ed25519 seed, mirroring the sign and ledger
// packages' own test pattern (base64 seed through the env KeySource; NewSigner
// never generates a key). It is distinct from the ledger's seed on purpose, so
// a key mix-up between chains cannot pass unnoticed.
var gapTestSeed = []byte{
	0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88,
	0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00,
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
}

// gapTestSigner builds a Signer from gapTestSeed through the env KeySource and a
// Verifier that trusts it, following the sign package's test pattern.
func gapTestSigner(t *testing.T) (*sign.Signer, *sign.Verifier) {
	t.Helper()
	t.Setenv("NOTARY_GAP_TEST_KEY", base64.StdEncoding.EncodeToString(gapTestSeed))
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_GAP_TEST_KEY"})
	require.NoError(t, err)
	pub := ed25519.NewKeyFromSeed(gapTestSeed).Public().(ed25519.PublicKey)
	return sg, sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})
}

// checkpointThree records three entries (counters 0, 1, 2), signs a checkpoint
// for the resulting head, closes the log, and returns the path and checkpoint.
func checkpointThree(t *testing.T, sg *sign.Signer) (string, sign.Checkpoint) {
	t.Helper()
	path := writeThree(t)
	g, err := gap.Open(path)
	require.NoError(t, err)
	cp, err := g.Checkpoint(sg, gapTestTime)
	require.NoError(t, err)
	require.NoError(t, g.Close())
	require.Equal(t, uint64(2), cp.Seq, "the checkpoint must attest to the head, counter 2")
	return path, cp
}

// TestGapCheckpointRoundTripClean proves an unchanged log verifies cleanly
// against the checkpoint it produced, through both the method and the read-only
// package-level function.
func TestGapCheckpointRoundTripClean(t *testing.T) {
	sg, v := gapTestSigner(t)
	path, cp := checkpointThree(t, sg)

	g, err := gap.Open(path)
	require.NoError(t, err)
	methodBreaks, err := g.VerifyAgainstCheckpoint(cp, v)
	require.NoError(t, err)
	assert.Empty(t, methodBreaks, "an unchanged log must verify clean via the method")
	require.NoError(t, g.Close())

	funcBreaks, err := gap.VerifyAgainstCheckpoint(path, cp, v)
	require.NoError(t, err)
	assert.Empty(t, funcBreaks, "an unchanged log must verify clean via the read-only function")
}

// TestGapCheckpointLongerLogStaysClean pins the lower-bound property: a
// checkpoint is a floor on history, not a fixed head, so growing the log past
// the checkpoint must stay clean.
func TestGapCheckpointLongerLogStaysClean(t *testing.T) {
	sg, v := gapTestSigner(t)
	path := writeThree(t) // counters 0,1,2

	g, err := gap.Open(path)
	require.NoError(t, err)
	cp, err := g.Checkpoint(sg, gapTestTime)
	require.NoError(t, err)
	require.Equal(t, uint64(2), cp.Seq)

	require.NoError(t, g.Record(gapEntry("c3", "u", "a")))
	require.NoError(t, g.Record(gapEntry("c4", "u", "a")))
	require.NoError(t, g.Close())

	breaks, err := gap.VerifyAgainstCheckpoint(path, cp, v)
	require.NoError(t, err, "appending after a checkpoint must not read as truncation")
	assert.Empty(t, breaks)
}

// TestGapCheckpointDetectsDeletedTail is the tail truncation case: deleting the
// last line leaves a shorter but self-consistent chain that Verify alone cannot
// see, so only the checkpoint catches it.
func TestGapCheckpointDetectsDeletedTail(t *testing.T) {
	sg, v := gapTestSigner(t)
	path, cp := checkpointThree(t, sg)

	lines := readLines(t, path)
	require.Len(t, lines, 3)
	writeLines(t, path, lines[:2]) // drop counter 2

	breaks, err := gap.VerifyAgainstCheckpoint(path, cp, v)
	require.Error(t, err, "a shortened log must be reported as an error")
	assert.ErrorIs(t, err, gap.ErrTruncated, "the error must wrap ErrTruncated")
	require.Len(t, breaks, 1, "a truncated log must yield exactly one break")
	assert.Equal(t, "truncation", breaks[0].Field, "the break must name the truncation field")
	assert.Equal(t, uint64(2), breaks[0].Counter, "the break must report the attested counter")
}

// TestGapCheckpointDetectsDeletedFile is the whole point of the feature:
// deleting the entire gap log -- the tamper readLog alone treats as "no gap
// ever happened" -- must now be detectable against a signed checkpoint.
func TestGapCheckpointDetectsDeletedFile(t *testing.T) {
	sg, v := gapTestSigner(t)
	path, cp := checkpointThree(t, sg)

	// The hazard first: with the file gone, plain Verify sees nothing wrong.
	plain, perr := gap.Verify(path)
	require.NoError(t, perr)
	assert.Empty(t, plain, "a missing gap log is indistinguishable from no gap without a checkpoint")

	require.NoError(t, os.Remove(path))

	breaks, err := gap.VerifyAgainstCheckpoint(path, cp, v)
	require.Error(t, err, "deleting the whole gap log must be detectable")
	assert.ErrorIs(t, err, gap.ErrTruncated, "a deleted log is a truncation of the attested history")
	require.Len(t, breaks, 1)
	assert.Equal(t, "truncation", breaks[0].Field)
}

// TestGapCheckpointDetectsRewrite covers a rewritten chain of the same length:
// the entry at the attested counter exists but carries a different hash, so the
// checkpoint's pinned (Counter, Hash) no longer matches.
func TestGapCheckpointDetectsRewrite(t *testing.T) {
	sg, v := gapTestSigner(t)
	path, cp := checkpointThree(t, sg)

	// Rewrite the last line's stored hash to a different 32-byte value, leaving
	// the line valid JSON that still decodes.
	lines := readLines(t, path)
	last := lines[2]
	idx := strings.Index(last, `"hash":"`)
	require.GreaterOrEqual(t, idx, 0, "the fixture line must carry a hash field")
	start := idx + len(`"hash":"`)
	last = last[:start] + strings.Repeat("ab", 32) + last[start+64:]
	require.NotEqual(t, lines[2], last, "the fixture must actually change the hash")
	lines[2] = last
	writeLines(t, path, lines)

	breaks, err := gap.VerifyAgainstCheckpoint(path, cp, v)
	require.Error(t, err, "a rewritten entry must be caught")
	assert.ErrorIs(t, err, gap.ErrTruncated, "a rewrite is a truncation of the attested history")
	require.Len(t, breaks, 1)
	assert.Equal(t, "truncation", breaks[0].Field)
	assert.Contains(t, breaks[0].Detail, "rewritten", "the detail must name the rewrite rule")
}

// TestGapCheckpointTamperedSignatureIsHardError proves an unverifiable
// checkpoint is not evidence: a mangled signature is a hard error, never a
// "tamper detected" truncation reading.
func TestGapCheckpointTamperedSignatureIsHardError(t *testing.T) {
	sg, v := gapTestSigner(t)
	path, cp := checkpointThree(t, sg)

	cp.Signature[0] ^= 0xFF

	breaks, err := gap.VerifyAgainstCheckpoint(path, cp, v)
	require.Error(t, err)
	assert.NotErrorIs(t, err, gap.ErrTruncated, "a bad checkpoint must not read as truncation")
	assert.ErrorIs(t, err, sign.ErrInvalidSignature)
	assert.Empty(t, breaks, "an unverifiable checkpoint must not produce a break")
}

// TestGapCheckpointWrongKeyIsHardError proves a checkpoint attributed to a key
// the verifier does not trust is a hard error, not a truncation break.
func TestGapCheckpointWrongKeyIsHardError(t *testing.T) {
	sg, _ := gapTestSigner(t)
	path, cp := checkpointThree(t, sg)

	otherPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	untrusting := sign.NewVerifier(map[string]ed25519.PublicKey{"not-the-signer": otherPub})

	breaks, err := gap.VerifyAgainstCheckpoint(path, cp, untrusting)
	require.Error(t, err)
	assert.NotErrorIs(t, err, gap.ErrTruncated)
	assert.ErrorIs(t, err, sign.ErrUnknownKey)
	assert.Empty(t, breaks)
}

// TestGapCheckpointOnEmptyLogErrors proves the log refuses to sign a checkpoint
// for a log with no entry to attest to, mirroring ledger.Checkpoint on an empty
// store.
func TestGapCheckpointOnEmptyLogErrors(t *testing.T) {
	sg, _ := gapTestSigner(t)
	g, err := gap.Open(filepath.Join(t.TempDir(), "gaps.log"))
	require.NoError(t, err)

	cp, err := g.Checkpoint(sg, gapTestTime)
	require.Error(t, err, "an empty gap log has no head to attest to")
	require.ErrorIs(t, err, gap.ErrEmptyLog,
		"an empty log must be recognisable by sentinel, not by matching its text")
	assert.Equal(t, sign.Checkpoint{}, cp, "a failed checkpoint must be the zero value")
	require.NoError(t, g.Close())
}

// TestGapVerifyAgainstCheckpointDoesNotMutateLog is the auditor-safety guard:
// gap.Open opens O_APPEND and heals a torn tail by writing a newline, so an
// auditor verifying against a checkpoint must use the read-only path and never
// mutate the evidence.
func TestGapVerifyAgainstCheckpointDoesNotMutateLog(t *testing.T) {
	sg, v := gapTestSigner(t)
	path, cp := checkpointThree(t, sg)

	// Append a torn (unterminated) fragment -- exactly what gap.Open would heal.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"counter":3,"detail":"torn`)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	before, err := os.ReadFile(path)
	require.NoError(t, err)

	// The torn tail does not change the attested head (counter 2), so this is
	// clean; the point is that the file is left byte-identical.
	breaks, verr := gap.VerifyAgainstCheckpoint(path, cp, v)
	require.NoError(t, verr)
	assert.Empty(t, breaks)

	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the read-only path must not heal or append to the log")

	// Contrast: opening the log WOULD heal the torn tail, proving the read-only
	// path's non-mutation is load-bearing and not merely a property of the file.
	og, err := gap.Open(path)
	require.NoError(t, err)
	require.NoError(t, og.Close())
	healed, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotEqual(t, before, healed,
		"gap.Open heals a torn tail; this is why the auditor path must not use it")
}
