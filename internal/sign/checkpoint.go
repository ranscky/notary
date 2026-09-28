package sign

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"notary/internal/record"
)

// checkpointDomain is the domain-separation prefix of the signed message. It
// carries the checkpoint wire version ("v1"), so no second version constant is
// needed. Because it is a fixed-length ASCII string that no other signed message
// in this package uses, a checkpoint signature can never be mistaken for a
// signature over a ledger record (or vice versa).
const checkpointDomain = "notary/checkpoint/v1"

// Checkpoint is a signed statement that a hash chain reached a given head at a
// given instant: "(Seq, Hash) at At", signed by the key identified by
// SignerKeyID. A self-consistent hash chain cannot detect truncation — delete
// the tail and the remainder still verifies — so a checkpoint records how far
// the chain had reached, letting a later run tell that the chain was shortened.
//
// A Checkpoint holds public data plus a signature only. It never carries key
// material, and it carries no expiry: freshness policy is the caller's concern.
//
// The canonical encoding of a Checkpoint is produced by MarshalCheckpoint and
// consumed by UnmarshalCheckpoint. Checkpoint's MarshalJSON and UnmarshalJSON
// delegate to those functions, so the obvious json.Marshal/json.Unmarshal path
// is the canonical one rather than a footgun: a bare json.Marshal would
// otherwise encode record.Hash as a 32-element number array that
// UnmarshalCheckpoint then rejects. See MarshalCheckpoint for the wire format.
type Checkpoint struct {
	// Seq is the chain position of the signed head: the sequence number of the
	// record the head hash belongs to.
	Seq uint64 `json:"seq"`
	// Hash is the hash of the chain head at Seq.
	Hash record.Hash `json:"hash"`
	// At is when the head was observed, always stored in UTC.
	At time.Time `json:"at"`
	// SignerKeyID is the key ID (public fingerprint) the signature is attributed
	// to. Verification resolves it against the trusted keyring.
	SignerKeyID string `json:"signer_key_id"`
	// Signature is the ed25519 signature over the canonical signed message.
	Signature []byte `json:"signature"`
}

// NewCheckpoint signs a statement that the chain reached (seq, hash) at the
// instant at, and returns the resulting Checkpoint. It fills SignerKeyID from
// signer.KeyID().
//
// at is normalised to UTC and the caller's time.Time is not mutated. A nil
// signer is a returned error, never a panic.
func NewCheckpoint(seq uint64, hash record.Hash, at time.Time, signer *Signer) (Checkpoint, error) {
	if signer == nil {
		return Checkpoint{}, errors.New("sign: cannot create checkpoint with a nil signer")
	}
	utc := at.UTC()
	sig, err := signer.Sign(checkpointMessage(seq, hash, utc))
	if err != nil {
		return Checkpoint{}, fmt.Errorf("sign: signing checkpoint: %w", err)
	}
	return Checkpoint{
		Seq:         seq,
		Hash:        hash,
		At:          utc,
		SignerKeyID: signer.KeyID(),
		Signature:   sig,
	}, nil
}

// VerifyCheckpoint checks a checkpoint's signature under the public key trusted
// for c.SignerKeyID. It reconstructs the canonical signed message from the
// checkpoint's own fields, so any change to Seq, Hash, or At invalidates the
// signature.
//
// It returns a wrapped ErrUnknownKey when c.SignerKeyID is absent from the
// keyring and a wrapped ErrInvalidSignature when the signature does not match. A
// zero-value Checkpoint is safe: verification reports the empty key ID as
// unknown rather than panicking.
func (v *Verifier) VerifyCheckpoint(c Checkpoint) error {
	if err := v.Verify(c.SignerKeyID, checkpointMessage(c.Seq, c.Hash, c.At), c.Signature); err != nil {
		return fmt.Errorf("sign: verifying checkpoint: %w", err)
	}
	return nil
}

// checkpointMessage builds the canonical bytes that a checkpoint signs:
//
//	"notary/checkpoint/v1" ‖ uint64-BE(Seq) ‖ Hash[:] ‖ Timestamp
//
// where Timestamp is At.UTC().Format(time.RFC3339Nano). This is the exact
// documented form; there is no length prefix on the timestamp. The domain
// prefix, Seq, and Hash are fixed width (19 + 8 + 32 = 59 bytes), and the
// timestamp is the last segment, so everything after byte 59 is unambiguous —
// no two distinct (Seq, Hash, At) triples can produce the same signed bytes.
// In particular RFC3339Nano's trailing-zero trimming is injective over
// nanosecond instants, so a whole second and a nanosecond later sign distinct
// messages.
func checkpointMessage(seq uint64, hash record.Hash, at time.Time) []byte {
	ts := at.UTC().Format(time.RFC3339Nano)

	var buf bytes.Buffer
	buf.Grow(len(checkpointDomain) + 8 + len(hash) + len(ts))
	buf.WriteString(checkpointDomain)

	var seqBuf [8]byte
	binary.BigEndian.PutUint64(seqBuf[:], seq)
	buf.Write(seqBuf[:])

	buf.Write(hash[:])
	buf.WriteString(ts)
	return buf.Bytes()
}

