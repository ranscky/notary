package gap

import (
	"errors"
	"fmt"
	"time"

	"notary/internal/sign"
)

// ErrTruncated reports that the gap log no longer reaches a state that a signed
// head checkpoint attested to: it was shortened, emptied, deleted, or rewritten
// to a different chain. A self-consistent hash chain cannot detect its own
// removed tail -- and readLog treats a missing file as an empty log -- so this
// is the only failure a checkpoint exists to expose, and deleting the whole log
// is the single most dangerous tamper for an audit trail. It is wrapped with
// context, so callers detect it with errors.Is.
var ErrTruncated = errors.New("gap: gap log is shorter than the checkpoint")

// fieldTruncation reports that the gap log no longer reaches a signed head
// checkpoint: it was shortened, rewritten, or emptied. It mirrors the ledger's
// fieldTruncation and is emitted only by VerifyAgainstCheckpoint, not by
// Verify, because the gap log's own chain cannot see its removed tail.
const fieldTruncation = "truncation"

// Checkpoint reads the gap log's head and signs a statement that the chain
// reached it at now, under sign.GapChain. The returned checkpoint attests to the
// head's (Counter, Hash) and names the signer's key, so a later run can tell
// that the log was shortened or deleted.
//
// A gap log with no entry has no head to attest to, so Checkpoint returns an
// error rather than signing a checkpoint for a nonexistent entry -- mirroring
// ledger.Checkpoint on an empty store. sg may be nil; the underlying signer
// rejects it with an error, never a panic.
func (l *Log) Checkpoint(sg *sign.Signer, now time.Time) (sign.Checkpoint, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	head, ok, err := logHead(l.path)
	if err != nil {
		return sign.Checkpoint{}, fmt.Errorf("gap: checkpoint: read head: %w", err)
	}
	if !ok {
		return sign.Checkpoint{}, errors.New("gap: checkpoint: gap log is empty, so there is no head to attest to")
	}

	cp, err := sign.NewChainCheckpoint(sign.GapChain, head.Counter, head.Hash, now, sg)
	if err != nil {
		return sign.Checkpoint{}, fmt.Errorf("gap: checkpoint: %w", err)
	}
	return cp, nil
}

// VerifyAgainstCheckpoint checks the log against a signed head checkpoint,
// catching the one tamper a plain chain walk cannot: truncation. Deleting the
// final line -- or the whole file -- yields a shorter but still self-consistent
// chain, which Verify alone would report as clean; a checkpoint records how far
// the chain had reached.
//
// The checkpoint's own signature is verified first, with
// v.VerifyChainCheckpoint(GapChain). A checkpoint whose signature does not
// verify, or whose signer v does not trust, is returned as a hard error -- not
// as a Break -- because an unverifiable checkpoint is not evidence: reporting it
// as a break would let an attacker who mangled a checkpoint produce a "tamper
// detected" reading instead of "your checkpoint is bad".
//
// It then compares the log against the checkpoint, in this order:
//
//   - the log is missing or empty while the checkpoint attests to an entry:
//     ErrTruncated (this is the deleted-file case);
//   - the highest counter present is below the checkpoint's counter:
//     ErrTruncated (the attested history was shortened);
//   - the entry at the checkpoint's counter exists but carries a different hash
//     -- or no entry sits there at all -- while the head is at or beyond it:
//     ErrTruncated (the chain was rewritten, or an interior entry was removed).
//
// Appending entries after the checkpoint was taken leaves it clean: a checkpoint
// is a lower bound on history, not a fixed head. When the log is not truncated,
// it returns a nil break slice and a nil error.
//
// On truncation it returns BOTH a single Break with Field "truncation" (whose
// Detail names the rule that fired) and a non-nil error wrapping ErrTruncated,
// so a caller can print the break uniformly with the rest of those from Verify
// and still detect the condition with errors.Is.
//
// Like the package-level VerifyAgainstCheckpoint, this method only reads the
// log; it never opens it for writing.
func (l *Log) VerifyAgainstCheckpoint(c sign.Checkpoint, v *sign.Verifier) ([]Break, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return verifyAgainstCheckpoint(l.path, c, v)
}

