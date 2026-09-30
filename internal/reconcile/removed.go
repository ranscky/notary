package reconcile

import (
	"context"
	"fmt"
	"time"

	"notary/internal/mem0"
	"notary/internal/record"
)

// Mem0 history event kinds that corroborate a removal. They are Mem0's own wire
// vocabulary, compared literally: the recorded ADD fixture uses "ADD" uppercase
// (internal/mem0/client_test.go, history_response.json), so DELETE and UPDATE
// are spelled the same way. An ADD is deliberately NOT here -- creation is not a
// removal, and treating it as one would turn every listed-then-added memory into
// a removal claim.
const (
	mem0HistoryDelete = "DELETE"
	mem0HistoryUpdate = "UPDATE"
)

// removedNote is the opacity marker the Internal removal claim carries. It
// states the two facts Notary actually has -- the memory is gone from a complete
// enumeration, and Mem0's history records the removal -- and states plainly that
// Mem0 exposed no reason. It deliberately contains NO cause: not "expired",
// "evicted", "pruned", "duplicate", "threshold", "superseded", "decayed", nor
// any other guess, and no speculation about intent. The EVENTS the claim rests
// on are what a reader audits; inventing a why would put a signed fiction into
// the tamper-evident chain. A test asserts none of those words appear.
const removedNote = "absent from a complete enumeration of its scope; Mem0's history records the removal and Mem0 exposed no reason"

// resolveRemoved is the removed_by_mem0 producer (spec §5 row 7, §9.1.4). It
// takes a COMPLETE enumeration of one scope and the known memories, and returns
// the memory_dropped claims a corroborated removal warrants, or none.
//
// For each known memory, against the complete enumeration e:
//
//	the memory id is present in the listing    -> nothing (it is kept, not removed)
//	the id is absent, and its Mem0 history
//	  records a DELETE or UPDATE entry         -> memory_dropped + removed_by_mem0 (Internal)
//	the id is absent and history corroborates
//	  nothing                                  -> nothing
//
// # Absence comes from a CompleteEnumeration, and only from one
//
// Absence is concluded ONLY from e, a mem0.CompleteEnumeration, which only
// GetAllComplete can construct -- its zero value is deliberately invalid, so an
// incomplete or single-page listing cannot even be passed here. An invalid value
// is a contract violation, not data, and fails loudly rather than being read as
// an absence (requirement 1).
//
// # Absence alone is not enough: history must corroborate
//
// Unlike memory_kept, absence from a listing does NOT by itself warrant a claim.
// A memory can be missing from a listing for reasons that are not a removal (a
// listing anomaly, a walk the store changed under), and an absent-but-still-
// present-elsewhere memory is not evidence about this scope. So the producer
// re-queries Mem0's own history for the memory and requires a DELETE or UPDATE
// entry -- Mem0's own record that the memory was changed or removed. Without
// that corroboration the claim is withheld (requirement 2). A History call that
// ERRORS fails the whole pass loudly; it is never read as "no removal", which
// would silently suppress a claim on a transient outage (spec §9.2).
//
// # Eligibility is the caller's scope
//
// e carries no scope (a CompleteEnumeration exposes only its items and count),
// so the producer cannot tell which scope it enumerates. It therefore trusts
// known to be EXACTLY the memories of the scope e enumerates, which Reconcile
// supplies via knownInScope. A memory in another scope must never reach here:
// it is not absent from e for any meaningful reason, and corroborating its
// history would manufacture a claim about a scope this enumeration says nothing
// about.
//
// It never panics and writes nothing itself: it returns records whose Seq and
// Hash are zero, for the caller to append.
func (rc *Reconciler) resolveRemoved(ctx context.Context, e mem0.CompleteEnumeration, known []knownMemory) ([]record.Record, error) {
	if !e.Valid() {
		// GetAllComplete never returns a valid-looking incomplete enumeration,
		// so an invalid one here is a contract violation, not data. Treat it as
		// an error rather than a value to read absence from (requirement 1).
		return nil, fmt.Errorf("resolve removed: complete enumeration is invalid")
	}

	listing := listedIDs(e)
	var out []record.Record
	// Two known entries could name the same memory id; records.id is UNIQUE, so
	// a second copy would be the same claim twice and would invite a raw SQLite
	// error. Emit it once.
	emitted := make(map[record.RecordID]struct{})

	for _, km := range known {
		if _, present := listing[km.MemoryID]; present {
			// The memory is in the complete listing: it is kept, not removed.
			// No history call is warranted.
			continue
		}

		at, removed, err := rc.removalAt(ctx, km.MemoryID)
		if err != nil {
			return nil, err
		}
		if !removed {
			// Absent from the listing, but Mem0's history exposes no removal:
			// the claim is withheld rather than fabricated from absence alone.
			continue
		}

		rec, err := buildRemoved(km, at)
		if err != nil {
			return nil, err
		}
		if _, dup := emitted[rec.ID]; dup {
			continue
		}
		emitted[rec.ID] = struct{}{}
		out = append(out, rec)
	}
	return out, nil
}

// listedIDs returns the set of memory ids a complete enumeration contains, so a
// membership test can tell "still listed" from "absent". e is valid (checked by
// the caller), so Items returns a copy of the verified listing.
func listedIDs(e mem0.CompleteEnumeration) map[string]struct{} {
	items := e.Items()
	ids := make(map[string]struct{}, len(items))
	for _, m := range items {
		ids[m.ID] = struct{}{}
	}
	return ids
}

