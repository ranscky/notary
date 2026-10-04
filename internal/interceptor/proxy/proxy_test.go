package proxy_test

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/gap"
	"notary/internal/interceptor"
	"notary/internal/interceptor/proxy"
	"notary/internal/ledger"
	"notary/internal/record"
)

// ---------------------------------------------------------------------------
// a minimal in-test Mem0 upstream
// ---------------------------------------------------------------------------

// seenRequest is what the stub upstream observed of one request. The body is a
// copy, so it survives after the handler returns.
type seenRequest struct {
	method      string
	path        string
	rawQuery    string
	authz       string
	contentType string
	// host is the Host header the upstream saw. The proxy sets it to the
	// upstream's own host, so a vhost-routed self-hosted Mem0 routes by its
	// name rather than by whatever the caller sent.
	host string
	body []byte
}

// mem0Stub is a minimal in-test Mem0: it records every request it receives and
// answers from its configured bodies. addBody answers POST /v3/memories/add/,
// searchBody answers POST /v3/memories/search/, and other answers anything
// else. A non-zero status is used verbatim (e.g. to simulate a Mem0 failure).
type mem0Stub struct {
	mu         sync.Mutex
	seen       []seenRequest
	addBody    string
	searchBody string
	other      string
	status     int
	header     http.Header
}

// ServeHTTP records the request and answers it from the stub's configuration.
func (s *mem0Stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.seen = append(s.seen, seenRequest{
		method:      r.Method,
		path:        r.URL.Path,
		rawQuery:    r.URL.RawQuery,
		authz:       r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"),
		host:        r.Host,
		body:        append([]byte(nil), body...),
	})
	st := s.status
	hdr := s.header
	var out string
	switch r.URL.Path {
	case "/v3/memories/add/":
		out = s.addBody
	case "/v3/memories/search/":
		out = s.searchBody
	default:
		out = s.other
	}
	s.mu.Unlock()

	for k, vs := range hdr {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if out != "" && w.Header().Get("Content-Type") == "" {
		w.Header().Set("Content-Type", "application/json")
	}
	if st != 0 {
		w.WriteHeader(st)
	}
	if out != "" {
		_, _ = io.WriteString(w, out)
	}
}

// lastSeen returns the most recent request the stub observed.
func (s *mem0Stub) lastSeen(t *testing.T) seenRequest {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.NotEmpty(t, s.seen, "the upstream must have received a request")
	return s.seen[len(s.seen)-1]
}

// newStubServer starts an httptest server backed by stub and closes it on
// cleanup.
func newStubServer(t *testing.T, stub *mem0Stub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(stub)
	t.Cleanup(srv.Close)
	return srv
}

// newTestProxy builds a proxy forwarding to upstreamURL whose Observer writes
// through sink. out receives drop/error markers; maxBody is the observation
// body cap. The proxy is closed on cleanup.
func newTestProxy(t *testing.T, upstreamURL string, sink proxy.RecordSink, out io.Writer, maxBody int64) *proxy.ProxyInterceptor {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	require.NoError(t, err)
	obs := interceptor.NewObserver(sink)
	p := proxy.New(u, obs, sink, out, maxBody)
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// doRequest drives p directly with an httptest recorder and returns it.
func doRequest(p http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, r)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	p.ServeHTTP(rr, req)
	return rr
}

// listRecords reads every record in l. The window is deliberately wide: the
// proxy stamps At from the wall clock while the parity test's library stamps a
// fixed instant, and both must be included.
func listRecords(t *testing.T, l *ledger.Ledger) []record.Record {
	t.Helper()
	recs, err := l.ListRecords(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	return recs
}

// ---------------------------------------------------------------------------
// forwarding fidelity and the unrecorded boundary
// ---------------------------------------------------------------------------

// TestProxyForwardsStatusHeadersAndBodyUnchanged is Review Focus 5's vehicle
// too: a non-observed path (a DELETE and a GET history) carries a deliberately
// malformed JSON body that reaches the upstream byte-for-byte, is never parsed,
// and produces no record. The proxy must not fail traffic it is not recording.
func TestProxyForwardsStatusHeadersAndBodyUnchanged(t *testing.T) {
	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"delete", http.MethodDelete, "/v1/memories/abc/"},
		{"history", http.MethodGet, "/v1/memories/abc/history/"},
		{"get_all", http.MethodPost, "/v3/memories/"},
		{"event_status", http.MethodGet, "/v1/event/evt-1/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &mem0Stub{
				other:  `{"ok":true}`,
				status: http.StatusMultiStatus,
				header: http.Header{"X-Upstream": []string{"yes"}},
			}
			srv := newStubServer(t, stub)
			sink, l, _, _ := newProxyHarness(t)
			p := newTestProxy(t, srv.URL, sink, io.Discard, 8<<20)

			malformed := `{"this is not valid json`
			rr := doRequest(p, tc.method, tc.path, malformed, nil)

			require.Equal(t, http.StatusMultiStatus, rr.Code, "the upstream status must pass through")
			require.Equal(t, "yes", rr.Header().Get("X-Upstream"), "upstream headers must pass through")
			require.Equal(t, `{"ok":true}`, rr.Body.String(), "the upstream body must pass through")

			seen := stub.lastSeen(t)
			require.Equal(t, tc.method, seen.method)
			require.Equal(t, tc.path, seen.path)
			require.Equal(t, malformed, string(seen.body), "the body must reach the upstream byte-for-byte")

			require.Empty(t, listRecords(t, l), "a non-observed endpoint records nothing")
		})
	}
}

