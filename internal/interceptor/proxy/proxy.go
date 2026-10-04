// This file implements the proxy's forwarding handler and its observation
// hooks. The handler forwards every request to the real Mem0 and returns the
// response untouched; on the two recorded endpoints it also builds what library
// mode would have recorded and hands it to the shared interceptor.Observer. The
// write pipeline those records feed lives in pipeline.go; this file never
// touches the ledger itself (design section 5).

package proxy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"

	"notary/internal/interceptor"
	"notary/internal/mem0"
	"notary/internal/record"
)

const (
	// addPath and searchPath are the only endpoints the proxy records (design
	// section 7). Every other path -- get_all, history, event status, delete --
	// is forwarded and not recorded, which is a boundary rather than a gap.
	addPath    = "/v3/memories/add/"
	searchPath = "/v3/memories/search/"

	// CorrelationHeader is the request header a caller sets to name the
	// logical operation a record belongs to. When it is absent the proxy
	// derives a correlation ID from the request (see correlationID), so a
	// caller who wants identical semantics across library and proxy mode,
	// including exact retry deduplication, sets it.
	CorrelationHeader = "X-Notary-Correlation-Id"

	// defaultMaxBody is the observation body cap used when a non-positive
	// maxBody is supplied, matching the 8 MiB bound internal/mem0/client.go
	// puts on a response.
	defaultMaxBody = 8 << 20
)

// observedKind identifies which recorded endpoint a request target belongs to.
type observedKind uint8

const (
	kindAdd observedKind = iota + 1
	kindSearch
)

// obsRequest is the per-request observation context carried from ServeHTTP
// into ModifyResponse. It holds everything the observer needs that is not
// recoverable from the HTTP response: the arrival instant, the correlation
// identity, the scope sourced from the request body, and the request fields a
// record is built from.
type obsRequest struct {
	kind      observedKind
	at        time.Time
	corrID    string
	scope     record.Scope
	messages  []string
	metadata  map[string]any
	searchReq mem0.SearchRequest
}

// ctxKey is the unexported context key that carries an *obsRequest from the
// request path into ModifyResponse.
type ctxKey struct{}

// ProxyInterceptor is Notary's reverse proxy in front of Mem0. It forwards
// every request to the upstream and returns the response untouched, recording
// what library mode would record for POST /v3/memories/add/ and
// POST /v3/memories/search/ and nothing else.
//
// It holds no clock and no ledger of its own: the Observer builds records and
// the borrowed RecordSink disposes of them. Every failure in the observation
// path degrades to "no record" -- a decode error, an over-cap body, a missing
// scope, an unreachable upstream, a non-2xx from Mem0 -- and never to a failed
// request, which is the phase's whole posture. It is safe for concurrent use.
type ProxyInterceptor struct {
	// obs builds the records. A nil Observer is tolerated -- nothing is then
	// recorded -- so a misconfiguration cannot panic a request.
	obs *interceptor.Observer
	// sink is the borrowed RecordSink the records flow to. It is closed by
	// Close; a nil sink is tolerated.
	sink RecordSink
	// out receives drop and error markers only. The response body is never
	// written there. A nil out discards markers.
	out io.Writer
	// maxBody caps how much of a request or response body is buffered for
	// observation. A body larger than it is forwarded untouched and not
	// observed.
	maxBody int64
	// rp is the stdlib reverse proxy that handles the forwarding, including
	// the hop-by-hop rules a hand-rolled forwarder gets wrong.
	rp *httputil.ReverseProxy

	// closeOnce makes Close idempotent; closeErr is its memoised result.
	closeOnce sync.Once
	closeErr  error
}

// Compile-time proof that *ProxyInterceptor is an Interceptor.
var _ interceptor.Interceptor = (*ProxyInterceptor)(nil)

// New returns a ProxyInterceptor that forwards to upstream, records through
// obs, closes sink on Close, writes drop and error markers to out, and buffers
// at most maxBody bytes of any body for observation.
//
// upstream is borrowed and never mutated. A nil upstream is tolerated and
// behaves as an unreachable endpoint rather than panicking. A non-positive
// maxBody falls back to defaultMaxBody, so a misconfiguration cannot silently
// stop every body from being observed. obs and sink are borrowed; obs must
// have been built over a Sink that is (or wraps) the same sink, so records
// reach it.
func New(upstream *url.URL, obs *interceptor.Observer, sink RecordSink, out io.Writer, maxBody int64) *ProxyInterceptor {
	if maxBody < 1 {
		maxBody = defaultMaxBody
	}
	if upstream == nil {
		upstream = &url.URL{}
	}
	p := &ProxyInterceptor{
		obs:     obs,
		sink:    sink,
		out:     out,
		maxBody: maxBody,
	}
	p.rp = httputil.NewSingleHostReverseProxy(upstream)
	p.rp.ModifyResponse = p.modifyResponse
	p.rp.ErrorHandler = p.handleError
	return p
}

