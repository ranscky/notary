package ledger

import (
	"fmt"

	"notary/internal/gap"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// CollectBreaks reports every integrity break in the ledger and its gap log --
// the one definition, shared by every caller that must answer "is this ledger
// intact?" -- running the three checks `notary verify` answers with, in the
// order it reports them:
//
//  1. the chain walk (Verify): each record's hash, link, signature, and
//     sequence continuity;
//  2. the gap cross-check: a gap-log entry whose (Kind, Scope, CorrelationID)
//     matches no stored record reports work that left no audit trail, and is
//     surfaced as a "gap" break;
//  3. the gap log's own integrity (gap.Verify, mapped by GapIntegrityBreaks): a
//     gap-log line that is corrupt, rewritten, or reordered, which the
//     cross-check cannot see.
//
// All three are needed to answer the question: a caller that ran the chain walk
// alone would call a ledger intact that `notary verify` exits non-zero on, and
// the two answers to one question would then disagree.
//
// It takes both l and st because Ledger deliberately does not expose its store:
// the chain walk goes through the ledger, which owns the chain semantics, while
// the gap cross-check needs the stored records themselves. The caller passes
// the store it opened the ledger over -- st must be that ledger's own store, or
// the two checks would read two different ledgers -- rather than the ledger
// handing out an internal.
//
// CollectBreaks prints nothing and decides no policy: it returns the breaks,
// and each caller decides what to do with them -- `notary verify` prints them
// and exits non-zero, a report renders them. It returns no breaks alongside a
// non-nil error, because a partial list is not an answer to the question that
// was asked; the error's text already carries the same context `notary verify`
// reports, so a caller must return it as it is rather than wrap it again. It
// never panics.
func CollectBreaks(l *Ledger, st store.Store, gapPath string, v *sign.Verifier) ([]Break, error) {
	breaks, err := l.Verify(v)
	if err != nil {
		return nil, fmt.Errorf("verifying ledger: %w", err)
	}

	// Cross-check the gap log against the store: a gap entry whose
	// (Kind, Scope, CorrelationID) matches no stored record reports work that
	// left no audit trail, and is surfaced as a "gap" break so the run exits
	// non-zero. The "matched" case -- a gap later reconciled back into the
	// ledger -- cannot be exercised until gaps have a corresponding record
	// (Task 15); it is revisited in Task 19.
	gapEntries, gerr := gap.Read(gapPath)
	if gerr != nil {
		return nil, fmt.Errorf("reading gap log %s: %w", gapPath, gerr)
	}
	if len(gapEntries) > 0 {
		// The records come from that same store, read through the same
		// Seq-ordered accessor the chain walk uses.
		seqEntries, serr := st.SeqEntries()
		if serr != nil {
			return nil, fmt.Errorf("reading ledger records for gap check: %w", serr)
		}
		records := make([]record.Record, 0, len(seqEntries))
		for _, se := range seqEntries {
			if se.DecodeErr == nil {
				records = append(records, se.Rec)
			}
		}
		breaks = append(breaks, GapBreaks(gapEntries, records)...)
	}

	// Check the gap log's own hash chain. gap.Read above only returns decodable
	// entries -- it silently skips a line that fails to decode and checks no
	// hashes -- so a corrupt, rewritten, or reordered gap-log line, the evidence
	// an attacker would most want to erase, is invisible to the cross-check.
	// gap.Verify reports exactly those breaks, so they exit non-zero here. A
	// missing or empty log yields no breaks and no error, so a system that never
	// logged a gap behaves exactly as before.
	gapIntegrityBreaks, gverr := gap.Verify(gapPath)
	if gverr != nil {
		return nil, fmt.Errorf("verifying gap log %s: %w", gapPath, gverr)
	}
	breaks = append(breaks, GapIntegrityBreaks(gapIntegrityBreaks)...)

	return breaks, nil
}
