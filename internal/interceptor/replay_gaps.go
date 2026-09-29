package interceptor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"notary/internal/gap"
	"notary/internal/ledger"
	"notary/internal/record"
)

// ReplayGaps reconciles gap-log entries back into the ledger once the store can
// accept writes again. It is the closing half of the fail-open-loud loop: Write
// records a durable gap when the ledger refuses a record (spec section 8); when
// the store recovers, ReplayGaps re-derives the missing record from a
// caller-supplied source and appends it through the ledger's chained, signed
// write path, then notes the reconciliation in the gap log.
//
// For every gap entry no stored record accounts for, ReplayGaps asks lookup for
// the missing record and appends it with Ledger.Append -- never through Write,
// the request-path fail-open wrapper: a second gap entry per run would make the
// gap log grow on every reconciliation attempt and ReplayGaps non-idempotent.
// Reconciliation is a background path where an error is actionable, unlike the
// request path.
//
// The int returned is the number of DISTINCT records reconciled in this run:
// entries for which lookup returned a record and the append ACTUALLY STORED
// that entry's record (see the keyed-no-op case below). Two gap
// entries naming the same missing record (they share a correlation ID) are
// reconciled once and counted once. A lookup that MISSES is not an error and is
// not counted -- that entry is left unreconciled -- so a caller can tell
// "nothing obtainable to reconcile" (a zero count, no error) from "a record
// could not be stored" (a non-nil error).
//
// A keyed no-op is not a reconciliation. Because Append deduplicates on the
// idempotency key and returns the EXISTING record's ID on a duplicate, a
// lookup that returns a record whose key already exists under a DIFFERENT
// record ID makes Append return that other ID with a nil error -- yet nothing
// new is stored and this entry still matches no record. An entry is therefore
// counted and marked reconciled only when the returned ID equals
// record.RecordID(e.CorrelationID), the exact tuple ledger.GapBreaks matches on
// (Ruling R45's single-definition rule). When the returned ID differs, the
// entry is SKIPPED: not counted, no marker written, no error returned, and the
// run continues with the remaining entries. The entry stays unreconciled and
// visible to `notary verify` -- the loud channel here -- rather than being
// papered over by a marker claiming a reconciliation that never happened.
//
// Reconcile-versus-stored is decided by ledger.GapBreaks -- the SAME function
// `notary verify` uses -- never by a rule invented here. An entry is reconciled
// when a stored record matches its (Kind, Scope, CorrelationID). Because the
// scheme Tasks 15/16 established makes a gap entry's CorrelationID the missing
// record's ID, the candidate record is read with GetRecord(correlationID): it
// is the only record that can match. Driving the decision through GapBreaks is
// exactly what keeps ReplayGaps and `notary verify` from disagreeing -- a
// re-derived match could drift and leave `notary verify` still reporting a gap
// break after a successful reconciliation.
//
// Idempotency. A second run reconciles 0 and appends nothing: after the first
// run every reconciled entry matches a stored record, so none is unreconciled.
// That relies on the replayed record carrying its ORIGINAL IdempotencyKey --
// Append treats it as a no-op -- and on the record the caller's lookup returns
// being used verbatim, never rebuilt or re-keyed. It also relies on the
// scheme's invariant that the returned record's ID is the entry's correlation
// ID; a lookup that returns a record under a different ID, or whose key is
// already stored under a different ID, does not reconcile the entry and is
// skipped (see above), leaving it to be re-attempted on the next run.
//
// Errors. ReplayGaps reports rather than swallows:
//   - ctx cancelled: returns the count reconciled so far and the wrapped
//     context error, without reconciling any further entry.
//   - a ledger Append failure: returns the count reconciled so far and the
//     wrapped error; the run stops at the failing entry, so the count is the
//     number of records this run DID store.
//   - a marker-record failure (g.Record): the record IS stored, but the run
//     reports the error and stops, and the count excludes this entry. Because
//     the record is now stored, a later run sees the entry as already
//     reconciled and will not retry the marker -- the ledger stays consistent
//     and `notary verify` reports no gap break, but the gap log carries no
//     reconciliation note for it.
//   - reading the gap log: returns 0 and the wrapped error.
//
// Nil safety. A nil gap log, nil ledger, or nil lookup makes reconciliation
// impossible, so each is reported as an error rather than a silent "0
// reconciled" -- a caller must not mistake a misconfigured reconciler for an
// idle one. None of the three panics.
func (w *AuditWriter) ReplayGaps(
	ctx context.Context,
	lookup func(correlationID string) (record.Record, bool),
) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("interceptor: replay gaps: %w", err)
	}
	if w.g == nil {
		return 0, errors.New("interceptor: replay gaps: no gap log configured")
	}
	if w.ledger == nil {
		return 0, errors.New("interceptor: replay gaps: no ledger configured")
	}
	if lookup == nil {
		return 0, errors.New("interceptor: replay gaps: no lookup configured")
	}

	entries, err := w.g.Entries()
	if err != nil {
		return 0, fmt.Errorf("interceptor: replay gaps: read gap log: %w", err)
	}

	reconciled := 0
	// done records the correlation IDs already reconciled in this run, so a
	// duplicate gap entry for the same missing record is reconciled once and
	// counted once.
	done := make(map[string]struct{})

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return reconciled, fmt.Errorf("interceptor: replay gaps: %w", err)
		}

		// Already accounted for by a stored record? Decide with GapBreaks, the
		// same rule `notary verify` uses -- never a re-derived match.
		if w.reconciledByStore(e) {
			continue
		}
		if _, ok := done[e.CorrelationID]; ok {
			continue
		}

		rec, ok := lookup(e.CorrelationID)
		if !ok {
			// A miss: leave the entry unreconciled. Do not count it and do not
			// error -- there is simply nothing to reconcile it with.
			continue
		}

		id, err := w.ledger.Append(rec)
		if err != nil {
			return reconciled, fmt.Errorf(
				"interceptor: replay gaps: append record %s: %w", rec.ID, err)
		}
		// Append returning a nil error is NOT proof this entry's record was
		// stored: Append is a keyed no-op that returns the EXISTING record's
		// ID, so a Lookup whose record carries an already-used key resolves to
		// some other ID and stores nothing new. Rather than trust the append,
		// re-ask the SAME question `notary verify` will ask -- reconciledByStore
		// wraps ledger.GapBreaks over the full (Kind, Scope, CorrelationID)
		// tuple. When the entry still does not match, SKIP it: do not count it,
		// do not append a marker, and do not error. The entry stays
		// unreconciled and visible to `notary verify` (ledger.GapBreaks) -- the
		// loud channel here -- and the run continues with the other entries, so
		// one bad Lookup result cannot block the rest.
		if !w.reconciledByStore(e) {
			continue
		}
		// Mark only what was actually reconciled, and only after the append
		// succeeded: a marker that lied about reconciliation would be worse
		// than none.
		if err := w.g.Record(reconcileMarker(e, id)); err != nil {
			return reconciled, fmt.Errorf(
				"interceptor: replay gaps: record reconciliation marker for %s: %w",
				e.CorrelationID, err)
		}
		done[e.CorrelationID] = struct{}{}
		reconciled++
	}
	return reconciled, nil
}

