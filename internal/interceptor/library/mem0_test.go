package library_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
	"notary/internal/interceptor"
	"notary/internal/interceptor/library"
	"notary/internal/ledger"
	"notary/internal/mem0"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// ---------------------------------------------------------------------------
// compile-time contract
// ---------------------------------------------------------------------------

// The library interceptor must satisfy interceptor.Interceptor.
var _ interceptor.Interceptor = (*library.Mem0Interceptor)(nil)

// ---------------------------------------------------------------------------
// test doubles and harnesses
// ---------------------------------------------------------------------------

// capture counts the requests an httptest server received. It is mutex-guarded
// because the handler runs on its own goroutine.
type capture struct {
	mu    sync.Mutex
	n     int
	paths []string
}

func (c *capture) hit(r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	c.paths = append(c.paths, r.URL.Path)
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// fixture reads a recorded Mem0 response from the mem0 package's testdata, so
// every assertion is against the bytes Mem0 actually returned. The mem0
// fixtures are the authority on wire shape (see testdata/FIXTURES.md).
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "mem0", "testdata", name))
	require.NoError(t, err)
	return b
}

// serve starts an httptest server that records each request and replies with
// status and body.
func serve(t *testing.T, cap *capture, status int, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.hit(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// testSeed is a fixed 32-byte ed25519 seed, so every run signs with the same
// key and the tests stay deterministic.
var testSeed = []byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

func newSigner(t *testing.T) *sign.Signer {
	t.Helper()
	t.Setenv("NOTARY_LIBRARY_TEST_KEY", base64.StdEncoding.EncodeToString(testSeed))
	s, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_LIBRARY_TEST_KEY"})
	require.NoError(t, err)
	require.NotNil(t, s)
	return s
}

// newHarness wires a fresh store, signer, ledger, and gap log over a temp dir.
func newHarness(t *testing.T) (*ledger.Ledger, *store.SQLiteStore, *gap.Log, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	l := ledger.New(st, newSigner(t), func() time.Time {
		return time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	})
	gapPath := filepath.Join(dir, "gaps.log")
	g, err := gap.Open(gapPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return l, st, g, gapPath
}

// fixedNow returns a clock that always reports at.
func fixedNow(at time.Time) func() time.Time { return func() time.Time { return at } }

// listAll returns every record the ledger holds, over a window wide enough to
// cover the test clock.
func listAll(t *testing.T, l *ledger.Ledger, at time.Time) []record.Record {
	t.Helper()
	recs, err := l.ListRecords(at.Add(-48*time.Hour), at.Add(48*time.Hour))
	require.NoError(t, err)
	return recs
}

// reasonEnvelope mirrors the JSON that record.Reason.Encode emits, so a test
// can read the evidence source and payload of a stored record without the
// record package exposing its unexported evidence fields.
type reasonEnvelope struct {
	V        string `json:"v"`
	Kind     string `json:"kind"`
	Tier     string `json:"tier"`
	Observed *struct {
		Source  string          `json:"source"`
		Payload json.RawMessage `json:"payload"`
	} `json:"observed"`
}

func decodeReason(t *testing.T, rec record.Record) reasonEnvelope {
	t.Helper()
	b, err := rec.Reason.Encode()
	require.NoError(t, err)
	var env reasonEnvelope
	require.NoError(t, json.Unmarshal(b, &env))
	require.NotNil(t, env.Observed, "an Observed reason must encode an observed payload")
	return env
}

func payloadMap(t *testing.T, env reasonEnvelope) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(env.Observed.Payload, &m))
	return m
}

func ids(recs []record.Record) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, string(r.ID))
	}
	sort.Strings(out)
	return out
}

// twoResultSearchBody takes the single recorded search result and duplicates
// it with a distinct id, producing a two-result response of exactly the
// recorded shape (no invented fields). It exists so a test can exercise
// rank 1 and rank 2 without a second recording.
func twoResultSearchBody(t *testing.T) []byte {
	t.Helper()
	var resp mem0.SearchResponse
	require.NoError(t, json.Unmarshal(fixture(t, "search_response.json"), &resp))
	require.Len(t, resp.Results, 1)
	second := resp.Results[0]
	second.ID = "second-memory-id"
	second.Memory.Memory = "A second recorded memory."
	second.Score = 0.5
	resp.Results = append(resp.Results, second)
	out, err := json.Marshal(resp)
	require.NoError(t, err)
	return out
}

