package ledger

import (
	"fmt"

	"notary/internal/gap"
	"notary/internal/record"
)

// fieldGap reports a gap-log entry that no record in the store accounts for:
// activity that was recorded as an audit gap and never reconciled back into
// the ledger.
const fieldGap = "gap"

// GapBreaks reports a Break with Field "gap" for every gap-log entry that no
// stored record accounts for. A record accounts for a gap entry when they
// agree on the event kind, the scope, and the correlation ID: the correlation
// ID is the record's ID, so a gap whose missing record is present again (for
// example after Task 19 replays it) stops being reported.
//
// It is a pure function over its inputs and never panics. It does not read the
// store or the gap log itself; the caller supplies both, so the cross-check
// stays independent of either failure domain.
func GapBreaks(entries []gap.Entry, records []record.Record) []Break {
	known := make(map[string]struct{}, len(records))
	for _, r := range records {
		known[gapKey(r.Event, r.Subject.Scope, string(r.ID))] = struct{}{}
	}

	var breaks []Break
	for _, e := range entries {
		if _, ok := known[gapKey(e.Kind, e.Scope, e.CorrelationID)]; ok {
			continue
		}
		breaks = append(breaks, Break{
			RecordID: record.RecordID(e.CorrelationID),
			Seq:      e.Counter,
			Field:    fieldGap,
			Detail: fmt.Sprintf("gap entry %d for %s (%s, scope %s/%s/%s/%s) matches no record",
				e.Counter, e.CorrelationID, e.Kind,
				e.Scope.UserID, e.Scope.AgentID, e.Scope.AppID, e.Scope.RunID),
		})
	}
	return breaks
}

// gapKey builds the map key that decides whether a gap entry is accounted for
// by a record. The fields are joined with a NUL separator, which none of the
// components can contain, so no two distinct triples collide into one key.
func gapKey(kind record.EventType, scope record.Scope, correlationID string) string {
	return string(kind) + "\x00" +
		scope.UserID + "\x00" + scope.AgentID + "\x00" + scope.AppID + "\x00" + scope.RunID + "\x00" +
		correlationID
}

// GapIntegrityBreaks converts the breaks reported by gap.Verify -- failures of
// the gap log's own hash chain, not of the ledger -- into ledger Breaks, so
// `notary verify` can surface a corrupt, rewritten, or reordered gap-log line
// and exit non-zero. Without this, a tampered gap line is invisible to the
// auditor's command: gap.Read silently skips lines that fail to decode (and
// checks no hashes), so the cross-check that uses it sees nothing wrong.
//
// Mirroring GapBreaks, Seq carries the GAP entry's Counter -- not a ledger
// sequence -- and a break's own Line, the 1-based line number gap.Verify found
// it on, is folded into Detail so an operator can locate it in the file. The
// remaining fields -- Field ("decode", "hash", "chain", or "counter") and
// Detail -- are carried across unchanged.
//
// It is a pure function over its input and never panics.
func GapIntegrityBreaks(gapBreaks []gap.Break) []Break {
	breaks := make([]Break, 0, len(gapBreaks))
	for _, b := range gapBreaks {
		breaks = append(breaks, Break{
			Seq:    b.Counter,
			Field:  b.Field,
			Detail: fmt.Sprintf("gap log line %d: %s", b.Line, b.Detail),
		})
	}
	return breaks
}
