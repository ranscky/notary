package phrase_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/phrase"
	"notary/internal/record"
)

// distinctive is seeded into a record's Content.Text and must never appear in a
// request. Its shape -- an unlikely token sequence -- means a substring match
// cannot pass by accident, so TestPromptNeverContainsContentText forces the
// prompt to be built from metadata alone.
const distinctive = "PLUMBUS-QUIXOTE-9137-ZEPHYR"

// mustFixture reads a recorded response from testdata. The bytes are served
// verbatim so every assertion is against what a provider actually returned,
// not against values this test invented.
func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

// captured records what a test server received. It is mutex-guarded because the
// handler runs on its own goroutine.
type captured struct {
	mu     sync.Mutex
	method string
	path   string
	header http.Header
	body   []byte
}

func (c *captured) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.method = r.Method
	c.path = r.URL.Path
	c.header = r.Header.Clone()
	c.body = body
}

func (c *captured) snapshot() (method, path string, header http.Header, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.method, c.path, c.header, c.body
}

// serve starts an httptest server that records the request and replies with
// status and the given body. Nothing here touches the network.
func serve(t *testing.T, cap *captured, status int, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sampleRecord builds a valid record carrying distinctive content text. The
// content text is the thing the prompt must never see; every other field is
// structured metadata the prompt is allowed to use.
func sampleRecord(t *testing.T) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"x":1}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonSearchPerformed, ev)
	require.NoError(t, err)

	var h record.Hash
	for i := range h {
		h[i] = byte(0xA0 + i)
	}
	return record.Record{
		ID:      record.RecordID("rec-1"),
		Event:   record.EventSearchPerformed,
		Reason:  reason,
		Subject: record.Subject{MemoryID: "mem-1", Scope: record.Scope{UserID: "u1"}, ContentHash: h},
		Content: &record.Content{Text: distinctive},
	}
}

func TestRequestPathIsChatCompletions(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "single_choice_response.json"))

	// A trailing slash on baseURL must be trimmed, so the path is single-slashed.
	c := phrase.NewClient(srv.URL+"/", "test-key", "test-model", nil)
	_, err := c.Paraphrase(context.Background(), []record.Record{sampleRecord(t)})
	require.NoError(t, err)

	method, path, _, _ := cap.snapshot()
	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "/chat/completions", path,
		"the compatible schema lives at /chat/completions; every other path is a provider-specific 404")
}

// TestRequestBodyCarriesOnlyModelAndMessages pins design D2. The top-level field
// set is asserted, not one field's presence: a richer request would appear to
// work against a provider that silently ignores unknown fields, while quietly
// dropping the extra intent.
func TestRequestBodyCarriesOnlyModelAndMessages(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "single_choice_response.json"))

	c := phrase.NewClient(srv.URL, "test-key", "test-model", nil)
	_, err := c.Paraphrase(context.Background(), []record.Record{sampleRecord(t)})
	require.NoError(t, err)

	_, _, _, body := cap.snapshot()
	var top map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &top), "request body must be a JSON object")

	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	assert.ElementsMatch(t, []string{"model", "messages"}, keys,
		"the request body must carry exactly model and messages and nothing else (D2)")
}

// TestPromptNeverContainsContentText seeds a record with distinctive text and
// asserts it appears NOWHERE in the request -- not in the body, not in a
// header. This is the whole-request check the design's organising principle
// demands: a paraphrase cannot leak what redaction hides if the payload was
// never sent.
func TestPromptNeverContainsContentText(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "single_choice_response.json"))

	rec := sampleRecord(t)
	require.Equal(t, distinctive, rec.Content.Text, "the seed record must actually carry the text")

	c := phrase.NewClient(srv.URL, "test-key", "test-model", nil)
	_, err := c.Paraphrase(context.Background(), []record.Record{rec})
	require.NoError(t, err)

	_, _, header, body := cap.snapshot()
	assert.NotContains(t, string(body), distinctive,
		"no record content text may appear anywhere in the request body")
	for name, vals := range header {
		for _, v := range vals {
			assert.NotContains(t, v, distinctive,
				"no record content text may appear in header %s", name)
		}
	}
}

// TestParaphraseDecodesRecordedResponse runs the happy path against the real
// captured provider response and asserts the wire shape decodes as designed.
func TestParaphraseDecodesRecordedResponse(t *testing.T) {
	cap := &captured{}
	body := mustFixture(t, "chat_completions_response.json")
	srv := serve(t, cap, http.StatusOK, body)

	c := phrase.NewClient(srv.URL, "test-key", "test-model", nil)
	before := time.Now()
	p, err := c.Paraphrase(context.Background(), []record.Record{sampleRecord(t)})
	require.NoError(t, err)

	// The model reported is the one the provider echoed, or the configured one
	// when it echoed none.
	var f struct {
		Model string `json:"model"`
	}
	require.NoError(t, json.Unmarshal(body, &f))
	wantModel := f.Model
	if wantModel == "" {
		wantModel = "test-model"
	}
	assert.NotEmpty(t, p.Text, "the recorded response carries paraphrase text")
	assert.Equal(t, wantModel, p.Model)
	assert.False(t, p.At.IsZero())
	assert.False(t, p.At.Before(before), "At is stamped at call time, not before it")
	assert.True(t, p.At.Before(time.Now().Add(time.Second)))
}