// ---------------------------------------------------------------------------
// DeriveCorrelationID (Ruling B/C)
// ---------------------------------------------------------------------------

// refDeriveCorrelationID is an independent re-derivation of the documented
// scheme: base64(sha256("notary/corr/v1" ‖ length-prefixed scope fields ‖
// length-prefixed seed)). It pins the domain tag, the field order, the
// length-prefixing, and the base64 encoding.
func refDeriveCorrelationID(scope record.Scope, seed string) string {
	h := sha256.New()
	h.Write([]byte("notary/corr/v1"))
	for _, f := range []string{scope.UserID, scope.AgentID, scope.AppID, scope.RunID, seed} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(f)))
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestDeriveCorrelationIDIsDeterministicAndPinned(t *testing.T) {
	scope := record.Scope{UserID: "u", AgentID: "a", AppID: "app", RunID: "run"}
	const seed = "seed-1"

	got := library.DeriveCorrelationID(scope, seed)
	assert.Equal(t, refDeriveCorrelationID(scope, seed), got, "must match the documented scheme")
	assert.Equal(t, got, library.DeriveCorrelationID(scope, seed), "must be deterministic")
	assert.NotEmpty(t, got)
	assert.NotContains(t, got, "#", "base64 output must never contain the '#' record-ID separator")
}

// TestDeriveCorrelationIDCollisionResistance constructs input pairs that WOULD
// collide under naive concatenation and asserts the length-prefixed scheme
// keeps them distinct.
func TestDeriveCorrelationIDCollisionResistance(t *testing.T) {
	t.Run("scope-field boundary", func(t *testing.T) {
		a := library.DeriveCorrelationID(record.Scope{UserID: "ab", AgentID: "c"}, "seed")
		b := library.DeriveCorrelationID(record.Scope{UserID: "a", AgentID: "bc"}, "seed")
		assert.NotEqual(t, a, b)
	})

	t.Run("scope/seed boundary", func(t *testing.T) {
		a := library.DeriveCorrelationID(record.Scope{UserID: "ab"}, "c")
		b := library.DeriveCorrelationID(record.Scope{UserID: "a"}, "bc")
		assert.NotEqual(t, a, b)
	})
}

// ---------------------------------------------------------------------------
// Add
// ---------------------------------------------------------------------------

func TestAddWritesOneObservedRecord(t *testing.T) {
	l, _, g, gapPath := newHarness(t)
	cap := &capture{}
	srv := serve(t, cap, http.StatusOK, fixture(t, "add_response.json"))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	scope := record.Scope{UserID: "u1", AgentID: "a1"}
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), scope, fixedNow(at))

	resp, err := ic.Add(context.Background(), "corr-add-1", []string{"hello", "world"})
	require.NoError(t, err)
	assert.Equal(t, "6e2b49d1-7c73-4947-bc9b-675224e43ddb", resp.EventID)
	assert.Equal(t, "PENDING", resp.Status)
	assert.Equal(t, 1, cap.count(), "Add makes exactly one Mem0 call")

	recs := listAll(t, l, at)
	require.Len(t, recs, 1, "Add writes exactly one record")

	rec := recs[0]
	assert.Equal(t, record.RecordID("corr-add-1"), rec.ID, "the primary record carries the caller's correlation ID")
	assert.Equal(t, record.EventAddRequested, rec.Event)
	assert.Equal(t, record.Observed, rec.Reason.Tier())
	assert.Equal(t, record.ReasonAddAcknowledged, rec.Reason.Kind())
	assert.Equal(t, scope, rec.Subject.Scope)
	assert.Equal(t, uint64(0), rec.Seq, "the ledger, not the interceptor, assigns Seq")
	assert.Equal(t, at, rec.At.UTC())
	assert.Equal(t, at, rec.RecordedAt.UTC())
	assert.NotEmpty(t, rec.IdempotencyKey, "Phase 4 writes records with a derived idempotency key")

	require.NotNil(t, rec.Content)
	assert.Equal(t, "hello\nworld", rec.Content.Text)
	assert.False(t, rec.Content.Sensitive)

	env := decodeReason(t, rec)
	assert.Equal(t, "mem0_response", env.Observed.Source, "the add evidence is the Mem0 response")
	payload := payloadMap(t, env)
	assert.Equal(t, "6e2b49d1-7c73-4947-bc9b-675224e43ddb", payload["event_id"])
	assert.Equal(t, "PENDING", payload["status"])

	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Empty(t, entries, "a healthy write must not log a gap")
}

