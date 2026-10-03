package export_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/export"
	"notary/internal/reconcile"
	"notary/internal/record"
)

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

// recordFor builds a valid record for one (event, kind) pair carrying memID.
func recordFor(t *testing.T, event record.EventType, kind record.ReasonKind, memID string) record.Record {
	t.Helper()
	var contentHash record.Hash
	for i := range contentHash {
		contentHash[i] = 0x11
	}
	return record.Record{
		ID:      record.RecordID("rec-" + string(event) + "-" + string(kind)),
		Event:   event,
		Reason:  reasonFor(t, kind),
		Subject: record.Subject{MemoryID: memID, ContentHash: contentHash},
	}
}

// TestPhraseIsTotalOverTheVocabulary is the guard the design calls "totality".
//
// It enumerates the FULL cross-product of record's event and reason-kind
// vocabularies -- not a hand-written list, which was the hole the review found:
// a pair could be declared and go unworded while the list stayed silent. The
// vocabulary is the single source of truth (record.EventTypes,
// record.ReasonKinds), and a gate in internal/record proves it complete against
// the declarations. So no pair is unworded by construction.
func TestPhraseIsTotalOverTheVocabulary(t *testing.T) {
	var unphrased, placeholders []string
	for _, event := range record.EventTypes() {
		for _, kind := range record.ReasonKinds() {
			rec := recordFor(t, event, kind, "mem-1")
			sentence, ok := export.Phrase(rec)
			if !ok || sentence == "" {
				unphrased = append(unphrased, string(event)+" + "+string(kind))
				continue
			}
			// A sentence must not read as a placeholder: no angle-bracket
			// template, no raw fmt verb.
			if strings.ContainsAny(sentence, "<>%") {
				placeholders = append(placeholders, string(event)+" + "+string(kind)+": "+sentence)
			}
		}
	}
	require.Emptyf(t, unphrased,
		"Phrase has no wording for %d vocabulary (event, kind) pair(s): %v", len(unphrased), unphrased)
	require.Emptyf(t, placeholders,
		"Phrase produced a placeholder-looking sentence for %d pair(s): %v", len(placeholders), placeholders)
}

// TestPhraseKeepsTheSpecifiedSentences pins the eleven sentences the design
// fixes for the pairs a producer emits today. Phrase may grow a generic
// sentence for every other pair, but these exact words must not change.
func TestPhraseKeepsTheSpecifiedSentences(t *testing.T) {
	cases := []struct {
		event record.EventType
		kind  record.ReasonKind
		memID string
		want  string
	}{
		{record.EventAddRequested, record.ReasonAddAcknowledged, "", "an add was requested and Mem0 acknowledged it"},
		{record.EventAddResolved, record.ReasonStoredByMem0, "mem-7", "the add resolved: Mem0 stored memory mem-7"},
		{record.EventAddResolved, record.ReasonNoFactsExtracted, "", "the add resolved: no facts were extracted from the interaction"},
		{record.EventAddResolved, record.ReasonAddFailed, "", "the add resolved: the add to Mem0 failed"},
		{record.EventMemoryKept, record.ReasonStoredByMem0, "mem-1", "the memory was kept; it is still present in the scope"},
		{record.EventMemoryKept, record.ReasonKeptByContentMatch, "mem-2", "the memory was kept; its content matched an earlier add"},
		{record.EventMemoryDropped, record.ReasonRemovedByMem0, "mem-3", "the memory was dropped: Mem0 no longer holds it"},
		{record.EventMemoryDropped, record.ReasonAbsentFromSearch, "mem-4", "the memory was dropped: a covering search did not return it"},
		{record.EventSearchPerformed, record.ReasonSearchPerformed, "", "a search ran in this scope"},
		{record.EventMemorySurfaced, record.ReasonReturnedBySearch, "mem-5", "the memory was returned by a search"},
		{record.EventAuditGap, record.ReasonAuditUnavailable, "", "an operation happened that Notary failed to record"},
	}
	for _, c := range cases {
		sentence, ok := export.Phrase(recordFor(t, c.event, c.kind, c.memID))
		require.Truef(t, ok, "%s + %s must be phrased", c.event, c.kind)
		assert.Equalf(t, c.want, sentence, "%s + %s", c.event, c.kind)
	}
}

// TestPhraseGenericSentenceForAnUnconstructedPair pins the shape of the generic
// sentence a pair no producer emits gets: it names the event and the reason
// kind and asserts nothing else, so it is honest rather than a placeholder.
func TestPhraseGenericSentenceForAnUnconstructedPair(t *testing.T) {
	sentence, ok := export.Phrase(recordFor(t, record.EventSearchPerformed, record.ReasonAddFailed, ""))
	require.True(t, ok, "an unconstructed but in-vocabulary pair still has wording")
	assert.Equal(t, "a search ran in this scope, recorded with reason: the add to Mem0 failed.", sentence)
}

// TestPhraseStoredAddWithoutAMemoryID exercises the fallback the specific
// sentences cannot cover: a stored add whose Mem0 event status reports no
// results, so firstResultID returned "". Phrase must still produce wording, and
// a NAMELESS one rather than a dangling "Mem0 stored memory " with an empty id.
func TestPhraseStoredAddWithoutAMemoryID(t *testing.T) {
	sentence, ok := export.Phrase(recordFor(t, record.EventAddResolved, record.ReasonStoredByMem0, ""))
	require.True(t, ok, "a memoryless stored add is still a constructible pair with wording")
	assert.Equal(t, "the add resolved: Mem0 stored a memory", sentence)
}

// TestRenderedLineCarriesTheStructuredFieldsBesideThePhrasing is the design's
// "subordination" property: the sentence is printed beside the structured
// fields, never instead of them.
func TestRenderedLineCarriesTheStructuredFieldsBesideThePhrasing(t *testing.T) {
	rec := recordFor(t, record.EventMemoryKept, record.ReasonStoredByMem0, "mem-1")
	got := renderJSON(t, rec, false)

	for _, field := range []string{"seq", "id", "event", "tier", "reason_kind", "memory_id", "content_hash", "hash"} {
		_, ok := got[field]
		assert.Truef(t, ok, "structured field %q must be present beside the phrasing", field)
	}
	assert.Equal(t, "memory_kept", got["event"])
	assert.Equal(t, "stored_by_mem0", got["reason_kind"])
	assert.Equal(t, "mem-1", got["memory_id"])

	phrasing, ok := got["phrasing"]
	require.True(t, ok, "the phrasing field must be present")
	assert.NotEmpty(t, phrasing, "the phrasing must not be empty")
}

// TestEveryReconcilerRuleKindIsPhrased keeps the reconciler honest: every kind
// its Rules() registry yields must be part of record's vocabulary, and the
// vocabulary is phrased in full (above) and proven complete in internal/record.
func TestEveryReconcilerRuleKindIsPhrased(t *testing.T) {
	vocabulary := map[record.ReasonKind]bool{}
	for _, kind := range record.ReasonKinds() {
		vocabulary[kind] = true
	}
	for _, rule := range reconcile.Rules() {
		assert.Truef(t, vocabulary[rule.Kind],
			"reconciler rule %q yields kind %q, which record's vocabulary does not list", rule.Name, rule.Kind)
	}
}
