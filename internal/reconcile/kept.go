package reconcile

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"notary/internal/mem0"
	"notary/internal/record"
)

// contentDomain domain-separates the subject content hash from every other use
// of SHA-256 in Notary. It is the SAME tag internal/interceptor/library uses
// (there: "notary/content/v1", mem0.go's contentDomain): the add record's
// Subject.ContentHash was computed by the interceptor with that tag, so a
// content-match comparison is only meaningful against a digest over the
// identical domain.
const contentDomain = "notary/content/v1"

// contentHashOf returns the interceptor's content digest of one or more text
// parts: sha256(contentDomain ‖ (uint32be(len(part)) ‖ part)…).
//
// It is a deliberate, byte-for-byte mirror of
// internal/interceptor/library/mem0.go's contentHashOf. The reconciler may NOT
// depend on internal/interceptor (the design spec §4 makes the two peers that
// both depend on ledger), that function is unexported, and this task may not
// modify that package -- so the algorithm is reproduced here rather than
// imported. Reproducing it is not "a second hashing scheme": it must be the
// SAME scheme, or the comparison can never fire and kept_by_content_match would
// silently stop matching, a failure that looks exactly like "nothing happened".
// A change here that is not mirrored there (or vice versa) breaks the rule, so
// kept_test.go pins the digest against independent golden bytes.
//
// Each part is length-prefixed individually, so ["ab","c"] and ["a","bc"] hash
// differently: the boundary between submitted messages cannot collide with a
// differently-split submission. The interceptor hashes an add as
// contentHashOf(messages...) and a listed memory as contentHashOf(memory text),
// which is why a single-message add and the memory it produced agree.
func contentHashOf(parts ...string) record.Hash {
	h := sha256.New()
	h.Write([]byte(contentDomain))
	var n [4]byte
	for _, p := range parts {
		binary.BigEndian.PutUint32(n[:], uint32(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	var out record.Hash
	copy(out[:], h.Sum(nil))
	return out
}

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

	at, err := rc.establishingTimes(scoped)
	if err != nil {
		return nil, err
	}

	memories := enum.Items()
	var out []record.Record
	emitted := make(map[record.RecordID]struct{})
	for _, km := range scoped {
		rec, ok, err := keptRecord(km, scope, memories, at[km.Basis])
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

// establishingTimes resolves, for each known memory's establishing record id,
// that record's At -- the add event's time.
//
// The At question (Task 6 settled the reasoning, this applies it). A memory_kept
// record describes the same Mem0 event as the add whose product it is, so its At
// is that event's time, and RecordedAt is left zero for ledger.Append to stamp
// the true write time (a late reconciliation still keeps the event's time on
// At). Mem0's own CreatedAt/UpdatedAt -- on the event status and on the listed
// memory -- are deliberately NOT used: neither is when the event happened.
//
// The add event's time is not carried by knownMemory, and this task may not
// change the worklist's shape, so it is recovered from the establishing record
// itself (knownMemory.Basis) through the Reader the reconciler already holds.
// That record's At IS the add event's time: buildAddResolved copies add.At onto
// the add_resolved record, and a memory_kept record established by a later pass
// carries the same At forward. A basis id that is not found leaves the zero time
// rather than guessing; it can only happen if the ledger contradicts itself.
func (rc *Reconciler) establishingTimes(known []knownMemory) (map[record.RecordID]time.Time, error) {
	at := make(map[record.RecordID]time.Time, len(known))
	if rc.reader == nil {
		// No reader to consult (the reconciler was built without one). Never
		// panic; the caller gets the zero time.
		return at, nil
	}

	wanted := make(map[record.RecordID]struct{}, len(known))
	for _, km := range known {
		wanted[km.Basis] = struct{}{}
	}

	records, err := rc.reader.ListRecords(ledgerEarliest(), ledgerLatest())
	if err != nil {
		return nil, fmt.Errorf("resolve kept: read ledger for establishing records: %w", err)
	}
	for _, r := range records {
		if _, ok := wanted[r.ID]; ok {
			at[r.ID] = r.At
		}
	}
	return at, nil
}

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
		// Subject.ContentHash). Comparing it against contentHashOf(m.Memory)
		// requires the interceptor's exact scheme (see contentHashOf). A zero
		// km.ContentHash cannot match: SHA-256 never produces the zero digest.
		if km.ContentHash != (record.Hash{}) && contentHashOf(m.Memory) == km.ContentHash {
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

	contentHash := contentHashOf(m.Memory)
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

	contentHash := contentHashOf(m.Memory)
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
