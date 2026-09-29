package sign_test

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/sign"
)

// canarySeed is a FIXED seed so the canary's secret material is deterministic
// and independently reproducible.
var canarySeed = [ed25519.SeedSize]byte{
	0x5a, 0x1e, 0xc4, 0x88, 0x02, 0xf7, 0x3b, 0x6d,
	0x91, 0x40, 0xab, 0x12, 0xde, 0x55, 0x77, 0x09,
	0xe3, 0x2a, 0xbc, 0x6f, 0x18, 0x84, 0xd1, 0x37,
	0x60, 0xaf, 0x5c, 0x23, 0x99, 0x0e, 0x46, 0xf2,
}

// TestKeyMaterialNeverAppearsInOutput is Review Focus #5: no formatting,
// encoding, logging, or error path may reveal the private key material.
//
// It builds a signer from the fixed canary seed, derives the printable forms of
// the private key, and asserts that none of them appears in any output that can
// be produced from the Signer, the KeySource, the errors NewSigner can return,
// or a log.Logger that has logged the signer.
func TestKeyMaterialNeverAppearsInOutput(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(canarySeed[:])
	keyB64 := base64.StdEncoding.EncodeToString(priv)
	seedB64 := base64.StdEncoding.EncodeToString(canarySeed[:])

	t.Setenv("NOTARY_SIGNING_KEY", seedB64)
	signer, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_SIGNING_KEY"})
	require.NoError(t, err)
	require.NotNil(t, signer)

	source := sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_SIGNING_KEY"}

	out := map[string]string{
		"%v source":  fmt.Sprintf("%v", source),
		"%+v source": fmt.Sprintf("%+v", source),
		"%#v source": fmt.Sprintf("%#v", source),
		"%v keyID":   fmt.Sprintf("%v", signer.KeyID()),
		"%T signer":  fmt.Sprintf("%T", signer),
	}

	// Build the signer format calls with a runtime format string so that the
	// deliberate %s/%q/%x against a struct do not trip the printf vet check.
	// %X is included so both hex cases are exercised.
	verbs := []string{"v", "+v", "#v", "s", "q", "x", "X"}
	for _, verb := range verbs {
		format := "%" + verb
		out[format+" signer"] = fmt.Sprintf(format, signer)
	}

	marshaledSigner, err := json.Marshal(signer)
	require.NoError(t, err)
	out["json.Marshal(signer)"] = string(marshaledSigner)

	marshaledSource, err := json.Marshal(source)
	require.NoError(t, err)
	out["json.Marshal(source)"] = string(marshaledSource)

	errs := map[string]error{
		"env unset":        signerErrEnvUnset(t),
		"empty ref":        signerErrEmptyRef(t),
		"keychain":         signerErrSource(t, sign.KeySource{Kind: sign.KeySourceKeychain}),
		"unknown kind":     signerErrSource(t, sign.KeySource{Kind: sign.KeySourceKind(200)}),
		"loose perms file": signerErrLoosePerms(t, keyB64),
		"bad length":       signerErrEnvValue(t, base64.StdEncoding.EncodeToString(make([]byte, 16))),
		"bad base64":       signerErrEnvValue(t, "not*valid*base64*!!!"),
	}
	for name, e := range errs {
		require.Errorf(t, e, "canary error %q must be non-nil", name)
		out["error "+name+" %v"] = fmt.Sprintf("%v", e)
		out["error "+name+" %+v"] = fmt.Sprintf("%+v", e)
		out["error "+name+" %#v"] = fmt.Sprintf("%#v", e)
	}

	var buf bytes.Buffer
	logger := log.New(&buf, "", 0)
	logger.Printf("signer=%v source=%+v keyid=%s", signer, source, signer.KeyID())
	logger.Printf("go=%#v", signer)
	out["log.Logger"] = buf.String()

	// Positive assertion: every rendering of the signer itself must be exactly
	// the redaction marker. A negative check against an enumerated list of
	// secret spellings can always miss a spelling — notably the 0x-prefixed,
	// comma-separated Go-syntax form that %#v emits — so this positive check is
	// what actually guards the verb. Had it used only the absence list below,
	// deleting Signer.Format and Signer.GoString (keeping Signer.String) would
	// leave this test green while %#v printed the raw private key.
	const redacted = "[redacted]"
	for _, verb := range verbs {
		format := "%" + verb
		assert.Equalf(t, redacted, out[format+" signer"],
			"%%%s of the signer must render exactly %q", verb, redacted)
	}
	assert.Equal(t, `"`+redacted+`"`, string(marshaledSigner),
		"json.Marshal(signer) must render exactly the redacted JSON string")
	assert.Containsf(t, out["log.Logger"], "signer="+redacted,
		"logged signer must render exactly %q", redacted)
	assert.Containsf(t, out["log.Logger"], "go="+redacted,
		"logged signer under %%#v must render exactly %q", redacted)

	// Every printable form of the private key. The base64 forms are what the
	// brief requires; the hex and decimal forms catch a leak that a raw %v/%x of
	// the key bytes would produce (which base64 would not match). The Go-syntax
	// form catches the 0x-prefixed, comma-separated rendering that %#v emits.
	secrets := map[string]string{
		"base64(private key)":    keyB64,
		"base64(seed)":           seedB64,
		"hex(private key)":       hex.EncodeToString(priv),
		"hex(seed)":              hex.EncodeToString(canarySeed[:]),
		"decimal(private key)":   fmt.Sprintf("%v", []byte(priv)),
		"decimal(seed)":          fmt.Sprintf("%v", canarySeed[:]),
		"go-syntax(private key)": fmt.Sprintf("%#v", ed25519.PrivateKey(priv)),
		"go-syntax(seed)":        fmt.Sprintf("%#v", canarySeed),
	}

	for outName, text := range out {
		for secretName, secret := range secrets {
			assert.NotContainsf(t, text, secret, "%s leaked %s", outName, secretName)
		}
	}
}