func TestAddMem0ErrorWritesNoRecord(t *testing.T) {
	l, _, _, gapPath := newHarness(t)
	cap := &capture{}
	srv := serve(t, cap, http.StatusBadRequest, fixture(t, "add_error_400.json"))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, mustGap(t), nil), record.Scope{UserID: "u1"}, fixedNow(at))

	_, err := ic.Add(context.Background(), "corr-400", []string{"x"})
	require.Error(t, err, "a Mem0 failure is a real error")
	assert.Equal(t, 1, cap.count(), "the call was made")

	assert.Empty(t, listAll(t, l, at), "a failed Mem0 call writes no record")
	entries, rerr := gap.Read(gapPath)
	require.NoError(t, rerr)
	assert.Empty(t, entries, "a failed Mem0 call is not an audit gap")
}

// mustGap opens a throwaway gap log on a temp path.
func mustGap(t *testing.T) *gap.Log {
	t.Helper()
	g, err := gap.Open(filepath.Join(t.TempDir(), "gaps.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	return g
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

func TestSearchWritesPerformedAndSurfacedRecords(t *testing.T) {
	l, _, g, gapPath := newHarness(t)
	cap := &capture{}
	srv := serve(t, cap, http.StatusOK, fixture(t, "search_response.json"))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	scope := record.Scope{UserID: "notary-fixture-user-a1b2c3"}
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), scope, fixedNow(at))

	q := mem0.SearchRequest{
		Query:     "office printer",
		Filters:   mem0.Filters{UserID: "notary-fixture-user-a1b2c3"},
		TopK:      5,
		Threshold: 0.1,
		Rerank:    true,
	}
	resp, err := ic.Search(context.Background(), "corr-search-1", q)
	require.NoError(t, err)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, 1, cap.count())

	recs := listAll(t, l, at)
	require.Len(t, recs, 2, "one search_performed record plus one memory_surfaced record")

	performed, err := l.GetRecord("corr-search-1")
	require.NoError(t, err)
	assert.Equal(t, record.EventSearchPerformed, performed.Event)
	assert.Equal(t, record.Observed, performed.Reason.Tier())
	assert.Equal(t, record.ReasonSearchPerformed, performed.Reason.Kind())
	assert.Equal(t, scope, performed.Subject.Scope)
	env := decodeReason(t, performed)
	payload := payloadMap(t, env)
	assert.Equal(t, "office printer", payload["query"])
	assert.EqualValues(t, 5, payload["top_k"])
	assert.EqualValues(t, 0.1, payload["threshold"])
	assert.Equal(t, true, payload["rerank"])
	assert.EqualValues(t, 1, payload["count"], "the payload carries the returned result count")

	surfaced, err := l.GetRecord("corr-search-1#1")
	require.NoError(t, err)
	assert.Equal(t, record.EventMemorySurfaced, surfaced.Event)
	assert.Equal(t, record.Observed, surfaced.Reason.Tier())
	assert.Equal(t, record.ReasonReturnedBySearch, surfaced.Reason.Kind())
	assert.Equal(t, "ca4535c6-4847-4a43-8d65-f4871b7f5d99", surfaced.Subject.MemoryID)
	require.NotNil(t, surfaced.Content)
	assert.Equal(t, "User's shared office printer is located on floor 3", surfaced.Content.Text)
	senv := decodeReason(t, surfaced)
	assert.Equal(t, "mem0_response", senv.Observed.Source)
	sp := payloadMap(t, senv)
	assert.EqualValues(t, 0.8728, sp["score"])
	assert.EqualValues(t, 1, sp["rank"])

	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestSearchZeroResultsWritesOnlyPerformed(t *testing.T) {
	l, _, g, _ := newHarness(t)
	srv := serve(t, &capture{}, http.StatusOK, []byte(`{"results":[]}`))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), record.Scope{UserID: "u1"}, fixedNow(at))

	resp, err := ic.Search(context.Background(), "corr-empty", mem0.SearchRequest{Query: "q"})
	require.NoError(t, err)
	assert.Empty(t, resp.Results)

	recs := listAll(t, l, at)
	require.Len(t, recs, 1, "the zero-results case writes only the search_performed record")
	assert.Equal(t, record.EventSearchPerformed, recs[0].Event)
	assert.Equal(t, record.RecordID("corr-empty"), recs[0].ID)
}

