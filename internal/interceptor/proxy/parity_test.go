package proxy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"notary/internal/interceptor"
	"notary/internal/interceptor/library"
	"notary/internal/interceptor/proxy"
	"notary/internal/mem0"
	"notary/internal/record"
)

// TestProxyAndLibraryProduceTheSameRecords is the phase's falsifier (design
// section 9): a deployment can move from library mode to proxy mode and the
// ledger's meaning does not change. One add and one search are driven once
// through library.Mem0Interceptor (with mem0.NewClient pointed at the test
// server) and once through the proxy (with the same server as upstream), each
// writing to its own real ledger over a temp SQLite file. The two record sets
// are then compared on every field the claim asserts.
//
// At, RecordedAt, Hash and Signature are EXCLUDED, and deliberately so: the
// canonical hash covers the instants (record/chain.go), so byte-identity is
// impossible by construction -- the two modes observe at different instants on
// different clocks -- and asserting it would be untestable. Comparing the
// remaining fields is the strongest property that is actually true.
func TestProxyAndLibraryProduceTheSameRecords(t *testing.T) {
	const (
		corrAdd    = "parity-add"
		corrSearch = "parity-search"
		message    = "hello world"
		query      = "why"
	)

	// The one Mem0 upstream both modes talk to. It answers add and search with
	// the recorded shapes.
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/memories/add/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"event_id":"evt-parity","status":"PENDING"}`)
	})
	mux.HandleFunc("/v3/memories/search/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"results":[
			{"id":"m1","memory":"alpha","score":0.9},
			{"id":"m2","memory":"beta","score":0.5}]}`)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	scope := record.Scope{UserID: "u1"}
	clock := func() time.Time { return time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC) }

	// --- library mode ---------------------------------------------------
	libSink, libLedger, _, _ := newProxyHarness(t)
	awLib, ok := libSink.(*interceptor.AuditWriter)
	require.True(t, ok, "the harness sink is an AuditWriter library.New accepts")
	lib := library.New(mem0.NewClient(server.URL, "test-key", server.Client()), awLib, scope, clock)

	_, err := lib.Add(context.Background(), corrAdd, []string{message})
	require.NoError(t, err)
	_, err = lib.Search(context.Background(), corrSearch, mem0.SearchRequest{
		Query:   query,
		Filters: mem0.Filters{UserID: "u1"},
	})
	require.NoError(t, err)

	// --- proxy mode -----------------------------------------------------
	proxySink, proxyLedger, _, _ := newProxyHarness(t)
	obs := interceptor.NewObserver(proxySink)
	up, err := url.Parse(server.URL)
	require.NoError(t, err)
	p := proxy.New(up, obs, proxySink, io.Discard, 8<<20)
	t.Cleanup(func() { _ = p.Close() })

	addBody, err := json.Marshal(mem0.AddRequest{
		Messages: []mem0.Message{{Role: "user", Content: message}},
		UserID:   "u1",
	})
	require.NoError(t, err)
	reqAdd := httptest.NewRequest(http.MethodPost, "/v3/memories/add/", bytes.NewReader(addBody))
	reqAdd.Header.Set(proxy.CorrelationHeader, corrAdd)
	p.ServeHTTP(httptest.NewRecorder(), reqAdd)

	searchBody, err := json.Marshal(mem0.SearchRequest{Query: query, Filters: mem0.Filters{UserID: "u1"}})
	require.NoError(t, err)
	reqSearch := httptest.NewRequest(http.MethodPost, "/v3/memories/search/", bytes.NewReader(searchBody))
	reqSearch.Header.Set(proxy.CorrelationHeader, corrSearch)
	p.ServeHTTP(httptest.NewRecorder(), reqSearch)

	// --- compare --------------------------------------------------------
	libRecs := listRecords(t, libLedger)
	proxyRecs := listRecords(t, proxyLedger)

	require.Len(t, libRecs, 4, "one add plus one search_performed plus two surfaced")
	require.Len(t, proxyRecs, 4)

	require.Equal(t, ids(libRecs), ids(proxyRecs), "record ID set")

	proxyByID := make(map[record.RecordID]record.Record, len(proxyRecs))
	for _, r := range proxyRecs {
		proxyByID[r.ID] = r
	}
	for _, libRec := range libRecs {
		proxyRec, found := proxyByID[libRec.ID]
		require.True(t, found, "the proxy must produce a record with ID %q", libRec.ID)
		assertRecordsAgree(t, libRec, proxyRec)
	}
}

// assertRecordsAgree compares the two modes' records on exactly the fields the
// parity claim asserts. Each assertion names the field it checks, so a
// divergence reports which field differed.
func assertRecordsAgree(t *testing.T, want, got record.Record) {
	t.Helper()
	require.Equal(t, want.ID, got.ID, "ID")
	require.Equal(t, want.Event, got.Event, "Event")
	require.Equal(t, want.Reason.Kind(), got.Reason.Kind(), "Reason kind")
	require.Equal(t, want.Reason.Tier(), got.Reason.Tier(), "Reason tier")

	wObs, ok := want.Reason.Observed()
	require.True(t, ok, "the library record's reason must be Observed")
	gObs, ok := got.Reason.Observed()
	require.True(t, ok, "the proxy record's reason must be Observed")
	require.Equal(t, wObs.Payload(), gObs.Payload(), "evidence payload bytes")

	require.Equal(t, want.Subject, got.Subject, "Subject (Scope, MemoryID, ContentHash)")
	require.Equal(t, want.Content, got.Content, "Content")
	require.Equal(t, want.IdempotencyKey, got.IdempotencyKey, "IdempotencyKey")
}

// ids returns the records' IDs, sorted so two sets compare as sets.
func ids(recs []record.Record) []record.RecordID {
	out := make([]record.RecordID, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
