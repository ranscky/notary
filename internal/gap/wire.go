package gap

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"notary/internal/record"
)

// instantLayout is the fixed-width UTC instant layout used for At. Exactly nine
// fractional digits are always written, so every value has the same width.
//
// time.RFC3339Nano cannot be used: it trims trailing zeros, so a whole-second
// value and a sub-second value would have different widths and their lexical
// order would disagree with chronological order ('Z' sorts after '.'). That
// variable width is exactly the ambiguity the gap hash must not have.
const instantLayout = "2006-01-02T15:04:05.000000000Z07:00"

// hashEntry returns the digest of e:
//
//	sha256(gapDomain ‖ uint64-BE(Counter) ‖ instantLayout(At.UTC) ‖ Kind ‖
//	       Scope fields ‖ CorrelationID ‖ Detail ‖ PrevHash[:])
//
// Every variable-length field (the formatted instant, the event kind, the four
// Scope dimensions, CorrelationID, and Detail) is length-prefixed with a
// big-endian uint32 byte count followed by its bytes, so no field's bytes can
// be read as another field's boundary. Without this, {UserID:"ab", AgentID:"c"}
// and {UserID:"a", AgentID:"bc"} would encode identically and a gap could be
// rewritten without detection. Counter is a fixed 8 bytes, PrevHash a fixed 32
// bytes, and the instant is fixed-width, so no width is variable.
//
// hashEntry never panics; SHA-256 over a byte slice cannot fail.
func hashEntry(e Entry) record.Hash {
	var buf bytes.Buffer
	buf.WriteString(gapDomain)

	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], e.Counter)
	buf.Write(counter[:])

	writeLengthPrefixed(&buf, e.At.UTC().Format(instantLayout))
	writeLengthPrefixed(&buf, string(e.Kind))
	writeLengthPrefixed(&buf, e.Scope.UserID)
	writeLengthPrefixed(&buf, e.Scope.AgentID)
	writeLengthPrefixed(&buf, e.Scope.AppID)
	writeLengthPrefixed(&buf, e.Scope.RunID)
	writeLengthPrefixed(&buf, e.CorrelationID)
	writeLengthPrefixed(&buf, e.Detail)
	buf.Write(e.PrevHash[:])

	return sha256.Sum256(buf.Bytes())
}

// writeLengthPrefixed appends a big-endian uint32 length followed by s's bytes.
func writeLengthPrefixed(buf *bytes.Buffer, s string) {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(s)))
	buf.Write(n[:])
	buf.WriteString(s)
}

// wireEntry is the on-disk JSON shape of an Entry. Hashes are lowercase hex and
// At uses the fixed-width instant layout, so the file is stable and readable
// while the hash is computed over the canonical bytes, not this JSON.
type wireEntry struct {
	Counter       uint64    `json:"counter"`
	At            string    `json:"at"`
	Kind          string    `json:"kind"`
	Scope         wireScope `json:"scope"`
	CorrelationID string    `json:"correlation_id"`
	Detail        string    `json:"detail"`
	PrevHash      string    `json:"prev_hash"`
	Hash          string    `json:"hash"`
}

// wireScope is the on-disk shape of a record.Scope.
type wireScope struct {
	UserID  string `json:"user_id"`
	AgentID string `json:"agent_id"`
	AppID   string `json:"app_id"`
	RunID   string `json:"run_id"`
}

// encodeEntry marshals e to its single-line JSON wire form (without the
// trailing newline). It returns a wrapped error and never panics.
func encodeEntry(e Entry) ([]byte, error) {
	b, err := json.Marshal(wireEntry{
		Counter:       e.Counter,
		At:            e.At.UTC().Format(instantLayout),
		Kind:          string(e.Kind),
		Scope:         wireScope{e.Scope.UserID, e.Scope.AgentID, e.Scope.AppID, e.Scope.RunID},
		CorrelationID: e.CorrelationID,
		Detail:        e.Detail,
		PrevHash:      hex.EncodeToString(e.PrevHash[:]),
		Hash:          hex.EncodeToString(e.Hash[:]),
	})
	if err != nil {
		return nil, fmt.Errorf("gap: encode entry %d: %w", e.Counter, err)
	}
	return b, nil
}

// decodeLine decodes one JSON line into an Entry. A line that is not valid JSON
// or whose hashes or timestamp are malformed returns a wrapped error and never
// panics.
func decodeLine(raw []byte) (Entry, error) {
	var w wireEntry
	if err := json.Unmarshal(raw, &w); err != nil {
		return Entry{}, fmt.Errorf("gap: decode entry: %w", err)
	}
	at, err := time.Parse(instantLayout, w.At)
	if err != nil {
		return Entry{}, fmt.Errorf("gap: decode entry: parse at %q: %w", w.At, err)
	}
	prev, err := hashFromHex("prev_hash", w.PrevHash)
	if err != nil {
		return Entry{}, fmt.Errorf("gap: decode entry: %w", err)
	}
	h, err := hashFromHex("hash", w.Hash)
	if err != nil {
		return Entry{}, fmt.Errorf("gap: decode entry: %w", err)
	}
	return Entry{
		Counter:       w.Counter,
		At:            at,
		Kind:          record.EventType(w.Kind),
		Scope:         record.Scope{UserID: w.Scope.UserID, AgentID: w.Scope.AgentID, AppID: w.Scope.AppID, RunID: w.Scope.RunID},
		CorrelationID: w.CorrelationID,
		Detail:        w.Detail,
		PrevHash:      prev,
		Hash:          h,
	}, nil
}

// hashFromHex decodes a fixed-width hash field, rejecting any width other than
// the record.Hash width so a truncated hash cannot pass as a real one.
func hashFromHex(field, s string) (record.Hash, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return record.Hash{}, fmt.Errorf("%s is not valid hex: %w", field, err)
	}
	if len(b) != 32 {
		return record.Hash{}, fmt.Errorf("%s is %d bytes, want 32", field, len(b))
	}
	var h record.Hash
	copy(h[:], b)
	return h, nil
}
