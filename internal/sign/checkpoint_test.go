package sign_test

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/record"
	"notary/internal/sign"
)

// newCheckpointSigner loads a fresh Signer from a random key in
// NOTARY_CHECKPOINT_KEY and returns the matching trusted keyring.
func newCheckpointSigner(t *testing.T) (*sign.Signer, map[string]ed25519.PublicKey) {
	t.Helper()
	seed := newSeed(t)
	priv := ed25519.NewKeyFromSeed(seed)
	t.Setenv("NOTARY_CHECKPOINT_KEY", base64.StdEncoding.EncodeToString(seed))
	s, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_CHECKPOINT_KEY"})
	require.NoError(t, err)
	require.NotNil(t, s)
	return s, map[string]ed25519.PublicKey{s.KeyID(): priv.Public().(ed25519.PublicKey)}
}

// testHash returns a fixed non-zero hash for deterministic messages.
func testHash() record.Hash {
	var h record.Hash
	for i := range h {
		h[i] = byte(i)
	}
	return h
}

// TestCheckpointRoundTrip proves a checkpoint survives MarshalCheckpoint and
// UnmarshalCheckpoint with every field intact, and that the JSON is indented.
func TestCheckpointRoundTrip(t *testing.T) {
	signer, _ := newCheckpointSigner(t)
	at := time.Date(2026, 3, 4, 5, 6, 7, 123456789, time.UTC)

	cp, err := sign.NewCheckpoint(42, testHash(), at, signer)
	require.NoError(t, err)

	data, err := sign.MarshalCheckpoint(cp)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Indented output is deliberate: an auditor must be able to read it.
	assert.Contains(t, string(data), "\n  ", "checkpoint JSON must be indented")

	got, err := sign.UnmarshalCheckpoint(data)
	require.NoError(t, err)

	assert.Equal(t, cp.Seq, got.Seq)
	assert.Equal(t, cp.Hash, got.Hash)
	assert.True(t, cp.At.Equal(got.At), "At instant must survive the round trip")
	assert.Equal(t, time.UTC, got.At.Location(), "At must come back in UTC")
	assert.Equal(t, cp.SignerKeyID, got.SignerKeyID)
	assert.Equal(t, cp.Signature, got.Signature)

	// Marshalling the decoded value again is byte-for-byte stable.
	again, err := sign.MarshalCheckpoint(got)
	require.NoError(t, err)
	assert.Equal(t, data, again)
}

// TestVerifyCheckpointAcceptsGenuine proves a freshly signed checkpoint verifies.
func TestVerifyCheckpointAcceptsGenuine(t *testing.T) {
	signer, keyring := newCheckpointSigner(t)
	verifier := sign.NewVerifier(keyring)

	cp, err := sign.NewCheckpoint(7, testHash(), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), signer)
	require.NoError(t, err)
	assert.Equal(t, signer.KeyID(), cp.SignerKeyID)
	assert.NotEmpty(t, cp.Signature)

	require.NoError(t, verifier.VerifyCheckpoint(cp))
}

// TestVerifyCheckpointDetectsTamper proves mutating any signed field after
// signing makes verification fail with ErrInvalidSignature.
func TestVerifyCheckpointDetectsTamper(t *testing.T) {
	signer, keyring := newCheckpointSigner(t)
	verifier := sign.NewVerifier(keyring)
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	cp, err := sign.NewCheckpoint(7, testHash(), base, signer)
	require.NoError(t, err)

	cases := map[string]func(c *sign.Checkpoint){
		"seq":  func(c *sign.Checkpoint) { c.Seq++ },
		"hash": func(c *sign.Checkpoint) { c.Hash[0] ^= 0xff },
		"at":   func(c *sign.Checkpoint) { c.At = base.Add(time.Second) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			tampered := cp
			mutate(&tampered)
			err := verifier.VerifyCheckpoint(tampered)
			require.Error(t, err)
			assert.ErrorIs(t, err, sign.ErrInvalidSignature)
		})
	}
}

