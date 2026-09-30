package reconcile

import (
	"encoding/json"
	"fmt"

	"notary/internal/mem0"
	"notary/internal/record"
)

// resolveAbsent is the absent_from_search producer (spec §5 row 6, §9.1.3). It
// takes the memories the ledger establishes exist and the search_performed
// records that may cover them, and returns the memory_dropped claims a
// SATURATED search warrants, or none.
//
// It emits record.EventMemoryDropped + record.ReasonAbsentFromSearch at the
// Reconstructed tier, justified by rule absent_from_search v1, with a basis
// naming BOTH the prior listing record that established the memory and the
// covering search record.
//
// # The saturation predicate is the entire evidential basis
//
// Notary claims absence from a search ONLY when the search returned FEWER
// results than its top_k -- i.e. exactly p.Count < p.TopK. When a search
// returned fewer results than top_k, the store had nothing further above the
// threshold to give, so a memory known to exist in that scope that the search
// did not return genuinely fell below the threshold: a real, explainable drop.
// When the result count EQUALS top_k the result window was TRUNCATED by top_k,
// so absence proves nothing at all and NO claim may be written. The predicate
// is therefore strictly `<`, never `<=`; a `<=` would fabricate a claim on
// every full-window search.
//
// A top_k of ZERO is treated as "not a covering search", NOT as "everything is
// absent". A zero top_k means the caller named no window -- there is no window
// to be saturated -- so the search carries no evidence about any memory.
// Writing `<` makes this fall out for free (Count, which is never negative,
// cannot be less than 0), but the distinction is stated here because a `<=`
// reading would make every search with the default top_k fire a claim. Note
// also that the predicate reads the parameters off the search_performed
// record's Observed evidence: top_k and the result count are both already on
// the record, so this producer computes the whole rule from the ledger alone
// and makes NO Mem0 call.
//
// # The cause is never attributed
//
// Design spec §5 forbids guessing WHY the memory did not come back -- it may
// have fallen below the threshold, or top_k may have truncated a larger pool.
// The record states the fact and the parameters (via the search record in its
// basis) and stops there; nothing here writes "threshold" or "top_k" as a
// cause.
//
// # Eligibility
//
// A memory is eligible only if the ledger ESTABLISHED it exists (knownMemory:
// a memory_kept or a listing add_resolved). A memory that merely appeared once
// in some search's results -- a memory_surfaced record, which the fold
// deliberately does not treat as establishing existence -- must never produce
// an absence claim. That is the difference between "we knew this existed and
// it did not come back" and "we never knew it existed at all".
//
// A search is a candidate for a memory only when it is in the SAME scope and
// was recorded AT OR AFTER the memory became known. A search from another
// scope, or one that predates the memory, describes a different world and
// cannot testify about this memory.
//
// # One claim per memory
//
// The claim's idempotency key (record.DeriveClaimIdemKey) is keyed on the
// memory id, the content hash and the rule version -- the per-memory claim
// identity the design settled on (spec §3 decision 4). This is deliberate and
// has a surprising consequence: several later covering searches that each omit
// the same memory derive the SAME key, so they collapse to ONE
// absent_from_search claim whose basis and At name the FIRST such covering
// search. The searches themselves remain recorded as search_performed, so a
// reader can still see that each happened; only the derived claim is single.
// That is the accepted cost of keeping the ledger bounded, and it is flagged in
// the plan's self-review. The producer therefore stops at the first covering
// search for a memory rather than emitting one record per search.
//
// It never panics and writes nothing itself: it returns records whose Seq and
// Hash are zero, for the caller to append.
func (rc *Reconciler) resolveAbsent(known []knownMemory, searches []record.Record) ([]record.Record, error) {
	var out []record.Record
	// Two distinct known entries can name the same Mem0 memory id in different
	// scopes; the record id is qualified by kind and memory id (not scope), so
	// those would share an id. records.id is UNIQUE, so a second copy would be
	// the same claim twice and would invite a raw SQLite error. Emit it once.
	emitted := make(map[record.RecordID]struct{})

	for _, km := range known {
		search, ok, err := firstCoveringSearch(km, searches)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}

		rec, err := buildAbsent(km, search)
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

// firstCoveringSearch returns the first search in searches that COVERS km, and
// ok=false when none does.
//
// A search covers km when it is in km's scope, its At is at or after km's At
// (it happened after the memory became known), and it is saturated: its result
// count is strictly less than its top_k. Searches are visited in the order the
// fold produced them (ledger order), so the FIRST covering search is the one
// the claim's basis and At name.
//
// The search parameters are read from the record's Observed evidence, which is
// decoded only once a search has passed the scope and time filters -- a
// malformed payload on an unrelated search is not this memory's problem. A
// candidate search whose payload cannot be decoded is a corrupt ledger and
// fails the pass loudly (spec §9.2).
func firstCoveringSearch(km knownMemory, searches []record.Record) (record.Record, bool, error) {
	for _, s := range searches {
		if s.Subject.Scope != km.Scope {
			continue
		}
		if s.At.Before(km.At) {
			// The search predates the memory becoming known: it cannot be
			// evidence that the memory later failed to come back.
			continue
		}
		params, err := searchParams(s)
		if err != nil {
			return record.Record{}, false, err
		}
		// The saturation predicate, and the whole rule: a claim ONLY when the
		// search returned strictly fewer results than its top_k.
		if params.Count < params.TopK {
			return s, true, nil
		}
	}
	return record.Record{}, false, nil
}

// searchParams decodes the SearchPerformedPayload from a search_performed
// record's Observed evidence.
func searchParams(s record.Record) (mem0.SearchPerformedPayload, error) {
	payload, ok := observedPayload(s)
	if !ok {
		return mem0.SearchPerformedPayload{}, fmt.Errorf("resolve absent: search record %s carries no observed evidence", s.ID)
	}
	var p mem0.SearchPerformedPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return mem0.SearchPerformedPayload{}, fmt.Errorf("resolve absent: decode search parameters of record %s: %w", s.ID, err)
	}
	return p, nil
}