// TestProxySetsTheUpstreamHostHeader pins finding F: the stdlib reverse proxy
// rewrites the outbound URL's scheme, host and path but deliberately leaves
// req.Host alone, so the caller's Host header would reach Mem0. A self-hosted
// Mem0 routed by virtual host would then misroute. The stub must see its own
// host, not httptest.NewRequest's "example.com".
func TestProxySetsTheUpstreamHostHeader(t *testing.T) {
	stub := &mem0Stub{addBody: `{"event_id":"e","status":"PENDING"}`}
	srv := newStubServer(t, stub)
	sink, _, _, _ := newProxyHarness(t)
	p := newTestProxy(t, srv.URL, sink, io.Discard, 8<<20)

	body := `{"messages":[{"role":"user","content":"hi"}],"user_id":"u1"}`
	rr := doRequest(p, http.MethodPost, "/v3/memories/add/", body,
		map[string]string{proxy.CorrelationHeader: "corr-host"})
	require.Equal(t, http.StatusOK, rr.Code)

	up, err := url.Parse(srv.URL)
	require.NoError(t, err)
	seen := stub.lastSeen(t)
	assert.Equal(t, up.Host, seen.host, "the upstream must see its own Host, not the caller's")
	assert.NotEqual(t, "example.com", seen.host,
		"httptest.NewRequest's caller Host must not be forwarded to the upstream")
}

// ---------------------------------------------------------------------------
// the add hook
// ---------------------------------------------------------------------------

// TestProxyRecordsAnAddFromTheRequestBodyAndResponse pins that one add produces
// one add_requested record whose scope came from the body's top-level entity
// ids and whose evidence is Mem0's acknowledgement.
func TestProxyRecordsAnAddFromTheRequestBodyAndResponse(t *testing.T) {
	stub := &mem0Stub{addBody: `{"event_id":"evt-123","status":"PENDING"}`}
	srv := newStubServer(t, stub)
	sink, l, _, _ := newProxyHarness(t)
	p := newTestProxy(t, srv.URL, sink, io.Discard, 8<<20)

	body := `{"messages":[{"role":"user","content":"remember this"}],"user_id":"u1","agent_id":"a1"}`
	rr := doRequest(p, http.MethodPost, "/v3/memories/add/", body,
		map[string]string{proxy.CorrelationHeader: "corr-add"})
	require.Equal(t, http.StatusOK, rr.Code)

	recs := listRecords(t, l)
	require.Len(t, recs, 1)
	rec := recs[0]
	assert.Equal(t, record.RecordID("corr-add"), rec.ID)
	assert.Equal(t, record.EventAddRequested, rec.Event)
	assert.Equal(t, record.ReasonAddAcknowledged, rec.Reason.Kind())
	assert.Equal(t, record.Observed, rec.Reason.Tier())
	assert.Equal(t, record.Scope{UserID: "u1", AgentID: "a1"}, rec.Subject.Scope)
	assert.Equal(t, record.ContentHash("remember this"), rec.Subject.ContentHash)
	require.NotNil(t, rec.Content)
	assert.Equal(t, "remember this", rec.Content.Text)

	ev, ok := rec.Reason.Observed()
	require.True(t, ok)
	assert.JSONEq(t, `{"event_id":"evt-123","status":"PENDING"}`, string(ev.Payload()))

	// The two instants (decision 1): At is the arrival instant the proxy
	// captured before forwarding, and RecordedAt is stamped by the ledger's
	// writer clock, so the two are different true facts. If the proxy copied
	// library mode's conflation and set RecordedAt to the arrival instant, the
	// ledger would not override it and this would fail.
	assert.Equal(t, proxyHarnessNow, rec.RecordedAt, "RecordedAt is the writer's clock, not the arrival instant")
	assert.NotEqual(t, rec.At, rec.RecordedAt, "At and RecordedAt must be two different instants")
}