// TestVerifyCheckpointMessageIsInjective is the property that makes a checkpoint
// meaningful: two different At values must produce different signed messages.
// A whole second and one nanosecond later are the tricky pair, because
// RFC3339Nano trims trailing fractional zeros and a second-resolution formatter
// would collapse them into the same bytes.
func TestVerifyCheckpointMessageIsInjective(t *testing.T) {
	signer, keyring := newCheckpointSigner(t)
	verifier := sign.NewVerifier(keyring)

	whole := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	later := whole.Add(time.Nanosecond) // .000000001 — distinguished only by the ns

	cpWhole, err := sign.NewCheckpoint(7, testHash(), whole, signer)
	require.NoError(t, err)
	require.NoError(t, verifier.VerifyCheckpoint(cpWhole))

	// Reusing the whole-second signature under the one-nanosecond-later instant
	// must fail: the two instants feed different bytes to the signer.
	reused := cpWhole
	reused.At = later
	err = verifier.VerifyCheckpoint(reused)
	require.Error(t, err, "a one-nanosecond difference must change the signed bytes")
	assert.ErrorIs(t, err, sign.ErrInvalidSignature)

	// Signing the later instant independently yields a different signature that
	// still verifies, confirming the difference is real and not a decode artifact.
	cpLater, err := sign.NewCheckpoint(7, testHash(), later, signer)
	require.NoError(t, err)
	require.NoError(t, verifier.VerifyCheckpoint(cpLater))
	assert.NotEqual(t, cpWhole.Signature, cpLater.Signature,
		"distinct instants must produce distinct signatures")
}

// TestVerifyCheckpointUnknownKeyID proves a signature attributed to a key absent
// from the keyring fails with a wrapped ErrUnknownKey.
func TestVerifyCheckpointUnknownKeyID(t *testing.T) {
	signer, _ := newCheckpointSigner(t)
	// A keyring that trusts a different key entirely.
	_, otherKeyring := newCheckpointSigner(t)
	verifier := sign.NewVerifier(otherKeyring)

	cp, err := sign.NewCheckpoint(7, testHash(), time.Now(), signer)
	require.NoError(t, err)

	err = verifier.VerifyCheckpoint(cp)
	require.Error(t, err)
	assert.ErrorIs(t, err, sign.ErrUnknownKey)
	assert.NotErrorIs(t, err, sign.ErrInvalidSignature)
}

// TestVerifyCheckpointZeroValueDoesNotPanic proves a zero-value Checkpoint is
// safe: it reports the empty key ID as unknown rather than panicking.
func TestVerifyCheckpointZeroValueDoesNotPanic(t *testing.T) {
	verifier := sign.NewVerifier(nil)
	require.NotPanics(t, func() {
		err := verifier.VerifyCheckpoint(sign.Checkpoint{})
		require.Error(t, err)
		assert.ErrorIs(t, err, sign.ErrUnknownKey)
	})
}

// TestNewCheckpointNilSignerErrors proves a nil signer is a returned error, not
// a panic.
func TestNewCheckpointNilSignerErrors(t *testing.T) {
	require.NotPanics(t, func() {
		cp, err := sign.NewCheckpoint(1, testHash(), time.Now(), nil)
		require.Error(t, err)
		assert.Equal(t, sign.Checkpoint{}, cp)
	})
}

// TestNewCheckpointNormalisesToUTC proves NewCheckpoint stores At in UTC and
// leaves the caller's time.Time untouched.
func TestNewCheckpointNormalisesToUTC(t *testing.T) {
	signer, _ := newCheckpointSigner(t)
	zone := time.FixedZone("UTC+2", 2*60*60)
	given := time.Date(2026, 5, 6, 7, 8, 9, 0, zone)

	cp, err := sign.NewCheckpoint(3, testHash(), given, signer)
	require.NoError(t, err)

	assert.Equal(t, time.UTC, cp.At.Location())
	assert.True(t, cp.At.Equal(given), "the instant must be preserved")
	assert.Equal(t, zone, given.Location(), "the caller's value must not be mutated")
}

