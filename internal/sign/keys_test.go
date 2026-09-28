package sign

import (
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeKeysFile writes content to a fresh temp file and returns its path.
func writeKeysFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trusted-keys.txt")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// pubFor derives a deterministic ed25519 public key from a repeated seed byte,
// so keyring tests do not depend on randomness.
func pubFor(seed byte) ed25519.PublicKey {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = seed
	}
	return ed25519.NewKeyFromSeed(s).Public().(ed25519.PublicKey)
}

// TestLoadTrustedKeysDerivesKeyIDs proves the returned keyring is keyed by the
// KeyID computed from each key itself -- the file never states a KeyID, so a
// declared identity can never disagree with the key actually present.
func TestLoadTrustedKeysDerivesKeyIDs(t *testing.T) {
	pubA := pubFor(0x11)
	pubB := pubFor(0x22)
	content := base64.StdEncoding.EncodeToString(pubA) + "\n" +
		base64.StdEncoding.EncodeToString(pubB) + "\n"

	ring, err := LoadTrustedKeys(writeKeysFile(t, content))
	require.NoError(t, err)
	require.Len(t, ring, 2)
	assert.Equal(t, pubA, ring[keyIDFor(pubA)],
		"the keyring must be keyed by the key-derived KeyID")
	assert.Equal(t, pubB, ring[keyIDFor(pubB)])
}

// TestLoadTrustedKeysSkipsBlanksAndComments proves blank lines and comments --
// including indented ones -- are ignored, and surrounding whitespace is trimmed.
func TestLoadTrustedKeysSkipsBlanksAndComments(t *testing.T) {
	pubA := pubFor(0x33)
	content := "" +
		"# trusted public keys for the audit ledger\n" +
		"\n" +
		"   \n" +
		"   # an indented comment\n" +
		"  " + base64.StdEncoding.EncodeToString(pubA) + "  \n" +
		"\n"

	ring, err := LoadTrustedKeys(writeKeysFile(t, content))
	require.NoError(t, err)
	require.Len(t, ring, 1)
	assert.Contains(t, ring, keyIDFor(pubA))
}

// TestLoadTrustedKeysErrorsOnMalformedBase64WithLineNumber proves a malformed
// line is an error naming the offending line, not a silent skip.
func TestLoadTrustedKeysErrorsOnMalformedBase64WithLineNumber(t *testing.T) {
	content := "# a comment\n" + "not!!base64\n"

	_, err := LoadTrustedKeys(writeKeysFile(t, content))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line 2", "the error must name the offending line")
}

// TestLoadTrustedKeysErrorsOnWrongLengthKey proves a key that does not decode to
// exactly 32 bytes is an error naming the line.
func TestLoadTrustedKeysErrorsOnWrongLengthKey(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("far too short to be a key"))

	_, err := LoadTrustedKeys(writeKeysFile(t, short+"\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "line 1")
}

// TestLoadTrustedKeysErrorsOnMissingFile proves a path that does not exist is an
// error, not an empty keyring.
func TestLoadTrustedKeysErrorsOnMissingFile(t *testing.T) {
	_, err := LoadTrustedKeys(filepath.Join(t.TempDir(), "does-not-exist.txt"))
	require.Error(t, err)
}

// TestLoadTrustedKeysErrorsOnCommentsOnly proves a file that yields no keys once
// comments and blanks are removed is an error, not an empty keyring that would
// make `verify` look like it verified everything.
func TestLoadTrustedKeysErrorsOnCommentsOnly(t *testing.T) {
	content := "# nothing but comments\n#\n\n   \n"

	_, err := LoadTrustedKeys(writeKeysFile(t, content))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no keys",
		"a file with no keys must be an error, not a silent empty keyring")
}