// ---------------------------------------------------------------------------
// the search hook
// ---------------------------------------------------------------------------

// TestProxyRecordsASearchPerformedAndEachSurfacedMemoryInRankOrder pins that a
// search produces one search_performed record plus one memory_surfaced record
// per result, in rank order.
func TestProxyRecordsASearchPerformedAndEachSurfacedMemoryInRankOrder(t *testing.T) {
	stub := &mem0Stub{searchBody: `{"results":[
		{"id":"m1","memory":"alpha","score":0.9},
		{"id":"m2","memory":"beta","score":0.5}]}`}
	srv := newStubServer(t, stub)
	sink, l, _, _ := newProxyHarness(t)
	p := newTestProxy(t, srv.URL, sink, io.Discard, 8<<20)

	body := `{"query":"what","filters":{"user_id":"u1"}}`
	rr := doRequest(p, http.MethodPost, "/v3/memories/search/", body,
		map[string]string{proxy.CorrelationHeader: "corr-search"})
	require.Equal(t, http.StatusOK, rr.Code)

	recs := listRecords(t, l)
	require.Len(t, recs, 3)

	assert.Equal(t, record.RecordID("corr-search"), recs[0].ID)
	assert.Equal(t, record.EventSearchPerformed, recs[0].Event)
	assert.Equal(t, record.ReasonSearchPerformed, recs[0].Reason.Kind())
	assert.Equal(t, record.Scope{UserID: "u1"}, recs[0].Subject.Scope)
	assert.Nil(t, recs[0].Content, "a search_performed record carries no content")

	assert.Equal(t, record.RecordID("corr-search#1"), recs[1].ID)
	assert.Equal(t, record.EventMemorySurfaced, recs[1].Event)
	assert.Equal(t, record.ReasonReturnedBySearch, recs[1].Reason.Kind())
	assert.Equal(t, "m1", recs[1].Subject.MemoryID)
	require.NotNil(t, recs[1].Content)
	assert.Equal(t, "alpha", recs[1].Content.Text)

	assert.Equal(t, record.RecordID("corr-search#2"), recs[2].ID)
	assert.Equal(t, "m2", recs[2].Subject.MemoryID)
	assert.Equal(t, "beta", recs[2].Content.Text)

	ev1, ok := recs[1].Reason.Observed()
	require.True(t, ok)
	assert.JSONEq(t, `{"score":0.9,"rank":1}`, string(ev1.Payload()))
	ev2, ok := recs[2].Reason.Observed()
	require.True(t, ok)
	assert.JSONEq(t, `{"score":0.5,"rank":2}`, string(ev2.Payload()))
}

// ---------------------------------------------------------------------------
// correlation identity
// ---------------------------------------------------------------------------

// TestProxyUsesTheCallerCorrelationHeader pins that the record's ID is the
// value of the X-Notary-Correlation-Id request header.
func TestProxyUsesTheCallerCorrelationHeader(t *testing.T) {
	stub := &mem0Stub{addBody: `{"event_id":"e","status":"PENDING"}`}
	srv := newStubServer(t, stub)
	sink, l, _, _ := newProxyHarness(t)
	p := newTestProxy(t, srv.URL, sink, io.Discard, 8<<20)

	body := `{"messages":[{"role":"user","content":"hi"}],"user_id":"u1"}`
	doRequest(p, http.MethodPost, "/v3/memories/add/", body,
		map[string]string{proxy.CorrelationHeader: "caller-supplied-id"})

	recs := listRecords(t, l)
	require.Len(t, recs, 1)
	assert.Equal(t, record.RecordID("caller-supplied-id"), recs[0].ID)
}

