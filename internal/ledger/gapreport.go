package ledger

import (
	"fmt"

	"notary/internal/gap"
	"notary/internal/record"
	"notary/internal/store"
)

// GapReport is the answer to "is anything outstanding in this ledger's audit
// trail?" -- the two independent ways a ledger can be missing audit coverage,
// kept separate so each caller decides how to present them:
//
//   - Unreconciled is every gap-log entry no stored record accounts for: work
//     recorded as an audit gap and never reconciled back into the trail.
//   - Integrity is every break in the gap log's own hash chain: a line that is
//     corrupt, rewritten, or reordered.
//
// Unreconciled is about the log's relationship to the store; Integrity is about
// the log's internal consistency. They are reported apart because a caller
// prints them differently and one can be non-empty while the other is.
type GapReport struct {
	// Unreconciled is the gap-log entries no stored record accounts for, in gap
	// log order. It is empty when every gap has been reconciled -- or when there
	// is no gap log at all.
	Unreconciled []gap.Entry
	// Integrity is the gap log's own integrity breaks, in log order. It is empty
	// for a clean or missing log.
	Integrity []gap.Break
}

// OutstandingGaps reports what is outstanding in the ledger over st and the gap
// log at gapPath: the gap entries no stored record accounts for, and any break
// in the gap log's own integrity. It is the ONE definition of "outstanding",
// shared by `notary gaps` and the serve console, so the two cannot disagree
// about the same log -- exactly as CollectBreaks is the one definition of
// "intact?".
//
// It runs the three stages `notary gaps` runs, in order, each wrapping its
// failure with the same text it did before the extraction moved the stage here:
//
//  1. read the gap log (gap.Read). A missing file is an empty log, not an
//     error: no gap log ever written means no gap ever occurred.
//  2. when the log has entries, load the stored records the cross-check needs,
//     keeping only the rows that decode -- a row that cannot decode has no
//     identity to match a gap against. The store is read ONLY when the log has
//     entries, so an empty log never touches the store.
//  3. decide "unreconciled" with GapBreaks -- the SAME function `notary verify`
//     uses, never a re-derived match. A second, independent notion of
//     "unreconciled" could drift and leave the two commands disagreeing about
//     the same log.
//
// It then surfaces the gap log's own integrity breaks. gap.Read silently skips
// a line that fails to decode (and checks no hashes), so a corrupt log would
// otherwise produce an incomplete list that still looked healthy; gap.Verify
// reports exactly those breaks. A missing or empty log yields none, so a system
// that never logged a gap behaves as before.
//
// OutstandingGaps prints nothing and decides no policy: it returns the report,
// and each caller decides what to do with it -- `notary gaps` prints it and
// exits non-zero, the console renders it. It returns a zero report alongside a
// non-nil error when a stage fails, because a partial report is not an answer
// to the question that was asked. It never panics.
func OutstandingGaps(st store.Store, gapPath string) (GapReport, error) {
	// Read the gap log read-only. A missing file is an empty log, not an error:
	// no gap log ever written means no gap ever occurred.
	gapEntries, err := gap.Read(gapPath)
	if err != nil {
		return GapReport{}, fmt.Errorf("reading gap log %s: %w", gapPath, err)
	}

	// Load the stored records the cross-check needs, the same way `notary
	// verify` does: only the rows that decode, since a row that cannot decode
	// has no identity to match a gap against.
	var records []record.Record
	if len(gapEntries) > 0 {
		seqEntries, serr := st.SeqEntries()
		if serr != nil {
			return GapReport{}, fmt.Errorf("reading ledger records for gap check: %w", serr)
		}
		records = make([]record.Record, 0, len(seqEntries))
		for _, se := range seqEntries {
			if se.DecodeErr == nil {
				records = append(records, se.Rec)
			}
		}
	}

	// "Unreconciled" is decided by GapBreaks -- the SAME function `notary
	// verify` uses, never a re-derived match. A second, independent notion of
	// "unreconciled" could drift and leave the two commands disagreeing about
	// the same log.
	breaks := GapBreaks(gapEntries, records)

	// Surface the gap log's own integrity breaks. gap.Read above silently skips
	// a line that fails to decode (and checks no hashes), so a corrupt log
	// would produce an incomplete list that still looked healthy; gap.Verify
	// reports exactly those breaks. A missing or empty log yields none, so a
	// system that never logged a gap behaves as before.
	integrityBreaks, verr := gap.Verify(gapPath)
	if verr != nil {
		return GapReport{}, fmt.Errorf("verifying gap log %s: %w", gapPath, verr)
	}

	return GapReport{
		Unreconciled: unreconciledEntries(gapEntries, breaks),
		Integrity:    integrityBreaks,
	}, nil
}

// unreconciledEntries joins GapBreaks' output back to the entries it names.
// GapBreaks emits one break per unreconciled entry, keyed by that entry's
// Counter in Break.Seq -- so this join always hits, and the entry's own fields
// (its kind, scope, correlation ID, time, and detail) are recovered for the
// caller to print. Which entries are unreconciled is still decided by
// GapBreaks; this only reads the chosen entries back.
func unreconciledEntries(entries []gap.Entry, breaks []Break) []gap.Entry {
	if len(breaks) == 0 {
		return nil
	}
	byCounter := make(map[uint64]gap.Entry, len(entries))
	for _, e := range entries {
		byCounter[e.Counter] = e
	}
	out := make([]gap.Entry, 0, len(breaks))
	for _, b := range breaks {
		if e, ok := byCounter[b.Seq]; ok {
			out = append(out, e)
		}
	}
	return out
}
