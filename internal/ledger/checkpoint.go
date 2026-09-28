package ledger

import (
	"errors"
	"fmt"
	"time"

	"notary/internal/record"
	"notary/internal/sign"
)

// ErrTruncated reports that the chain no longer reaches a state that a signed
// head checkpoint attested to: it was shortened, emptied, or rewritten to a
// different chain. A self-consistent hash chain cannot detect its own removed
// tail, so this is the only failure a checkpoint exists to expose, and it is
// the single most dangerous tamper for an audit trail. It is wrapped with
// context, so callers detect it with errors.Is.
var ErrTruncated = errors.New("ledger: chain is shorter than the checkpoint")

// Checkpoint reads the ledger's head and signs a statement that the chain
// reached it at now. The returned checkpoint attests to the head's (Seq, Hash)
// and names the signer's key, so a later run can tell that the chain was
// shortened.
//
// An empty ledger has no head to attest to, so Checkpoint returns an error
// rather than signing a checkpoint for a nonexistent record. sg may be nil; the
// underlying signer rejects it with an error, never a panic.
func (l *Ledger) Checkpoint(sg *sign.Signer, now time.Time) (sign.Checkpoint, error) {
	head, ok, err := l.store.Head()
	if err != nil {
		return sign.Checkpoint{}, fmt.Errorf("ledger: checkpoint: read head: %w", err)
	}
	if !ok {
		return sign.Checkpoint{}, errors.New("ledger: checkpoint: ledger is empty, so there is no head to attest to")
	}

	cp, err := sign.NewCheckpoint(head.Seq, head.Hash, now, sg)
	if err != nil {
		return sign.Checkpoint{}, fmt.Errorf("ledger: checkpoint: %w", err)
	}
	return cp, nil
}

// VerifyAgainstCheckpoint checks the ledger against a signed head checkpoint,
// catching the one tamper a plain chain walk cannot: truncation. A hash chain
// with its tail deleted still verifies, so Verify alone would report a shortened
// ledger as clean; a checkpoint records how far the chain had reached.
//
// The checkpoint's own signature is verified first, with v.VerifyCheckpoint. A
// checkpoint whose signature does not verify, or whose signer v does not trust,
// is returned as a hard error -- not as a Break -- because an unverifiable
// checkpoint is not evidence: reporting it as a break would let an attacker who
// mangled a checkpoint produce a "tamper detected" reading instead of "your
// checkpoint is bad".
//
// It then compares the store against the checkpoint, in this order:
//
//   - the store is empty while the checkpoint attests to a head: ErrTruncated;
//   - the store head's Seq is less than the checkpoint's Seq: ErrTruncated
//     (the attested history was shortened);
//   - the store head's Seq is at least the checkpoint's Seq: the record at the
//     checkpoint's Seq must carry the checkpoint's Hash. A mismatch -- or a
//     missing or undecodable record at that position -- means the chain was
//     rewritten to a different chain, which is also ErrTruncated.
//
// Appending records after the checkpoint was taken leaves it clean: a
// checkpoint is a lower bound on history, not a fixed head. When the ledger is
// not truncated, it returns a nil break slice and a nil error.
//
// On truncation it returns BOTH a single Break with Field "truncation" (whose
// Detail names the rule that fired) and a non-nil error wrapping ErrTruncated,
// so a caller can print the break uniformly with the rest of those from Verify
// and still detect the condition with errors.Is.
func (l *Ledger) VerifyAgainstCheckpoint(c sign.Checkpoint, v *sign.Verifier) ([]Break, error) {
	// (1) The checkpoint's own signature comes first, before any comparison:
	// an untrustworthy checkpoint proves nothing about the store.
	if err := v.VerifyCheckpoint(c); err != nil {
		return nil, fmt.Errorf("ledger: verify against checkpoint: %w", err)
	}

	// (2) Compare the store against the attested head.
	head, ok, err := l.store.Head()
	if err != nil {
		return nil, fmt.Errorf("ledger: verify against checkpoint: read head: %w", err)
	}
	if !ok {
		return truncation("", c.Seq, fmt.Sprintf(
			"checkpoint attests to seq %d, but the ledger is now empty", c.Seq))
	}
	if head.Seq < c.Seq {
		return truncation("", c.Seq, fmt.Sprintf(
			"checkpoint attests to seq %d, but the ledger head is only at seq %d: the tail was removed",
			c.Seq, head.Seq))
	}

	// The head is at or beyond the checkpoint. It must still carry the attested
	// hash at the checkpoint's Seq, or the chain was rewritten.
	entries, err := l.store.SeqEntries()
	if err != nil {
		return nil, fmt.Errorf("ledger: verify against checkpoint: read chain: %w", err)
	}
	for i := range entries {
		if entries[i].Seq != c.Seq {
			continue
		}
		if entries[i].DecodeErr != nil {
			return truncation(entries[i].ID, c.Seq, fmt.Sprintf(
				"checkpoint attests to hash %x at seq %d, but that record no longer decodes: %v",
				c.Hash[:], c.Seq, entries[i].DecodeErr))
		}
		if entries[i].Rec.Hash != c.Hash {
			return truncation(entries[i].ID, c.Seq, fmt.Sprintf(
				"checkpoint attests to hash %x at seq %d, but the store holds %x: the chain was rewritten",
				c.Hash[:], c.Seq, entries[i].Rec.Hash[:]))
		}
		return nil, nil
	}

	// The head is at or past the checkpoint yet no row sits at that Seq: an
	// interior record was removed, so the attested history is gone.
	return truncation("", c.Seq, fmt.Sprintf(
		"checkpoint attests to seq %d, but the store has no record at that position", c.Seq))
}

// truncation builds the single truncation Break and the ErrTruncated-wrapping
// error that VerifyAgainstCheckpoint returns together. id is the record ID at
// the checkpoint's Seq when it is known (a rewrite or a decode failure names
// it), and empty when the removed tail's identity is unknowable.
func truncation(id record.RecordID, seq uint64, detail string) ([]Break, error) {
	return []Break{{RecordID: id, Seq: seq, Field: fieldTruncation, Detail: detail}},
		fmt.Errorf("ledger: verify against checkpoint: %w: %s", ErrTruncated, detail)
}