// TestProxyDerivesACorrelationIDWhenTheHeaderIsAbsent pins decision 2's
// fallback: with no header the proxy derives
// DeriveCorrelationID(scope, hex(ContentHash(method, path, body))).
func TestProxyDerivesACorrelationIDWhenTheHeaderIsAbsent(t *testing.T) {
	stub := &mem0Stub{addBody: `{"event_id":"e","status":"PENDING"}`}
	srv := newStubServer(t, stub)
	sink, l, _, _ := newProxyHarness(t)
	p := newTestProxy(t, srv.URL, sink, io.Discard, 8<<20)

	body := `{"messages":[{"role":"user","content":"hi"}],"user_id":"u1"}`
	doRequest(p, http.MethodPost, "/v3/memories/add/", body, nil)

	digest := record.ContentHash(http.MethodPost, "/v3/memories/add/", body)
	want := interceptor.DeriveCorrelationID(record.Scope{UserID: "u1"}, hex.EncodeToString(digest[:]))

	recs := listRecords(t, l)
	require.Len(t, recs, 1)
	assert.Equal(t, record.RecordID(want), recs[0].ID)
}

// TestProxyCollapsesTwoIdenticalHeaderlessRequests pins Review Focus 4:
// decision 2's documented collapse. Two identical headerless requests derive
// the same correlation ID, and therefore the same idempotency key, so the
// ledger holds ONE record, not two.
func TestProxyCollapsesTwoIdenticalHeaderlessRequests(t *testing.T) {
	stub := &mem0Stub{addBody: `{"event_id":"e","status":"PENDING"}`}
	srv := newStubServer(t, stub)
	sink, l, _, _ := newProxyHarness(t)
	p := newTestProxy(t, srv.URL, sink, io.Discard, 8<<20)

	body := `{"messages":[{"role":"user","content":"dup"}],"user_id":"u1"}`
	doRequest(p, http.MethodPost, "/v3/memories/add/", body, nil)
	doRequest(p, http.MethodPost, "/v3/memories/add/", body, nil)

	assert.Len(t, listRecords(t, l), 1, "two identical headerless requests must collapse to one record")
}

// ---------------------------------------------------------------------------
// the body cap
// ---------------------------------------------------------------------------

// TestProxyForwardsAnOverCapBodyWithoutRecordingIt pins Review Focus 2: a body
// over --max-body is forwarded intact to the upstream and to the caller, with
// no record AND no gap entry.
func TestProxyForwardsAnOverCapBodyWithoutRecordingIt(t *testing.T) {
	const maxBody = 32
	stub := &mem0Stub{addBody: `{"event_id":"e","status":"PENDING"}`}
	srv := newStubServer(t, stub)
	sink, l, _, gapPath := newProxyHarness(t)
	p := newTestProxy(t, srv.URL, sink, io.Discard, maxBody)

	big := strings.Repeat("x", 200)
	body := `{"messages":[{"role":"user","content":"` + big + `"}],"user_id":"u1"}`
	require.Greater(t, int64(len(body)), int64(maxBody))

	rr := doRequest(p, http.MethodPost, "/v3/memories/add/", body,
		map[string]string{proxy.CorrelationHeader: "corr-big"})
	require.Equal(t, http.StatusOK, rr.Code)

	seen := stub.lastSeen(t)
	require.Equal(t, body, string(seen.body), "the whole body must reach the upstream")

	require.Empty(t, listRecords(t, l), "an over-cap body is not recorded")
	entries, err := gap.Read(gapPath)
	require.NoError(t, err)
	require.Empty(t, entries, "an over-cap body leaves no gap entry")
}

// ---------------------------------------------------------------------------
// upstream failure, credential hygiene
// ---------------------------------------------------------------------------

