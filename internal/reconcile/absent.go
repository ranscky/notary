package reconcile

import (
	"encoding/json"
	"fmt"
	"strings"

	"notary/internal/mem0"
	"notary/internal/record"
)

// resolveAbsent is the absent_from_search producer (spec §5 row 6, §9.1.3). It
// takes the memories the ledger establishes exist, the search_performed records
// that may cover them, and the memory_surfaced records that say which memories
// each search returned, and returns the memory_dropped claims a COVERING search
// warrants, or none.
//
// It emits record.EventMemoryDropped + record.ReasonAbsentFromSearch at the
// Reconstructed tier, justified by rule absent_from_search v1, with a basis
// naming BOTH the prior listing record that established the memory and the
// covering search record.
//
// # A search covers a memory only when it is saturated AND did not return it
//
// TWO conditions must BOTH hold for a search to be a valid basis, and both are
// checked in firstCoveringSearch.
//
//  1. SATURATION. The search returned FEWER results than its top_k -- exactly
//     p.Count < p.TopK. When a search returned fewer results than top_k, the
//     store had nothing further above the threshold to give. When the result
//     count EQUALS top_k the result window was TRUNCATED by top_k, so absence
//     proves nothing at all and NO claim may be written. The predicate is
//     therefore strictly `<`, never `<=`; a `<=` would fabricate a claim on
//     every full-window search.
//
//  2. NON-RETURN. The search did NOT return the memory. Saturation alone is NOT
//     sufficient: a saturated search returned everything above the threshold,
//     so a memory it RETURNED was above the threshold and did not drop under
//     any reading of the claim. Claiming absence for a returned memory would
//     fabricate a signed record, and it would fire for every known memory any
//     saturated same-scope search happens to return -- the ordinary
//     list-then-search workflow, not an edge case. The returned set is read
//     from the search's memory_surfaced records (see searchReturned), which is
//     why resolveAbsent takes them.
//
// A top_k of ZERO is treated as "not a covering search", NOT as "everything is
// absent". A zero top_k means the caller named no window -- there is no window
// to be saturated -- so the search carries no evidence about any memory.
// Writing `<` makes this fall out for free (Count, which is never negative,
// cannot be less than 0), but the distinction is stated here because a `<=`
// reading would make every search with the default top_k fire a claim. Note
// also that the parameters come from the search_performed record's Observed
// evidence: top_k and the result count are both already on the record, and the
// returned set is already on the memory_surfaced records, so this producer
// computes the whole rule from the ledger alone and makes NO Mem0 call.
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
// A memory is eligible on two counts, and memory_surfaced features in BOTH: it
// can neither grant eligibility nor be granted eligibility.
//
//   - Existence. The ledger must have ESTABLISHED the memory exists (knownMemory:
//     a memory_kept or a listing add_resolved). A memory that merely appeared
//     once in some search's results -- a memory_surfaced record, which the fold
//     deliberately does not treat as establishing existence -- must never produce
//     an absence claim. That is the difference between "we knew this existed and
//     it did not come back" and "we never knew it existed at all".
//   - Non-return. A search is a candidate for a memory only when it is in the
//     SAME scope, was recorded AT OR AFTER the memory became known, AND did not
//     return the memory. A search from another scope, or one that predates the
//     memory, describes a different world and cannot testify about this memory;
//     and a search that returned the memory is positive evidence that it did not
//     drop, so it too is not a basis for an absence claim.
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
func (rc *Reconciler) resolveAbsent(known []knownMemory, searches []record.Record, surfaced []record.Record) ([]record.Record, error) {
	var out []record.Record
	// Two distinct known entries can name the same Mem0 memory id in different
	// scopes; the record id is qualified by kind and memory id (not scope), so
	// those would share an id. records.id is UNIQUE, so a second copy would be
	// the same claim twice and would invite a raw SQLite error. Emit it once.
	emitted := make(map[record.RecordID]struct{})

	for _, km := range known {
		search, ok, err := firstCoveringSearch(km, searches, surfaced)
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
// A search covers km when ALL of the following hold:
//
//   - it is in km's scope (the same entity context);
//   - its At is at or after km's At (it happened after the memory became known);
//   - it is SATURATED: its result count is strictly less than its top_k
//     (Count < TopK); and
//   - it did NOT return km: no memory_surfaced record ties km's id to it.
//
// The last two are the two conditions of the rule, and both are visible in the
// condition below. Saturation alone is NOT enough -- a saturated search returned
// everything above the threshold, so a memory it returned was above the
// threshold and did not drop. Claiming absence for a returned memory would be a
// fabricated signed record.
//
// Searches are visited in the order the fold produced them (ledger order), so
// the FIRST covering search is the one the claim's basis and At name.
//
// The search parameters are read from the record's Observed evidence, which is
// decoded only once a search has passed the scope and time filters -- a
// malformed payload on an unrelated search is not this memory's problem. A
// candidate search whose payload cannot be decoded is a corrupt ledger and
// fails the pass loudly (spec §9.2).
func firstCoveringSearch(km knownMemory, searches []record.Record, surfaced []record.Record) (record.Record, bool, error) {
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
		// BOTH conditions of the rule: saturated AND the memory was not
		// returned. Either one alone is not enough to claim the memory dropped.
		if params.Count < params.TopK && !searchReturned(s, km.MemoryID, surfaced) {
			return s, true, nil
		}
	}
	return record.Record{}, false, nil
}

// searchReturned reports whether the search s returned the memory with id
// memoryID, by finding a memory_surfaced record tied to s.
//
// The linkage is the record-id convention the interceptor writes, not a
// heuristic: a search_performed record's id IS the caller's correlation id, and
// each of its results is written as a memory_surfaced record whose id is
// "<correlation id>#<rank>" with the returned memory's id on Subject.MemoryID
// (internal/interceptor/library/mem0.go). So "s returned memoryID" is exactly
// "some memory_surfaced record r has r.Subject.MemoryID == memoryID and r.ID
// carries the prefix s.ID + \"#\"".
//
// The prefix test is unambiguous because the separator is "#" and a correlation
// id is base64, which never contains "#": no other search's surfaced id can
// carry s.ID + "#" as a prefix. A scope-and-timestamp match, by contrast, could
// collide two searches of one scope in the same clock tick, so it is not sound.
func searchReturned(s record.Record, memoryID string, surfaced []record.Record) bool {
	prefix := string(s.ID) + "#"
	for _, r := range surfaced {
		if r.Subject.MemoryID == memoryID && strings.HasPrefix(string(r.ID), prefix) {
			return true
		}
	}
	return false
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
