package reconcile

import (
	"encoding/json"
	"fmt"
	"time"

	"notary/internal/mem0"
	"notary/internal/record"
)

// worklist is the reconciler's entire to-do list, derived from the ledger it
// already holds. There is deliberately no cursor, no queue and no side state
// here: the list is a pure function of the records passed in, which is exactly
// why re-running a pass is a no-op (spec §3 decision 4, §9.1).
//
// The stages are kept apart rather than flattened because later stages consume
// earlier results: scopes come from memory-bearing records, coverage candidates
// pair known memories with later same-scope searches, and removal candidates
// are known memories absent from an enumeration.
type worklist struct {
	// unresolvedAdds are add_requested records whose Mem0 event id has no
	// corresponding add_resolved (§9.1.1).
	unresolvedAdds []record.Record
	// scopes are the distinct entity scopes appearing in the ledger, to be
	// enumerated completely (§9.1.2).
	scopes []record.Scope
	// known are the memories the ledger establishes Notary knows exist
	// (§9.1.3-4).
	known []knownMemory
	// searches are the search_performed records that may cover a known memory
	// (§9.1.3).
	searches []record.Record
}

// knownMemory is a memory the ledger establishes Notary knows exists: its Mem0
// id, the scope it lives in, its content hash, and the id of the record that
// established it. That establishing record id is the basis a later absence
// claim rests on.
//
// At is the establishing record's At -- the Mem0 event time. It is carried here
// because a claim derived from this memory (memory_kept, memory_dropped) is
// about that same event and must adopt its time without re-reading the ledger:
// a producer that has only the id, scope and hash cannot recover the time, and
// looking it up again would mean a second, window-ignoring ledger scan. The
// value is the add event's time as buildAddResolved carried it forward, so it
// is exactly what a claim about this memory should record on Record.At.
type knownMemory struct {
	MemoryID    string
	Scope       record.Scope
	ContentHash record.Hash
	Basis       record.RecordID
	At          time.Time
}

// buildWorklist derives the worklist, in dependency order, from records.
//
// Nothing about the pass enters the result -- not the wall-clock time, not a
// pass counter -- so the same ledger always yields the same worklist.
func buildWorklist(records []record.Record) (worklist, error) {
	var wl worklist

	// Pass 1: index the event ids that have been resolved.
	resolved := make(map[string]struct{})
	for _, r := range records {
		if r.Event != record.EventAddResolved {
			continue
		}
		id, ok, err := eventID(r)
		if err != nil {
			return worklist{}, err
		}
		if ok {
			resolved[id] = struct{}{}
		}
	}

	// Pass 2: an add_requested is unresolved exactly when its event id is not
	// in that set. The match is on the event id -- the identifier Mem0 issued
	// and both records carry -- never on a content or timing heuristic.
	for _, r := range records {
		if r.Event != record.EventAddRequested {
			continue
		}
		id, ok, err := eventID(r)
		if err != nil {
			return worklist{}, err
		}
		if !ok {
			// An add with no event id cannot be matched or polled; it is not a
			// work item.
			continue
		}
		if _, done := resolved[id]; !done {
			wl.unresolvedAdds = append(wl.unresolvedAdds, r)
		}
	}

	wl.scopes = scopesToEnumerate(records)
	wl.known = knownMemories(records)
	wl.searches = searchPerformed(records)
	return wl, nil
}

