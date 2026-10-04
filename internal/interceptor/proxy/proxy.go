// This file implements the proxy's forwarding handler and its observation
// hooks. The handler forwards every request to the real Mem0 and returns the
// response untouched; on the two recorded endpoints it also builds what library
// mode would have recorded and hands it to the shared interceptor.Observer. The
// write pipeline those records feed lives in pipeline.go; this file never
// touches the ledger itself (design section 5).

package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
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
	// written there. A nil out discards markers. Writes to it are serialized
	// under markMu (see mark), so out need not itself be safe for concurrent
	// use.
	out io.Writer
	// maxBody caps how much of a request or response body is buffered for
	// observation. A body larger than it is forwarded untouched and not
	// observed.
	maxBody int64
	// rp is the stdlib reverse proxy that handles the forwarding, including
	// the hop-by-hop rules a hand-rolled forwarder gets wrong.
	rp *httputil.ReverseProxy

	// markMu serializes writes to out. The handler runs concurrently, so two
	// requests failing at once must not interleave bytes in a writer that is
	// not itself concurrency-safe (a bytes.Buffer, for example).
	markMu sync.Mutex

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
//
// out receives one-line markers from request goroutines that run concurrently.
// The proxy serializes its own writes to out under an internal lock, so a
// caller may hand over a writer that is not itself safe for concurrent use (a
// bytes.Buffer, for example). If the caller also writes to out from outside,
// the caller owns that synchronization.
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
	// NewSingleHostReverseProxy rewrites the outbound URL's scheme, host
	// and path but deliberately leaves req.Host alone, so the caller's Host
	// header would reach Mem0. A self-hosted Mem0 routed by virtual host
	// would then misroute (or refuse) the request, so the director sets it
	// to the upstream's host. The reverse proxy also matches on the
	// outbound URL, not req.Host, so this changes what the upstream sees
	// and nothing about how the request is dialed. The empty-host case is
	// left as it was: there is no upstream host to name.
	inner := p.rp.Director
	p.rp.Director = func(req *http.Request) {
		inner(req)
		if upstream.Host != "" {
			req.Host = upstream.Host
		}
	}
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

	o, decodeReason := p.buildObservation(r, kind, buf, at)
	if o == nil {
		if decodeReason != "" {
			p.mark("notary: proxy: %s; forwarded unrecorded: method=%s path=%s\n",
				decodeReason, r.Method, r.URL.Path)
		}
		p.rp.ServeHTTP(w, r)
		return
	}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, o)))
}

// buildObservation decodes the request body and assembles the observation
// context. It returns a non-nil obsRequest on success. When it returns nil the
// request is forwarded without a record, and the returned reason distinguishes
// the two cases: a non-empty reason means the body could not be read -- a
// decode failure, which the caller marks loud -- while an empty reason means
// the body decoded but carries no scope, a documented boundary that is not a
// read failure (design section 10). The scope is sourced from the body because
// the proxy has no construction-time scope; an add carries it at the top level
// and a search inside filters (internal/mem0/types.go).
func (p *ProxyInterceptor) buildObservation(r *http.Request, kind observedKind, body []byte, at time.Time) (*obsRequest, string) {
	switch kind {
	case kindAdd:
		var req mem0.AddRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, "request body is not valid JSON"
		}
		scope := record.Scope{UserID: req.UserID, AgentID: req.AgentID, AppID: req.AppID, RunID: req.RunID}
		if scopeEmpty(scope) {
			return nil, ""
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
		}, ""
	case kindSearch:
		var req mem0.SearchRequest
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, "request body is not valid JSON"
		}
		scope := record.Scope{
			UserID:  req.Filters.UserID,
			AgentID: req.Filters.AgentID,
			AppID:   req.Filters.AppID,
			RunID:   req.Filters.RunID,
		}
		if scopeEmpty(scope) {
			return nil, ""
		}
		return &obsRequest{
			kind:      kindSearch,
			at:        at,
			corrID:    p.correlationID(r, scope, body),
			scope:     scope,
			searchReq: req,
		}, ""
	default:
		return nil, ""
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
// audit problem into the caller's problem. A failure that means "we could not
// read this" is marked loud rather than left to look like "there was nothing
// to record".
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
	// exactly as library.Add and library.Search write no record on error. This
	// is checked before decoding so a failure response never yields a marker.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil
	}

	// Decode the OBSERVATION COPY only. resp.Body was already restored to the
	// upstream's exact bytes above, so the caller's response is untouched:
	// gzip bytes still reach a client that asked for gzip. Go's transport
	// decompresses transparently only when IT added the Accept-Encoding header;
	// when the caller set it -- Python requests and Node fetch both do by
	// default -- the body reaches us still compressed, so it must be decoded
	// here or the record would be lost in silence.
	payload, decodeReason := decodeResponseBody(body, resp.Header.Get("Content-Encoding"), p.maxBody)
	if decodeReason != "" {
		p.mark("notary: proxy: %s; not recorded: method=%s path=%s\n",
			decodeReason, resp.Request.Method, resp.Request.URL.Path)
		return nil
	}
	if p.obs == nil {
		return nil
	}

	switch o.kind {
	case kindAdd:
		var ar mem0.AddResponse
		if err := json.Unmarshal(payload, &ar); err != nil {
			p.mark("notary: proxy: add response is not valid JSON; not recorded: method=%s path=%s\n",
				resp.Request.Method, resp.Request.URL.Path)
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
		if err := json.Unmarshal(payload, &sr); err != nil {
			p.mark("notary: proxy: search response is not valid JSON; not recorded: method=%s path=%s\n",
				resp.Request.Method, resp.Request.URL.Path)
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

// decodeResponseBody returns the bytes to OBSERVE for a response whose
// Content-Encoding is encoding, decoding only the observation copy; the
// caller's response bytes are never affected. It returns decoded with an empty
// reason on success. On failure it returns a non-empty reason -- the response
// is compressed in a way this proxy will not or cannot decode -- so the caller
// can mark the loss loudly instead of recording nothing in silence.
//
// gzip is decoded, because it is the encoding a caller that sets its own
// Accept-Encoding asks for in practice, and Go's transport will not have
// transparently decompressed the body in that case. "identity" and an absent
// encoding are returned unchanged. Any other encoding (br, zstd, deflate) is
// not decoded: stdlib has no decoder for br or zstd, and declining is safer
// than guessing. The decoded copy is bounded by maxBody, so a small compressed
// response cannot expand without limit.
func decodeResponseBody(body []byte, encoding string, maxBody int64) ([]byte, string) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, ""
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, "gzip response could not be decoded"
		}
		defer func() { _ = zr.Close() }()
		decoded, err := io.ReadAll(io.LimitReader(zr, maxBody+1))
		if err != nil {
			return nil, "gzip response could not be read"
		}
		if int64(len(decoded)) > maxBody {
			return nil, fmt.Sprintf("decoded response body exceeds %d bytes", maxBody)
		}
		return decoded, ""
	default:
		// %q escapes the header value, so a hostile upstream cannot inject a
		// newline (or other control byte) into the marker line.
		return nil, fmt.Sprintf("response Content-Encoding %q is not decoded", encoding)
	}
}

