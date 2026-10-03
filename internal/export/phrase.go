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
// Totality is the property that matters most: Phrase is a total function over
// the (event, reason kind) pairs record can construct, so a new claim kind
// cannot ship with no wording. The second result is false only for a pairing
// record does not construct, which TestPhraseIsTotalOverConstructibleRecords
// proves is not reachable from the producers. Callers must treat a false result
// as a defect, not as an empty sentence that is fine to render.
func Phrase(rec record.Record) (string, bool) {
	kind := rec.Reason.Kind()

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
