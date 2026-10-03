package mem0

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// clientRedacted is what every printable path of Client renders, so the API
// key can never be recovered from a log line, a %#v dump, or a JSON encoding.
const clientRedacted = "[redacted]"

// defaultTimeout bounds a single Mem0 call when the caller supplies no client.
const defaultTimeout = 30 * time.Second

// maxResponseBytes bounds how much of a response body the client will read, so
// that a misbehaving, broken-proxy, or hostile endpoint cannot exhaust the
// client's memory by streaming unboundedly. It is far above any legitimate
// Mem0 response. A SUCCESS body that exceeds it surfaces as a wrapped error
// naming the call, rather than being silently truncated and decoded; a non-2xx
// response is reported by its STATUS regardless of body size (see do).
const maxResponseBytes = 8 << 20 // 8 MiB

// maxErrorBodyBytes caps how much of a non-2xx response body is stored in
// HTTPError and echoed in its message. An error body is the provider's own
// explanation and is small in practice; capping keeps an oversized or hostile
// error body from becoming a multi-megabyte error string, while the status --
// the part an operator needs to see -- is always preserved (see do).
const maxErrorBodyBytes = 8 << 10 // 8 KiB

// Client is a thin REST client for the hosted Mem0 API.
//
// It holds the API key in an unexported field and never renders it: String,
// GoString, Format, and MarshalJSON all emit a redacted placeholder, so no fmt
// verb (%v, %+v, %s, %q, %x, %#v) and no json.Marshal call can reveal the key.
type Client struct {
	baseURL string
	apiKey  string
	hc      *http.Client
}

// String implements fmt.Stringer, redacting the API key.
func (c Client) String() string { return clientRedacted }

// GoString implements fmt.GoStringer, redacting the API key under %#v.
func (c Client) GoString() string { return clientRedacted }

// Format implements fmt.Formatter, redacting the API key under every verb,
// including %v, %+v, %s, %q, %x and %#v. Because Format has a value receiver,
// both Client and *Client satisfy fmt.Formatter, and fmt consults it before
// the Stringer/GoStringer interfaces, so no verb can reach the struct's
// fields.
func (c Client) Format(f fmt.State, _ rune) { io.WriteString(f, clientRedacted) }

// MarshalJSON implements json.Marshaler, redacting the API key.
func (c Client) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// NewClient returns a Client for the Mem0 API at baseURL, authenticating with
// apiKey. A nil hc uses a default http.Client with a 30s timeout. A trailing
// slash on baseURL is trimmed so joining it with an endpoint path cannot
// produce a double slash.
func NewClient(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		hc:      hc,
	}
}

// safeURL renders raw for an error message as just its scheme and host. The
// client never prints a configured base URL verbatim: an operator may place a
// credential in NOTARY_MEM0_BASE_URL's query string (the ?key= form), its
// userinfo, or even its path, and net/http echoes the request URL in a transport
// error, so a verbatim print would leak it. Dropping the path, query, userinfo
// and fragment removes every place a credential can hide while keeping the part
// that identifies the endpoint. (The Mem0 API key itself travels in the
// Authorization header, so a leak here needs an operator-supplied credential in
// the base URL -- but that is exactly the config this package must not echo.)
//
// A URL with no host, or an opaque one -- in which url.Parse keeps everything
// after the scheme in u.Opaque and never splits or strips it -- is rendered as a
// constant rather than reconstructed: there is nothing safe to show, and no
// legitimate http(s) endpoint has that shape.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "[url omitted]"
	}
	return u.Scheme + "://" + u.Host
}

// sanitizeTransportError returns err with any *url.Error's URL stripped by
// safeURL. net/http builds a *url.Error carrying the request URL for a dial,
// DNS or timeout failure AND for the parse error a malformed base URL produces;
// its stripPassword removes only the userinfo password, so a query-string
// credential survives into the printed message. Rebuilding the *url.Error over a
// safe URL keeps errors.Is/As and the Timeout method working while removing the
// leak.
func sanitizeTransportError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return &url.Error{Op: uerr.Op, URL: safeURL(uerr.URL), Err: uerr.Err}
	}
	return err
}