// TestProxyReturnsAnErrorWhenTheUpstreamIsUnreachable is Review Focus 1: the
// caller gets a real HTTP error promptly, nothing is recorded, and neither the
// error nor the marker output contains the Authorization header value or a
// query-string sentinel. The proxy forwards the caller's credentials and holds
// none of its own, so it must never render them.
func TestProxyReturnsAnErrorWhenTheUpstreamIsUnreachable(t *testing.T) {
	// A server that is closed before use: the dial will be refused promptly.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	sink, l, _, _ := newProxyHarness(t)
	var out bytes.Buffer
	p := newTestProxy(t, deadURL, sink, &out, 8<<20)

	const (
		apiKey   = "Token SUPER-SECRET-KEY-123"
		sentinel = "QUERY-SENTINEL-XYZ"
	)
	body := `{"messages":[{"role":"user","content":"secret"}],"user_id":"u1"}`
	rr := doRequest(p, http.MethodPost, "http://proxy/v3/memories/add/?sentinel="+sentinel, body,
		map[string]string{
			proxy.CorrelationHeader: "corr-unreach",
			"Authorization":         apiKey,
		})

	require.Equal(t, http.StatusBadGateway, rr.Code, "the caller must get a real HTTP error")
	// The marker carries the failure REASON (finding E): a DNS failure, a
	// refused connection, a TLS error and a timeout must be distinguishable to
	// an operator. It is the transport's own cause, not a placeholder, and the
	// *url.Error's outbound URL -- whose path and query are the caller's -- is
	// stripped to scheme and host before it is rendered.
	assert.Contains(t, out.String(), "reason=", "the marker must say why the upstream request failed")
	assert.NotContains(t, out.String(), "reason=unknown error", "the transport's own reason must survive")
	require.NotContains(t, out.String(), "SUPER-SECRET-KEY-123", "the marker must not leak the API key")
	require.NotContains(t, out.String(), sentinel, "the marker must not leak the query string")
	require.NotContains(t, out.String(), apiKey)
	require.NotContains(t, rr.Body.String(), "SUPER-SECRET-KEY-123")
	require.NotContains(t, rr.Body.String(), sentinel)

	require.Empty(t, listRecords(t, l), "an unreachable upstream records nothing")
}

// TestProxyRecordsNothingWhenMem0ReturnsAnError pins parity with library.Add: a
// non-2xx from Mem0 is a real error, not an audit gap, and writes no record.
func TestProxyRecordsNothingWhenMem0ReturnsAnError(t *testing.T) {
	stub := &mem0Stub{status: http.StatusInternalServerError}
	srv := newStubServer(t, stub)
	sink, l, _, _ := newProxyHarness(t)
	p := newTestProxy(t, srv.URL, sink, io.Discard, 8<<20)

	body := `{"messages":[{"role":"user","content":"hi"}],"user_id":"u1"}`
	rr := doRequest(p, http.MethodPost, "/v3/memories/add/", body,
		map[string]string{proxy.CorrelationHeader: "corr-err"})

	require.Equal(t, http.StatusInternalServerError, rr.Code)
	require.Empty(t, listRecords(t, l), "a Mem0 failure records nothing")
}

// TestProxyForwardsAScopelessRequestWithoutRecording pins the last of decision
// 5's degrade cases: a recorded endpoint carrying no scope in its body cannot
// be attributed, so it is forwarded and recorded nothing, but never failed.
func TestProxyForwardsAScopelessRequestWithoutRecording(t *testing.T) {
	stub := &mem0Stub{addBody: `{"event_id":"e","status":"PENDING"}`}
	srv := newStubServer(t, stub)
	sink, l, _, _ := newProxyHarness(t)
	p := newTestProxy(t, srv.URL, sink, io.Discard, 8<<20)

	// A well-formed add body with no user_id/agent_id/app_id/run_id.
	body := `{"messages":[{"role":"user","content":"hi"}]}`
	rr := doRequest(p, http.MethodPost, "/v3/memories/add/", body,
		map[string]string{proxy.CorrelationHeader: "corr-scopeless"})

	require.Equal(t, http.StatusOK, rr.Code, "a scopeless request must still be forwarded")
	seen := stub.lastSeen(t)
	require.Equal(t, body, string(seen.body), "the body must reach the upstream untouched")
	require.Empty(t, listRecords(t, l), "a scopeless request records nothing")
}

// ---------------------------------------------------------------------------
// compressed responses
// ---------------------------------------------------------------------------

