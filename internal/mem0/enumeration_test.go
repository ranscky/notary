package mem0_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/mem0"
)

// memPage is the JSON body a paginated get_all server replies with. next is a
// ready-to-follow absolute URL (as Mem0 documents), null on the last page.
type memPage struct {
	Count    int           `json:"count"`
	Next     *string       `json:"next"`
	Previous *string       `json:"previous"`
	Results  []mem0.Memory `json:"results"`
}

func writeMemPage(t *testing.T, w http.ResponseWriter, p memPage) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(p))
}

// absoluteNext builds the full next-page URL a real Mem0 server would return.
// The client deliberately does not follow it — it re-derives the offset itself
// — but the response shape must still be faithful.
func absoluteNext(r *http.Request, page int) string {
	return "http://" + r.Host + "/v3/memories/?page=" + strconv.Itoa(page)
}

// TestGetAllCompleteWalksEveryPage proves the enumeration follows every page,
// requests the documented maximum page_size, and reports the memories it saw.
func TestGetAllCompleteWalksEveryPage(t *testing.T) {
	pages := [][]mem0.Memory{
		{{ID: "m1"}, {ID: "m2"}},
		{{ID: "m3"}},
	}

	var mu sync.Mutex
	var gotPages, gotPageSizes []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		mu.Lock()
		gotPages = append(gotPages, r.URL.Query().Get("page"))
		gotPageSizes = append(gotPageSizes, r.URL.Query().Get("page_size"))
		mu.Unlock()

		p := memPage{Count: 3, Results: []mem0.Memory{}}
		idx := page - 1
		if idx >= 0 && idx < len(pages) {
			p.Results = pages[idx]
		}
		if idx+1 < len(pages) {
			next := absoluteNext(r, page+1)
			p.Next = &next
		}
		writeMemPage(t, w, p)
	}))
	t.Cleanup(srv.Close)

	c := mem0.NewClient(srv.URL, "test-key", nil)
	enum, err := c.GetAllComplete(context.Background(), mem0.GetAllRequest{
		Filters: mem0.Filters{UserID: "notary-fixture-user-a1b2c3"},
	})
	require.NoError(t, err)

	assert.True(t, enum.Valid())
	assert.Equal(t, 3, enum.Len())
	assert.Equal(t, 3, enum.Count())

	ids := []string{}
	for _, m := range enum.Items() {
		ids = append(ids, m.ID)
	}
	assert.Equal(t, []string{"m1", "m2", "m3"}, ids)

	// Every page must have been requested, in order, starting at 1.
	assert.Equal(t, []string{"1", "2"}, gotPages)
	// page_size must be requested explicitly at the documented maximum, never
	// omitted to fall back on a third-party-reported default.
	require.Len(t, gotPageSizes, 2)
	for _, ps := range gotPageSizes {
		assert.Equal(t, "200", ps)
	}
}

// TestGetAllCompleteRejectsShortRead is the whole point: the server says 5
// memories match the filter but serves only 3. The enumeration must detect the
// mismatch (len(collected) != count), return an explicit error, and hand back
// nothing usable — a short read must never look like a (smaller) complete scope.
func TestGetAllCompleteRejectsShortRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		p := memPage{Count: 5, Results: []mem0.Memory{}}
		switch page {
		case 1:
			p.Results = []mem0.Memory{{ID: "m1"}, {ID: "m2"}}
			next := absoluteNext(r, 2)
			p.Next = &next
		case 2:
			p.Results = []mem0.Memory{{ID: "m3"}}
			// next is null: the walk ends here, yielding 3 of a reported 5.
		}
		writeMemPage(t, w, p)
	}))
	t.Cleanup(srv.Close)

	c := mem0.NewClient(srv.URL, "test-key", nil)
	enum, err := c.GetAllComplete(context.Background(), mem0.GetAllRequest{})

	require.Error(t, err, "an enumeration that collected 3 of a reported 5 must error")
	assert.ErrorIs(t, err, mem0.ErrEnumerationIncomplete)

	// Both halves of the contract: the error identity above, and that no
	// usable enumeration came back. A consumer handed this value must be
	// unable to write an absence claim from it.
	assert.False(t, enum.Valid(), "a short read must not yield a valid enumeration")
	assert.Nil(t, enum.Items())
	assert.Equal(t, 0, enum.Len())
	assert.Equal(t, 0, enum.Count())
}

// TestGetAllCompleteRejectsExcessivePages proves a server that always claims
// another page cannot drive the client forever: the page cap stops the walk and
// surfaces ErrEnumerationIncomplete rather than looping unboundedly.
func TestGetAllCompleteRejectsExcessivePages(t *testing.T) {
	var mu sync.Mutex
	var requests int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		next := absoluteNext(r, 2) // always non-null: never reaches a last page
		writeMemPage(t, w, memPage{
			Count:   1,
			Next:    &next,
			Results: []mem0.Memory{{ID: "m1"}},
		})
	}))
	t.Cleanup(srv.Close)

	c := mem0.NewClient(srv.URL, "test-key", nil)
	enum, err := c.GetAllComplete(context.Background(), mem0.GetAllRequest{})

	require.Error(t, err)
	assert.ErrorIs(t, err, mem0.ErrEnumerationIncomplete)
	assert.False(t, enum.Valid())

	mu.Lock()
	defer mu.Unlock()
	// The cap bounds the number of round trips, so the walk terminates rather
	// than following a non-null next forever.
	assert.Greater(t, requests, 1, "at least two pages must have been attempted")
	assert.LessOrEqual(t, requests, 1001, "the page cap must bound the number of requests")
}

// TestZeroCompleteEnumerationIsInvalid pins the zero-value hazard: Go permits
// CompleteEnumeration{} from any package, and a zero value holds no data. Every
// accessor must be safe and must not dress the empty value up as a real scope.
func TestZeroCompleteEnumerationIsInvalid(t *testing.T) {
	assert.False(t, mem0.CompleteEnumeration{}.Valid())
	assert.Nil(t, mem0.CompleteEnumeration{}.Items())
	assert.Equal(t, 0, mem0.CompleteEnumeration{}.Len())
	assert.Equal(t, 0, mem0.CompleteEnumeration{}.Count())
}

// TestGetAllSendsPagingAsQueryParametersNotBody pins that page/page_size travel
// in the URL query string, not the JSON body: sent in the body they would be
// silently ignored and the missing pagination would be invisible.
func TestGetAllSendsPagingAsQueryParametersNotBody(t *testing.T) {
	var mu sync.Mutex
	var query string
	var body []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		mu.Lock()
		query = r.URL.RawQuery
		body = buf
		mu.Unlock()
		writeMemPage(t, w, memPage{Count: 0, Results: []mem0.Memory{}})
	}))
	t.Cleanup(srv.Close)

	c := mem0.NewClient(srv.URL, "test-key", nil)
	_, err := c.GetAll(context.Background(), mem0.GetAllRequest{
		Filters:  mem0.Filters{UserID: "notary-fixture-user-a1b2c3"},
		Page:     3,
		PageSize: 200,
	})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	var sent map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &sent))
	assert.NotContains(t, sent, "page", "page must not be a body field")
	assert.NotContains(t, sent, "page_size", "page_size must not be a body field")
	require.Contains(t, sent, "filters")

	assert.Contains(t, query, "page=3")
	assert.Contains(t, query, "page_size=200")
}