// checkpointJSON is the JSON wire shape of a Checkpoint, used by
// MarshalCheckpoint and UnmarshalCheckpoint. Every exported field carries a
// json tag.
//
// Hash and Signature are encoded as lowercase hexadecimal strings rather than
// JSON arrays or base64. Hex is what an auditor reads directly, and a string
// encoding lets UnmarshalCheckpoint detect a missing or wrong-length value
// (an encoded array or a zero-filled record.Hash could not be distinguished
// from a legitimately absent one).
type checkpointJSON struct {
	Seq         uint64 `json:"seq"`
	Hash        string `json:"hash"`
	At          string `json:"at"`
	SignerKeyID string `json:"signer_key_id"`
	Signature   string `json:"signature"`
}

// MarshalCheckpoint encodes a checkpoint as indented JSON for a human auditor.
//
// Wire format (stable):
//
//	{
//	  "seq":           <uint64>,
//	  "hash":          <lowercase hex of the 32-byte chain-head hash>,
//	  "at":            <RFC3339Nano, UTC>,
//	  "signer_key_id": <key ID string>,
//	  "signature":     <lowercase hex of the 64-byte ed25519 signature>
//	}
//
// The two-space indentation is deliberate: this artifact is meant to be read,
// not just parsed. UnmarshalCheckpoint is the exact inverse.
func MarshalCheckpoint(c Checkpoint) ([]byte, error) {
	wire := checkpointJSON{
		Seq:         c.Seq,
		Hash:        hex.EncodeToString(c.Hash[:]),
		At:          c.At.UTC().Format(time.RFC3339Nano),
		SignerKeyID: c.SignerKeyID,
		Signature:   hex.EncodeToString(c.Signature),
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("sign: marshalling checkpoint: %w", err)
	}
	return data, nil
}

// UnmarshalCheckpoint decodes JSON produced by MarshalCheckpoint (see its doc
// comment for the wire format). Every field is validated; on any defect it
// returns a zero-value Checkpoint and a non-nil error rather than a
// partially-populated struct. It rejects:
//
//   - invalid JSON,
//   - a Hash that is missing, not valid hex, or not 32 bytes,
//   - a Signature that is missing, not valid hex, or not 64 bytes,
//   - an empty SignerKeyID,
//   - an At that is not a valid RFC3339 timestamp.
//
// There is no best-effort fallback: malformed input is always an error.
func UnmarshalCheckpoint(data []byte) (Checkpoint, error) {
	var wire checkpointJSON
	if err := json.Unmarshal(data, &wire); err != nil {
		return Checkpoint{}, fmt.Errorf("sign: checkpoint is not valid JSON: %w", err)
	}

	rawHash, err := hex.DecodeString(wire.Hash)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("sign: checkpoint hash is not valid hex: %w", err)
	}
	if want := len(record.Hash{}); len(rawHash) != want {
		return Checkpoint{}, fmt.Errorf("sign: checkpoint hash must be %d bytes, got %d", want, len(rawHash))
	}

	rawSig, err := hex.DecodeString(wire.Signature)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("sign: checkpoint signature is not valid hex: %w", err)
	}
	if len(rawSig) != ed25519.SignatureSize {
		return Checkpoint{}, fmt.Errorf("sign: checkpoint signature must be %d bytes, got %d",
			ed25519.SignatureSize, len(rawSig))
	}

	if wire.SignerKeyID == "" {
		return Checkpoint{}, errors.New("sign: checkpoint signer_key_id must not be empty")
	}

	at, err := time.Parse(time.RFC3339Nano, wire.At)
	if err != nil {
		return Checkpoint{}, fmt.Errorf("sign: checkpoint at is not a valid RFC3339 timestamp: %w", err)
	}

	var hash record.Hash
	copy(hash[:], rawHash)

	return Checkpoint{
		Seq:         wire.Seq,
		Hash:        hash,
		At:          at.UTC(),
		SignerKeyID: wire.SignerKeyID,
		Signature:   rawSig,
	}, nil
}

// MarshalJSON implements json.Marshaler by delegating to MarshalCheckpoint, so
// json.Marshal(checkpoint) emits the canonical hex wire form. It uses a value
// receiver, so both Checkpoint and *Checkpoint are covered — the same lesson as
// Signer's redaction methods in sign.go.
func (c Checkpoint) MarshalJSON() ([]byte, error) {
	return MarshalCheckpoint(c)
}

// UnmarshalJSON implements json.Unmarshaler by delegating to
// UnmarshalCheckpoint, so json.Unmarshal into a Checkpoint rejects the same
// malformed input and never leaves a partially-populated value. The pointer
// receiver is required to write through to the caller's value.
func (c *Checkpoint) UnmarshalJSON(data []byte) error {
	decoded, err := UnmarshalCheckpoint(data)
	if err != nil {
		return err
	}
	*c = decoded
	return nil
}
