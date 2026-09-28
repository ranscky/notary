package mem0

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
// Mem0 response; a body that exceeds it surfaces as a wrapped error naming the
// call, rather than being silently truncated and decoded.
const maxResponseBytes = 8 << 20 // 8 MiB

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

// HTTPError reports a non-2xx response from Mem0. It carries the status code
// and the raw response body, because Mem0's error bodies are Python-repr
// strings whose exact text is evidence, not a structured envelope. The API key
// is never part of the error.
type HTTPError struct {
	StatusCode int
	Body       []byte
}

// Error implements error. It prefers the "error" field when the body is a JSON
// object with one, and otherwise falls back to the raw body text, so a
// non-JSON error body is still surfaced rather than lost.
func (e *HTTPError) Error() string {
	msg := strings.TrimSpace(string(e.Body))
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(e.Body, &payload); err == nil && payload.Error != "" {
		msg = payload.Error
	}
	if msg == "" {
		return fmt.Sprintf("mem0: unexpected status %d", e.StatusCode)
	}
	return fmt.Sprintf("mem0: unexpected status %d: %s", e.StatusCode, msg)
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
// Search, the entity scope belongs in req.Filters.
func (c *Client) GetAll(ctx context.Context, req GetAllRequest) (GetAllResponse, error) {
	var out GetAllResponse
	if err := c.do(ctx, http.MethodPost, "/v3/memories/", req, &out); err != nil {
		return GetAllResponse{}, err
	}
	return out, nil
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
		return fmt.Errorf("mem0: building %s %s request: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Token "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("mem0: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Read one byte past the cap so an over-size body is detected rather than
	// silently truncated (a truncated body could still be valid JSON).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("mem0: reading %s %s response: %w", method, path, err)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("mem0: %s %s response exceeds %d-byte limit", method, path, maxResponseBytes)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("mem0: %s %s: %w", method, path, &HTTPError{StatusCode: resp.StatusCode, Body: raw})
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("mem0: decoding %s %s response: %w", method, path, err)
	}
	return nil
}
