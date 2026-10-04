package interceptor_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/interceptor"
	"notary/internal/mem0"
	"notary/internal/record"
)

// ---------------------------------------------------------------------------
// test doubles
// ---------------------------------------------------------------------------

// recordingSink is a Sink that appends every record it is handed. It is
// mutex-guarded so the concurrency test can share one across goroutines.
type recordingSink struct {
	mu   sync.Mutex
	recs []record.Record
}

func (s *recordingSink) Write(rec record.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs = append(s.recs, rec)
	return nil
}

func (s *recordingSink) records() []record.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]record.Record(nil), s.recs...)
}

// obsEnvelope mirrors the JSON record.Reason.Encode emits, so a test can read
// the evidence source and payload of a record the Observer built without the
// record package exposing its unexported evidence fields.
type obsEnvelope struct {
	Kind     string `json:"kind"`
	Observed *struct {
		Source  string          `json:"source"`
		Payload json.RawMessage `json:"payload"`
	} `json:"observed"`
}

func decodeObsEnvelope(t *testing.T, rec record.Record) obsEnvelope {
	t.Helper()
	b, err := rec.Reason.Encode()
	require.NoError(t, err)
	var env obsEnvelope
	require.NoError(t, json.Unmarshal(b, &env))
	require.NotNil(t, env.Observed, "an Observed reason must encode an observed payload")
	return env
}

func obsPayloadMap(t *testing.T, env obsEnvelope) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(env.Observed.Payload, &m))
	return m
}

// ---------------------------------------------------------------------------
// the Observer's own tests
// ---------------------------------------------------------------------------

// TestObserverAddSinksOnePhrasedRecord checks that one AddObservation produces
// exactly one add_requested / add_acknowledged record with the expected
// identity, scope, and Mem0-response evidence.
func TestObserverAddSinksOnePhrasedRecord(t *testing.T) {
	sink := &recordingSink{}
	o := interceptor.NewObserver(sink)

	scope := record.Scope{UserID: "u1", AgentID: "a1"}
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	o.Add(interceptor.AddObservation{
		Scope:         scope,
		CorrelationID: "corr-add-1",
		Messages:      []string{"hello", "world"},
		Response:      mem0.AddResponse{EventID: "evt-1", Status: "PENDING"},
		At:            at,
		RecordedAt:    at,
	})

	recs := sink.records()
	require.Len(t, recs, 1, "one AddObservation sinks exactly one record")

	rec := recs[0]
	assert.Equal(t, record.RecordID("corr-add-1"), rec.ID, "the record carries the caller's correlation ID")
	assert.Equal(t, record.EventAddRequested, rec.Event)
	assert.Equal(t, record.Observed, rec.Reason.Tier())
	assert.Equal(t, record.ReasonAddAcknowledged, rec.Reason.Kind())
	assert.Equal(t, scope, rec.Subject.Scope)
	assert.Equal(t, at, rec.At.UTC())
	assert.Equal(t, at, rec.RecordedAt.UTC())
	require.NotNil(t, rec.Content)
	assert.Equal(t, "hello\nworld", rec.Content.Text)
	assert.False(t, rec.Content.Sensitive)

	env := decodeObsEnvelope(t, rec)
	assert.Equal(t, "mem0_response", env.Observed.Source, "the add evidence is the Mem0 response")
	payload := obsPayloadMap(t, env)
	assert.Equal(t, "evt-1", payload["event_id"])
	assert.Equal(t, "PENDING", payload["status"])
}

// TestObserverSearchSinksPerformedThenSurfacedInRankOrder checks that one
// SearchObservation sinks the search_performed record first and then one
// memory_surfaced record per result, in rank order, each with a 1-based "#rank"
// ID suffix.
func TestObserverSearchSinksPerformedThenSurfacedInRankOrder(t *testing.T) {
	sink := &recordingSink{}
	o := interceptor.NewObserver(sink)

	scope := record.Scope{UserID: "u1"}
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	o.Search(interceptor.SearchObservation{
		Scope:         scope,
		CorrelationID: "corr-search-1",
		Request:       mem0.SearchRequest{Query: "office printer"},
		Response: mem0.SearchResponse{Results: []mem0.SearchResult{
			{Memory: mem0.Memory{ID: "m1", Memory: "first memory"}, Score: 0.9},
			{Memory: mem0.Memory{ID: "m2", Memory: "second memory"}, Score: 0.5},
		}},
		At:         at,
		RecordedAt: at,
	})

	recs := sink.records()
	require.Len(t, recs, 3, "one search_performed plus one memory_surfaced per result")

	// The search_performed record comes first.
	performed := recs[0]
	assert.Equal(t, record.EventSearchPerformed, performed.Event)
	assert.Equal(t, record.ReasonSearchPerformed, performed.Reason.Kind())
	assert.Equal(t, record.RecordID("corr-search-1"), performed.ID)

	// Then one memory_surfaced record per result, in rank order.
	assert.Equal(t, record.EventMemorySurfaced, recs[1].Event)
	assert.Equal(t, record.RecordID("corr-search-1#1"), recs[1].ID)
	require.NotNil(t, recs[1].Content)
	assert.Equal(t, "first memory", recs[1].Content.Text)
	assert.Equal(t, "m1", recs[1].Subject.MemoryID)

	assert.Equal(t, record.EventMemorySurfaced, recs[2].Event)
	assert.Equal(t, record.RecordID("corr-search-1#2"), recs[2].ID)
	require.NotNil(t, recs[2].Content)
	assert.Equal(t, "second memory", recs[2].Content.Text)
	assert.Equal(t, "m2", recs[2].Subject.MemoryID)

	// The rank in each surfaced record's evidence confirms the order.
	rank1 := obsPayloadMap(t, decodeObsEnvelope(t, recs[1]))
	rank2 := obsPayloadMap(t, decodeObsEnvelope(t, recs[2]))
	assert.EqualValues(t, 1, rank1["rank"])
	assert.EqualValues(t, 2, rank2["rank"])
}

