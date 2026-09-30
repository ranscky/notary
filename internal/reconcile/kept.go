package reconcile

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"notary/internal/mem0"
	"notary/internal/record"
)

// The content digest is mem0.ContentHash, which lives in internal/mem0 -- the
// leaf package both this reconciler and internal/interceptor/library already
// depend on -- so the scheme has ONE definition. It must be the same scheme the
// interceptor used to compute the add record's Subject.ContentHash, or the
// comparison below can never fire and kept_by_content_match would silently stop
// matching, a failure that looks exactly like "nothing happened". That is why
// the scheme was not duplicated here: see mem0.ContentHash and its golden test.

// scopeFilters projects a record.Scope onto the mem0.Filters an enumeration
// takes. Entity ids must travel nested in "filters" (mem0.Filters), never at
// the top level of the request, which Mem0 rejects with HTTP 400.
func scopeFilters(sc record.Scope) mem0.Filters {
	return mem0.Filters{
		UserID:  sc.UserID,
		AgentID: sc.AgentID,
		AppID:   sc.AppID,
		RunID:   sc.RunID,
	}
}

// resolveKept is the memory_kept producer (spec §5 rows 4-5). It takes a scope
// and the known memories and returns the memory_kept claims a COMPLETE
// enumeration of that scope warrants, or none.
//
// For each known memory in scope, against one complete enumeration of the scope:
//
//	the produced memory id is present -> memory_kept + stored_by_mem0     (Observed)
//	the id is absent, but a listed memory's content hash equals the
//	  add's submitted text             -> memory_kept + kept_by_content_match (Reconstructed, rule v1)
//	neither                            -> nothing
//
// The enumeration must be COMPLETE, and the type enforces that: absence may only
// be concluded from a mem0.CompleteEnumeration, which only GetAllComplete can
// construct. This producer never fabricates one -- it rejects an invalid value
// loudly -- and a walk that cannot prove itself exhaustive (a count mismatch, a
// repeated id, the page cap, or a transport error) yields an error and NO
// memory_kept claim, never a partial conclusion (requirement 1).
//
// It writes nothing itself: it returns records whose Seq and Hash are zero, for
// the caller to append. It never panics.
func (rc *Reconciler) resolveKept(ctx context.Context, scope record.Scope, known []knownMemory) ([]record.Record, error) {
	scoped := knownInScope(known, scope)
	if len(scoped) == 0 {
		// No known memory in this scope is a subject to claim about, so there is
		// nothing to compare a listing against. Enumerating anyway would be a
		// Mem0 call with no possible conclusion; skip it rather than require a
		// client a pass might not need.
		return nil, nil
	}

	if rc.client == nil {
		// A nil client is a misconfigured reconciler, not a "nothing to
		// claim" state: report it loudly rather than let a broken deployment
		// look like a clean pass (spec §9.2).
		return nil, fmt.Errorf("resolve kept for scope %s: %w", scopeKey(scope), ErrNoMem0Client)
	}

	enum, err := rc.client.GetAllComplete(ctx, mem0.GetAllRequest{Filters: scopeFilters(scope)})
	if err != nil {
		return nil, fmt.Errorf("resolve kept for scope %s: enumerate scope: %w", scopeKey(scope), err)
	}
	if !enum.Valid() {
		// GetAllComplete never returns a valid-looking incomplete enumeration,
		// so an invalid one here is a contract violation, not data. Treat it as
		// an error rather than a value to read absence from.
		return nil, fmt.Errorf("resolve kept for scope %s: complete enumeration is invalid", scopeKey(scope))
	}

	memories := enum.Items()
	var out []record.Record
	emitted := make(map[record.RecordID]struct{})
	for _, km := range scoped {
		rec, ok, err := keptRecord(km, scope, memories, km.At)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		// Two known memories can warrant the SAME claim (e.g. two adds with
		// identical text producing one listed memory). The subject -- and so the
		// record id -- is identical, so emit it once: a second copy would be the
		// same claim, and returning it twice would invite a raw UNIQUE error on
		// records.id.
		if _, dup := emitted[rec.ID]; dup {
			continue
		}
		emitted[rec.ID] = struct{}{}
		out = append(out, rec)
	}
	return out, nil
}

// knownInScope returns the known memories whose scope is exactly scope, in
// order. resolveKept is handed the whole worklist's known memories and must not
// compare a memory in one scope against another scope's listing.
func knownInScope(known []knownMemory, scope record.Scope) []knownMemory {
	var out []knownMemory
	for _, km := range known {
		if km.Scope == scope {
			out = append(out, km)
		}
	}
	return out
}

// The At question (Task 6 settled the reasoning, this applies it). A memory_kept
// record describes the same Mem0 event as the add whose product it is, so its At
// is that event's time -- knownMemory.At, which the fold took from the
// establishing record -- and RecordedAt is left zero for ledger.Append to stamp
// the true write time (a late reconciliation still keeps the event's time on
// At). Mem0's own CreatedAt/UpdatedAt -- on the event status and on the listed
// memory -- are deliberately NOT used: neither is when the event happened.