// ServeHTTP forwards r to the upstream. On a recorded endpoint it also captures
// the arrival instant, the scope and the correlation identity from r, and
// stashes them for ModifyResponse to observe once Mem0's response exists.
func (p *ProxyInterceptor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	kind, observed := observedKindFor(r.Method, r.URL.Path)
	if !observed {
		p.rp.ServeHTTP(w, r)
		return
	}
	p.serveObserved(w, r, kind)
}

// observedKindFor reports whether method and path name a recorded endpoint.
// It matches on both, so paths that share a prefix cannot be confused with a
// recorded one.
func observedKindFor(method, path string) (observedKind, bool) {
	if method != http.MethodPost {
		return 0, false
	}
	switch path {
	case addPath:
		return kindAdd, true
	case searchPath:
		return kindSearch, true
	default:
		return 0, false
	}
}

// serveObserved buffers the request body up to the cap, decides what (if
// anything) can be recorded, and forwards the request with the body restored
// exactly as it arrived. An over-cap body, a decode failure or a missing scope
// all fall through to a plain forward that records nothing; the caller's
// traffic is never failed and never altered.
func (p *ProxyInterceptor) serveObserved(w http.ResponseWriter, r *http.Request, kind observedKind) {
	at := time.Now().UTC()

	orig := r.Body
	if orig == nil {
		orig = http.NoBody
	}
	// Read one byte past the cap so an over-size body is detected rather than
	// silently truncated -- a truncated body could still look decodable.
	buf, _ := io.ReadAll(io.LimitReader(orig, p.maxBody+1))
	// Restore the body so the forward is byte-for-byte whatever arrived: the
	// buffered prefix followed by whatever the reader has left (nothing when
	// the whole body fit under the cap).
	r.Body = restoreBody(buf, orig)

	if int64(len(buf)) > p.maxBody {
		p.mark("notary: proxy: request body exceeds %d bytes; forwarded unobserved: method=%s path=%s\n",
			p.maxBody, r.Method, r.URL.Path)
		p.rp.ServeHTTP(w, r)
		return
	}

	o, ok := p.buildObservation(r, kind, buf, at)
	if !ok {
		p.rp.ServeHTTP(w, r)
		return
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, o)))
}

// buildObservation decodes the request body and assembles the observation
// context, reporting false when this request cannot be attributed and should
// therefore be forwarded without a record: a body that does not decode, or a
// body carrying no scope. The scope is sourced from the body because the proxy
// has no construction-time scope; an add carries it at the top level and a
// search inside filters (internal/mem0/types.go).
func (p *ProxyInterceptor) buildObservation(r *http.Request, kind observedKind, body []byte, at time.Time) (*obsRequest, bool) {
	switch kind {
	case kindAdd:
		var req mem0.AddRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, false
		}
		scope := record.Scope{UserID: req.UserID, AgentID: req.AgentID, AppID: req.AppID, RunID: req.RunID}
		if scopeEmpty(scope) {
			return nil, false
		}
		messages := make([]string, 0, len(req.Messages))
		for _, m := range req.Messages {
			messages = append(messages, m.Content)
		}
		return &obsRequest{
			kind:     kindAdd,
			at:       at,
			corrID:   p.correlationID(r, scope, body),
			scope:    scope,
			messages: messages,
			metadata: req.Metadata,
		}, true
	case kindSearch:
		var req mem0.SearchRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, false
		}
		scope := record.Scope{
			UserID:  req.Filters.UserID,
			AgentID: req.Filters.AgentID,
			AppID:   req.Filters.AppID,
			RunID:   req.Filters.RunID,
		}
		if scopeEmpty(scope) {
			return nil, false
		}
		return &obsRequest{
			kind:      kindSearch,
			at:        at,
			corrID:    p.correlationID(r, scope, body),
			scope:     scope,
			searchReq: req,
		}, true
	default:
		return nil, false
	}
}

// correlationID returns the record identity for this request: the caller's
// X-Notary-Correlation-Id when set, else a digest derived from the scope and
// the request. The derived seed is the hex of record.ContentHash(method, path,
// body), whose per-part length-prefixing stops the three fields being re-split
// ambiguously.
func (p *ProxyInterceptor) correlationID(r *http.Request, scope record.Scope, body []byte) string {
	if id := r.Header.Get(CorrelationHeader); id != "" {
		return id
	}
	digest := record.ContentHash(r.Method, r.URL.Path, string(body))
	return interceptor.DeriveCorrelationID(scope, hex.EncodeToString(digest[:]))
}