// TestProxyRecordsAGzippedResponseAndLeavesTheCallersBytesGzipped pins the
// finding that a caller which sets its own Accept-Encoding makes Go's transport
// forward a still-compressed body -- the transport decompresses transparently
// only when IT added the header itself. The proxy must decode the OBSERVATION
// COPY (recording the add) while the caller still receives the upstream's gzip
// bytes, byte for byte. Without decoding the copy, json.Unmarshal fails, no
// record is written, and the loss is silent.
func TestProxyRecordsAGzippedResponseAndLeavesTheCallersBytesGzipped(t *testing.T) {
	plain := `{"event_id":"evt-gz","status":"PENDING"}`
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, err := zw.Write([]byte(plain))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	gzBytes := gz.Bytes()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gzBytes)
			return
		}
		_, _ = io.WriteString(w, plain)
	}))
	t.Cleanup(srv.Close)

	sink, l, _, _ := newProxyHarness(t)
	var out bytes.Buffer
	p := newTestProxy(t, srv.URL, sink, &out, 8<<20)

	body := `{"messages":[{"role":"user","content":"hi"}],"user_id":"u1"}`
	rr := doRequest(p, http.MethodPost, "/v3/memories/add/", body, map[string]string{
		proxy.CorrelationHeader: "corr-gz",
		"Accept-Encoding":       "gzip",
	})
	require.Equal(t, http.StatusOK, rr.Code)

	// The caller's response is still gzip, byte-identical to what the upstream
	// sent: the decompression was for the observation copy only.
	assert.Equal(t, "gzip", rr.Header().Get("Content-Encoding"))
	assert.Equal(t, gzBytes, rr.Body.Bytes(), "the caller's bytes must be the upstream's gzip bytes unchanged")

	// Yet the add was recorded, from the decoded copy.
	recs := listRecords(t, l)
	require.Len(t, recs, 1, "a gzipped response must still be recorded")
	assert.Equal(t, record.RecordID("corr-gz"), recs[0].ID)
	assert.Empty(t, out.String(), "a successful decode must not mark")
	ev, ok := recs[0].Reason.Observed()
	require.True(t, ok)
	assert.JSONEq(t, plain, string(ev.Payload()))
}

// TestProxyMarksAndRecordsNothingForAnUndecodableResponseEncoding pins the loud
// half of the gzip fix: a response compressed with an encoding the proxy will
// not decode (br, zstd, deflate) is recorded nothing but MARKED, so the loss is
// never silent. The caller's response is unaffected.
func TestProxyMarksAndRecordsNothingForAnUndecodableResponseEncoding(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"event_id":"e","status":"PENDING"}`)
	}))
	t.Cleanup(srv.Close)

	sink, l, _, _ := newProxyHarness(t)
	var out bytes.Buffer
	p := newTestProxy(t, srv.URL, sink, &out, 8<<20)

	body := `{"messages":[{"role":"user","content":"hi"}],"user_id":"u1"}`
	rr := doRequest(p, http.MethodPost, "/v3/memories/add/", body, map[string]string{
		proxy.CorrelationHeader: "corr-br",
		"Accept-Encoding":       "br",
	})
	require.Equal(t, http.StatusOK, rr.Code, "the caller's response is unaffected")

	assert.Empty(t, listRecords(t, l), "an undecodable response is not recorded")
	assert.Contains(t, out.String(), "br", "the loss must be loud, not silent")
}

// TestProxyMarksAndRecordsNothingWhenAGzippedResponseExpandsPastTheCap pins the
// loud half of the decoded-copy bound: a small compressed response whose decoded
// copy exceeds --max-body is recorded nothing but MARKED, exactly like an
// undecodable encoding, and the caller still receives the upstream's gzip bytes
// untouched. Without the bound the decoded copy would be read into memory in
// full for observation, so this is the half that keeps the cap a cap.
func TestProxyMarksAndRecordsNothingWhenAGzippedResponseExpandsPastTheCap(t *testing.T) {
	const maxBody = 1024
	plain := `{"event_id":"evt-gz-big","status":"PENDING","pad":"` +
		strings.Repeat("z", 64<<10) + `"}`
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, err := zw.Write([]byte(plain))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	gzBytes := gz.Bytes()
	require.Less(t, int64(len(gzBytes)), int64(maxBody),
		"the compressed body must fit under the cap so the decode path runs")
	require.Greater(t, int64(len(plain)), int64(maxBody),
		"the decoded body must exceed the cap so the decode is declined")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(gzBytes)
	}))
	t.Cleanup(srv.Close)

	sink, l, _, _ := newProxyHarness(t)
	var out bytes.Buffer
	p := newTestProxy(t, srv.URL, sink, &out, maxBody)

	body := `{"messages":[{"role":"user","content":"hi"}],"user_id":"u1"}`
	require.Less(t, len(body), maxBody, "the request must be under the cap so it is observed")
	rr := doRequest(p, http.MethodPost, "/v3/memories/add/", body, map[string]string{
		proxy.CorrelationHeader: "corr-gz-over",
		"Accept-Encoding":       "gzip",
	})
	require.Equal(t, http.StatusOK, rr.Code)

	assert.Equal(t, "gzip", rr.Header().Get("Content-Encoding"))
	assert.Equal(t, gzBytes, rr.Body.Bytes(), "the caller's bytes are the upstream's gzip bytes unchanged")
	require.Empty(t, listRecords(t, l), "a decoded body past the cap is not recorded")
	assert.Contains(t, out.String(), "decoded response body exceeds",
		"the loss must be loud: the bounded decode declined instead of recording nothing in silence")
}

