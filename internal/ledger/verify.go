package ledger

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"notary/internal/record"
	"notary/internal/sign"
)

// Field names reported in a Break. They name the exact part of the chain that
// changed, so an auditor can say which byte broke, not merely that it did.
const (
	// fieldHash reports that the record's stored Hash is not ComputeHash over
	// the stored record.
	fieldHash = "hash"
	// fieldPrevHash reports that the record's PrevHash is not its predecessor's
	// Hash (or GenesisHash at the start of the chain).
	fieldPrevHash = "prev_hash"
	// fieldSignature reports that the record's Signature does not verify under
	// a trust the verifier holds (unknown key) or does not verify at all
	// (invalid signature).
	fieldSignature = "signature"
	// fieldSeq reports that the record's chain position is not exactly one more
	// than its predecessor's -- a gap or a duplicate.
	fieldSeq = "seq"
	// fieldDecode reports that a stored row could not be rebuilt into a record
	// at all (a SQLite-level tamper that breaks the Reason).
	fieldDecode = "decode"
)

// chainMinAt and chainMaxAt bound the full-time read used to enumerate the
// chain. The store's ListRecords selects on the At column, so a bounded window
// would silently omit records whose At falls outside it -- the very omission
// verification exists to catch. This window spans the whole range the store's
// fixed-width UTC layout can order: the zero time (the earliest a time.Time can
// hold) through the last instant of year 9999 (the largest value that layout
// sorts last). Since real timestamps are four-digit years, this includes every
// record; the walk then orders by Seq and reports any gap that remains.
var (
	chainMinAt = time.Time{}
	chainMaxAt = time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
)

// Break records one integrity failure found while verifying the chain. It names
// the exact record (RecordID and Seq) and the exact field ("hash", "prev_hash",
// "signature", "seq", or "decode") that broke, with a human-readable Detail.
type Break struct {
	// RecordID identifies the record the break was found on. It is empty only
	// for a decode break, where the row could not be read far enough to name.
	RecordID record.RecordID
	// Seq is the broken record's chain position.
	Seq uint64
	// Field names the part of the record that broke.
	Field string
	// Detail explains the failure in plain language.
	Detail string
}

// Records returns every record in the chain ordered by Seq ascending.
//
// The store exposes no Seq-ordered or by-Seq accessor, so the full set is read
// through ListRecords over the whole representable time range and ordered by
// Seq here. Callers that walk the chain (Verify, and `notary verify --verbose`)
// use this rather than a bounded ListRecords window, so an interior record is
// never silently skipped.
func (l *Ledger) Records() ([]record.Record, error) {
	recs, err := l.store.ListRecords(chainMinAt, chainMaxAt)
	if err != nil {
		return nil, err
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].Seq < recs[j].Seq })
	return recs, nil
}

// Verify walks the chain in Seq order and reports every integrity break it
// finds -- the hash, the chain link, the signature, and the sequence
// continuity of each record -- rather than stopping at the first. On a clean
// chain, and on an empty store, it returns an empty break slice and a nil
// error; it never panics.
//
// The verifier is used as given: Verify stays pure over it and reports a
// signature break for any key the verifier does not trust. Deciding that no
// trusted keys are configured is the caller's job, not this method's.
//
// A stored row that cannot be decoded at all (a SQLite-level tamper that breaks
// the Reason) surfaces as a single break with Field "decode"; because the
// store's bulk read aborts on the first undecodable row, that break carries the
// store's error text rather than a record ID.
func (l *Ledger) Verify(v *sign.Verifier) ([]Break, error) {
	recs, err := l.Records()
	if err != nil {
		if isDecodeError(err) {
			return []Break{{Field: fieldDecode, Detail: err.Error()}}, nil
		}
		return nil, fmt.Errorf("ledger: verify: read chain: %w", err)
	}

	var breaks []Break
	var prev *record.Record
	var expected uint64
	for i := range recs {
		rec := &recs[i]

		// (1) The stored hash must be ComputeHash over the stored record.
		recomputed, herr := record.ComputeHash(*rec)
		switch {
		case herr != nil:
			breaks = append(breaks, Break{rec.ID, rec.Seq, fieldHash, herr.Error()})
		case recomputed != rec.Hash:
			breaks = append(breaks, Break{rec.ID, rec.Seq, fieldHash, fmt.Sprintf(
				"stored hash %x does not match hash recomputed over the record: %x",
				rec.Hash[:], recomputed[:])})
		}

		// (2) The record must link to its predecessor (genesis at the start).
		wantPrev := record.GenesisHash
		what := "genesis hash"
		if prev != nil {
			wantPrev = prev.Hash
			what = fmt.Sprintf("predecessor %s hash", prev.ID)
		}
		if rec.PrevHash != wantPrev {
			breaks = append(breaks, Break{rec.ID, rec.Seq, fieldPrevHash, fmt.Sprintf(
				"prev_hash %x does not match %s %x", rec.PrevHash[:], what, wantPrev[:])})
		}

		// (3) The signature must verify over the stored hash. An unknown key and
		// an invalid signature are distinct Details because the verifier
		// distinguishes them.
		if serr := v.Verify(rec.SignerKeyID, rec.Hash[:], rec.Signature); serr != nil {
			breaks = append(breaks, Break{rec.ID, rec.Seq, fieldSignature, serr.Error()})
		}

		// (4) The chain position must be exactly one past its predecessor.
		if rec.Seq != expected {
			breaks = append(breaks, Break{rec.ID, rec.Seq, fieldSeq, fmt.Sprintf(
				"expected seq %d, found %d", expected, rec.Seq)})
		}

		prev = rec
		expected = rec.Seq + 1
	}
	return breaks, nil
}

// decodeMarkers are fragments of the errors the store returns when a stored row
// cannot be rebuilt into a record. The store wraps each with its own context
// ("store: list records: ...") but defines no sentinel for them, so they are
// recognised by their text; anything that does not match is a genuine read
// failure and is propagated rather than mislabelled as a decode break.
var decodeMarkers = []string{
	"decode reason",
	"parse at",
	"parse recorded_at",
	"disagrees with payload",
	"bytes, want",
}

// isDecodeError reports whether err came from the store failing to rebuild a
// stored row into a record.
func isDecodeError(err error) bool {
	msg := err.Error()
	for _, marker := range decodeMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