// TestUnmarshalCheckpointRejectsMalformed proves every malformed shape is
// rejected with a non-nil error and a zero-value struct, never a panic and never
// a partially-populated checkpoint that then validates.
func TestUnmarshalCheckpointRejectsMalformed(t *testing.T) {
	h := testHash()
	validHash := hex.EncodeToString(h[:])
	validSig := hex.EncodeToString(make([]byte, ed25519.SignatureSize))
	const at = "2026-01-02T03:04:05Z"
	const keyID = "some-key-id"

	// A well-formed baseline, to prove the rejections are about the targeted
	// defect and not a blanket failure.
	good := `{"seq":1,"hash":"` + validHash + `","at":"` + at +
		`","signer_key_id":"` + keyID + `","signature":"` + validSig + `"}`
	_, err := sign.UnmarshalCheckpoint([]byte(good))
	require.NoError(t, err, "baseline JSON must decode")

	cases := map[string]string{
		"invalid JSON":        `{"seq":1,`,
		"JSON array":          `[1,2,3]`,
		"missing hash":        `{"seq":1,"at":"` + at + `","signer_key_id":"` + keyID + `","signature":"` + validSig + `"}`,
		"empty hash":          `{"seq":1,"hash":"","at":"` + at + `","signer_key_id":"` + keyID + `","signature":"` + validSig + `"}`,
		"short hash":          `{"seq":1,"hash":"deadbeef","at":"` + at + `","signer_key_id":"` + keyID + `","signature":"` + validSig + `"}`,
		"non-hex hash":        `{"seq":1,"hash":"` + strings.Repeat("z", 64) + `","at":"` + at + `","signer_key_id":"` + keyID + `","signature":"` + validSig + `"}`,
		"missing signature":   `{"seq":1,"hash":"` + validHash + `","at":"` + at + `","signer_key_id":"` + keyID + `"}`,
		"short signature":     `{"seq":1,"hash":"` + validHash + `","at":"` + at + `","signer_key_id":"` + keyID + `","signature":"abcd"}`,
		"non-hex signature":   `{"seq":1,"hash":"` + validHash + `","at":"` + at + `","signer_key_id":"` + keyID + `","signature":"` + strings.Repeat("z", 128) + `"}`,
		"empty signer key id": `{"seq":1,"hash":"` + validHash + `","at":"` + at + `","signer_key_id":"","signature":"` + validSig + `"}`,
	}

	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			var got sign.Checkpoint
			require.NotPanics(t, func() {
				var err error
				got, err = sign.UnmarshalCheckpoint([]byte(input))
				require.Error(t, err)
			})
			assert.Equal(t, sign.Checkpoint{}, got,
				"a rejected input must not yield a partially-populated checkpoint")
		})
	}
}

// TestMarshalCheckpointUsesStableFieldNames locks the auditable wire format:
// hex-encoded Hash and Signature, RFC3339Nano UTC `at`, and named fields.
func TestMarshalCheckpointUsesStableFieldNames(t *testing.T) {
	signer, _ := newCheckpointSigner(t)
	at := time.Date(2026, 7, 8, 9, 10, 11, 0, time.UTC)
	cp, err := sign.NewCheckpoint(9, testHash(), at, signer)
	require.NoError(t, err)

	data, err := sign.MarshalCheckpoint(cp)
	require.NoError(t, err)

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &raw))
	for _, field := range []string{"seq", "hash", "at", "signer_key_id", "signature"} {
		assert.Contains(t, raw, field, "wire format must expose field %q", field)
	}
	assert.JSONEq(t, `"`+hex.EncodeToString(cp.Hash[:])+`"`, string(raw["hash"]))
	assert.JSONEq(t, `"`+hex.EncodeToString(cp.Signature)+`"`, string(raw["signature"]))
	assert.JSONEq(t, `"2026-07-08T09:10:11Z"`, string(raw["at"]))
}