// reconciledByStore reports whether a stored record already accounts for the
// gap entry e, using ledger.GapBreaks as the single definition of
// "reconciled". The candidate is the record stored under e's correlation ID --
// which IS the missing record's ID, so it is the only record that can match. A
// read miss or error yields no candidate, and the entry is then reported as
// unreconciled.
func (w *AuditWriter) reconciledByStore(e gap.Entry) bool {
	cand, err := w.ledger.GetRecord(record.RecordID(e.CorrelationID))
	if err != nil {
		return false
	}
	return len(ledger.GapBreaks([]gap.Entry{e}, []record.Record{cand})) == 0
}

// reconcileMarker builds the gap-log entry that records the reconciliation of
// e. It keeps the ORIGINAL Kind, Scope, and CorrelationID so the marker
// identifies the same missing record as e -- ledger.GapBreaks accounts for it
// once the record is stored, so the marker adds no new break -- and puts the
// human explanation, naming the appended record, in Detail.
func reconcileMarker(e gap.Entry, appended record.RecordID) gap.Entry {
	return gap.Entry{
		At:            time.Now().UTC(),
		Kind:          e.Kind,
		Scope:         e.Scope,
		CorrelationID: e.CorrelationID,
		Detail:        fmt.Sprintf("reconciled: appended record %s", appended),
	}
}
