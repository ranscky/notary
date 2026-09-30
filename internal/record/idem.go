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
//	sha256(idemDomain ‖ kind ‖ reasonKind ‖ scope ‖ id ‖ requestDigest[:])
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
// reasonKind is part of the key, not decoration. Spec §7 says "a different
// event OR TIER yields a different key, so later knowledge appends rather than
// mutates", and a ReasonKind maps 1:1 onto a VisibilityTier through
// AllowedTier. Spec §6 emits the SAME event at different tiers: memory_kept is
// Observed (stored_by_mem0) or Reconstructed (kept_by_content_match), and
// memory_dropped is Reconstructed or Internal. With only the event in the key
// those distinct claims -- same event, same scope, same identifier -- would
// derive the same key, and the second would be treated as a duplicate and
// silently never written: exactly the silent data loss this key exists to
// prevent. Hashing reasonKind also separates different bases WITHIN one tier
// (e.g. absent_from_search vs no_facts_extracted), all of which the spec wants
// to append rather than mutate.
//
// The variable-length fields (kind, reasonKind, the four Scope dimensions, and
// the chosen identifier) are each length-prefixed with the shared
// writeLengthPrefixed helper, and requestDigest is a fixed 32 bytes.
// Length-prefixing is what keeps two different events from colliding: without
// it, {UserID:"ab", AgentID:"c"} and {UserID:"a", AgentID:"bc"} would encode
// identically and one event would silently deduplicate the other. Reusing the
// chain's helper -- rather than a second, hand-rolled copy -- keeps the two
// encoders from drifting apart.
//
// The key is rendered as lowercase hex rather than base64 because it travels
// into SQLite and into log and verify output, where base64's '+', '/', and '='
// characters are awkward; hex matches how hashes are rendered elsewhere in the
// project (internal/gap/wire.go).
//
// DeriveIdemKey never panics; SHA-256 over a byte slice cannot fail. The only
// error it returns is ErrIdemKeyUnavailable.
func DeriveIdemKey(kind EventType, reasonKind ReasonKind, scope Scope, eventID, correlationID string, requestDigest Hash) (IdemKey, error) {
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
	writeLengthPrefixed(&buf, []byte(reasonKind))
	writeLengthPrefixed(&buf, []byte(scope.UserID))
	writeLengthPrefixed(&buf, []byte(scope.AgentID))
	writeLengthPrefixed(&buf, []byte(scope.AppID))
	writeLengthPrefixed(&buf, []byte(scope.RunID))
	writeLengthPrefixed(&buf, []byte(id))
	writeLengthPrefixed(&buf, requestDigest[:])

	sum := sha256.Sum256(buf.Bytes())
	return IdemKey(hex.EncodeToString(sum[:])), nil
}

// claimIdemDomain separates the idempotency keys of claims the reconciler
// DERIVES from every other domain. It differs from idemDomain
// ("notary/idem/v1"), so a derived-claim key can never collide with a
// request-path key, and from hashDomain ("notary/record/v1"), so it can never
// be confused with a record digest. A distinct domain is what lets
// DeriveClaimIdemKey reuse the same length-prefixed, lowercase-hex shape as
// DeriveIdemKey without any chance of the two encoders agreeing on a key.
const claimIdemDomain = "notary/idem/claim/v1"

// DeriveClaimIdemKey returns the deterministic idempotency key for a claim the
// reconciler derives about a memory:
//
//	sha256(claimIdemDomain | kind | reasonKind | memoryID | contentHash | ruleVersion)
//
// where every field is length-prefixed with the shared writeLengthPrefixed
// helper, so no field's bytes can be read as another field's boundary.
//
// The key is a pure function of its arguments: it carries no clock, no
// randomness, no counter and no package-level mutable state. Crucially, and by
// design, NOTHING about the reconciliation RUN enters it -- neither the time
// the pass started, nor a pass or attempt counter, nor a cursor. That is the
// single property that makes a second pass a no-op: the same subject, the same
// reason and the same rule always derive the same key, so Ledger.Append treats
// the second write as a duplicate and the reconciler never re-claims.
//
// memoryID is the Mem0 memory the claim concerns and contentHash is the
// subject's content digest (rendered as a string, e.g. hex); at least one must
// be non-empty, since a claim with no subject cannot be keyed. ruleVersion is
// the version of the rule that justified the inference -- not decoration: a
// rule whose meaning changes records a new version, and that version belongs in
// the key so the new claim appends rather than silently deduplicating the old.
// It is legitimately the EMPTY STRING for an observation-based claim, such as
// memory_kept carrying stored_by_mem0: no rule justifies an observation, so
// there is no version to record, and the empty string is the honest encoding
// of "no rule" rather than a missing value to be filled in. The claim stays
// subject-specific because reasonKind and kind separate it from every other
// claim -- including the reconstructed kept_by_content_match, which does carry
// ruleVersion "1".
// reasonKind is the claim's reason kind and kind its event type; both are in
// the key for the same reason DeriveIdemKey includes them: the same event at a
// different tier, or on a different basis, is a different claim.
//
// If memoryID and contentHash are both empty there is no subject to key on, so
// it returns ErrIdemKeyUnavailable and a zero key. The key is lowercase hex,
// matching DeriveIdemKey and how hashes are rendered elsewhere.
// DeriveClaimIdemKey never panics; SHA-256 over a byte slice cannot fail.
func DeriveClaimIdemKey(kind EventType, reasonKind ReasonKind, memoryID, contentHash, ruleVersion string) (IdemKey, error) {
	if memoryID == "" && contentHash == "" {
		return IdemKey(""), ErrIdemKeyUnavailable
	}

	var buf bytes.Buffer
	buf.WriteString(claimIdemDomain)
	writeLengthPrefixed(&buf, []byte(kind))
	writeLengthPrefixed(&buf, []byte(reasonKind))
	writeLengthPrefixed(&buf, []byte(memoryID))
	writeLengthPrefixed(&buf, []byte(contentHash))
	writeLengthPrefixed(&buf, []byte(ruleVersion))

	sum := sha256.Sum256(buf.Bytes())
	return IdemKey(hex.EncodeToString(sum[:])), nil
}
