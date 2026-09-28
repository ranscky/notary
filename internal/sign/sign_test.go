package sign_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/sign"
)

// newSeed returns a fresh random ed25519 seed.
func newSeed(t *testing.T) []byte {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	_, err := rand.Read(seed)
	require.NoError(t, err)
	return seed
}

// newEnvSigner loads a Signer from base64 key material held in NOTARY_SIGNING_KEY.
func newEnvSigner(t *testing.T, material string) *sign.Signer {
	t.Helper()
	t.Setenv("NOTARY_SIGNING_KEY", material)
	s, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_SIGNING_KEY"})
	require.NoError(t, err)
	require.NotNil(t, s)
	return s
}

// envSignerErr sets NOTARY_SIGNING_KEY and requires NewSigner to fail.
func envSignerErr(t *testing.T, material string) error {
	t.Helper()
	t.Setenv("NOTARY_SIGNING_KEY", material)
	_, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_SIGNING_KEY"})
	require.Error(t, err)
	return err
}

// TestNewSignerFailsWithoutKey proves NewSigner refuses to invent a key: with no
// key configured it returns ErrNoKeyConfigured and a nil Signer.
func TestNewSignerFailsWithoutKey(t *testing.T) {
	t.Run("env var unset", func(t *testing.T) {
		t.Setenv("NOTARY_SIGNING_KEY_UNSET", "")
		signer, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_SIGNING_KEY_UNSET"})
		require.Error(t, err)
		assert.Nil(t, signer)
		assert.ErrorIs(t, err, sign.ErrNoKeyConfigured)
	})

	t.Run("empty ref", func(t *testing.T) {
		signer, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: ""})
		require.Error(t, err)
		assert.Nil(t, signer)
		assert.ErrorIs(t, err, sign.ErrNoKeyConfigured)
	})
}

// TestNewSignerRejectsLoosePerms proves a key file whose mode is not exactly
// 0600 is refused, and that the same file rewritten 0600 loads.
func TestNewSignerRejectsLoosePerms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	require.NoError(t, os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(newSeed(t))), 0o600))
	require.NoError(t, os.Chmod(path, 0o644))

	signer, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceFile, Ref: path})
	require.Error(t, err)
	assert.Nil(t, signer)
	assert.ErrorIs(t, err, sign.ErrLoosePerms)

	require.NoError(t, os.Chmod(path, 0o600))
	signer, err = sign.NewSigner(sign.KeySource{Kind: sign.KeySourceFile, Ref: path})
	require.NoError(t, err)
	assert.NotNil(t, signer)
	assert.NotEmpty(t, signer.KeyID())
}

// TestKeychainUnsupportedOnThisPlatform proves KeySourceKeychain fails honestly
// instead of silently falling back to a file or the environment.
func TestKeychainUnsupportedOnThisPlatform(t *testing.T) {
	signer, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceKeychain, Ref: "notary"})
	require.Error(t, err)
	assert.Nil(t, signer)
	assert.ErrorIs(t, err, sign.ErrKeychainUnsupported)
}

// TestNewSignerKeyMaterialFormats proves both accepted encodings (32-byte seed
// and 64-byte private key), whitespace trimming, and rejection of bad input.
func TestNewSignerKeyMaterialFormats(t *testing.T) {
	seed := newSeed(t)
	priv := ed25519.NewKeyFromSeed(seed)

	t.Run("32-byte seed accepted", func(t *testing.T) {
		assert.NotEmpty(t, newEnvSigner(t, base64.StdEncoding.EncodeToString(seed)).KeyID())
	})

	t.Run("64-byte private key accepted", func(t *testing.T) {
		assert.NotEmpty(t, newEnvSigner(t, base64.StdEncoding.EncodeToString(priv)).KeyID())
	})

	t.Run("seed and full key of the same key share an ID", func(t *testing.T) {
		fromSeed := newEnvSigner(t, base64.StdEncoding.EncodeToString(seed))
		fromKey := newEnvSigner(t, base64.StdEncoding.EncodeToString(priv))
		assert.Equal(t, fromSeed.KeyID(), fromKey.KeyID())
	})

	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		assert.NotEmpty(t, newEnvSigner(t, " \t"+base64.StdEncoding.EncodeToString(seed)+"\n").KeyID())
	})

	t.Run("wrong length rejected", func(t *testing.T) {
		err := envSignerErr(t, base64.StdEncoding.EncodeToString(make([]byte, 16)))
		assert.NotErrorIs(t, err, sign.ErrNoKeyConfigured)
	})

	t.Run("invalid base64 rejected", func(t *testing.T) {
		err := envSignerErr(t, "not*valid*base64*!!!")
		assert.NotErrorIs(t, err, sign.ErrNoKeyConfigured)
	})
}

