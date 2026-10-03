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
	"fmt"
	"io"
	"net/http"
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
// memory by streaming unboundedly. It is far above any legitimate completion; a
// body that exceeds it surfaces as a wrapped error naming the call, rather than
// being silently truncated and decoded.
const maxResponseBytes = 8 << 20 // 8 MiB

// chatPath is the compatible chat-completions endpoint. It is the only path the
// client ever requests.
const chatPath = "/chat/completions"

// Client is a thin HTTP client for an OpenAI-compatible chat-completions API.
//
// It holds the API key in an unexported field and never renders it: String,
// GoString, Format, and MarshalJSON all emit a redacted placeholder, so no fmt
// verb (%v, %+v, %s, %q, %x, %#v) and no json.Marshal call can reveal the key.
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
// including %v, %+v, %s, %q, %x and %#v. Because Format has a value receiver,
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

// HTTPError reports a non-2xx response from the provider. It carries the status
// code and the raw response body, because the body is the provider's own
// explanation and is evidence, not a structured envelope. The API key is never
// part of the error.
type HTTPError struct {
	StatusCode int
	Body       []byte
}

// Error implements error. It prefers the "error" field when the body is a JSON
// object with one, and otherwise falls back to the raw body text, so a non-JSON
// error body is still surfaced rather than lost.
func (e *HTTPError) Error() string {
	msg := strings.TrimSpace(string(e.Body))
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(e.Body, &payload); err == nil && payload.Error != "" {
		msg = payload.Error
	}
	if msg == "" {
		return fmt.Sprintf("phrase: unexpected status %d", e.StatusCode)
	}
	return fmt.Sprintf("phrase: unexpected status %d: %s", e.StatusCode, msg)
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
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("phrase: encoding %s %s request: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("phrase: building %s %s request: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("phrase: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Read one byte past the cap so an over-size body is detected rather than
	// silently truncated (a truncated body could still be valid JSON).
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("phrase: reading %s %s response: %w", method, path, err)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("phrase: %s %s response exceeds %d-byte limit", method, path, maxResponseBytes)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("phrase: %s %s: %w", method, path, &HTTPError{StatusCode: resp.StatusCode, Body: raw})
	}

	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("phrase: decoding %s %s response: %w", method, path, err)
	}
	return nil
}
