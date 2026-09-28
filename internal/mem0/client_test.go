package mem0_test

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/mem0"
)

// mustFixture reads a recorded response from testdata. The files are served
// verbatim so every assertion is against the bytes Mem0 actually returned,
// not against values this test invented.
func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

// captured records what a test server received. It is mutex-guarded because
// the handler runs on its own goroutine.
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
// status and the given body.
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

func TestAddPostsAndDecodes(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "add_response.json"))

	// A trailing slash on baseURL must be trimmed, so the path is single-slashed.
	c := mem0.NewClient(srv.URL+"/", "test-key", nil)
	infer := true
	resp, err := c.Add(context.Background(), mem0.AddRequest{
		Messages: []mem0.Message{{Role: "user", Content: "Notary fixture probe"}},
		UserID:   "notary-fixture-user-a1b2c3",
		Infer:    &infer,
	})
	require.NoError(t, err)

	method, path, header, body := cap.snapshot()
	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "/v3/memories/add/", path)
	assert.Equal(t, "Token test-key", header.Get("Authorization"))
	assert.Equal(t, "application/json", header.Get("Content-Type"))

	var sent map[string]any
	require.NoError(t, json.Unmarshal(body, &sent))
	assert.Equal(t, "notary-fixture-user-a1b2c3", sent["user_id"])
	assert.Equal(t, true, sent["infer"])
	msgs, ok := sent["messages"].([]any)
	require.True(t, ok)
	require.Len(t, msgs, 1)
	assert.Equal(t, "user", msgs[0].(map[string]any)["role"])

	assert.Equal(t, "6e2b49d1-7c73-4947-bc9b-675224e43ddb", resp.EventID)
	assert.Equal(t, "PENDING", resp.Status)
}

func TestSearchNestsEntityIDInFilters(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "search_response.json"))

	c := mem0.NewClient(srv.URL, "test-key", nil)
	resp, err := c.Search(context.Background(), mem0.SearchRequest{
		Query:     "office printer",
		Filters:   mem0.Filters{UserID: "notary-fixture-user-a1b2c3"},
		TopK:      5,
		Threshold: 0.1,
		Rerank:    true,
	})
	require.NoError(t, err)

	method, path, _, body := cap.snapshot()
	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "/v3/memories/search/", path)

	// The entity id must be nested under "filters" and nowhere else: a
	// top-level entity id is a 400 from Mem0.
	var sent map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &sent))
	require.Contains(t, sent, "filters")
	assert.NotContains(t, sent, "user_id")
	assert.NotContains(t, sent, "agent_id")
	assert.NotContains(t, sent, "app_id")
	assert.NotContains(t, sent, "run_id")

	var filters map[string]string
	require.NoError(t, json.Unmarshal(sent["filters"], &filters))
	assert.Equal(t, "notary-fixture-user-a1b2c3", filters["user_id"])

	require.Len(t, resp.Results, 1)
	r := resp.Results[0]
	assert.Equal(t, "ca4535c6-4847-4a43-8d65-f4871b7f5d99", r.ID)
	// r.Memory is the embedded struct (the type and its field share a name);
	// the memory text itself is r.Memory.Memory.
	assert.Equal(t, "User's shared office printer is located on floor 3", r.Memory.Memory)
	assert.InDelta(t, 0.8728, r.Score, 1e-9)
	assert.InDelta(t, 0.7323, r.ScoreBreakdown.Semantic, 1e-9)
	assert.InDelta(t, 0.0432, r.ScoreBreakdown.BM25, 1e-9)
	assert.InDelta(t, 0.0, r.ScoreBreakdown.Entity, 1e-9)
	// search emits agent_id/app_id/run_id as explicit null.
	assert.Nil(t, r.AgentID)
	assert.Nil(t, r.AppID)
	assert.Nil(t, r.RunID)
}

func TestGetAllDecodesEnvelopeWithAbsentIDs(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "get_all_response.json"))

	c := mem0.NewClient(srv.URL, "test-key", nil)
	resp, err := c.GetAll(context.Background(), mem0.GetAllRequest{
		Filters: mem0.Filters{UserID: "notary-fixture-user-a1b2c3"},
	})
	require.NoError(t, err)

	method, path, _, body := cap.snapshot()
	assert.Equal(t, http.MethodPost, method)
	assert.Equal(t, "/v3/memories/", path)

	var sent map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &sent))
	require.Contains(t, sent, "filters")
	assert.NotContains(t, sent, "user_id")

	assert.Equal(t, 1, resp.Count)
	assert.Nil(t, resp.Next)
	assert.Nil(t, resp.Previous)
	require.Len(t, resp.Results, 1)
	m := resp.Results[0]
	assert.Equal(t, "ca4535c6-4847-4a43-8d65-f4871b7f5d99", m.ID)
	// get_all omits the entity ids entirely: they must decode as nil, not error.
	assert.Nil(t, m.AgentID)
	assert.Nil(t, m.AppID)
	assert.Nil(t, m.RunID)
	// metadata is null here and {} in search; both must decode.
	assert.Nil(t, m.Metadata)
	// get_all-only fields.
	assert.Nil(t, m.ReplacedBy)
	assert.False(t, m.Synthesized)
	assert.Equal(t, "monday", m.StructuredAttributes["day_of_week"])
}

