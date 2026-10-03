package ledger

import (
	"fmt"

	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// Field names reported in a Break. They name the exact part of the chain that
// changed, so an auditor can say which byte broke, not merely that it did.
const (
	// fieldHash reports that the record's stored Hash is not ComputeHash over
	// the stored record.
	fieldHash = "hash"
	// fieldPrevHash reports that the record's PrevHash is not its predecessor's
	// Hash (or GenesisHash() at the start of the chain).
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
	// fieldTruncation reports that the chain no longer reaches a signed head
	// checkpoint: it was shortened, rewritten, or emptied. It is emitted by
	// VerifyAgainstCheckpoint, not by Verify, because a hash chain alone cannot
	// see its own removed tail.
	fieldTruncation = "truncation"
)

// Break records one integrity failure found while verifying the chain. It names
// the exact record (RecordID and Seq) and the exact field ("hash", "prev_hash",
// "signature", "seq", "decode", or "truncation") that broke, with a
// human-readable Detail. VerifyAgainstCheckpoint emits a "truncation" break,
// whose RecordID is empty when the shortened tail's identity is unknowable.
type Break struct {
	// RecordID identifies the record the break was found on. It is always
	// populated: even a row that fails to decode carries its identity in the
	// id column, which is read independently of the record payload.
	RecordID record.RecordID
	// Seq is the broken record's chain position. For a "truncation" break it is
	// instead the checkpoint's attested Seq, which may have no surviving row:
	// the shortened tail's own position is gone, so the attested position is the
	// only meaningful one to report.
	Seq uint64
	// Field names the part of the record that broke.
	Field string
	// Detail explains the failure in plain language.
	Detail string
}

// Verify walks the chain in Seq order and reports every integrity break it
// finds -- the hash, the chain link, the signature, and the sequence
// continuity of each record -- rather than stopping at the first. On a clean
// chain, and on an empty store, it returns an empty break slice and a nil
// error; it never panics.
//
// The chain is read through the store's Seq-ordered accessor, which selects on
// seq and does not abort on an undecodable row, so no record can fall outside a
// time window and a hand-edited row is still named by its exact ID and
// position. A row that cannot be decoded surfaces as a break with Field
// "decode"; its own hash and signature cannot be checked, and because its Hash
// is unknown the link check is skipped for its immediate successor.
//
// The verifier is used as given: Verify stays pure over it and reports a
// signature break for any key the verifier does not trust. Deciding that no
// trusted keys are configured is the caller's job, not this method's.
func (l *Ledger) Verify(v *sign.Verifier) ([]Break, error) {
	entries, err := l.store.SeqEntries()
	if err != nil {
		return nil, fmt.Errorf("ledger: verify: read chain: %w", err)
	}
	return verifyChain(entries, v), nil
}

// verifyChain walks the chain rows in Seq order and reports every integrity
// break it finds -- the hash, the chain link, the signature, and the sequence
// continuity of each record -- rather than stopping at the first. On a clean
// chain, and on an empty slice, it returns an empty break slice.
//
// Each row carries its identity and position on its own columns, so a row that
// could not be decoded is still named. Such a row surfaces as a break with
// Field "decode"; its own hash and signature cannot be checked, and because
// its Hash is unknown the link check is skipped for its immediate successor.
//
// The walk expects a chain prefix: it measures sequence continuity from the
// chain's first position, which the ledger numbers seq 0, and expects each
// following row to step by one. A slice that starts there and steps by one
// verifies; a slice with a hole -- a missing interior position -- reports a
// "seq" break naming the gap. This is exactly what an as-of read produces and
// what ReplayAsOf relies on; a caller passing an arbitrary mid-chain slice
// would get false "seq" breaks.
//
// The verifier is used as given: verifyChain stays pure over it and reports a
// signature break for any key the verifier does not trust. Deciding that no
// trusted keys are configured is the caller's job, not this function's.
func verifyChain(entries []store.SeqEntry, v *sign.Verifier) []Break {
	var breaks []Break
	var prev *record.Record
	prevKnown := true
	var expected uint64
	for i := range entries {
		e := entries[i]

		if e.DecodeErr != nil {
			// The row's identity and position come from their own columns, so
			// a payload that will not decode can still be named exactly.
			breaks = append(breaks, Break{e.ID, e.Seq, fieldDecode, e.DecodeErr.Error()})
			// Its Hash is unknown, so the next record's link cannot be checked.
			prev, prevKnown = nil, false
		} else {
			rec := e.Rec

			// (1) The stored hash must be ComputeHash over the stored record.
			recomputed, herr := record.ComputeHash(rec)
			switch {
			case herr != nil:
				breaks = append(breaks, Break{rec.ID, rec.Seq, fieldHash, herr.Error()})
			case recomputed != rec.Hash:
				breaks = append(breaks, Break{rec.ID, rec.Seq, fieldHash, fmt.Sprintf(
					"stored hash %x does not match hash recomputed over the record: %x",
					rec.Hash[:], recomputed[:])})
			}

			// (2) The record must link to its predecessor (genesis at the
			// start). It is skipped when the predecessor's hash is unknown.
			if prevKnown {
				wantPrev := record.GenesisHash()
				what := "genesis hash"
				if prev != nil {
					wantPrev = prev.Hash
					what = fmt.Sprintf("predecessor %s hash", prev.ID)
				}
				if rec.PrevHash != wantPrev {
					breaks = append(breaks, Break{rec.ID, rec.Seq, fieldPrevHash, fmt.Sprintf(
						"prev_hash %x does not match %s %x", rec.PrevHash[:], what, wantPrev[:])})
				}
			}

			// (3) The signature must verify over the stored hash. An unknown key
			// and an invalid signature are distinct Details because the verifier
			// distinguishes them.
			if serr := v.Verify(rec.SignerKeyID, rec.Hash[:], rec.Signature); serr != nil {
				breaks = append(breaks, Break{rec.ID, rec.Seq, fieldSignature, serr.Error()})
			}

			r := rec
			prev, prevKnown = &r, true
		}

		// (4) The chain position must be exactly one past its predecessor.
		// Seq is read from its own column, so this holds for every row,
		// decoded or not.
		if e.Seq != expected {
			breaks = append(breaks, Break{e.ID, e.Seq, fieldSeq, fmt.Sprintf(
				"expected seq %d, found %d", expected, e.Seq)})
		}
		expected = e.Seq + 1
	}
	return breaks
}