// credentialComponents returns every substring of the client's configuration
// that must never appear in an error: the API key, and every part of the base
// URL an operator could have hidden a credential in -- its userinfo (username
// and password), its query string, and its path. Which query parameter is a
// credential cannot be known, so the whole query is treated as one. The path is
// included even though credentials rarely live there, because the invariant is
// "no error this package returns can contain a credential, whatever the operator
// configured".
//
// The result is ordered longest-first so a shorter component cannot partially
// mask a longer one when they overlap.
func (c *Client) credentialComponents() []string {
	var out []string
	add := func(s string) {
		if s != "" {
			out = append(out, s)
		}
	}
	add(c.apiKey)
	if u, err := url.Parse(c.baseURL); err == nil {
		if u.User != nil {
			add(u.User.Username())
			if pw, ok := u.User.Password(); ok {
				add(pw)
			}
		}
		add(u.RawQuery)
		add(u.Path)
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

// redact replaces every credential component in body with the placeholder, so
// Mem0 or a gateway that reflects the request -- the Authorization header or the
// request URI, with its query string, included -- in an error body cannot carry
// a credential into HTTPError's message or its exported Body field. It is
// applied before the body is stored, so the field is already clean.
func redact(body []byte, components []string) []byte {
	out := body
	for _, s := range components {
		out = bytes.ReplaceAll(out, []byte(s), []byte(clientRedacted))
	}
	return out
}

// capErrorBody truncates b to maxErrorBodyBytes. It bounds the bytes stored in
// an HTTPError so an oversized error body cannot become a multi-megabyte error
// string; the status is what carries the essential information and is never
// affected by this.
func capErrorBody(b []byte) []byte {
	if len(b) > maxErrorBodyBytes {
		return b[:maxErrorBodyBytes]
	}
	return b
}

// HTTPError reports a non-2xx response from Mem0. It carries the status code
// and the provider's response body, because Mem0's error bodies are Python-repr
// strings whose exact text is evidence, not a structured envelope.
//
// The body is scrubbed of every configured credential -- the API key and the
// base URL's userinfo, query and path -- before it is stored, so BOTH this
// error's message and its exported Body field are credential-free, even when
// Mem0 or a gateway reflects the request back. Body is the provider's bytes with
// each credential replaced by "[redacted]" and truncated to maxErrorBodyBytes;
// it is not the raw response.
type HTTPError struct {
	StatusCode int
	Body       []byte
}

// Error implements error. It surfaces the provider's own message from the body
// in either shape seen in the wild -- {"error":"..."} (a string) or the
// OpenAI-shaped object {"error":{"message":"..."}} -- and otherwise falls back
// to the body text, so a non-JSON (e.g. Python-repr) error body is still
// surfaced rather than lost.
func (e *HTTPError) Error() string {
	msg := strings.TrimSpace(string(e.Body))
	if m := errorMessage(e.Body); m != "" {
		msg = m
	}
	if msg == "" {
		return fmt.Sprintf("mem0: unexpected status %d", e.StatusCode)
	}
	return fmt.Sprintf("mem0: unexpected status %d: %s", e.StatusCode, msg)
}

// errorMessage extracts a provider's human message from a JSON error body in
// either the string or the object form. It returns "" when neither is present,
// so the caller falls back to the body text (a non-JSON or message-less body is
// still shown rather than swallowed).
func errorMessage(body []byte) string {
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || len(payload.Error) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(payload.Error, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(payload.Error, &obj); err == nil {
		return strings.TrimSpace(obj.Message)
	}
	return ""
}

// Add submits messages to POST /v3/memories/add/. Mem0 acknowledges
// asynchronously: the response reports an event id and a PENDING status, and
// the outcome must be fetched later with EventStatus.
func (c *Client) Add(ctx context.Context, req AddRequest) (AddResponse, error) {
	var out AddResponse
	if err := c.do(ctx, http.MethodPost, "/v3/memories/add/", req, &out); err != nil {
		return AddResponse{}, err
	}
	return out, nil
}

// Search runs POST /v3/memories/search/ and returns the ranked results. The
// entity scope must be set on req.Filters; Mem0 rejects a top-level entity id
// with HTTP 400.
func (c *Client) Search(ctx context.Context, req SearchRequest) (SearchResponse, error) {
	var out SearchResponse
	if err := c.do(ctx, http.MethodPost, "/v3/memories/search/", req, &out); err != nil {
		return SearchResponse{}, err
	}
	return out, nil
}

// GetAll runs POST /v3/memories/ and returns one page of memories. As with
// Search, the entity scope belongs in req.Filters. A non-zero req.Page or
// req.PageSize is sent as the page/page_size URL query parameters; when both
// are zero the request carries no query string and the platform defaults
// apply. This still returns a single page — GetAllComplete is what walks them.
func (c *Client) GetAll(ctx context.Context, req GetAllRequest) (GetAllResponse, error) {
	var out GetAllResponse
	if err := c.do(ctx, http.MethodPost, getAllPath(req), req, &out); err != nil {
		return GetAllResponse{}, err
	}
	return out, nil
}

// getAllPath builds the POST /v3/memories/ request path, appending page and
// page_size as query parameters when they are set. Mem0 takes pagination from
// the URL, not the JSON body, so the body must never carry them (see
// GetAllRequest). A zero value omits the parameter rather than sending 0,
// which is below page's documented minimum of 1.
func getAllPath(req GetAllRequest) string {
	q := url.Values{}
	if req.Page > 0 {
		q.Set("page", strconv.Itoa(req.Page))
	}
	if req.PageSize > 0 {
		q.Set("page_size", strconv.Itoa(req.PageSize))
	}
	if len(q) == 0 {
		return "/v3/memories/"
	}
	return "/v3/memories/?" + q.Encode()
}

// History runs GET /v1/memories/{id}/history/ and returns the memory's change
// log.
func (c *Client) History(ctx context.Context, memoryID string) (HistoryResponse, error) {
	var out HistoryResponse
	path := "/v1/memories/" + url.PathEscape(memoryID) + "/history/"
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// EventStatus runs GET /v1/event/{id}/ and returns the asynchronous event
// record for eventID.
func (c *Client) EventStatus(ctx context.Context, eventID string) (EventStatusResponse, error) {
	var out EventStatusResponse
	path := "/v1/event/" + url.PathEscape(eventID) + "/"
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return EventStatusResponse{}, err
	}
	return out, nil
}

// Delete runs DELETE /v1/memories/{id}/. Only success or failure is reported;
// the response body carries just a message and is deliberately discarded.
func (c *Client) Delete(ctx context.Context, memoryID string) error {
	path := "/v1/memories/" + url.PathEscape(memoryID) + "/"
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// do performs one HTTP round trip against path (appended to the base URL),
// sending body as JSON when non-nil and decoding a 2xx response into out when
// non-nil. It sets the Authorization header on every request, honours ctx for
// cancellation, and never retries or follows up with any other endpoint.
//
// Its errors are credential-free by construction: the path in the message is the
// fixed API endpoint, transport errors are rebuilt over a safe URL, and a
// non-2xx body is scrubbed of every configured credential and capped before it
// is stored. The request itself still carries the base URL and the Authorization
// header -- only the messages and the stored body are scrubbed.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("mem0: encoding %s %s request: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("mem0: building %s %s request: %w", method, path, sanitizeTransportError(err))
	}
	req.Header.Set("Authorization", "Token "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("mem0: %s %s: %w", method, path, sanitizeTransportError(err))
	}
	defer resp.Body.Close()

	// Read one byte past the cap so an over-size body is detected rather than
	// silently truncated (a truncated body could still be valid JSON).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("mem0: reading %s %s response: %w", method, path, err)
	}

	// Status BEFORE size. A non-2xx is reported by its status, which is known
	// without reference to the body, even when the body is oversized -- the
	// status is what an operator needs on the write path, and a masked
	// 401/403/429/5xx is the failure this ordering exists to prevent. The stored
	// body is scrubbed of every configured credential and capped, never
	// discarded.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("mem0: %s %s: %w", method, path,
			&HTTPError{StatusCode: resp.StatusCode, Body: capErrorBody(redact(raw, c.credentialComponents()))})
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("mem0: %s %s response exceeds %d-byte limit", method, path, maxResponseBytes)
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mem0: decoding %s %s response: %w", method, path, err)
	}
	return nil
}