func signerErrEnvUnset(t *testing.T) error {
	t.Helper()
	t.Setenv("NOTARY_CANARY_UNSET", "")
	_, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_CANARY_UNSET"})
	return err
}

func signerErrEmptyRef(t *testing.T) error {
	t.Helper()
	_, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: ""})
	return err
}

func signerErrSource(t *testing.T, src sign.KeySource) error {
	t.Helper()
	_, err := sign.NewSigner(src)
	return err
}

func signerErrEnvValue(t *testing.T, value string) error {
	t.Helper()
	t.Setenv("NOTARY_CANARY_INVALID", value)
	_, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_CANARY_INVALID"})
	return err
}

// TestNewSignerNeverEchoesMaterialRef guards the defect where Config.SigningKeyEnv
// was populated from the VALUE of NOTARY_SIGNING_KEY. Under that bug the "ref"
// passed to NewSigner was base64 key material, and loadFromEnv echoed it in its
// error text. A material-shaped ref must now be refused WITHOUT being echoed,
// while a genuine variable name -- which is not secret -- is still named so the
// operator can act on it.
func TestNewSignerNeverEchoesMaterialRef(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(canarySeed[:])
	keyB64 := base64.StdEncoding.EncodeToString(priv)
	seedB64 := base64.StdEncoding.EncodeToString(canarySeed[:])

	// The canary is unmistakable and material-shaped: base64 of a 64-byte
	// private key ends in "==" padding, which is invalid in an env var name.
	require.Contains(t, keyB64, "=", "the canary must be material-shaped (base64 padding)")

	t.Run("material-shaped ref is refused without echoing it", func(t *testing.T) {
		signer, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: keyB64})
		require.Error(t, err)
		assert.Nil(t, signer)
		assert.ErrorIs(t, err, sign.ErrNoKeyConfigured, "ErrNoKeyConfigured must still wrap")
		assert.NotContains(t, err.Error(), keyB64,
			"key material used as a ref must never be echoed")
		assert.NotContains(t, err.Error(), seedB64,
			"nor may the seed form leak")
	})

	t.Run("a valid variable name is still named in the error", func(t *testing.T) {
		t.Setenv("NOTARY_CANARY_UNSET", "")
		_, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_CANARY_UNSET"})
		require.Error(t, err)
		assert.ErrorIs(t, err, sign.ErrNoKeyConfigured)
		assert.Contains(t, err.Error(), "NOTARY_CANARY_UNSET",
			"a variable name is not secret, so it stays in the error")
	})
}

func signerErrLoosePerms(t *testing.T, content string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "canary.key")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, os.Chmod(path, 0o644))
	_, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceFile, Ref: path})
	return err
}