// TestKeyIDIsStableAndDistinct proves KeyID is a stable fingerprint of the
// public key: base64(sha256(pub)), shared by equal keys and distinct otherwise.
func TestKeyIDIsStableAndDistinct(t *testing.T) {
	seed := newSeed(t)
	priv := ed25519.NewKeyFromSeed(seed)
	material := base64.StdEncoding.EncodeToString(seed)

	a := newEnvSigner(t, material)
	b := newEnvSigner(t, material)
	assert.Equal(t, a.KeyID(), b.KeyID(), "same key must yield the same KeyID")

	pub := priv.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	assert.Equal(t, base64.StdEncoding.EncodeToString(sum[:]), a.KeyID(),
		"KeyID must be base64(sha256(public key))")

	c := newEnvSigner(t, base64.StdEncoding.EncodeToString(newSeed(t)))
	assert.NotEqual(t, a.KeyID(), c.KeyID(), "different keys must yield different KeyIDs")
}

// TestSignVerifyRoundTrip proves a genuine signature verifies and that both a
// one-byte message tamper and a one-byte signature tamper are rejected.
func TestSignVerifyRoundTrip(t *testing.T) {
	seed := newSeed(t)
	priv := ed25519.NewKeyFromSeed(seed)
	signer := newEnvSigner(t, base64.StdEncoding.EncodeToString(seed))

	verifier := sign.NewVerifier(map[string]ed25519.PublicKey{
		signer.KeyID(): priv.Public().(ed25519.PublicKey),
	})

	msg := []byte("ledger record payload")
	sig, err := signer.Sign(msg)
	require.NoError(t, err)
	require.NotEmpty(t, sig)
	require.NoError(t, verifier.Verify(signer.KeyID(), msg, sig))

	t.Run("message tampered by one byte", func(t *testing.T) {
		bad := append([]byte(nil), msg...)
		bad[0] ^= 0x01
		err := verifier.Verify(signer.KeyID(), bad, sig)
		require.Error(t, err)
		assert.ErrorIs(t, err, sign.ErrInvalidSignature)
	})

	t.Run("signature tampered by one byte", func(t *testing.T) {
		bad := append([]byte(nil), sig...)
		bad[0] ^= 0x01
		err := verifier.Verify(signer.KeyID(), msg, bad)
		require.Error(t, err)
		assert.ErrorIs(t, err, sign.ErrInvalidSignature)
	})

	t.Run("unknown key id errors distinctly", func(t *testing.T) {
		err := verifier.Verify("unknown-key-id", msg, sig)
		require.Error(t, err)
		assert.ErrorIs(t, err, sign.ErrUnknownKey)
		assert.NotErrorIs(t, err, sign.ErrInvalidSignature)
	})
}

// TestNewVerifierNilDoesNotPanic proves a nil/empty keyring is safe: Verify
// reports an unknown key rather than panicking.
func TestNewVerifierNilDoesNotPanic(t *testing.T) {
	v := sign.NewVerifier(nil)
	require.NotNil(t, v)
	require.NotPanics(t, func() {
		err := v.Verify("any-key", []byte("msg"), []byte("sig"))
		require.ErrorIs(t, err, sign.ErrUnknownKey)
	})

	empty := sign.NewVerifier(map[string]ed25519.PublicKey{})
	require.ErrorIs(t, empty.Verify("any-key", []byte("msg"), []byte("sig")), sign.ErrUnknownKey)
}
