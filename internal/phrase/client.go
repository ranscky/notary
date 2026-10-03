// Package phrase is Notary's optional language-model pass: it restates a run of
// records' structured claims as one sentence of prose. It is the only package in
// Notary that talks to an LLM, and its only allowed importers are internal/export
// and cmd/notary, so no decision package can ever name the generated type
// (design D7; the design's §4.1 records the direct-import limit).
//
// The client speaks the OpenAI-compatible chat-completions schema, which the
// major providers expose, so no provider SDK is added (design D1). A request
// carries ONLY model and messages and posts to /chat/completions (design D2):
// this is not stylistic. A richer request would appear to work against a
// provider whose compatible layer silently ignores unknown fields, while
// quietly dropping the extra intent -- a minimal request is the only one every
// provider honours as written.
//
// The prompt carries the structured claim -- event, tier, reason kind, scope,
// memory id and content hash -- and never the memory text, so the pass cannot
// leak what redaction exists to hide (design §8).
package phrase

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"notary/internal/record"
)

// clientRedacted is what every printable path of Client renders, so the API key
// can never be recovered from a log line, a %#v dump, or a JSON encoding.
const clientRedacted = "[redacted]"

// defaultTimeout bounds a single provider call when the caller supplies no
// client.
const defaultTimeout = 30 * time.Second

// maxResponseBytes bounds how much of a response body the client will read, so
// a misbehaving, broken-proxy, or hostile endpoint cannot exhaust the client's
// memory by streaming unboundedly. It is far above any legitimate completion. A
// SUCCESS body that exceeds it surfaces as a wrapped error naming the call,
// rather than being silently truncated and decoded; an ERROR status is reported
// by its status regardless of body size (see do).
const maxResponseBytes = 8 << 20 // 8 MiB

// chatPath is the compatible chat-completions endpoint. It is the only path the
// client ever requests.
const chatPath = "/chat/completions"

// Client is a thin HTTP client for an OpenAI-compatible chat-completions API.
//
// It holds the API key in an unexported field and never renders it: String,
// GoString, Format, and MarshalJSON all emit a redacted placeholder, so no fmt
// verb (%v, %+v, %s, %q, %x, %X, %#v) and no json.Marshal call can reveal the key.
type Client struct {
	baseURL string
	apiKey  string
	model   string
	hc      *http.Client
}

// String implements fmt.Stringer, redacting the API key.
func (c Client) String() string { return clientRedacted }

// GoString implements fmt.GoStringer, redacting the API key under %#v.
func (c Client) GoString() string { return clientRedacted }

// Format implements fmt.Formatter, redacting the API key under every verb,
// including %v, %+v, %s, %q, %x, %X and %#v. Because Format has a value receiver,
// both Client and *Client satisfy fmt.Formatter, and fmt consults it before the
// Stringer/GoStringer interfaces, so no verb can reach the struct's fields.
func (c Client) Format(f fmt.State, _ rune) { io.WriteString(f, clientRedacted) }

// MarshalJSON implements json.Marshaler, redacting the API key.
func (c Client) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// NewClient returns a Client for an OpenAI-compatible API at baseURL,
// authenticating with apiKey and asking for model. A nil hc uses a default
// http.Client with a 30s timeout. A trailing slash on baseURL is trimmed so
// joining it with the endpoint path cannot produce a double slash.
func NewClient(baseURL, apiKey, model string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		hc:      hc,
	}
}

// safeURL renders raw for an error message with its userinfo, query and
// fragment removed. The client never prints a configured base URL verbatim: an
// operator may place a credential in its query string -- the ?key= form several
// compatible providers accept -- and net/http echoes the request URL in a
// transport error, so a verbatim print would leak it. The query is STRIPPED from
// the message rather than rejected at construction because a legitimate endpoint
// may need a non-secret query parameter (Azure OpenAI's api-version, for one);
// rejecting every query would break providers the design promises to support.
func safeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "[url omitted]"
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
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

// redactKey replaces every occurrence of key in body with the redacted
// placeholder, so a provider that reflects the request -- the Authorization
// header included -- in an error body cannot carry the key into HTTPError's
// message. An empty key has nothing to redact.
func redactKey(body []byte, key string) []byte {
	if key == "" {
		return body
	}
	return bytes.ReplaceAll(body, []byte(key), []byte(clientRedacted))
}

