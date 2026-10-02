package library_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
	"notary/internal/interceptor"
	"notary/internal/interceptor/library"
	"notary/internal/mem0"
	"notary/internal/record"
)

// ---------------------------------------------------------------------------
// the per-call sensitivity option (spec §5, caller-supplied path)
// ---------------------------------------------------------------------------

// TestSensitiveOptionMarksObservedContent verifies that library.Sensitive()
// makes the content an Add records classified sensitive: the option sets
// Content.Sensitive on the add_requested record before it is hashed.
func TestSensitiveOptionMarksObservedContent(t *testing.T) {
	l, _, g, gapPath := newHarness(t)
	srv := serve(t, &capture{}, http.StatusOK, fixture(t, "add_response.json"))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	scope := record.Scope{UserID: "u1", AgentID: "a1"}
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), scope, fixedNow(at))

	_, err := ic.Add(context.Background(), "corr-sens", []string{"hello", "world"}, library.Sensitive())
	require.NoError(t, err)

	rec, err := l.GetRecord("corr-sens")
	require.NoError(t, err)
	require.NotNil(t, rec.Content)
	assert.True(t, rec.Content.Sensitive, "Sensitive() must mark the recorded content sensitive")
	assert.Equal(t, "hello\nworld", rec.Content.Text, "the option changes classification, not content")

	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Empty(t, entries, "a healthy write logs no gap")
}

// TestSensitiveOptionMarksSurfacedContent verifies that the option is honoured
// at the OTHER Content construction site: every memory_surfaced record a Search
// writes carries the classification.
func TestSensitiveOptionMarksSurfacedContent(t *testing.T) {
	l, _, g, gapPath := newHarness(t)
	srv := serve(t, &capture{}, http.StatusOK, fixture(t, "search_response.json"))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	scope := record.Scope{UserID: "notary-fixture-user-a1b2c3"}
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), scope, fixedNow(at))

	_, err := ic.Search(context.Background(), "corr-sens-search", mem0.SearchRequest{Query: "q"}, library.Sensitive())
	require.NoError(t, err)

	surfaced, err := l.GetRecord("corr-sens-search#1")
	require.NoError(t, err)
	require.NotNil(t, surfaced.Content)
	assert.True(t, surfaced.Content.Sensitive, "Sensitive() must mark each surfaced record's content sensitive")

	// The search_performed record carries no Content at all: there is nothing
	// there to classify, and the option must not invent one.
	performed, err := l.GetRecord("corr-sens-search")
	require.NoError(t, err)
	assert.Nil(t, performed.Content, "the search_performed record stays content-free")

	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestDefaultIsNotSensitive verifies that a call passing no option leaves the
// content unclassified: Sensitive false, meaning "unclassified", not verified
// non-sensitive.
func TestDefaultIsNotSensitive(t *testing.T) {
	l, _, g, _ := newHarness(t)
	srv := serve(t, &capture{}, http.StatusOK, fixture(t, "add_response.json"))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), record.Scope{UserID: "u1"}, fixedNow(at))

	_, err := ic.Add(context.Background(), "corr-default", []string{"hello"})
	require.NoError(t, err)

	rec, err := l.GetRecord("corr-default")
	require.NoError(t, err)
	require.NotNil(t, rec.Content)
	assert.False(t, rec.Content.Sensitive, "with no option the content stays unclassified")
}

// TestSensitiveChangesTheHash pins that the classification is tamper-evident:
// Content.Sensitive is mixed into the record hash (see record.CanonicalBytes),
// so the SAME record encoded once sensitive and once unclassified computes two
// different digests. This is why the per-call option needs no hashing change --
// the digest already covers the flag, so a record whose flag is flipped after
// the fact no longer verifies.
func TestSensitiveChangesTheHash(t *testing.T) {
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"event_id":"e","status":"PENDING"}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonAddAcknowledged, ev)
	require.NoError(t, err)

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	// Everything but Content.Sensitive is held identical -- including the chain
	// position (Seq, PrevHash), which is otherwise assigned per append and would
	// confound the comparison.
	base := record.Record{
		ID:         record.RecordID("corr-hash"),
		At:         at,
		RecordedAt: at,
		Event:      record.EventAddRequested,
		Reason:     reason,
		Subject:    record.Subject{Scope: record.Scope{UserID: "u1"}, ContentHash: record.ContentHash("hello")},
	}

	plain := base
	plain.Content = &record.Content{Text: "hello"}
	marked := base
	marked.Content = &record.Content{Text: "hello", Sensitive: true}

	plainHash, err := record.ComputeHash(plain)
	require.NoError(t, err)
	markedHash, err := record.ComputeHash(marked)
	require.NoError(t, err)

	assert.NotEqual(t, plainHash, markedHash,
		"flipping Content.Sensitive must change the record hash")
	assert.NotEqual(t, record.Hash{}, plainHash, "sanity: a real digest, not the zero value")
}

// ---------------------------------------------------------------------------
// the rule-supplied path, applied at construction and consulted at write time
// (spec §5, rule-supplied path; spec §3 D3)
// ---------------------------------------------------------------------------

