package record

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
)

// idemDomain separates idempotency-key digests from every other use of
// SHA-256 in Notary. In particular it differs from the record-chain domain
// (hashDomain, "notary/record/v1") so an idempotency key can never be confused
// with a record digest over the same bytes.
const idemDomain = "notary/idem/v1"

// ErrIdemKeyUnavailable is returned by DeriveIdemKey when neither an event ID
// nor a correlation ID is supplied, so there is no content-derived identifier
// to build a key from. Notary must error rather than invent a key from a clock
// or a random source: a non-reproducible key would break deduplication and make
// the reconciler re-append duplicate records.
var ErrIdemKeyUnavailable = errors.New("record: idempotency key unavailable: no event or correlation identifier")

// DeriveIdemKey returns the deterministic idempotency key for a logical event:
//
//	sha256(idemDomain ‖ kind ‖ scope ‖ id ‖ requestDigest[:])
//
// where id is eventID when it is non-empty, otherwise correlationID. If both
// are empty it returns ErrIdemKeyUnavailable and a zero key.
//
// The key is a pure function of its arguments: it carries no clock, no
// randomness, and no counter, and there is no package-level mutable state, so
// the same logical event always derives the same key (spec §7). Determinism is
// not cosmetic -- Task 18 makes Ledger.Append deduplicate on this key, and
// Task 19's reconciler depends on the key being reproducible; a non-reproducible
// key would make reconciliation re-append duplicate records.
//
// The variable-length fields (kind, the four Scope dimensions, and the chosen
// identifier) are each length-prefixed with the shared writeLengthPrefixed
// helper, and requestDigest is a fixed 32 bytes. Length-prefixing is what keeps
// two different events from colliding: without it, {UserID:"ab", AgentID:"c"}
// and {UserID:"a", AgentID:"bc"} would encode identically and one event would
// silently deduplicate the other. Reusing the chain's helper -- rather than a
// second, hand-rolled copy -- keeps the two encoders from drifting apart.
//
// The key is rendered as lowercase hex rather than base64 because it travels
// into SQLite and into log and verify output, where base64's '+', '/', and '='
// characters are awkward; hex matches how hashes are rendered elsewhere in the
// project (internal/gap/wire.go).
//
// DeriveIdemKey never panics; SHA-256 over a byte slice cannot fail. The only
// error it returns is ErrIdemKeyUnavailable.
func DeriveIdemKey(kind EventType, scope Scope, eventID, correlationID string, requestDigest Hash) (IdemKey, error) {
	var id string
	switch {
	case eventID != "":
		id = eventID
	case correlationID != "":
		id = correlationID
	default:
		return IdemKey(""), ErrIdemKeyUnavailable
	}

	var buf bytes.Buffer
	buf.WriteString(idemDomain)
	writeLengthPrefixed(&buf, []byte(kind))
	writeLengthPrefixed(&buf, []byte(scope.UserID))
	writeLengthPrefixed(&buf, []byte(scope.AgentID))
	writeLengthPrefixed(&buf, []byte(scope.AppID))
	writeLengthPrefixed(&buf, []byte(scope.RunID))
	writeLengthPrefixed(&buf, []byte(id))
	writeLengthPrefixed(&buf, requestDigest[:])

	sum := sha256.Sum256(buf.Bytes())
	return IdemKey(hex.EncodeToString(sum[:])), nil
}
