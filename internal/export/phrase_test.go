package export_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/export"
	"notary/internal/reconcile"
	"notary/internal/record"
)

// constructiblePair is one (event, reason kind) combination the record package
// can actually build, drawn from the two production paths: the interceptor
// library (add_requested, search_performed, memory_surfaced) and the
// reconciler (add_resolved, memory_kept, memory_dropped), plus the audit-gap
// path in the interceptor itself.
type constructiblePair struct {
	event   record.EventType
	kind    record.ReasonKind
	memID   string // the memory id the record carries, "" when the event has none
	comment string
}

// constructiblePairs is the enumeration totality is checked against. It is the
// source of the design's §7 table. A pair missing here is a pair a new claim
// kind could ship without wording, so it is written out in full rather than
// computed: the point is that a human reconciles it against the producers.
var constructiblePairs = []constructiblePair{
	{record.EventAddRequested, record.ReasonAddAcknowledged, "", "interceptor/library: an add was acknowledged"},
	{record.EventAddResolved, record.ReasonStoredByMem0, "mem-stored", "reconcile/adds: the add produced a memory"},
	{record.EventAddResolved, record.ReasonNoFactsExtracted, "", "reconcile/adds: the add produced no facts"},
	{record.EventAddResolved, record.ReasonAddFailed, "", "reconcile/adds: the add failed"},
	{record.EventMemoryKept, record.ReasonStoredByMem0, "mem-kept", "reconcile/kept: the memory is present"},
	{record.EventMemoryKept, record.ReasonKeptByContentMatch, "mem-matched", "reconcile/kept: kept under a different id"},
	{record.EventMemoryDropped, record.ReasonRemovedByMem0, "mem-removed", "reconcile/removed: Mem0 removed it"},
	{record.EventMemoryDropped, record.ReasonAbsentFromSearch, "mem-absent", "reconcile/absent: a search did not return it"},
	{record.EventSearchPerformed, record.ReasonSearchPerformed, "", "interceptor/library: a search ran"},
	{record.EventMemorySurfaced, record.ReasonReturnedBySearch, "mem-surfaced", "interceptor/library: a result surfaced"},
	{record.EventAuditGap, record.ReasonAuditUnavailable, "", "interceptor: an operation was not recorded"},
}

// reasonFor builds a valid Reason for kind, choosing the tier constructor the
// kind's AllowedTier fixes, so the test does not re-encode the tier mapping.
func reasonFor(t *testing.T, kind record.ReasonKind) record.Reason {
	t.Helper()
	tier, ok := kind.AllowedTier()
	require.Truef(t, ok, "kind %q has no allowed tier", kind)

	switch tier {
	case record.Observed:
		ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"x":1}`))
		require.NoError(t, err)
		reason, err := record.NewObservedReason(kind, ev)
		require.NoError(t, err)
		return reason
	case record.Reconstructed:
		ev, err := record.NewReconstructedEvidence([]record.RecordID{"basis-1"}, "rule", "1", 0)
		require.NoError(t, err)
		reason, err := record.NewReconstructedReason(kind, ev)
		require.NoError(t, err)
		return reason
	case record.Internal:
		note, err := record.NewInternalNote("note")
		require.NoError(t, err)
		reason, err := record.NewInternalReason(kind, note)
		require.NoError(t, err)
		return reason
	default:
		t.Fatalf("unhandled tier %v for kind %q", tier, kind)
		return record.Reason{}
	}
}

// recordFor builds a valid record for a constructible pair.
func recordFor(t *testing.T, p constructiblePair) record.Record {
	t.Helper()
	var contentHash record.Hash
	for i := range contentHash {
		contentHash[i] = 0x11
	}
	return record.Record{
		ID:      record.RecordID("rec-" + string(p.event) + "-" + string(p.kind)),
		Event:   p.event,
		Reason:  reasonFor(t, p.kind),
		Subject: record.Subject{MemoryID: p.memID, ContentHash: contentHash},
	}
}

// TestPhraseIsTotalOverConstructibleRecords is the guard the design calls
// "totality": every pair the record package can construct must have wording. An
// unphrased claim in a compliance export reads as a missing record, so Phrase
// returning ("", false) for a constructible pair is a failure here.
func TestPhraseIsTotalOverConstructibleRecords(t *testing.T) {
	var unphrased []string
	for _, p := range constructiblePairs {
		rec := recordFor(t, p)
		sentence, ok := export.Phrase(rec)
		if !ok || sentence == "" {
			unphrased = append(unphrased,
				fmt.Sprintf("%s + %s (%s)", p.event, p.kind, p.comment))
			continue
		}
	}
	if len(unphrased) > 0 {
		t.Fatalf("Phrase has no wording for %d constructible (event, kind) pair(s): %v",
			len(unphrased), unphrased)
	}
}

// TestRenderedLineCarriesTheStructuredFieldsBesideThePhrasing is the design's
// "subordination" property: the sentence is printed beside the structured
// fields, never instead of them. It renders a constructible record and asserts
// both are present, so a reader can ignore the prose and no consumer can depend
// on it.
func TestRenderedLineCarriesTheStructuredFieldsBesideThePhrasing(t *testing.T) {
	rec := recordFor(t, constructiblePair{
		event: record.EventMemoryKept,
		kind:  record.ReasonStoredByMem0,
		memID: "mem-1",
	})
	got := renderJSON(t, rec, false)

	// The structured fields the sentence restates are all present, unchanged.
	for _, field := range []string{"seq", "id", "event", "tier", "reason_kind", "memory_id", "content_hash", "hash"} {
		_, ok := got[field]
		assert.Truef(t, ok, "structured field %q must be present beside the phrasing", field)
	}
	assert.Equal(t, "memory_kept", got["event"])
	assert.Equal(t, "stored_by_mem0", got["reason_kind"])
	assert.Equal(t, "mem-1", got["memory_id"])

	// ...and the phrasing sits beside them, never instead of them.
	phrasing, ok := got["phrasing"]
	require.True(t, ok, "the phrasing field must be present")
	assert.NotEmpty(t, phrasing, "the phrasing must not be empty")
}

// TestEveryReconcilerRuleKindIsPhrased ties totality to the reconciler's rule
// registry: a new rule that yields a kind the enumeration does not cover fails
// here, so the registry cannot grow a claim kind past this guard.
func TestEveryReconcilerRuleKindIsPhrased(t *testing.T) {
	covered := map[record.ReasonKind]bool{}
	for _, p := range constructiblePairs {
		covered[p.kind] = true
	}
	for _, rule := range reconcile.Rules() {
		assert.Truef(t, covered[rule.Kind],
			"reconciler rule %q yields kind %q, which the constructible enumeration does not cover",
			rule.Name, rule.Kind)
	}
}
