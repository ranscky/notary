package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"notary/internal/mem0"
	"notary/internal/record"
)

// The Mem0 event-status values the add-resolution producer maps. They are
// Mem0's own wire vocabulary, compared literally (spec §5 rows 1-3).
const (
	mem0StatusSucceeded = "SUCCEEDED"
	mem0StatusFailed    = "FAILED"
)

// ErrNoMem0Client reports that resolveAdd was asked to resolve an add but the
// reconciler holds no Mem0 client, so the event cannot be polled. A nil client
// is a MISCONFIGURED reconciler, not a "nothing to resolve" state: returning no
// record would make a broken deployment look like a clean pass that found
// nothing, conflating "not configured" with "still PENDING". The pass fails
// loudly instead (spec §9.2). It is exported so a caller can detect it.
var ErrNoMem0Client = errors.New("reconcile: no Mem0 client configured")

// resolveAdd derives the add_resolved claim (spec §5 rows 1-3) for one
// unresolved add_requested record, or none when the add has no terminal
// outcome yet.
//
// It reads the Mem0 event id from the add record's Observed AddPayload, asks
// Mem0 for that event's status, and maps it:
//
//	SUCCEEDED, results non-empty -> add_resolved + stored_by_mem0     (Observed)
//	FAILED                       -> add_resolved + add_failed         (Observed)
//	SUCCEEDED, results empty     -> add_resolved + no_facts_extracted (Reconstructed)
//	anything else (e.g. PENDING) -> nothing
//
// The first two are Observed because Mem0 reported them directly: the evidence
// is the event-status response itself. The third is Reconstructed because the
// observation is only "the results array was empty"; the claim that Mem0
// extracted no facts is an interpretation of it, and
// ReasonNoFactsExtracted.AllowedTier already fixes its tier.
//
// A non-terminal status (still PENDING, or anything unrecognised) is NOT an
// error and writes NOTHING. The add is simply not resolved yet, so the producer
// invents no resolution; the add stays in the worklist on every pass, which
// keeps a stuck add visible to an operator without a placeholder polluting the
// signed chain (spec §9.1). A Mem0 error, and a nil Mem0 client (a
// misconfigured reconciler), by contrast, fail the pass loudly (spec §9.2).
//
// It never panics and writes nothing itself: it returns records whose Seq and
// Hash are zero, for the caller to append.
func (rc *Reconciler) resolveAdd(ctx context.Context, add record.Record) ([]record.Record, error) {
	eventID, ok, err := eventID(add)
	if err != nil {
		return nil, err
	}
	if !ok {
		// An add with no event id cannot be polled; there is nothing to
		// resolve it against.
		return nil, nil
	}
	if rc.client == nil {
		// A nil client is a misconfigured reconciler, not a state that means
		// "nothing to resolve". It must not panic (never panic in library
		// code), and it must not look like a clean pass that found nothing:
		// report the misconfiguration loudly (spec §9.2).
		return nil, fmt.Errorf("resolve add %s: %w", add.ID, ErrNoMem0Client)
	}

	status, err := rc.client.EventStatus(ctx, eventID)
	if err != nil {
		return nil, fmt.Errorf("resolve add %s: fetch event status %s: %w", add.ID, eventID, err)
	}

	var kind record.ReasonKind
	switch {
	case status.Status == mem0StatusSucceeded && len(status.Results) > 0:
		kind = record.ReasonStoredByMem0
	case status.Status == mem0StatusFailed:
		kind = record.ReasonAddFailed
	case status.Status == mem0StatusSucceeded && len(status.Results) == 0:
		kind = record.ReasonNoFactsExtracted
	default:
		// Not terminal (PENDING, ...), or a status Notary does not map: no
		// claim is warranted.
		return nil, nil
	}

	rec, err := buildAddResolved(add, eventID, kind, status)
	if err != nil {
		return nil, err
	}
	return []record.Record{rec}, nil
}

// buildAddResolved assembles the single add_resolved record for a resolved add.
//
// The record carries the add's scope and content hash, so it describes the same
// operation as the add_requested it resolves and stays inside that operation's
// scope for the fold.
//
// At is the ADD EVENT's time (the add record's At): the resolution is a fact
// about that same Mem0 event, so it belongs to that event's window. Mem0's
// CompletedAt and UpdatedAt are deliberately NOT used -- neither is when the
// event happened, and the record's own write time belongs in RecordedAt, which
// is left zero for ledger.Append to stamp at append time. That stamping is what
// keeps a late write honest: a record reconciled hours after its event still
// carries the true write time on RecordedAt while At keeps the event's time.
//
// Subject.Scope and Subject.ContentHash are COPIED FROM THE ADD record: the
// ContentHash is the ADD's submitted content digest (the hash of the text the
// caller asked Mem0 to store), NOT the stored memory's digest. A reader must
// not mistake it for the produced memory's content hash.
//
// An observed storage additionally carries the produced memory's id on the
// Subject, which is the link a later coverage claim uses to pair the memory
// with an enumeration (spec §4.2, decision 4). The other outcomes produce no
// memory, so their Subject.MemoryID is left empty.
func buildAddResolved(add record.Record, eventID string, kind record.ReasonKind, status mem0.EventStatusResponse) (record.Record, error) {
	memoryID := ""
	if kind == record.ReasonStoredByMem0 {
		memoryID = firstResultID(status.Results)
	}

	reason, err := addResolvedReason(kind, add.ID, status)
	if err != nil {
		return record.Record{}, err
	}

	key, err := addResolvedIdemKey(add, eventID, kind)
	if err != nil {
		return record.Record{}, err
	}

	return record.Record{
		ID:     addResolvedID(kind, eventID),
		At:     add.At,
		Event:  record.EventAddResolved,
		Reason: reason,
		Subject: record.Subject{
			MemoryID:    memoryID,
			Scope:       add.Subject.Scope,
			ContentHash: add.Subject.ContentHash,
		},
		IdempotencyKey: key,
	}, nil
}