// handleError answers a caller whose request could not reach the upstream. It
// writes a real HTTP error, records nothing, and marks the failure on the loud
// channel with the REASON it failed: a DNS failure, a refused connection, a TLS
// error and a timeout are otherwise indistinguishable to whoever runs this
// proxy. The reason is rendered through redactError, which strips the transport
// error's outbound request URL down to its scheme and host first -- that URL's
// path and query come from the caller and can carry a credential (the
// query-string sentinel), and the proxy holds no credential of its own to
// justify echoing one. The reason text is the transport's own cause, which
// names the failure and not the caller's headers.
func (p *ProxyInterceptor) handleError(w http.ResponseWriter, r *http.Request, err error) {
	p.mark("notary: proxy: upstream request failed: method=%s path=%s reason=%s\n",
		r.Method, r.URL.Path, redactError(err))
	w.WriteHeader(http.StatusBadGateway)
}

// redactError renders err for the operator with no credential-bearing URL. A
// transport error is a *url.Error whose message is `Op "URL": cause`, where URL
// is the OUTBOUND request URL -- the caller's path and query joined onto the
// upstream -- so it is rebuilt over RedactedURL before rendering, keeping
// errors.Is/As and the Timeout method working while removing the leak. This
// mirrors internal/mem0/client.go's sanitizeTransportError, which gives a Mem0
// call error the same treatment. Every other error is rendered as-is.
func redactError(err error) string {
	if err == nil {
		return "unknown error"
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		err = &url.Error{Op: uerr.Op, URL: RedactedURL(uerr.URL), Err: uerr.Err}
	}
	return err.Error()
}

// RedactedURL renders raw for an error message or an operator banner as just
// its scheme and host -- never its path, query, userinfo or fragment, the
// places an operator-supplied credential can hide (an operator may put a key in
// the base URL's query string, and net/http echoes the request URL in a
// transport error). It mirrors internal/mem0/client.go's safeURL, so the
// proxy's printable paths and that package's errors render a URL the same way.
//
// A value that does not parse, has no host, or is opaque (url.Parse keeps
// everything after a non-slash scheme in u.Opaque and never splits or strips
// it) is rendered as the constant "[url omitted]": there is nothing safe to
// show, and no legitimate http(s) endpoint has that shape.
func RedactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "[url omitted]"
	}
	return u.Scheme + "://" + u.Host
}

// mark writes a one-line marker to out, best-effort. A nil out discards it. Its
// writes are serialized under markMu, so concurrent failures cannot interleave
// their bytes in a writer that is not itself concurrency-safe. Markers name
// only the request method and path -- never a header value, a query string or a
// body -- so no credential can reach the operator through this path.
func (p *ProxyInterceptor) mark(format string, args ...any) {
	if p.out == nil {
		return
	}
	p.markMu.Lock()
	defer p.markMu.Unlock()
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