// removalAt asks Mem0 for memoryID's history and reports whether it corroborates
// a removal, together with the time of the corroborating entry.
//
// removed is false when history exposes no DELETE or UPDATE entry -- absence
// from a listing alone does not warrant the claim (requirement 2). A History
// error is returned wrapped, so the pass fails loudly rather than reading the
// outage as "no removal". A nil client cannot make the call and is a
// misconfigured reconciler, reported as ErrNoMem0Client (never a panic).
func (rc *Reconciler) removalAt(ctx context.Context, memoryID string) (time.Time, bool, error) {
	if rc.client == nil {
		return time.Time{}, false, fmt.Errorf("resolve removed %s: %w", memoryID, ErrNoMem0Client)
	}
	history, err := rc.client.History(ctx, memoryID)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("resolve removed %s: fetch history: %w", memoryID, err)
	}
	entry, ok := removalEntry(history)
	if !ok {
		return time.Time{}, false, nil
	}
	at, err := historyEntryTime(entry)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("resolve removed %s: %w", memoryID, err)
	}
	return at, true, nil
}

// removalEntry returns the LAST history entry that corroborates a removal -- one
// whose event is DELETE or UPDATE -- and ok=false when history exposes none.
//
// The last such entry is chosen because it is the terminal change to the memory:
// entries are a change log, so the final DELETE/UPDATE is the change that left
// the memory absent. (A log of ADD, UPDATE, DELETE corroborates on the DELETE;
// one ending in UPDATE corroborates on that UPDATE.) ADD entries are skipped:
// creation is not a removal.
func removalEntry(history mem0.HistoryResponse) (mem0.HistoryEvent, bool) {
	for i := len(history) - 1; i >= 0; i-- {
		switch history[i].Event {
		case mem0HistoryDelete, mem0HistoryUpdate:
			return history[i], true
		}
	}
	return mem0.HistoryEvent{}, false
}

// historyEntryTime parses the time of a corroborating history entry.
//
// # Which field, and why
//
// Task 6 settled the principle: At is when the EVENT happened. Mem0's history
// entry exposes two timestamps, and they are not equal -- the recorded ADD
// fixture carries CreatedAt "2026-09-28T15:07:50-07:00" and UpdatedAt four
// seconds later (history_response.json). CreatedAt is when Mem0 recorded the
// event; UpdatedAt is when the entry was last modified and, as Task 6 already
// argued for add_resolution, is NOT when the event happened. So CreatedAt is the
// honest At, and UpdatedAt is consulted ONLY when CreatedAt is absent -- never
// silently preferred over the event time.
//
// # Failure is loud, never a wrong date
//
// If the chosen timestamp is present but unparseable, or neither field is
// present, this returns a wrapped error rather than falling back to the memory's
// event time, the zero time, or the wall clock. A claim dated at the zero time
// is invisible to any windowed read -- silently wrong in exactly the way this
// product exists to prevent -- so the pass must fail instead.
func historyEntryTime(entry mem0.HistoryEvent) (time.Time, error) {
	raw := entry.CreatedAt
	if raw == "" {
		raw = entry.UpdatedAt
	}
	if raw == "" {
		return time.Time{}, fmt.Errorf("history entry %s carries no timestamp", entry.ID)
	}
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse history timestamp %q: %w", raw, err)
	}
	return at, nil
}

// buildRemoved assembles the single memory_dropped + removed_by_mem0 record for
// a known memory km whose removal the history corroborated at time at.
//
// The tier is Internal, and the InternalNote is the opacity marker: the event
// (the memory is gone) is witnessed, the reason is not, and Notary does not
// invent one. The rule is looked up from the registry so the recorded kind and
// the idempotency key's version both come from the one declaration; the rule
// name is the reason kind on the signed record and the rule version rides in the
// claim key (an Internal reason carries no rule field of its own).
//
// At is the corroborating history entry's time -- the removal event's time --
// and RecordedAt is left zero for ledger.Append to stamp the true write time
// (Task 6's principle: At is when the event happened).
//
// The claim identity is per memory, per rule: the memory id, the content hash
// and the rule version. Nothing about the run enters it, so a second pass over
// an unchanged store derives the identical key and ledger.Append appends
// nothing.
func buildRemoved(km knownMemory, at time.Time) (record.Record, error) {
	rule, ok := LookupRule(RuleRemovedByMem0)
	if !ok {
		return record.Record{}, fmt.Errorf("resolve removed: rule %q is not registered", RuleRemovedByMem0)
	}

	note, err := record.NewInternalNote(removedNote)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve removed: build internal note for %s: %w", km.MemoryID, err)
	}
	reason, err := record.NewInternalReason(record.ReasonRemovedByMem0, note)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve removed: build internal reason for %s: %w", km.MemoryID, err)
	}

	key, err := record.DeriveClaimIdemKey(record.EventMemoryDropped, record.ReasonRemovedByMem0, km.MemoryID, hashString(km.ContentHash), rule.Version)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve removed: derive idempotency key for %s: %w", km.MemoryID, err)
	}

	return record.Record{
		ID:             absentID(record.ReasonRemovedByMem0, km.MemoryID),
		At:             at,
		Event:          record.EventMemoryDropped,
		Reason:         reason,
		Subject:        record.Subject{MemoryID: km.MemoryID, Scope: km.Scope, ContentHash: km.ContentHash},
		IdempotencyKey: key,
	}, nil
}
