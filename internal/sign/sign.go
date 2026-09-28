// Package sign provides the ed25519 signing and verification primitives behind
// Notary's tamper-evident audit trail. The ledger signs every record with a
// Signer, and `notary verify` checks those signatures with a Verifier built
// from a trusted keyring.
//
// Key hygiene is a security invariant of this package:
//
//   - NewSigner never generates a key. If no key is configured it returns
//     ErrNoKeyConfigured and a nil Signer, so a throwaway key can never be
//     mistaken for the operator's real signing identity.
//   - Loaded key material is wrapped in a type that redacts itself on every
//     formatting and JSON path, so it cannot leak through logs or errors.
package sign

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// KeySourceKind identifies where signing key material is loaded from.
type KeySourceKind uint8

const (
	// KeySourceEnv reads base64 key material from an environment variable.
	KeySourceEnv KeySourceKind = iota + 1
	// KeySourceFile reads base64 key material from a dev-only file that must be
	// mode 0600 and live outside the repository.
	KeySourceFile
	// KeySourceKeychain is reserved for an OS keychain. This platform has no
	// keychain API available to us, so it is unsupported (ErrKeychainUnsupported)
	// and never silently falls back to a file or the environment.
	KeySourceKeychain
)

// KeySource names where signing key material should be loaded from. It carries
// no key material itself and is safe to log.
type KeySource struct {
	// Kind selects the loading mechanism.
	Kind KeySourceKind
	// Ref identifies the key: an environment variable name for KeySourceEnv, or a
	// file path for KeySourceFile.
	Ref string
}

// Sentinel errors, compared with errors.Is.
var (
	// ErrNoKeyConfigured reports that no signing key was configured. NewSigner
	// never generates a key to fill the gap.
	ErrNoKeyConfigured = errors.New("sign: no signing key configured")
	// ErrKeychainUnsupported reports that KeySourceKeychain is unavailable on
	// this platform.
	ErrKeychainUnsupported = errors.New("sign: OS keychain is unsupported on this platform")
	// ErrLoosePerms reports that a key file's permissions are not exactly 0600.
	ErrLoosePerms = errors.New("sign: key file permissions must be 0600")
	// ErrUnknownKey reports that a signature was checked against a KeyID that is
	// absent from the verifier's trusted keyring.
	ErrUnknownKey = errors.New("sign: unknown key")
	// ErrInvalidSignature reports that a signature did not verify under the
	// trusted public key.
	ErrInvalidSignature = errors.New("sign: invalid signature")
)

// redactedPlaceholder is emitted by every printable path of secretKey.
const redactedPlaceholder = "[redacted]"

// keyFilePerm is the only acceptable permission mode for a key file.
const keyFilePerm = os.FileMode(0o600)

// secretKey holds ed25519 private key material. It deliberately implements
// String, GoString, Format, and MarshalJSON so that no fmt verb (%v, %+v, %s,
// %q, %x, %#v) and no json.Marshal call can print the raw key bytes.
type secretKey struct {
	key ed25519.PrivateKey
}

// String implements fmt.Stringer.
func (secretKey) String() string { return redactedPlaceholder }

// GoString implements fmt.GoStringer, catching the %#v verb.
func (secretKey) GoString() string { return redactedPlaceholder }

// Format implements fmt.Formatter, catching %v, %+v, %s, %q, %x and friends.
func (secretKey) Format(f fmt.State, _ rune) { fmt.Fprint(f, redactedPlaceholder) }

// MarshalJSON implements json.Marshaler.
func (secretKey) MarshalJSON() ([]byte, error) { return json.Marshal(redactedPlaceholder) }

// Signer signs messages with a loaded ed25519 private key. Its key material is
// held in an unexported, self-redacting field and is never returned from any
// exported method.
//
// Signer redacts itself too, with value-receiver methods, so that both Signer
// and *Signer satisfy fmt.Stringer, fmt.Formatter, fmt.GoStringer and
// json.Marshaler. This is load-bearing for %#v: when fmt prints an outer struct
// in Go syntax it emits the whole value and never delegates to a nested field's
// GoStringer, so no method on the inner key type could ever stop %#v from
// revealing the private key on Signer.
type Signer struct {
	secretKey secretKey
	keyID     string
}

// String implements fmt.Stringer, redacting the private key.
func (s Signer) String() string { return redactedPlaceholder }

// GoString implements fmt.GoStringer, redacting the private key under %#v.
func (s Signer) GoString() string { return redactedPlaceholder }

// Format implements fmt.Formatter, redacting the private key under every verb,
// including %v, %+v, %s, %q, %x and %#v.
func (s Signer) Format(f fmt.State, _ rune) { io.WriteString(f, redactedPlaceholder) }

