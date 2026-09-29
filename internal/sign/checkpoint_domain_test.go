package sign_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
	"notary/internal/sign"
)

// ledgerCheckpointMessage reconstructs, from the *documented* v1 wire form
// alone, the exact bytes a ledger checkpoint signs:
//
//	"notary/checkpoint/v1" ‖ uint64-BE(Seq) ‖ Hash[:] ‖ RFC3339Nano(At.UTC)
//
// It deliberately does not call into package sign: ed25519 verification demands
// byte-exact input, so verifying a checkpoint's signature against these bytes
// proves the signer still produces the historical ledger message, byte for
// byte. That is the regression lock the refactor must not break.
func ledgerCheckpointMessage(seq uint64, h record.Hash, at time.Time) []byte {
	var buf bytes.Buffer
	buf.WriteString("notary/checkpoint/v1")

	var seqBuf [8]byte
	binary.BigEndian.PutUint64(seqBuf[:], seq)
	buf.Write(seqBuf[:])

	buf.Write(h[:])
	buf.WriteString(at.UTC().Format(time.RFC3339Nano))
	return buf.Bytes()
}

// TestChainDomainConstantsAreStable pins the domain values the wire format
// depends on. LedgerChain must stay the historical "notary/checkpoint/v1" so no
// existing signature changes meaning, and the two chains must be distinct so
// neither can be replayed as evidence about the other.
func TestChainDomainConstantsAreStable(t *testing.T) {
	assert.Equal(t, "notary/checkpoint/v1", string(sign.LedgerChain),
		"LedgerChain must remain the historical v1 domain")
	assert.Equal(t, "notary/gapcheckpoint/v1", string(sign.GapChain),
		"GapChain must be a distinct domain")
	assert.NotEqual(t, sign.LedgerChain, sign.GapChain,
		"the ledger and gap chains must not share a domain")
}

// TestNewCheckpointSignatureUnchanged proves the refactor left the ledger
// checkpoint's signed bytes byte-identical: a checkpoint from the existing
// NewCheckpoint verifies against the documented v1 ledger message, and the
// domain-parameterised builder under LedgerChain returns the very same
// checkpoint (same signature).
func TestNewCheckpointSignatureUnchanged(t *testing.T) {
	signer, keyring := newCheckpointSigner(t)
	verifier := sign.NewVerifier(keyring)

	seq := uint64(42)
	h := testHash()
	at := time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)

	cp, err := sign.NewCheckpoint(seq, h, at, signer)
	require.NoError(t, err)

	// The existing public API still verifies the checkpoint it produced.
	require.NoError(t, verifier.VerifyCheckpoint(cp))

	// The signature is over the historically documented ledger message.
	pub := keyring[signer.KeyID()]
	require.NotNil(t, pub, "the signer's key must be in the trusted keyring")
	assert.True(t, ed25519.Verify(pub, ledgerCheckpointMessage(seq, h, at), cp.Signature),
		"the ledger checkpoint must still sign the documented v1 ledger message")

	// NewCheckpoint delegates to NewChainCheckpoint(LedgerChain, ...): identical
	// output, field for field (a deterministic signature included).
	same, err := sign.NewChainCheckpoint(sign.LedgerChain, seq, h, at, signer)
	require.NoError(t, err)
	assert.Equal(t, cp, same,
		"NewCheckpoint must delegate to NewChainCheckpoint(LedgerChain, ...)")
}

// TestCheckpointCrossDomainReplayFails is the domain-separation proof: a
// checkpoint signed for one chain must never verify as evidence about the
// other. Without it the two chains share a signature space and the separation
// is decorative.
func TestCheckpointCrossDomainReplayFails(t *testing.T) {
	signer, keyring := newCheckpointSigner(t)
	verifier := sign.NewVerifier(keyring)

	seq := uint64(7)
	h := testHash()
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	gapCP, err := sign.NewChainCheckpoint(sign.GapChain, seq, h, at, signer)
	require.NoError(t, err)
	ledgerCP, err := sign.NewCheckpoint(seq, h, at, signer)
	require.NoError(t, err)

	// Each verifies under its own domain -- so the failures below are about the
	// domain, not a broken signature.
	require.NoError(t, verifier.VerifyChainCheckpoint(sign.GapChain, gapCP))
	require.NoError(t, verifier.VerifyChainCheckpoint(sign.LedgerChain, ledgerCP))

	// A gap checkpoint replayed as evidence about the ledger must fail.
	err = verifier.VerifyChainCheckpoint(sign.LedgerChain, gapCP)
	require.Error(t, err, "a gap checkpoint must not verify under the ledger domain")
	assert.ErrorIs(t, err, sign.ErrInvalidSignature)

	// A ledger checkpoint replayed as evidence about gaps must fail.
	err = verifier.VerifyChainCheckpoint(sign.GapChain, ledgerCP)
	require.Error(t, err, "a ledger checkpoint must not verify under the gap domain")
	assert.ErrorIs(t, err, sign.ErrInvalidSignature)

	// The legacy ledger-only API must reject a gap checkpoint too.
	err = verifier.VerifyCheckpoint(gapCP)
	require.Error(t, err, "the ledger-only API must reject a gap checkpoint")
	assert.ErrorIs(t, err, sign.ErrInvalidSignature)
}
