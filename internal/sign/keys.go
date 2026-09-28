package sign

import (
	"bufio"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// LoadTrustedKeys reads a file of base64-encoded ed25519 public keys, one per
// line, and returns a keyring mapping each key's derived KeyID to the key.
//
// Blank lines, and lines whose first non-space character is '#', are ignored;
// surrounding whitespace is trimmed. The file never states a KeyID -- each is
// derived from the key bytes via keyIDFor -- because a file that could declare
// a KeyID alongside a key would admit a mismatch where the declared identity
// differs from the key actually used, which is a verification-integrity hole.
//
// A malformed base64 line, a key that does not decode to exactly
// ed25519.PublicKeySize bytes, a file that yields no keys once comments are
// removed, and a path that cannot be opened are all errors (never silent
// skips); the first two name the offending line number. It never panics.
func LoadTrustedKeys(path string) (map[string]ed25519.PublicKey, error) {
	if path == "" {
		return nil, fmt.Errorf("sign: trusted keys path is empty")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("sign: open trusted keys file: %w", err)
	}
	defer func() { _ = f.Close() }()

	keyring := make(map[string]ed25519.PublicKey)
	sc := bufio.NewScanner(f)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return nil, fmt.Errorf("sign: trusted keys file %s line %d: not valid standard base64: %w",
				path, lineNo, err)
		}
		if len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("sign: trusted keys file %s line %d: key decodes to %d bytes, want %d",
				path, lineNo, len(raw), ed25519.PublicKeySize)
		}

		pub := make(ed25519.PublicKey, ed25519.PublicKeySize)
		copy(pub, raw)
		keyring[keyIDFor(pub)] = pub
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sign: reading trusted keys file %s: %w", path, err)
	}
	if len(keyring) == 0 {
		return nil, fmt.Errorf("sign: trusted keys file %s contains no keys", path)
	}
	return keyring, nil
}