// buildAbsent assembles the single memory_dropped record for a known memory km
// covered by search.
//
// At is the COVERING SEARCH's At: the event being described is the search that
// failed to return the memory, so the record belongs to that event's window.
// The memory's own event time (km.At) is not used -- it is when the memory was
// established, not when the drop was observed -- and RecordedAt is left zero
// for ledger.Append to stamp the true write time (Task 6 settled the general
// principle: At is when the EVENT happened).
//
// The basis names BOTH records the inference rests on: the prior listing
// (km.Basis, the record that established the memory) and the covering search
// (search.ID). Listing first, then the search that failed to return it.
//
// The Subject carries km's scope and content hash, so the claim describes the
// same memory the listing did, and the rule and its version are recorded while
// the confidence is left at ZERO -- an unset confidence is omitted from the
// encoded reason rather than rendered as "0% confident" (spec §8).
func buildAbsent(km knownMemory, search record.Record) (record.Record, error) {
	rule, ok := LookupRule(RuleAbsentFromSearch)
	if !ok {
		return record.Record{}, fmt.Errorf("resolve absent: rule %q is not registered", RuleAbsentFromSearch)
	}

	ev, err := record.NewReconstructedEvidence([]record.RecordID{km.Basis, search.ID}, rule.Name, rule.Version, 0)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve absent: build reconstructed evidence for %s: %w", km.MemoryID, err)
	}
	reason, err := record.NewReconstructedReason(record.ReasonAbsentFromSearch, ev)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve absent: build reconstructed reason for %s: %w", km.MemoryID, err)
	}

	// The claim identity is per memory, per rule: the memory id, the content
	// hash and the rule version. The search record id is deliberately NOT part
	// of it, so several covering searches for one memory derive one key and
	// ledger.Append suppresses all but the first (see resolveAbsent).
	key, err := record.DeriveClaimIdemKey(record.EventMemoryDropped, record.ReasonAbsentFromSearch, km.MemoryID, hashString(km.ContentHash), rule.Version)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve absent: derive idempotency key for %s: %w", km.MemoryID, err)
	}

	return record.Record{
		ID:             absentID(record.ReasonAbsentFromSearch, km.MemoryID),
		At:             search.At,
		Event:          record.EventMemoryDropped,
		Reason:         reason,
		Subject:        record.Subject{MemoryID: km.MemoryID, Scope: km.Scope, ContentHash: km.ContentHash},
		IdempotencyKey: key,
	}, nil
}

// absentID is the stable record id for a memory_dropped claim about memoryID
// with the given reason kind.
//
// The invariant, as in keptID and addResolvedID: the id must vary with the
// reason kind. memory_dropped carries two kinds -- absent_from_search
// (Reconstructed) and, later, removed_by_mem0 (Internal) -- and records.id is
// UNIQUE while duplicate suppression matches only the idempotency_key column.
// Two claims about the SAME memory with different kinds must therefore have
// different ids, or the second append dies on a raw SQLite UNIQUE violation
// instead of appending the legitimate claim. The reason-kind qualifier mirrors
// the kind the idempotency key already carries, so the two constraints agree.
func absentID(kind record.ReasonKind, memoryID string) record.RecordID {
	return record.RecordID(string(record.EventMemoryDropped) + ":" + string(kind) + ":" + memoryID)
}