func TestHistoryDecodesAddEntry(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "history_response.json"))

	c := mem0.NewClient(srv.URL, "test-key", nil)
	const id = "ca4535c6-4847-4a43-8d65-f4871b7f5d99"
	resp, err := c.History(context.Background(), id)
	require.NoError(t, err)

	method, path, _, _ := cap.snapshot()
	assert.Equal(t, http.MethodGet, method)
	assert.Equal(t, "/v1/memories/"+id+"/history/", path)

	require.Len(t, resp, 1)
	ev := resp[0]
	assert.Equal(t, "ADD", ev.Event)
	assert.Nil(t, ev.OldMemory)
	require.NotNil(t, ev.NewMemory)
	assert.Equal(t, "User's shared office printer is located on floor 3", *ev.NewMemory)
	assert.Equal(t, id, ev.MemoryID)
}

func TestEventStatusDecodesFullRecord(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "event_status_response.json"))

	c := mem0.NewClient(srv.URL, "test-key", nil)
	const id = "6e2b49d1-7c73-4947-bc9b-675224e43ddb"
	resp, err := c.EventStatus(context.Background(), id)
	require.NoError(t, err)

	method, path, _, _ := cap.snapshot()
	assert.Equal(t, http.MethodGet, method)
	assert.Equal(t, "/v1/event/"+id+"/", path)

	assert.Equal(t, id, resp.ID)
	assert.Equal(t, "ADD", resp.EventType)
	assert.Equal(t, "SUCCEEDED", resp.Status)
	assert.Equal(t, "MEM0", resp.Source)
	assert.Greater(t, resp.Latency, 0.0)
	assert.Nil(t, resp.GraphStatus)
	assert.Nil(t, resp.Error)
	require.Len(t, resp.Results, 1)
	assert.Equal(t, "ADD", resp.Results[0].Event)
	assert.Equal(t, "ca4535c6-4847-4a43-8d65-f4871b7f5d99", resp.Results[0].ID)
	assert.NotEmpty(t, resp.Payload)
}

func TestDeleteIssuesDeleteAndAcceptsMessageBody(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusOK, mustFixture(t, "delete_response.json"))

	c := mem0.NewClient(srv.URL, "test-key", nil)
	const id = "ca4535c6-4847-4a43-8d65-f4871b7f5d99"
	require.NoError(t, c.Delete(context.Background(), id))

	method, path, header, _ := cap.snapshot()
	assert.Equal(t, http.MethodDelete, method)
	assert.Equal(t, "/v1/memories/"+id+"/", path)
	assert.Equal(t, "Token test-key", header.Get("Authorization"))
}

func TestErrorSurfacesStatusAndBody(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusBadRequest, mustFixture(t, "add_error_400.json"))

	c := mem0.NewClient(srv.URL, "test-key", nil)
	_, err := c.Add(context.Background(), mem0.AddRequest{Messages: []mem0.Message{}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "400")
	assert.Contains(t, err.Error(), "This list may not be empty")

	var httpErr *mem0.HTTPError
	require.ErrorAs(t, err, &httpErr)
	assert.Equal(t, http.StatusBadRequest, httpErr.StatusCode)
	assert.Equal(t, mustFixture(t, "add_error_400.json"), httpErr.Body)
}

func TestServerErrorIsAnErrorNotAPanic(t *testing.T) {
	cap := &captured{}
	srv := serve(t, cap, http.StatusInternalServerError, []byte(`{"error":"boom"}`))

	c := mem0.NewClient(srv.URL, "test-key", nil)
	var err error
	require.NotPanics(t, func() {
		_, err = c.Search(context.Background(), mem0.SearchRequest{Query: "x"})
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestNoImplicitPingRequest(t *testing.T) {
	fixtures := map[string][]byte{
		"/v3/memories/add/":       mustFixture(t, "add_response.json"),
		"/v3/memories/search/":    mustFixture(t, "search_response.json"),
		"/v3/memories/":           mustFixture(t, "get_all_response.json"),
		"/v1/memories/x/history/": mustFixture(t, "history_response.json"),
		"/v1/event/x/":            mustFixture(t, "event_status_response.json"),
		"/v1/memories/x/":         mustFixture(t, "delete_response.json"),
	}

	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixtures[r.URL.Path])
	}))
	t.Cleanup(srv.Close)

	c := mem0.NewClient(srv.URL, "test-key", nil)
	ctx := context.Background()
	_, _ = c.Add(ctx, mem0.AddRequest{Messages: []mem0.Message{{Role: "user", Content: "x"}}})
	_, _ = c.Search(ctx, mem0.SearchRequest{Query: "x"})
	_, _ = c.GetAll(ctx, mem0.GetAllRequest{})
	_, _ = c.History(ctx, "x")
	_, _ = c.EventStatus(ctx, "x")
	_ = c.Delete(ctx, "x")

	mu.Lock()
	defer mu.Unlock()
	assert.Len(t, paths, 6)
	for _, p := range paths {
		assert.NotEqual(t, "/v1/ping/", p, "the client must make no implicit ping request")
	}
}

func TestClientNeverRendersAPIKey(t *testing.T) {
	const key = "super-secret-mem0-key-9f3c1b"
	c := mem0.NewClient("https://api.mem0.ai/", key, nil)

	// Build the format calls with a runtime format string so the deliberate
	// %s/%q/%x against a struct do not trip the printf vet check (this mirrors
	// the canary in internal/sign).
	verbs := []string{"v", "+v", "#v", "s", "q", "x", "X"}
	for _, verb := range verbs {
		format := "%" + verb
		got := fmt.Sprintf(format, c)
		assert.Equalf(t, "[redacted]", got, "%%%s of Client must render the redaction marker", verb)
		assert.NotContainsf(t, got, key, "%%%s of Client leaked the API key", verb)
	}

	encoded, err := json.Marshal(c)
	require.NoError(t, err)
	assert.Equal(t, `"[redacted]"`, string(encoded))
	assert.NotContains(t, string(encoded), key)
}