// HTTPError reports a non-2xx response from the provider. It carries the status
// code and the raw response body, because the body is the provider's own
// explanation and is evidence, not a structured envelope. The body is passed
// through redactKey before it is stored, so the API key is never part of the
// error even when a provider echoes the request back.
type HTTPError struct {
	StatusCode int
	Body       []byte
}

// Error implements error. It surfaces the provider's own message from the body
// in either shape seen in the wild -- {"error":"..."} (a string) or the OpenAI
// object form {"error":{"message":"..."}} -- and otherwise falls back to the raw
// body text, so a non-JSON error body is still surfaced rather than lost.
func (e *HTTPError) Error() string {
	msg := strings.TrimSpace(string(e.Body))
	if m := errorMessage(e.Body); m != "" {
		msg = m
	}
	if msg == "" {
		return fmt.Sprintf("phrase: unexpected status %d", e.StatusCode)
	}
	return fmt.Sprintf("phrase: unexpected status %d: %s", e.StatusCode, msg)
}

// errorMessage extracts a provider's human message from a JSON error body in
// either the string or the object form. It returns "" when neither is present,
// so the caller falls back to the raw body (a non-JSON or message-less body is
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

// Paraphrase restates the claims in records as one sentence, returning the
// model's text together with the model that produced it and the time it was
// generated.
//
// Each record carries its own tier, so no tier is passed separately. The
// request is a minimal compatible chat-completions POST, and the prompt is
// built from structured metadata alone -- never the content text (see
// buildMessages). An empty request is a programming error: the caller must not
// call the provider with nothing to restate.
func (c *Client) Paraphrase(ctx context.Context, records []record.Record) (Paraphrase, error) {
	if len(records) == 0 {
		return Paraphrase{}, fmt.Errorf("phrase: no records to paraphrase")
	}

	req := chatRequest{Model: c.model, Messages: buildMessages(records)}

	var resp chatResponse
	if err := c.do(ctx, http.MethodPost, chatPath, req, &resp); err != nil {
		return Paraphrase{}, err
	}

	if len(resp.Choices) == 0 {
		return Paraphrase{}, fmt.Errorf("phrase: response carried no choices")
	}
	text := strings.TrimSpace(resp.Choices[0].Message.Content)
	if text == "" {
		return Paraphrase{}, fmt.Errorf("phrase: response carried an empty paraphrase")
	}

	model := resp.Model
	if model == "" {
		model = c.model
	}
	return Paraphrase{Text: text, Model: model, At: time.Now()}, nil
}

// do performs one HTTP round trip against path (appended to the base URL),
// sending body as JSON when non-nil and decoding a 2xx response into out when
// non-nil. It sets the Bearer Authorization header on every request, honours ctx
// for cancellation, and never retries or follows up with any other endpoint: a
// retry would multiply a bill the operator cannot see, and degradation is the
// documented behaviour (design §12).
//
// Every error it returns is credential-free by construction: the base URL is
// rendered through safeURL, transport errors are rebuilt over a safe URL, and an
// error body is passed through redactKey. The request itself still carries the
// base URL's query and the Authorization header -- only the messages are
// scrubbed.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	safe := safeURL(c.baseURL) + path

	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("phrase: encoding %s %s request: %w", method, safe, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("phrase: building %s %s request: %w", method, safe, sanitizeTransportError(err))
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("phrase: %s %s: %w", method, safe, sanitizeTransportError(err))
	}
	defer resp.Body.Close()

	// Read one byte past the cap so an over-size body is detected rather than
	// silently truncated (a truncated body could still be valid JSON).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("phrase: reading %s %s response: %w", method, safe, err)
	}

	// Status BEFORE size: when a provider errors, the status is what an
	// operator needs, and it is known without reference to the body. An
	// over-size SUCCESS body, which cannot be decoded, is still caught below.
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("phrase: %s %s: %w", method, safe,
			&HTTPError{StatusCode: resp.StatusCode, Body: redactKey(raw, c.apiKey)})
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("phrase: %s %s response exceeds %d-byte limit", method, safe, maxResponseBytes)
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("phrase: decoding %s %s response: %w", method, safe, err)
	}
	return nil
}