// keptRecord builds the memory_kept claim a single known memory warrants against
// an already-complete set of listed memories, or ok=false when it warrants none.
//
// The produced id is tried first: an exact id match is the strongest evidence
// (the very memory Mem0 said it produced is in the listing) and yields the
// Observed stored_by_mem0 claim, exactly as an add_resolved success does. Only
// when the id is ABSENT is the content-hash rule consulted -- "absent but
// content-equal" is an inference, so it is Reconstructed and carries the rule.
func keptRecord(km knownMemory, scope record.Scope, memories []mem0.Memory, at time.Time) (record.Record, bool, error) {
	for _, m := range memories {
		if m.ID == km.MemoryID {
			rec, err := buildKeptObserved(scope, m, at)
			return rec, err == nil, err
		}
	}

	for _, m := range memories {
		// The submitted text digest is km.ContentHash (the add record's
		// Subject.ContentHash). Comparing it against mem0.ContentHash(m.Memory)
		// requires the interceptor's exact scheme, now shared (see
		// mem0.ContentHash). A zero km.ContentHash cannot match: SHA-256 never
		// produces the zero digest.
		if km.ContentHash != (record.Hash{}) && mem0.ContentHash(m.Memory) == km.ContentHash {
			rec, err := buildKeptReconstructed(km, scope, m, at)
			return rec, err == nil, err
		}
	}

	return record.Record{}, false, nil
}

// buildKeptObserved assembles the Observed memory_kept (spec §5 row 4): the
// produced id was seen in the complete listing, so Mem0's own report is the
// evidence. The Observed payload is the listing entry Mem0 returned for the
// memory -- the observation that the memory is present.
func buildKeptObserved(scope record.Scope, m mem0.Memory, at time.Time) (record.Record, error) {
	payload, err := json.Marshal(m)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve kept: encode listed memory %s: %w", m.ID, err)
	}
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, payload)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve kept: build observed evidence for %s: %w", m.ID, err)
	}
	reason, err := record.NewObservedReason(record.ReasonStoredByMem0, ev)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve kept: build observed reason for %s: %w", m.ID, err)
	}

	contentHash := mem0.ContentHash(m.Memory)
	// No rule justifies an observation, so the rule version is empty; the key is
	// still a pure function of (kind, reasonKind, memoryID, contentHash) and
	// stable across passes.
	key, err := record.DeriveClaimIdemKey(record.EventMemoryKept, record.ReasonStoredByMem0, m.ID, hashString(contentHash), "")
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve kept: derive idempotency key for %s: %w", m.ID, err)
	}

	return record.Record{
		ID:             keptID(record.ReasonStoredByMem0, m.ID),
		At:             at,
		Event:          record.EventMemoryKept,
		Reason:         reason,
		Subject:        record.Subject{MemoryID: m.ID, Scope: scope, ContentHash: contentHash},
		IdempotencyKey: key,
	}, nil
}

// buildKeptReconstructed assembles the Reconstructed memory_kept (spec §5 row
// 5): the produced id was absent from the complete listing, but a listed
// memory's content hash equals the add's submitted text, so the memory is kept
// under a different id.
//
// The Subject identifies the memory that actually EXISTS in the store -- the
// matched listing entry -- while the basis records the add record the inference
// rests on, so the link back to the originating add survives even though the id
// changed (spec §3 decision 4). The rule and its version are recorded and the
// confidence is left unset (0), which the encoding omits rather than rendering
// as "0% confident".
func buildKeptReconstructed(km knownMemory, scope record.Scope, m mem0.Memory, at time.Time) (record.Record, error) {
	rule, ok := LookupRule(RuleKeptByContentMatch)
	if !ok {
		return record.Record{}, fmt.Errorf("resolve kept: rule %q is not registered", RuleKeptByContentMatch)
	}
	ev, err := record.NewReconstructedEvidence([]record.RecordID{km.Basis}, rule.Name, rule.Version, 0)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve kept: build reconstructed evidence for %s: %w", m.ID, err)
	}
	reason, err := record.NewReconstructedReason(record.ReasonKeptByContentMatch, ev)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve kept: build reconstructed reason for %s: %w", m.ID, err)
	}

	contentHash := mem0.ContentHash(m.Memory)
	// The claim identity is per memory, per rule: the listed memory id, the
	// content hash and the rule version. Nothing about the run enters it, so a
	// second pass over an unchanged store derives the identical key and
	// ledger.Append appends nothing.
	key, err := record.DeriveClaimIdemKey(record.EventMemoryKept, record.ReasonKeptByContentMatch, m.ID, hashString(contentHash), rule.Version)
	if err != nil {
		return record.Record{}, fmt.Errorf("resolve kept: derive idempotency key for %s: %w", m.ID, err)
	}

	return record.Record{
		ID:             keptID(record.ReasonKeptByContentMatch, m.ID),
		At:             at,
		Event:          record.EventMemoryKept,
		Reason:         reason,
		Subject:        record.Subject{MemoryID: m.ID, Scope: scope, ContentHash: contentHash},
		IdempotencyKey: key,
	}, nil
}

// keptID is the stable record id for a memory_kept claim about memoryID with
// the given reason kind.
//
// The invariant, as in addResolvedID: the id must vary with the reason kind.
// records.id is UNIQUE while duplicate suppression matches only the
// idempotency_key column, so two claims about the SAME memory with different
// kinds -- stored_by_mem0 (Observed) and kept_by_content_match (Reconstructed)
// -- must have different ids or the second append dies on a raw SQLite UNIQUE
// violation instead of appending the legitimate claim. The reason-kind
// qualifier mirrors the kind the idempotency key already carries, so the two
// constraints agree.
func keptID(kind record.ReasonKind, memoryID string) record.RecordID {
	return record.RecordID(string(record.EventMemoryKept) + ":" + string(kind) + ":" + memoryID)
}

// hashString renders a content hash as the lowercase hex string the claim
// idempotency key is built from, matching DeriveClaimIdemKey's documented
// "rendered as a string, e.g. hex" contract and how hashes are rendered
// elsewhere in the project.
func hashString(h record.Hash) string {
	return hex.EncodeToString(h[:])
}