// ---------------------------------------------------------------------------
// the response cap
// ---------------------------------------------------------------------------

// TestProxyForwardsAnOverCapResponseWithoutRecordingIt is the request-side cap
// test's response twin: a response body over --max-body is still delivered to
// the caller in full, with no record.
//
// The cap sits between the request body and the response body: the request
// (small) is observed so the response path runs, and the response (large) is
// not.
func TestProxyForwardsAnOverCapResponseWithoutRecordingIt(t *testing.T) {
	const maxBody = 100
	big := strings.Repeat("y", 500)
	stub := &mem0Stub{addBody: `{"event_id":"e","status":"PENDING","pad":"` + big + `"}`}
	srv := newStubServer(t, stub)
	sink, l, _, _ := newProxyHarness(t)
	p := newTestProxy(t, srv.URL, sink, io.Discard, maxBody)

	body := `{"messages":[{"role":"user","content":"hi"}],"user_id":"u1"}`
	require.Less(t, len(body), maxBody, "the request must be under the cap so it is observed")
	require.Greater(t, len(stub.addBody), maxBody, "the response must be over the cap so it is not observed")

	rr := doRequest(p, http.MethodPost, "/v3/memories/add/", body,
		map[string]string{proxy.CorrelationHeader: "corr-bigresp"})
	require.Equal(t, http.StatusOK, rr.Code)

	require.Greater(t, int64(rr.Body.Len()), int64(maxBody), "the caller must receive the whole over-cap response")
	assert.Equal(t, stub.addBody, rr.Body.String(), "the caller's over-cap response body is the upstream's bytes, in full")
	require.Empty(t, listRecords(t, l), "an over-cap response is not recorded")
}

// ---------------------------------------------------------------------------
// redaction of printable URLs
// ---------------------------------------------------------------------------

// TestRedactedURLRendersSchemeAndHostOnly pins the redaction both the proxy's
// error marker and the command's banner and refusals use: scheme and host
// survive, and every place an operator-supplied credential can hide -- userinfo,
// path, query, fragment -- does not. A value that does not parse, has no host,
// or is opaque is rendered as a constant rather than reconstructed, because
// there is nothing safe to show.
func TestRedactedURLRendersSchemeAndHostOnly(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://mem0.internal:8081", "https://mem0.internal:8081"},
		{"https://operator:SUPER-SECRET@mem0.internal:8081/v1/long/path?key=SUPER-SECRET#frag", "https://mem0.internal:8081"},
		{"http://[::1]:8081/x?q=1", "http://[::1]:8081"},
		{"mem0.internal:8081", "[url omitted]"},
		{"https://", "[url omitted]"},
		{"http://[::1]:namedport/?key=SUPER-SECRET", "[url omitted]"},
	} {
		got := proxy.RedactedURL(tc.in)
		assert.Equal(t, tc.want, got, "RedactedURL(%q)", tc.in)
		assert.NotContains(t, got, "SUPER-SECRET", "RedactedURL(%q) must not echo a credential", tc.in)
	}
}