// TestRuleMarksContentSensitive verifies that a rule matching the interceptor's
// scope marks an ordinary Add -- one passing no per-call option -- sensitive:
// the rule set is consulted where the add_requested record's Content is built,
// before it is hashed.
func TestRuleMarksContentSensitive(t *testing.T) {
	l, _, g, gapPath := newHarness(t)
	srv := serve(t, &capture{}, http.StatusOK, fixture(t, "add_response.json"))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	scope := record.Scope{UserID: "u1", AgentID: "a1"}
	rules := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "health-data", Scope: record.Scope{UserID: "u1"}},
	})
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), scope, fixedNow(at),
		library.WithSensitivityRules(rules))

	_, err := ic.Add(context.Background(), "corr-rule", []string{"hello", "world"})
	require.NoError(t, err)

	rec, err := l.GetRecord("corr-rule")
	require.NoError(t, err)
	require.NotNil(t, rec.Content)
	assert.True(t, rec.Content.Sensitive, "a rule matching the interceptor's scope must mark an ordinary Add sensitive")
	assert.Equal(t, "hello\nworld", rec.Content.Text, "the rule changes classification, not content")

	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Empty(t, entries, "a healthy write logs no gap")
}

// TestCallOptionAlsoMarksWhenNoRuleMatches verifies the other half of the OR: a
// per-call option marks the content even when the configured rule set matches
// nothing.
func TestCallOptionAlsoMarksWhenNoRuleMatches(t *testing.T) {
	l, _, g, gapPath := newHarness(t)
	srv := serve(t, &capture{}, http.StatusOK, fixture(t, "add_response.json"))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	rules := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "someone-else", Scope: record.Scope{UserID: "a-different-user"}},
	})
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), record.Scope{UserID: "u1"}, fixedNow(at),
		library.WithSensitivityRules(rules))

	_, err := ic.Add(context.Background(), "corr-opt-no-rule", []string{"hello"}, library.Sensitive())
	require.NoError(t, err)

	rec, err := l.GetRecord("corr-opt-no-rule")
	require.NoError(t, err)
	require.NotNil(t, rec.Content)
	assert.True(t, rec.Content.Sensitive, "Sensitive() must mark content even when no rule matches")

	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

// TestRuleAndOptionBothMark verifies the OR is a true disjunction: when a rule
// matches AND the caller passes Sensitive(), the content is still marked, and
// there is no interaction that cancels one out.
func TestRuleAndOptionBothMark(t *testing.T) {
	l, _, g, _ := newHarness(t)
	srv := serve(t, &capture{}, http.StatusOK, fixture(t, "add_response.json"))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	rules := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "health-data", Scope: record.Scope{UserID: "u1"}},
	})
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), record.Scope{UserID: "u1"}, fixedNow(at),
		library.WithSensitivityRules(rules))

	_, err := ic.Add(context.Background(), "corr-both", []string{"hello"}, library.Sensitive())
	require.NoError(t, err)

	rec, err := l.GetRecord("corr-both")
	require.NoError(t, err)
	require.NotNil(t, rec.Content)
	assert.True(t, rec.Content.Sensitive, "a matching rule and the option together still mark the content")
}

// TestNoRulesMeansNothingIsMarked verifies the safe default: an interceptor
// with no rule set and a call with no option marks nothing, so the feature
// changes no existing behaviour when it is not used.
func TestNoRulesMeansNothingIsMarked(t *testing.T) {
	l, _, g, _ := newHarness(t)
	srv := serve(t, &capture{}, http.StatusOK, fixture(t, "add_response.json"))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), record.Scope{UserID: "u1"}, fixedNow(at))

	_, err := ic.Add(context.Background(), "corr-nothing", []string{"hello"})
	require.NoError(t, err)

	rec, err := l.GetRecord("corr-nothing")
	require.NoError(t, err)
	require.NotNil(t, rec.Content)
	assert.False(t, rec.Content.Sensitive, "with no rule set and no option the content stays unclassified")
}

// TestRuleMarksSurfacedContent verifies the rule set is consulted at the OTHER
// Content construction site too: on a Search, the per-result metadata Mem0
// returned is what a metadata rule matches against, and each memory_surfaced
// record carries the classification.
func TestRuleMarksSurfacedContent(t *testing.T) {
	l, _, g, gapPath := newHarness(t)

	// Give the recorded search result the Mem0 metadata a metadata rule keys on,
	// so the test proves the rule reads what Mem0 returned per result rather
	// than anything the caller supplied. The recorded fixture itself carries an
	// empty metadata object (testdata/FIXTURES.md).
	var resp mem0.SearchResponse
	require.NoError(t, json.Unmarshal(fixture(t, "search_response.json"), &resp))
	require.Len(t, resp.Results, 1)
	resp.Results[0].Metadata = map[string]any{"category": "health"}
	body, err := json.Marshal(resp)
	require.NoError(t, err)
	srv := serve(t, &capture{}, http.StatusOK, body)

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	rules := interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "health-data", MetadataKey: "category", MetadataValue: "health"},
	})
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil),
		record.Scope{UserID: "notary-fixture-user-a1b2c3"}, fixedNow(at), library.WithSensitivityRules(rules))

	_, err = ic.Search(context.Background(), "corr-rule-search", mem0.SearchRequest{Query: "q"})
	require.NoError(t, err)

	surfaced, err := l.GetRecord("corr-rule-search#1")
	require.NoError(t, err)
	require.NotNil(t, surfaced.Content)
	assert.True(t, surfaced.Content.Sensitive, "a metadata rule must read the per-result metadata Mem0 returned")

	// The search_performed record carries no Content, so no rule marks it.
	performed, err := l.GetRecord("corr-rule-search")
	require.NoError(t, err)
	assert.Nil(t, performed.Content, "the search_performed record stays content-free")

	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
