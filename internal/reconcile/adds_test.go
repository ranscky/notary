package reconcile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/mem0"
	"notary/internal/record"
)

// pathRecorder captures the URL path of the last request the test server saw.
// It is mutex-guarded because the handler runs on its own goroutine.
type pathRecorder struct {
	mu   sync.Mutex
	path string
}

func (p *pathRecorder) record(r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.path = r.URL.Path
}

func (p *pathRecorder) last() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.path
}

// eventStatusClient starts an httptest server that answers every EventStatus
// request from handler, keyed on the event id parsed out of the path. It never
// touches the network and returns a client plus a recorder so a test can prove
// which event id was polled.
func eventStatusClient(t *testing.T, handler func(eventID string) mem0.EventStatusResponse) (*mem0.Client, *pathRecorder) {
	t.Helper()
	rec := &pathRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		eventID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/event/"), "/")
		body, err := json.Marshal(handler(eventID))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return mem0.NewClient(srv.URL, "test-key", nil), rec
}

// staticEventStatus is a handler that answers every event id with resp.
func staticEventStatus(resp mem0.EventStatusResponse) func(string) mem0.EventStatusResponse {
	return func(string) mem0.EventStatusResponse { return resp }
}

// errorStatusClient starts an httptest server that answers every EventStatus
// request with a non-2xx status and a small JSON error body, so a test can prove
// a Mem0 outage is reported rather than mistaken for a pending add.
func errorStatusClient(t *testing.T, status int) *mem0.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"mem0 outage"}`))
	}))
	t.Cleanup(srv.Close)
	return mem0.NewClient(srv.URL, "test-key", nil)
}

// resolveOne calls resolveAdd and requires it to yield exactly one record that
// would be accepted by ledger.Append (a valid claim).
func resolveOne(t *testing.T, rc *Reconciler, add record.Record) record.Record {
	t.Helper()
	got, err := rc.resolveAdd(context.Background(), add)
	require.NoError(t, err)
	require.Len(t, got, 1, "expected exactly one produced record")
	require.NoError(t, got[0].Validate(), "the produced record must be a valid claim the caller can append")
	return got[0]
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------------
// Spec §5 rows 1-3: map a Mem0 event's terminal status to an add_resolved.
// ---------------------------------------------------------------------------

func TestResolveAddWritesNothingWhilePending(t *testing.T) {
	add := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	client, _ := eventStatusClient(t, staticEventStatus(mem0.EventStatusResponse{ID: "evt-1", Status: "PENDING"}))
	rc := New(&fakeReader{}, client)

	got, err := rc.resolveAdd(context.Background(), add)
	require.NoError(t, err, "a PENDING status is not an error: the add is simply not resolved yet")
	assert.Empty(t, got, "a non-terminal add must write nothing, so a stuck add stays visible to an operator without polluting the chain")
}

func TestResolveAddRecordsObservedSuccess(t *testing.T) {
	add := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	env := mem0.EventStatusResponse{
		ID:      "evt-1",
		Status:  "SUCCEEDED",
		Results: []mem0.EventResult{{ID: "mem-1", Event: "ADD"}},
	}
	client, paths := eventStatusClient(t, staticEventStatus(env))
	rc := New(&fakeReader{}, client)

	rec := resolveOne(t, rc, add)
	assert.Equal(t, "/v1/event/evt-1/", paths.last(), "the polled event id must come from the add record's AddPayload")

	assert.Equal(t, record.EventAddResolved, rec.Event)
	assert.Equal(t, record.ReasonStoredByMem0, rec.Reason.Kind())
	assert.Equal(t, record.Observed, rec.Reason.Tier(), "stored_by_mem0 is an observation, not an inference")
	assert.NotEmpty(t, rec.ID, "a produced record must have an ID or ledger.Append rejects it")
	assert.NotEqual(t, record.Hash{}, rec.Subject.ContentHash, "a produced record must carry a non-zero subject content hash or ledger.Append rejects it")

	// The Observed evidence payload is the event-status response itself.
	ev, ok := rec.Reason.Observed()
	require.True(t, ok)
	want, err := json.Marshal(env)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(ev.Payload()))

	// The idempotency key is deterministic across two identical calls: a
	// re-run must re-derive the identical key so ledger.Append treats it as a
	// duplicate rather than writing a second add_resolved.
	assert.NotEmpty(t, rec.IdempotencyKey)
	assert.Equal(t, rec.IdempotencyKey, resolveOne(t, rc, add).IdempotencyKey)
}

func TestResolveAddRecordsObservedFailure(t *testing.T) {
	add := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	env := mem0.EventStatusResponse{ID: "evt-1", Status: "FAILED", Error: strPtr("synthesis failed")}
	client, _ := eventStatusClient(t, staticEventStatus(env))
	rc := New(&fakeReader{}, client)

	rec := resolveOne(t, rc, add)
	assert.Equal(t, record.EventAddResolved, rec.Event)
	assert.Equal(t, record.ReasonAddFailed, rec.Reason.Kind())
	assert.Equal(t, record.Observed, rec.Reason.Tier(), "a failed add is an observation reported by Mem0, not an inference")

	ev, ok := rec.Reason.Observed()
	require.True(t, ok)
	want, err := json.Marshal(env)
	require.NoError(t, err)
	assert.JSONEq(t, string(want), string(ev.Payload()))

	assert.NotEmpty(t, rec.IdempotencyKey)
	assert.Equal(t, rec.IdempotencyKey, resolveOne(t, rc, add).IdempotencyKey)
}

func TestResolveAddInfersNoFactsExtractedFromEmptyResults(t *testing.T) {
	add := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	// SUCCEEDED with an EMPTY results array.
	env := mem0.EventStatusResponse{ID: "evt-1", Status: "SUCCEEDED"}
	client, _ := eventStatusClient(t, staticEventStatus(env))
	rc := New(&fakeReader{}, client)

	rec := resolveOne(t, rc, add)
	assert.Equal(t, record.EventAddResolved, rec.Event)
	assert.Equal(t, record.ReasonNoFactsExtracted, rec.Reason.Kind())
	assert.Equal(t, record.Reconstructed, rec.Reason.Tier(),
		"the observation is 'the results array was empty'; 'Mem0 extracted no facts' is an interpretation of it")

	// The record package exposes no accessor for basis/rule/ruleVersion/
	// confidence, so the reconstructed evidence is asserted on its encoded
	// bytes.
	encoded, err := rec.Reason.Encode()
	require.NoError(t, err)
	var wire struct {
		Reconstructed struct {
			Basis       []string `json:"basis"`
			Rule        string   `json:"rule"`
			RuleVersion string   `json:"rule_version"`
			Confidence  *float64 `json:"confidence"`
		} `json:"reconstructed"`
	}
	require.NoError(t, json.Unmarshal(encoded, &wire))

	rule, ok := LookupRule(RuleNoFactsExtracted)
	require.True(t, ok, "the rule must be in the registry")
	assert.Equal(t, []string{"r-add-1"}, wire.Reconstructed.Basis, "basis is the add_requested record's id")
	assert.Equal(t, rule.Name, wire.Reconstructed.Rule)
	assert.Equal(t, rule.Version, wire.Reconstructed.RuleVersion)
	assert.Nil(t, wire.Reconstructed.Confidence, "an unset confidence must be omitted from the encoded reason, never rendered as 0")

	assert.NotEmpty(t, rec.IdempotencyKey)
	assert.Equal(t, rec.IdempotencyKey, resolveOne(t, rc, add).IdempotencyKey)
}

// TestResolveAddIdempotencyKeyIsEventAndReasonSpecific pins the two ways the
// key must vary: by event id (a different add is a different record) and by
// reason kind (a FAILED-then-SUCCEEDED transition still writes a distinct
// record).
func TestResolveAddIdempotencyKeyIsEventAndReasonSpecific(t *testing.T) {
	add := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)

	successClient, _ := eventStatusClient(t, staticEventStatus(mem0.EventStatusResponse{
		ID: "evt-1", Status: "SUCCEEDED", Results: []mem0.EventResult{{ID: "mem-1"}},
	}))
	failedClient, _ := eventStatusClient(t, staticEventStatus(mem0.EventStatusResponse{ID: "evt-1", Status: "FAILED"}))

	success := resolveOne(t, New(&fakeReader{}, successClient), add)
	failed := resolveOne(t, New(&fakeReader{}, failedClient), add)
	assert.NotEqual(t, success.IdempotencyKey, failed.IdempotencyKey,
		"the reason kind is part of the key, so a FAILED-then-SUCCEEDED transition appends a distinct record")

	other := mustAddRequested(t, "r-add-2", "evt-2", fixedTime)
	otherClient, _ := eventStatusClient(t, staticEventStatus(mem0.EventStatusResponse{
		ID: "evt-2", Status: "SUCCEEDED", Results: []mem0.EventResult{{ID: "mem-2"}},
	}))
	otherRec := resolveOne(t, New(&fakeReader{}, otherClient), other)
	assert.NotEqual(t, success.IdempotencyKey, otherRec.IdempotencyKey,
		"two different event ids must derive different keys")
}

// TestResolveAddOneEventTwoKindsCoexist pins that BOTH the record id and the
// idempotency key vary with the reason kind, so two resolutions of the SAME
// event with different kinds are two records the store can hold side by side.
// records.id is UNIQUE and duplicate suppression matches only the
// idempotency_key column, so an id that ignored the kind would make the second
// append die on a raw SQLite UNIQUE violation while the key was telling the
// ledger it was a distinct claim.
func TestResolveAddOneEventTwoKindsCoexist(t *testing.T) {
	add := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)

	successClient, _ := eventStatusClient(t, staticEventStatus(mem0.EventStatusResponse{
		ID: "evt-1", Status: "SUCCEEDED", Results: []mem0.EventResult{{ID: "mem-1"}},
	}))
	failedClient, _ := eventStatusClient(t, staticEventStatus(mem0.EventStatusResponse{ID: "evt-1", Status: "FAILED"}))

	success := resolveOne(t, New(&fakeReader{}, successClient), add)
	failed := resolveOne(t, New(&fakeReader{}, failedClient), add)

	require.Equal(t, record.ReasonStoredByMem0, success.Reason.Kind())
	require.Equal(t, record.ReasonAddFailed, failed.Reason.Kind())
	assert.NotEqual(t, success.ID, failed.ID,
		"records.id is UNIQUE, so the two kinds must have distinct ids or the second append fails on id")
	assert.NotEqual(t, success.IdempotencyKey, failed.IdempotencyKey,
		"duplicate suppression matches only idempotency_key, so the keys must differ for both to coexist")
}

// TestResolveAddFailsLoudlyWithoutAClient pins that a nil Mem0 client is a
// misconfiguration, not a "nothing to resolve" state: it must fail the pass
// loudly (spec §9.2), never look like a clean pass that found nothing.
func TestResolveAddFailsLoudlyWithoutAClient(t *testing.T) {
	add := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	rc := New(&fakeReader{}, nil)

	got, err := rc.resolveAdd(context.Background(), add)
	require.Error(t, err, "a nil client is a misconfigured reconciler, not a clean pass")
	assert.ErrorIs(t, err, ErrNoMem0Client)
	assert.Empty(t, got, "no record may be emitted when the reconciler cannot poll Mem0")
}

// TestResolveAddPropagatesMem0Error pins that a Mem0 outage fails loudly and
// emits nothing, so it can never be misread as a not-yet-resolved add.
func TestResolveAddPropagatesMem0Error(t *testing.T) {
	add := mustAddRequested(t, "r-add-1", "evt-1", fixedTime)
	rc := New(&fakeReader{}, errorStatusClient(t, http.StatusInternalServerError))

	got, err := rc.resolveAdd(context.Background(), add)
	require.Error(t, err, "a Mem0 outage must fail loudly, never be misread as pending")
	assert.Contains(t, err.Error(), "evt-1", "the wrapped error must name the event it failed to poll")

	var httpErr *mem0.HTTPError
	require.ErrorAs(t, err, &httpErr, "the underlying Mem0 error must survive wrapping")
	assert.Equal(t, http.StatusInternalServerError, httpErr.StatusCode)

	assert.Empty(t, got, "no record may be emitted on a Mem0 error")
}