// ---------------------------------------------------------------------------
// correlation-ID validation (spec §7)
// ---------------------------------------------------------------------------

func TestMissingCorrelationIDMakesZeroHTTPRequests(t *testing.T) {
	l, _, g, _ := newHarness(t)
	cap := &capture{}
	srv := serve(t, cap, http.StatusOK, fixture(t, "add_response.json"))
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), record.Scope{UserID: "u1"}, fixedNow(time.Now()))

	_, err := ic.Add(context.Background(), "", []string{"x"})
	require.ErrorIs(t, err, library.ErrMissingCorrelationID)

	_, err = ic.Search(context.Background(), "", mem0.SearchRequest{Query: "q"})
	require.ErrorIs(t, err, library.ErrMissingCorrelationID)

	assert.Equal(t, 0, cap.count(), "validation must happen before any HTTP call")
}

// ---------------------------------------------------------------------------
// fail-open (Ruling A)
// ---------------------------------------------------------------------------

func TestFailOpenWhenLedgerBroken(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	scope := record.Scope{UserID: "notary-fixture-user-a1b2c3"}

	t.Run("Search returns results and logs a gap", func(t *testing.T) {
		l, st, g, gapPath := newHarness(t)
		srv := serve(t, &capture{}, http.StatusOK, fixture(t, "search_response.json"))
		ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), scope, fixedNow(at))
		require.NoError(t, st.Close(), "break the ledger")

		resp, err := ic.Search(context.Background(), "corr-fo", mem0.SearchRequest{Query: "q"})
		require.NoError(t, err, "an audit failure must never become the caller's error")
		require.Len(t, resp.Results, 1, "the Mem0 results are still returned")

		entries, err := gap.Read(gapPath)
		require.NoError(t, err)
		require.Len(t, entries, 2, "each record the ledger refused becomes one gap entry")
		got := map[string]record.EventType{}
		for _, e := range entries {
			got[e.CorrelationID] = e.Kind
			assert.Equal(t, scope, e.Scope)
		}
		assert.Equal(t, record.EventSearchPerformed, got["corr-fo"])
		assert.Equal(t, record.EventMemorySurfaced, got["corr-fo#1"])

		breaks, err := gap.Verify(gapPath)
		require.NoError(t, err)
		assert.Empty(t, breaks, "the gap log's own chain must stay valid")
	})

	t.Run("Add returns the response and logs a gap", func(t *testing.T) {
		l, st, g, gapPath := newHarness(t)
		srv := serve(t, &capture{}, http.StatusOK, fixture(t, "add_response.json"))
		ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), scope, fixedNow(at))
		require.NoError(t, st.Close())

		resp, err := ic.Add(context.Background(), "corr-fo-add", []string{"hello"})
		require.NoError(t, err)
		assert.Equal(t, "6e2b49d1-7c73-4947-bc9b-675224e43ddb", resp.EventID)

		entries, err := gap.Read(gapPath)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		assert.Equal(t, "corr-fo-add", entries[0].CorrelationID)
		assert.Equal(t, record.EventAddRequested, entries[0].Kind)
	})
}

// TestFailOpenWhenBothLedgerAndGapBroken is Ruling A's core: Mem0's result is
// returned with a nil error even when the record is refused by both the ledger
// and the gap log.
func TestFailOpenWhenBothLedgerAndGapBroken(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	scope := record.Scope{UserID: "notary-fixture-user-a1b2c3"}

	l, st, g, _ := newHarness(t)
	srv := serve(t, &capture{}, http.StatusOK, fixture(t, "search_response.json"))
	var loud bytes.Buffer
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, []io.Writer{&loud}), scope, fixedNow(at))

	require.NoError(t, st.Close(), "break the ledger")
	require.NoError(t, g.Close(), "break the gap log")

	resp, err := ic.Search(context.Background(), "corr-both", mem0.SearchRequest{Query: "q"})
	require.NoError(t, err, "a doubly-failed audit must still not fail the caller")
	require.Len(t, resp.Results, 1, "the Mem0 results are still returned")
	assert.NotEmpty(t, loud.String(), "the loud channel is the operator's signal")
}