func TestSingleElementChoicesIsAccepted(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "single_choice_response.json"))

	c := phrase.NewClient(srv.URL, "test-key", "test-model", nil)
	p, err := c.Paraphrase(context.Background(), []record.Record{sampleRecord(t)})
	require.NoError(t, err, "a single-element choices array is a normal, complete response")
	assert.NotEmpty(t, p.Text)
}

func TestUnknownResponseFieldsAreIgnored(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "unknown_fields_response.json"))

	c := phrase.NewClient(srv.URL, "test-key", "test-model", nil)
	p, err := c.Paraphrase(context.Background(), []record.Record{sampleRecord(t)})
	require.NoError(t, err, "unknown fields must be ignored, not refused")
	assert.NotEmpty(t, p.Text)
}

func TestEmptyChoicesIsAnError(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, []byte(`{"model":"m","choices":[]}`))

	c := phrase.NewClient(srv.URL, "test-key", "test-model", nil)
	_, err := c.Paraphrase(context.Background(), []record.Record{sampleRecord(t)})
	require.Error(t, err, "an empty choices array is an error, not an empty paraphrase")
	assert.Contains(t, err.Error(), "choices")
}

func TestEmptyContentIsAnError(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, []byte(`{"choices":[{"message":{"role":"assistant","content":""}}]}`))

	c := phrase.NewClient(srv.URL, "test-key", "test-model", nil)
	_, err := c.Paraphrase(context.Background(), []record.Record{sampleRecord(t)})
	require.Error(t, err, "an empty content string is an error, not an empty paraphrase")
}

func TestNonJSONBodyIsAnError(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, []byte("<html>not json</html>"))

	c := phrase.NewClient(srv.URL, "test-key", "test-model", nil)
	_, err := c.Paraphrase(context.Background(), []record.Record{sampleRecord(t)})
	require.Error(t, err, "a non-JSON body must fail loudly, never yield an empty paraphrase")
}

func TestNon2xxIsAnErrorNamingTheStatus(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusServiceUnavailable, []byte(`{"error":"busy"}`))

	c := phrase.NewClient(srv.URL, "test-key", "test-model", nil)
	_, err := c.Paraphrase(context.Background(), []record.Record{sampleRecord(t)})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503", "the error must name the status")

	var httpErr *phrase.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusServiceUnavailable, httpErr.StatusCode)
}

// TestAuthorizationIsBearer asserts the OpenAI-compatible auth scheme, which is
// Bearer -- a provider will reject any other scheme.
func TestAuthorizationIsBearer(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "single_choice_response.json"))

	c := phrase.NewClient(srv.URL, "test-key", "test-model", nil)
	_, err := c.Paraphrase(context.Background(), []record.Record{sampleRecord(t)})
	require.NoError(t, err)

	_, _, header, _ := cap.snapshot()
	assert.Equal(t, "Bearer test-key", header.Get("Authorization"))
}

// TestKeyNeverRenders verifies the API key cannot be recovered from any fmt verb
// or JSON encoding of the client.
func TestKeyNeverRenders(t *testing.T) {
	c := phrase.NewClient("https://example.test", "sk-secret-value", "test-model", nil)
	for _, s := range []string{
		fmt.Sprintf("%v", c), fmt.Sprintf("%+v", c), fmt.Sprintf("%s", c),
		fmt.Sprintf("%q", c), fmt.Sprintf("%#v", c),
	} {
		assert.NotContains(t, s, "sk-secret-value")
	}
	b, err := json.Marshal(c)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "sk-secret-value")
}

// TestZeroRecordsIsAnError pins the empty-request guard: the client must never
// make a pointless billed call with nothing to restate, so it errors locally
// and sends no request at all.
func TestZeroRecordsIsAnError(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "single_choice_response.json"))

	c := phrase.NewClient(srv.URL, "test-key", "test-model", nil)
	_, err := c.Paraphrase(context.Background(), nil)
	require.Error(t, err, "an empty request is a programming error, not a billed call")
	assert.Contains(t, err.Error(), "no records")

	method, _, _, _ := cap.snapshot()
	assert.Empty(t, method, "no request may be sent when there is nothing to paraphrase")
}

// TestDialFailureIsAnError pins the fourth degradation mode at the client: with
// nothing listening on the endpoint, the call must surface a wrapped error
// naming the call -- not an empty paraphrase. The export layer degrades on such
// an error (TestParaphraseFailureDegradesAndDoesNotFailTheExport); this proves
// the client itself produces one.
func TestDialFailureIsAnError(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "single_choice_response.json"))
	url := srv.URL
	srv.Close() // nothing is listening on url now

	c := phrase.NewClient(url, "test-key", "test-model", nil)
	_, err := c.Paraphrase(context.Background(), []record.Record{sampleRecord(t)})
	require.Error(t, err, "a dial failure must surface as an error, not an empty paraphrase")
	assert.Contains(t, err.Error(), "phrase:", "the error must be wrapped in this package's prefix")
	assert.Contains(t, err.Error(), "/chat/completions", "the error must name the call that failed")
}
