package export

import "notary/internal/record"

// Phrase restates a record's claim as one human sentence, and reports whether
// it has one.
//
// The sentence is derived mechanically, in the words of the design's §7, from
// fields the record already carries: Event, the tier Reason.Kind() fixes, and
// the subject's memory id when the record has one. It invents nothing about the
// claim, so a reader can ignore it; it is printed beside the structured fields,
// never instead of them, and no consumer may depend on it.
//
// Totality is the property that matters most: an unphrased claim reads as a
// MISSING RECORD in a compliance export, so Phrase is a total function over the
// full cross-product of record's event and reason-kind vocabularies. Those
// vocabularies are the single source of truth (record.EventTypes,
// record.ReasonKinds), and a gate in internal/record source-scans the package's
// typed constant and var declarations to keep them complete: a kind declared in
// one of those forms cannot be left out of the list without the gate failing.
// The gate does not see an untyped `const X = "..."` (no type to key on), so
// that one form could still slip past it; a value from that form reaching a
// producer is caught by Render rather than silently exported unphrased.
//
// Two tiers of wording. The eleven pairs a producer emits today get a specific,
// fixed sentence (the design's §7 table); those are the words an auditor reads.
// Every other pair the vocabulary admits -- fifty-nine of the seventy, which no
// producer emits today -- gets a deliberate generic sentence built from the
// event and the reason kind, e.g. "a search ran in this scope, recorded with
// reason: the add to Mem0 failed." What the generic sentence does NOT give a
// reader is any specificity it cannot support: it names the event and the
// reason kind and asserts nothing about a relationship between them that no
// claim actually makes. It is honest about being generic rather than pretending
// to describe an operation that never happened.
//
// The second result is false only for an event or reason kind outside the
// vocabulary. That is not dead weight: Render does not otherwise validate the
// event, so a record carrying an unknown event reaches Phrase, and the false
// result lets Render fail loudly instead of emitting a line whose prose reads
// as a missing record.
func Phrase(rec record.Record) (string, bool) {
	kind := rec.Reason.Kind()

	if sentence, ok := specifiedPhrase(rec, kind); ok {
		return sentence, true
	}

	event, ok := eventClause(rec.Event)
	if !ok {
		return "", false
	}
	reason, ok := reasonClause(kind)
	if !ok {
		return "", false
	}
	return event + ", recorded with reason: " + reason + ".", true
}

// specifiedPhrase returns the fixed sentence for a pair a producer emits today,
// or ("", false) for any other pair. These eleven sentences are specified in
// the design (§7) and must not change.
func specifiedPhrase(rec record.Record, kind record.ReasonKind) (string, bool) {
	switch rec.Event {
	case record.EventAddRequested:
		if kind == record.ReasonAddAcknowledged {
			return "an add was requested and Mem0 acknowledged it", true
		}

	case record.EventAddResolved:
		switch kind {
		case record.ReasonStoredByMem0:
			// The produced memory's id is on the subject (the reconciler
			// records it from Mem0's response). Fall back to a nameless
			// sentence rather than printing a dangling id.
			if rec.Subject.MemoryID == "" {
				return "the add resolved: Mem0 stored a memory", true
			}
			return "the add resolved: Mem0 stored memory " + rec.Subject.MemoryID, true
		case record.ReasonNoFactsExtracted:
			return "the add resolved: no facts were extracted from the interaction", true
		case record.ReasonAddFailed:
			return "the add resolved: the add to Mem0 failed", true
		}

	case record.EventMemoryKept:
		switch kind {
		case record.ReasonStoredByMem0:
			return "the memory was kept; it is still present in the scope", true
		case record.ReasonKeptByContentMatch:
			return "the memory was kept; its content matched an earlier add", true
		}

	case record.EventMemoryDropped:
		switch kind {
		case record.ReasonRemovedByMem0:
			return "the memory was dropped: Mem0 no longer holds it", true
		case record.ReasonAbsentFromSearch:
			return "the memory was dropped: a covering search did not return it", true
		}

	case record.EventSearchPerformed:
		if kind == record.ReasonSearchPerformed {
			return "a search ran in this scope", true
		}

	case record.EventMemorySurfaced:
		if kind == record.ReasonReturnedBySearch {
			return "the memory was returned by a search", true
		}

	case record.EventAuditGap:
		if kind == record.ReasonAuditUnavailable {
			return "an operation happened that Notary failed to record", true
		}
	}

	return "", false
}

// eventClause names the event generically, for the generic sentence. The second
// result is false for an event outside the vocabulary. It covers every member
// of record.EventTypes(); the export totality test fails if one is missing.
func eventClause(event record.EventType) (string, bool) {
	switch event {
	case record.EventAddRequested:
		return "an add was requested", true
	case record.EventAddResolved:
		return "an add request was resolved", true
	case record.EventSearchPerformed:
		return "a search ran in this scope", true
	case record.EventMemorySurfaced:
		return "a memory surfaced from a search", true
	case record.EventMemoryKept:
		return "a memory was kept", true
	case record.EventMemoryDropped:
		return "a memory was dropped", true
	case record.EventAuditGap:
		return "an operation went unrecorded", true
	default:
		return "", false
	}
}

// reasonClause names the reason kind generically, for the generic sentence. The
// second result is false for a kind outside the vocabulary. It covers every
// member of record.ReasonKinds(); the export totality test fails if one is
// missing.
func reasonClause(kind record.ReasonKind) (string, bool) {
	switch kind {
	case record.ReasonSearchPerformed:
		return "a search was performed", true
	case record.ReasonAddAcknowledged:
		return "Mem0 acknowledged the add", true
	case record.ReasonReturnedBySearch:
		return "a search returned the memory", true
	case record.ReasonStoredByMem0:
		return "Mem0 stored the memory", true
	case record.ReasonKeptByContentMatch:
		return "its content matched an earlier add", true
	case record.ReasonAbsentFromSearch:
		return "a covering search did not return the memory", true
	case record.ReasonNoFactsExtracted:
		return "no facts were extracted", true
	case record.ReasonRemovedByMem0:
		return "Mem0 removed the memory", true
	case record.ReasonAddFailed:
		return "the add to Mem0 failed", true
	case record.ReasonAuditUnavailable:
		return "the audit trail was unavailable", true
	default:
		return "", false
	}
}
