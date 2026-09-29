package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"time"
)

// GenesisHash returns the PrevHash of the first record in a ledger: 32 zero
// bytes. It is a fixed sentinel rather than a hash of anything, so the genesis
// link is unambiguous and can never collide with a real digest.
//
// It is a function, not a package variable, so no caller in any package can
// reassign it and silently redefine where a chain starts -- which would make
// verification accept or reject the wrong genesis link. Go has no const
// arrays, so a function is the form that makes the value immutable while
// keeping the name recognisable.
func GenesisHash() Hash { return Hash{} }

// hashDomain separates Notary's record digests from every other use of
// SHA-256, so a digest produced here can never be confused with a bare hash of
// the same bytes computed elsewhere.
const hashDomain = "notary/record/v1"

// CanonicalBytes returns the deterministic byte encoding of r over which the
// record hash is computed.
//
// It writes the fields in a fixed order: ID, Event, Reason, Subject
// (MemoryID, the Scope dimensions, ContentHash), Content, IdempotencyKey, At,
// RecordedAt, and SignerKeyID. The variable-length byte fields (the identifiers,
// the Reason encoding, the Scope dimensions, Content.Text, IdempotencyKey, the
// formatted timestamps, and SignerKeyID) are each length-prefixed with a
// big-endian uint32 byte count followed by the bytes, so no field's bytes can
// be read as another field's boundary. The fixed-width values are written
// directly: ContentHash is its raw 32 bytes, Content is preceded by a one-byte
// presence flag (0 for nil, 1 for present), and when present its Sensitive flag
// is a single byte. The presence flag is what keeps a nil *Content from
// encoding identically to a non-nil but empty one.
//
// Four fields are deliberately excluded, each for a specific reason:
//
//   - Hash is the output of ComputeHash, so feeding it back in would make the
//     digest depend on itself.
//   - Signature is computed over the hash, so including it would be circular
//     and would make the hash unverifiable with the signature stripped.
//   - Seq and PrevHash are the record's chain position. They are excluded here
//     and mixed in separately by ComputeHash, so that a record's content
//     encoding is independent of where in the chain it sits.
//
// For the Reason segment, CanonicalBytes reuses the already-versioned,
// deterministic Reason.Encode rather than re-deriving a reason encoding: a
// second encoding would be free to diverge from the one the store persists.
// Its error is propagated.
//
// A presence flag precedes Content so that a nil *Content and a non-nil but
// empty *Content encode differently -- they are different claims and must not
// collide. At and RecordedAt are normalised with UTC before formatting as
// time.RFC3339Nano, so two equal instants in different zones encode
// identically.
//
// CanonicalBytes never panics; every fallible step returns a wrapped error.
func CanonicalBytes(r Record) ([]byte, error) {
	var buf bytes.Buffer

	writeLengthPrefixed(&buf, []byte(r.ID))
	writeLengthPrefixed(&buf, []byte(r.Event))

	reasonBytes, err := r.Reason.Encode()
	if err != nil {
		return nil, fmt.Errorf("record: canonicalise reason for %s: %w", r.ID, err)
	}
	writeLengthPrefixed(&buf, reasonBytes)

	writeLengthPrefixed(&buf, []byte(r.Subject.MemoryID))
	writeLengthPrefixed(&buf, []byte(r.Subject.Scope.UserID))
	writeLengthPrefixed(&buf, []byte(r.Subject.Scope.AgentID))
	writeLengthPrefixed(&buf, []byte(r.Subject.Scope.AppID))
	writeLengthPrefixed(&buf, []byte(r.Subject.Scope.RunID))
	writeLengthPrefixed(&buf, r.Subject.ContentHash[:])

	if r.Content == nil {
		buf.WriteByte(0)
	} else {
		buf.WriteByte(1)
		writeLengthPrefixed(&buf, []byte(r.Content.Text))
		if r.Content.Sensitive {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
	}

	writeLengthPrefixed(&buf, []byte(r.IdempotencyKey))
	writeLengthPrefixed(&buf, []byte(r.At.UTC().Format(time.RFC3339Nano)))
	writeLengthPrefixed(&buf, []byte(r.RecordedAt.UTC().Format(time.RFC3339Nano)))
	writeLengthPrefixed(&buf, []byte(r.SignerKeyID))

	return buf.Bytes(), nil
}

// writeLengthPrefixed appends a big-endian uint32 length followed by b's bytes.
func writeLengthPrefixed(buf *bytes.Buffer, b []byte) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	buf.Write(n[:])
	buf.Write(b)
}

// ComputeHash returns the record's digest:
//
//	sha256(hashDomain ‖ CanonicalBytes(r) ‖ uint64-BE(r.Seq) ‖ r.PrevHash[:])
//
// The hash covers both the record's content (through CanonicalBytes) and its
// chain position (Seq and PrevHash, mixed in here), so changing any field or
// moving a record to a different link changes the digest. It returns an error
// only when CanonicalBytes cannot encode the record -- for example an invalid
// Reason -- and never panics.
func ComputeHash(r Record) (Hash, error) {
	canon, err := CanonicalBytes(r)
	if err != nil {
		return Hash{}, fmt.Errorf("record: compute hash for %s: %w", r.ID, err)
	}

	h := sha256.New()
	if _, err := h.Write([]byte(hashDomain)); err != nil {
		return Hash{}, fmt.Errorf("record: compute hash for %s: %w", r.ID, err)
	}
	if _, err := h.Write(canon); err != nil {
		return Hash{}, fmt.Errorf("record: compute hash for %s: %w", r.ID, err)
	}
	var seqBytes [8]byte
	binary.BigEndian.PutUint64(seqBytes[:], r.Seq)
	if _, err := h.Write(seqBytes[:]); err != nil {
		return Hash{}, fmt.Errorf("record: compute hash for %s: %w", r.ID, err)
	}
	if _, err := h.Write(r.PrevHash[:]); err != nil {
		return Hash{}, fmt.Errorf("record: compute hash for %s: %w", r.ID, err)
	}

	var out Hash
	copy(out[:], h.Sum(nil))
	return out, nil
}

// LinkOK reports whether cur is correctly linked to prev: it is true exactly
// when cur.PrevHash equals prev.Hash. It is a convenience for chain
// verification and deliberately does no more than that comparison.
func LinkOK(prev, cur Record) bool {
	return cur.PrevHash == prev.Hash
}