// addResolvedIdemKey derives the idempotency key for an add_resolved, keyed on
// the Mem0 EVENT ID using the existing record.DeriveIdemKey (not
// DeriveClaimIdemKey, which keys a claim whose subject is a memory).
//
// The resolution of a given event is a unique and stable fact about that
// event, so re-running the reconciler derives the identical key and
// Ledger.Append treats the second write as a duplicate rather than appending a
// second add_resolved.
//
// The reason kind is part of the key, exactly as it is part of the record id
// (addResolvedID): a different kind for the same event is a DIFFERENT claim, so
// it must both derive a different key -- or the store's duplicate suppression,
// which matches on idempotency_key alone, would silently drop it -- and carry a
// different id -- or the store's UNIQUE records.id column would reject it with
// a raw SQLite error. Key and id move together so the two constraints agree.
//
// This does NOT contradict the design spec's "never key on Mem0's event_id"
// rule for the request path. That rule exists because a RETRIED add comes back
// with a fresh event id, so keying an add_requested on the response's event id
// would fail to deduplicate the caller's retry and could collide two distinct
// operations sharing a response id. Here the event id IS the subject — there is
// exactly one resolution per event — so it is the correct, stable identifier.
//
// The scope is the add's scope, the request digest is the zero Hash (nothing
// content-derived belongs in this key), and there is no correlation id: the
// event id alone identifies the logical event.
func addResolvedIdemKey(add record.Record, eventID string, kind record.ReasonKind) (record.IdemKey, error) {
	key, err := record.DeriveIdemKey(record.EventAddResolved, kind, add.Subject.Scope, eventID, "", record.Hash{})
	if err != nil {
		return "", fmt.Errorf("resolve add %s: derive idempotency key: %w", add.ID, err)
	}
	return key, nil
}

// addResolvedReason builds the Reason for an add_resolved of the given kind,
// through the tier-specific constructors so the tier can never contradict the
// kind: the two Observed kinds carry the event-status response as their
// evidence, and the one Reconstructed kind carries the add record's id as its
// basis, the registry rule and version, and NO confidence (an unset confidence
// is omitted from the encoded reason — spec §8).
func addResolvedReason(kind record.ReasonKind, addID record.RecordID, status mem0.EventStatusResponse) (record.Reason, error) {
	switch kind {
	case record.ReasonStoredByMem0, record.ReasonAddFailed:
		payload, err := json.Marshal(status)
		if err != nil {
			return record.Reason{}, fmt.Errorf("resolve add %s: encode event status evidence: %w", addID, err)
		}
		ev, err := record.NewObservedEvidence(record.SourceMem0Response, payload)
		if err != nil {
			return record.Reason{}, fmt.Errorf("resolve add %s: build observed evidence: %w", addID, err)
		}
		reason, err := record.NewObservedReason(kind, ev)
		if err != nil {
			return record.Reason{}, fmt.Errorf("resolve add %s: build observed reason: %w", addID, err)
		}
		return reason, nil

	case record.ReasonNoFactsExtracted:
		rule, ok := LookupRule(RuleNoFactsExtracted)
		if !ok {
			return record.Reason{}, fmt.Errorf("resolve add %s: rule %q is not registered", addID, RuleNoFactsExtracted)
		}
		ev, err := record.NewReconstructedEvidence([]record.RecordID{addID}, rule.Name, rule.Version, 0)
		if err != nil {
			return record.Reason{}, fmt.Errorf("resolve add %s: build reconstructed evidence: %w", addID, err)
		}
		reason, err := record.NewReconstructedReason(record.ReasonNoFactsExtracted, ev)
		if err != nil {
			return record.Reason{}, fmt.Errorf("resolve add %s: build reconstructed reason: %w", addID, err)
		}
		return reason, nil

	default:
		return record.Reason{}, fmt.Errorf("resolve add %s: unmapped reason kind %q", addID, kind)
	}
}

// firstResultID returns the first non-empty memory id among the event's
// results, or "" when none carries one.
func firstResultID(results []mem0.EventResult) string {
	for _, r := range results {
		if r.ID != "" {
			return r.ID
		}
	}
	return ""
}

// addResolvedID is the stable record id for the add_resolved of eventID with
// the given reason kind. It is qualified by BOTH the event type and the reason
// kind.
//
// The event-type prefix keeps it from colliding with the add_requested record's
// id (the caller's correlation id), which describes the same operation. The
// kind qualifier keeps it from colliding with a DIFFERENT claim about the SAME
// event: records.id is UNIQUE, and duplicate suppression matches only the
// idempotency_key column, so two kinds sharing one id would make the second
// append die on a raw SQLite UNIQUE violation instead of appending the
// legitimate second claim. Qualifying the id mirrors the kind the idempotency
// key already carries, so both constraints agree.
func addResolvedID(kind record.ReasonKind, eventID string) record.RecordID {
	return record.RecordID(string(record.EventAddResolved) + ":" + string(kind) + ":" + eventID)
}