// ---------------------------------------------------------------------------
// record-ID determinism (Ruling B)
// ---------------------------------------------------------------------------

func TestRecordIDsAreDeterministic(t *testing.T) {
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	scope := record.Scope{UserID: "notary-fixture-user-a1b2c3"}
	body := twoResultSearchBody(t)

	var runs [][]string
	for i := 0; i < 2; i++ {
		l, _, g, _ := newHarness(t)
		srv := serve(t, &capture{}, http.StatusOK, body)
		ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), scope, fixedNow(at))
		_, err := ic.Search(context.Background(), "corr-det", mem0.SearchRequest{Query: "q"})
		require.NoError(t, err)
		runs = append(runs, ids(listAll(t, l, at)))
	}

	assert.Equal(t, runs[0], runs[1], "the same inputs must produce the same record IDs")
	assert.Equal(t, []string{"corr-det", "corr-det#1", "corr-det#2"}, runs[0],
		"the primary record carries the correlation ID; each derived record adds a 1-based rank suffix")
	for _, id := range runs[0] {
		assert.NotEmpty(t, id, "every written record's ID must be non-empty")
	}
}

// ---------------------------------------------------------------------------
// content-hash collision resistance (Ruling C)
// ---------------------------------------------------------------------------

func TestAddContentHashCollisionResistance(t *testing.T) {
	l, _, g, _ := newHarness(t)
	srv := serve(t, &capture{}, http.StatusOK, fixture(t, "add_response.json"))

	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), record.Scope{UserID: "u1"}, fixedNow(at))
	ctx := context.Background()

	_, err := ic.Add(ctx, "corr-coll-a", []string{"ab", "c"})
	require.NoError(t, err)
	_, err = ic.Add(ctx, "corr-coll-b", []string{"a", "bc"})
	require.NoError(t, err)
	// A second identical input pins determinism of the content hash.
	_, err = ic.Add(ctx, "corr-coll-c", []string{"a", "bc"})
	require.NoError(t, err)

	a, err := l.GetRecord("corr-coll-a")
	require.NoError(t, err)
	b, err := l.GetRecord("corr-coll-b")
	require.NoError(t, err)
	c, err := l.GetRecord("corr-coll-c")
	require.NoError(t, err)

	assert.NotEqual(t, a.Subject.ContentHash, b.Subject.ContentHash,
		"length-prefixing must stop ['ab','c'] and ['a','bc'] from colliding")
	assert.Equal(t, b.Subject.ContentHash, c.Subject.ContentHash,
		"identical messages must hash identically")
}

// ---------------------------------------------------------------------------
// interface behaviour and nil safety
// ---------------------------------------------------------------------------

func TestFailModeAndClose(t *testing.T) {
	t.Run("FailMode is FailOpenLoud", func(t *testing.T) {
		ic := library.New(nil, nil, record.Scope{}, nil)
		assert.Equal(t, interceptor.FailOpenLoud, ic.FailMode())
	})

	t.Run("Close is nil-safe with no audit writer", func(t *testing.T) {
		ic := library.New(nil, nil, record.Scope{}, nil)
		require.NoError(t, ic.Close())
	})

	t.Run("Close closes the borrowed audit writer", func(t *testing.T) {
		l, _, g, _ := newHarness(t)
		ic := library.New(nil, interceptor.NewAuditWriter(l, g, nil), record.Scope{}, nil)
		require.NoError(t, ic.Close())
		require.Error(t, g.Record(gap.Entry{Kind: record.EventAuditGap, CorrelationID: "x", Detail: "y"}),
			"Close must have closed the underlying gap log")
	})
}

func TestNilNowDoesNotPanic(t *testing.T) {
	l, _, g, _ := newHarness(t)
	srv := serve(t, &capture{}, http.StatusOK, fixture(t, "add_response.json"))
	ic := library.New(mem0.NewClient(srv.URL, "k", nil), interceptor.NewAuditWriter(l, g, nil), record.Scope{UserID: "u1"}, nil)

	var err error
	require.NotPanics(t, func() {
		_, err = ic.Add(context.Background(), "corr-nilnow", []string{"x"})
	})
	require.NoError(t, err)

	rec, gerr := l.GetRecord("corr-nilnow")
	require.NoError(t, gerr)
	assert.False(t, rec.At.IsZero(), "a nil clock must fall back to a real one, not a zero time")
	assert.False(t, rec.RecordedAt.IsZero())
}