// TestObserverMarksContentSensitiveFromACallFlagAndFromARule checks that the
// per-call flag and a matching rule each mark the content sensitive, and that
// the two combine as an OR (either marks it).
func TestObserverMarksContentSensitiveFromACallFlagAndFromARule(t *testing.T) {
	scope := record.Scope{UserID: "u1"}
	rules := interceptor.NewRuleSet([]interceptor.Rule{{Name: "the-scope", Scope: scope}})
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		rules     *interceptor.RuleSet
		sensitive bool
		want      bool
	}{
		{"call flag only", nil, true, true},
		{"rule only", rules, false, true},
		{"both", rules, true, true},
		{"neither", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sink := &recordingSink{}
			var opts []interceptor.ObserverOption
			if tc.rules != nil {
				opts = append(opts, interceptor.WithRules(tc.rules))
			}
			o := interceptor.NewObserver(sink, opts...)

			o.Add(interceptor.AddObservation{
				Scope:         scope,
				CorrelationID: "corr-sens",
				Messages:      []string{"secret"},
				At:            at,
				RecordedAt:    at,
				Sensitive:     tc.sensitive,
			})

			recs := sink.records()
			require.Len(t, recs, 1)
			require.NotNil(t, recs[0].Content)
			assert.Equal(t, tc.want, recs[0].Content.Sensitive)
		})
	}
}

// TestObserverWithANilSinkWritesNothing checks that a nil Sink is tolerated --
// the Observer builds records but writes none, and never panics.
func TestObserverWithANilSinkWritesNothing(t *testing.T) {
	o := interceptor.NewObserver(nil)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	require.NotPanics(t, func() {
		o.Add(interceptor.AddObservation{
			Scope:         record.Scope{UserID: "u1"},
			CorrelationID: "corr-nil",
			Messages:      []string{"x"},
			At:            at,
			RecordedAt:    at,
		})
		o.Search(interceptor.SearchObservation{
			Scope:         record.Scope{UserID: "u1"},
			CorrelationID: "corr-nil",
			Request:       mem0.SearchRequest{Query: "q"},
			Response: mem0.SearchResponse{Results: []mem0.SearchResult{
				{Memory: mem0.Memory{ID: "m1", Memory: "y"}},
			}},
			At:         at,
			RecordedAt: at,
		})
	})
}

// TestObserverIsSafeForConcurrentUse drives one Observer from many goroutines.
// It asserts no record is lost (the sink is mutex-guarded) and, under -race,
// that the Observer reads no shared mutable state. Run with -race to make the
// race check meaningful.
func TestObserverIsSafeForConcurrentUse(t *testing.T) {
	sink := &recordingSink{}
	o := interceptor.NewObserver(sink, interceptor.WithRules(interceptor.NewRuleSet([]interceptor.Rule{
		{Name: "all", Scope: record.Scope{UserID: "u1"}},
	})))
	scope := record.Scope{UserID: "u1"}
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			o.Add(interceptor.AddObservation{
				Scope: scope, CorrelationID: "corr", Messages: []string{"m"}, At: at, RecordedAt: at,
			})
			o.Search(interceptor.SearchObservation{
				Scope: scope, CorrelationID: "corr",
				Response:   mem0.SearchResponse{Results: []mem0.SearchResult{{Memory: mem0.Memory{ID: "m", Memory: "m"}}}},
				At:         at,
				RecordedAt: at,
			})
		}()
	}
	wg.Wait()

	// Each goroutine sinks 1 add + 2 search records.
	assert.Len(t, sink.records(), n*3)
}