// MarshalJSON implements json.Marshaler, redacting the private key.
func (s Signer) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// NewSigner loads signing key material from src and returns a Signer ready to
// sign messages.
//
// NewSigner never generates a key: a missing or empty source yields
// ErrNoKeyConfigured and a nil Signer, so an operator cannot mistake a
// throwaway key for the configured signing identity.
func NewSigner(src KeySource) (*Signer, error) {
	var (
		key ed25519.PrivateKey
		err error
	)
	switch src.Kind {
	case KeySourceEnv:
		key, err = loadFromEnv(src.Ref)
	case KeySourceFile:
		key, err = loadFromFile(src.Ref)
	case KeySourceKeychain:
		return nil, ErrKeychainUnsupported
	default:
		return nil, fmt.Errorf("%w: unknown key source kind %d", ErrNoKeyConfigured, src.Kind)
	}
	if err != nil {
		return nil, err
	}
	return newSigner(key)
}

// newSigner validates a loaded private key and derives its public fingerprint.
func newSigner(key ed25519.PrivateKey) (*Signer, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("sign: loaded private key has length %d, want %d",
			len(key), ed25519.PrivateKeySize)
	}
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("sign: private key did not yield an ed25519 public key")
	}
	return &Signer{
		secretKey: secretKey{key: key},
		keyID:     keyIDFor(pub),
	}, nil
}

// loadFromEnv reads base64 key material from the environment variable named ref.
func loadFromEnv(ref string) (ed25519.PrivateKey, error) {
	if ref == "" {
		return nil, fmt.Errorf("%w: no environment variable name provided", ErrNoKeyConfigured)
	}
	material := strings.TrimSpace(os.Getenv(ref))
	if material == "" {
		return nil, fmt.Errorf("%w: environment variable %q is unset or empty", ErrNoKeyConfigured, ref)
	}
	return parsePrivateKey(material)
}

// loadFromFile reads base64 key material from the 0600 file at path.
func loadFromFile(path string) (ed25519.PrivateKey, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: no key file path provided", ErrNoKeyConfigured)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("sign: reading key file: %w", err)
	}
	if perm := info.Mode().Perm(); perm != keyFilePerm {
		return nil, fmt.Errorf("%w: %s has mode %04o", ErrLoosePerms, path, perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("sign: reading key file: %w", err)
	}
	return parsePrivateKey(strings.TrimSpace(string(data)))
}

// parsePrivateKey decodes base64 key material. It accepts either a 32-byte
// ed25519 seed or a 64-byte full private key and rejects anything else. The
// returned errors never include the key material.
func parsePrivateKey(material string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(material)
	if err != nil {
		return nil, fmt.Errorf("sign: key material is not valid standard base64: %w", err)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		key := make(ed25519.PrivateKey, ed25519.PrivateKeySize)
		copy(key, raw)
		return key, nil
	default:
		return nil, fmt.Errorf("sign: key material must decode to %d bytes (seed) or %d bytes (private key), got %d",
			ed25519.SeedSize, ed25519.PrivateKeySize, len(raw))
	}
}

// keyIDFor returns the stable public fingerprint of a public key: the standard
// base64 encoding of SHA-256 over the public key bytes. It depends only on the
// public key, so it is safe to log and share.
func keyIDFor(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return base64.StdEncoding.EncodeToString(sum[:])
}

// KeyID returns the signer's stable, loggable public fingerprint. It derives
// from the public key only, so two signers with the same key share a KeyID and
// different keys differ.
func (s *Signer) KeyID() string { return s.keyID }

// Sign returns the ed25519 signature of msg produced by the signer's private
// key. ed25519 signing is deterministic and cannot fail for a valid key.
func (s *Signer) Sign(msg []byte) ([]byte, error) {
	return ed25519.Sign(s.secretKey.key, msg), nil
}

// Verifier checks message signatures against a trusted keyring of public keys
// keyed by KeyID.
type Verifier struct {
	keyring map[string]ed25519.PublicKey
}

// NewVerifier builds a Verifier over the supplied trusted keyring. A nil or
// empty keyring is allowed: such a Verifier trusts no keys, and Verify reports
// every key as unknown rather than panicking.
func NewVerifier(keyring map[string]ed25519.PublicKey) *Verifier {
	return &Verifier{keyring: keyring}
}

// Verify checks that sig is a valid signature of msg under the public key
// trusted for keyID. It returns ErrUnknownKey (wrapped) when keyID is absent
// from the keyring and ErrInvalidSignature (wrapped) when the signature does
// not verify, so callers can distinguish the two cases.
func (v *Verifier) Verify(keyID string, msg, sig []byte) error {
	var pub ed25519.PublicKey
	if v != nil {
		pub = v.keyring[keyID]
	}
	if pub == nil {
		return fmt.Errorf("%w: %q is not in the trusted keyring", ErrUnknownKey, keyID)
	}
	if !ed25519.Verify(pub, msg, sig) {
		return fmt.Errorf("%w: signature does not match key %q", ErrInvalidSignature, keyID)
	}
	return nil
}