// modifyResponse observes the upstream response. It reads the body up to the
// cap, hands the same bytes back to the caller by restoring resp.Body, and only
// then builds records. It never returns an error: every failure degrades to
// "no record" rather than to a broken response, so nothing here can turn an
// audit problem into the caller's problem.
func (p *ProxyInterceptor) modifyResponse(resp *http.Response) error {
	if resp == nil || resp.Request == nil {
		return nil
	}
	o, _ := resp.Request.Context().Value(ctxKey{}).(*obsRequest)
	if o == nil {
		return nil // not a recorded request
	}

	orig := resp.Body
	if orig == nil {
		orig = http.NoBody
	}
	body, err := io.ReadAll(io.LimitReader(orig, p.maxBody+1))
	// Hand the caller the whole body whether or not it is observed: the bytes
	// read plus whatever the reader has left equals exactly the upstream body.
	resp.Body = restoreBody(body, orig)

	if err != nil {
		p.mark("notary: proxy: reading upstream response failed; not recorded: method=%s path=%s\n",
			resp.Request.Method, resp.Request.URL.Path)
		return nil
	}
	if int64(len(body)) > p.maxBody {
		p.mark("notary: proxy: response body exceeds %d bytes; not recorded: method=%s path=%s\n",
			p.maxBody, resp.Request.Method, resp.Request.URL.Path)
		return nil
	}
	// A Mem0 failure records nothing: it is a real error, not an audit gap,
	// exactly as library.Add and library.Search write no record on error.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil
	}
	if p.obs == nil {
		return nil
	}

	switch o.kind {
	case kindAdd:
		var ar mem0.AddResponse
		if err := json.Unmarshal(body, &ar); err != nil {
			return nil
		}
		p.obs.Add(interceptor.AddObservation{
			Scope:         o.scope,
			CorrelationID: o.corrID,
			Messages:      o.messages,
			Metadata:      o.metadata,
			Response:      ar,
			At:            o.at,
			// RecordedAt is left zero: ledger.Append stamps it with the
			// writer's clock, so "when it happened" (At) and "when it was
			// written" (RecordedAt) stay two different true facts (section 5).
		})
	case kindSearch:
		var sr mem0.SearchResponse
		if err := json.Unmarshal(body, &sr); err != nil {
			return nil
		}
		p.obs.Search(interceptor.SearchObservation{
			Scope:         o.scope,
			CorrelationID: o.corrID,
			Request:       o.searchReq,
			Response:      sr,
			At:            o.at,
		})
	}
	return nil
}

// handleError answers a caller whose request could not reach the upstream. It
// writes a real HTTP error, records nothing, and deliberately never renders err
// or the request URL: either can carry the caller's credentials -- the
// Authorization header or a query-string sentinel -- and the proxy holds no
// credential of its own to justify echoing one.
func (p *ProxyInterceptor) handleError(w http.ResponseWriter, r *http.Request, _ error) {
	p.mark("notary: proxy: upstream request failed: method=%s path=%s\n", r.Method, r.URL.Path)
	w.WriteHeader(http.StatusBadGateway)
}

// mark writes a one-line marker to out, best-effort. A nil out discards it.
// Markers name only the request method and path -- never a header value, a
// query string or a body -- so no credential can reach the operator through
// this path.
func (p *ProxyInterceptor) mark(format string, args ...any) {
	if p.out == nil {
		return
	}
	_, _ = fmt.Fprintf(p.out, format, args...)
}

// FailMode reports the mode this interceptor runs under: always FailOpenLoud,
// matching library mode. An audit failure is reported loudly and durably while
// the caller's traffic proceeds.
func (p *ProxyInterceptor) FailMode() interceptor.FailMode { return interceptor.FailOpenLoud }

// Close closes the borrowed sink and returns its error. It is idempotent and
// nil-safe: a proxy with no sink closes cleanly, and calling Close more than
// once is safe.
func (p *ProxyInterceptor) Close() error {
	p.closeOnce.Do(func() {
		if p.sink != nil {
			p.closeErr = p.sink.Close()
		}
	})
	return p.closeErr
}

// restoreBody rebuilds a request or response body from the bytes already read
// and the reader they came from, so the caller sees exactly what arrived.
// orig is at EOF when buf holds the whole body, and still has bytes when the
// body was over the cap; in both cases buf followed by orig is the whole body.
func restoreBody(buf []byte, orig io.ReadCloser) io.ReadCloser {
	return readCloser{Reader: io.MultiReader(bytes.NewReader(buf), orig), Closer: orig}
}

// readCloser pairs the reconstructed reader with the original body's Closer,
// so closing the restored body closes the underlying stream.
type readCloser struct {
	io.Reader
	io.Closer
}

// scopeEmpty reports whether a scope carries no identity at all. A recorded
// request always carries a scope in its body (both endpoints do), so a
// scopeless body is a request this phase cannot attribute and therefore does
// not record (design section 10).
func scopeEmpty(s record.Scope) bool {
	return s.UserID == "" && s.AgentID == "" && s.AppID == "" && s.RunID == ""
}