// filter returns the records w admits, preserving order: those whose At is at
// or after w.Since (when Since is non-zero) and whose subject scope matches
// every non-zero dimension of w.Scope.
//
// The At test is deliberate and load-bearing: it filters on when the Mem0 event
// happened, NOT on when Notary wrote the record. A record written late about an
// old event still belongs to that old event's window, so filtering on
// RecordedAt would wrongly admit it.
func (w Window) filter(records []record.Record) []record.Record {
	out := make([]record.Record, 0, len(records))
	for _, r := range records {
		if !w.Since.IsZero() && r.At.Before(w.Since) {
			continue
		}
		if !scopeMatches(w.Scope, r.Subject.Scope) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// scopeMatches reports whether sc matches filter. An all-zero filter is no
// filter; otherwise every non-zero dimension of filter must equal sc's. A
// caller can therefore combine dimensions with AND, as the CLI flags do.
func scopeMatches(filter, sc record.Scope) bool {
	if filter == (record.Scope{}) {
		return true
	}
	return (filter.UserID == "" || filter.UserID == sc.UserID) &&
		(filter.AgentID == "" || filter.AgentID == sc.AgentID) &&
		(filter.AppID == "" || filter.AppID == sc.AppID) &&
		(filter.RunID == "" || filter.RunID == sc.RunID)
}

// scopeKey is a map key for a Scope. A NUL byte separates the dimensions,
// because a Mem0 entity id never contains one, so {UserID:"a",AgentID:"b"} and
// {UserID:"a\x00b"} cannot collide.
func scopeKey(sc record.Scope) string {
	return sc.UserID + "\x00" + sc.AgentID + "\x00" + sc.AppID + "\x00" + sc.RunID
}

// scopesToEnumerate returns the distinct, non-zero scopes appearing in records,
// in first-seen order.
func scopesToEnumerate(records []record.Record) []record.Scope {
	var out []record.Scope
	seen := make(map[string]struct{})
	for _, r := range records {
		sc := r.Subject.Scope
		if sc == (record.Scope{}) {
			continue
		}
		k := scopeKey(sc)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, sc)
	}
	return out
}

// knownMemories returns the distinct memories the ledger establishes exist, in
// first-seen order.
func knownMemories(records []record.Record) []knownMemory {
	var out []knownMemory
	seen := make(map[string]struct{})
	for _, r := range records {
		if !establishesMemory(r) {
			continue
		}
		k := r.Subject.MemoryID + "\x00" + scopeKey(r.Subject.Scope)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, knownMemory{
			MemoryID:    r.Subject.MemoryID,
			Scope:       r.Subject.Scope,
			ContentHash: r.Subject.ContentHash,
			Basis:       r.ID,
			// The establishing record's At IS the memory's event time:
			// buildAddResolved copies add.At onto the add_resolved record, and a
			// memory_kept record written by a later pass carries the same At
			// forward. Nothing else in the fold needs a clock, so the worklist
			// stays a pure function of the records.
			At: r.At,
		})
	}
	return out
}

// establishesMemory reports whether r asserts that a memory with a concrete
// Mem0 id exists.
//
// A memory_kept record does: it records presence in a listing. An add_resolved
// does when it recorded storage -- but not when the add failed or produced
// nothing. A memory_surfaced record does NOT: appearing once in a search does
// not establish a memory the reconciler may later call absent, so a memory that
// only ever surfaced in a search must never become a coverage or removal
// candidate (design review focus 4).
func establishesMemory(r record.Record) bool {
	if r.Subject.MemoryID == "" {
		return false
	}
	switch r.Event {
	case record.EventMemoryKept:
		return true
	case record.EventAddResolved:
		return r.Reason.Kind() == record.ReasonStoredByMem0
	default:
		return false
	}
}

// searchPerformed returns the search_performed records, in order.
func searchPerformed(records []record.Record) []record.Record {
	var out []record.Record
	for _, r := range records {
		if r.Event == record.EventSearchPerformed {
			out = append(out, r)
		}
	}
	return out
}

// eventID returns the Mem0 event id a record's Observed evidence carries, and
// ok false when the record carries none.
//
// add_requested carries the acknowledgement Mem0 returned (mem0.AddPayload) and
// add_resolved carries the event-status response that resolved it
// (mem0.EventStatusResponse); both expose the event id, which is what lets the
// fold match a request to its resolution exactly rather than by a heuristic.
func eventID(r record.Record) (string, bool, error) {
	payload, ok := observedPayload(r)
	if !ok {
		return "", false, nil
	}

	switch r.Event {
	case record.EventAddRequested:
		var p mem0.AddPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return "", false, fmt.Errorf("reconcile: decode add payload of record %s: %w", r.ID, err)
		}
		return p.EventID, p.EventID != "", nil
	case record.EventAddResolved:
		var s mem0.EventStatusResponse
		if err := json.Unmarshal(payload, &s); err != nil {
			return "", false, fmt.Errorf("reconcile: decode event status of record %s: %w", r.ID, err)
		}
		return s.ID, s.ID != "", nil
	default:
		return "", false, nil
	}
}

// observedPayload returns the Observed evidence payload bytes carried by r, and
// ok false when r carries no Observed evidence.
func observedPayload(r record.Record) ([]byte, bool) {
	ev, ok := r.Reason.Observed()
	if !ok {
		return nil, false
	}
	return ev.Payload(), true
}