// VerifyAgainstCheckpoint checks the gap log at path against a signed head
// checkpoint, with the same semantics as (*Log).VerifyAgainstCheckpoint.
//
// It is read-only, and that is load-bearing: gap.Open opens the file O_APPEND
// and heals a torn tail by writing a newline, so an auditor verifying against a
// checkpoint must never use Open. This function only reads the file, so it can
// never mutate the very evidence it is checking. Use it -- not Open -- on any
// path where the log must be left exactly as found.
func VerifyAgainstCheckpoint(path string, c sign.Checkpoint, v *sign.Verifier) ([]Break, error) {
	return verifyAgainstCheckpoint(path, c, v)
}

// verifyAgainstCheckpoint is the shared body of the method and the package-level
// function. It only reads; it never opens the log for writing.
func verifyAgainstCheckpoint(path string, c sign.Checkpoint, v *sign.Verifier) ([]Break, error) {
	// (1) The checkpoint's own signature comes first, before any comparison: an
	// untrustworthy checkpoint proves nothing about the log.
	if err := v.VerifyChainCheckpoint(sign.GapChain, c); err != nil {
		return nil, fmt.Errorf("gap: verify against checkpoint: %w", err)
	}

	// (2) Compare the log against the attested head.
	entries, err := readAll(path)
	if err != nil {
		return nil, fmt.Errorf("gap: verify against checkpoint: %w", err)
	}
	if len(entries) == 0 {
		return truncation(c.Seq, fmt.Sprintf(
			"checkpoint attests to counter %d, but the gap log is now empty or missing", c.Seq))
	}
	if head := entries[len(entries)-1]; head.Counter < c.Seq {
		return truncation(c.Seq, fmt.Sprintf(
			"checkpoint attests to counter %d, but the gap log head is only at counter %d: the tail was removed",
			c.Seq, head.Counter))
	}

	// The head is at or beyond the checkpoint. It must still carry the attested
	// hash at the checkpoint's counter, or the chain was rewritten.
	for i := range entries {
		if entries[i].Counter != c.Seq {
			continue
		}
		if entries[i].Hash != c.Hash {
			return truncation(c.Seq, fmt.Sprintf(
				"checkpoint attests to hash %x at counter %d, but the gap log holds %x: the chain was rewritten",
				c.Hash[:], c.Seq, entries[i].Hash[:]))
		}
		return nil, nil
	}

	// The head is at or past the checkpoint yet no entry sits at that counter: an
	// interior entry was removed, so the attested history is gone.
	return truncation(c.Seq, fmt.Sprintf(
		"checkpoint attests to counter %d, but the gap log has no entry at that position", c.Seq))
}

// readAll returns every decodable entry of the log at path in file order. A
// missing file is an empty log. It reads the file directly, so it never opens it
// for writing and never heals a torn tail.
//
// It mirrors Open's recovery: a torn (unterminated) tail is considered only when
// it is a complete, self-consistent entry that continues the chain -- the write
// finished and only its newline was lost. Including it keeps the attested head
// from looking "removed" merely because its terminator is missing.
func readAll(path string) ([]Entry, error) {
	data, err := readLog(path)
	if err != nil {
		return nil, err
	}
	return headEntries(data), nil
}

// headEntries returns every decodable entry of data in file order, adopting a
// complete torn tail that legitimately continues the chain (see readAll). Lines
// that fail to decode are skipped -- Verify is what reports them.
func headEntries(data []byte) []Entry {
	lines, torn, hasTorn := splitLines(data)

	var out []Entry
	var last Entry
	found := false
	for _, raw := range lines {
		e, derr := decodeLine(raw)
		if derr != nil {
			continue
		}
		out = append(out, e)
		last = e
		found = true
	}
	if hasTorn {
		if e, ok := tornHead(torn, last, found); ok {
			out = append(out, e)
		}
	}
	return out
}

// logHead returns the last decodable entry of the log at path, and ok=false when
// the log is missing, empty, or holds no decodable entry.
func logHead(path string) (Entry, bool, error) {
	entries, err := readAll(path)
	if err != nil {
		return Entry{}, false, err
	}
	if len(entries) == 0 {
		return Entry{}, false, nil
	}
	return entries[len(entries)-1], true, nil
}

// truncation builds the single truncation Break and the ErrTruncated-wrapping
// error that VerifyAgainstCheckpoint returns together. counter is the
// checkpoint's attested counter: the shortened tail's own position is gone, so
// the attested position is the only meaningful one to report. Line is 0 because
// a removed line has no position in the surviving file.
func truncation(counter uint64, detail string) ([]Break, error) {
	return []Break{{Counter: counter, Field: fieldTruncation, Detail: detail}},
		fmt.Errorf("gap: verify against checkpoint: %w: %s", ErrTruncated, detail)
}
