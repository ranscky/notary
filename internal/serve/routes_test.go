package serve

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/explain"
	"notary/internal/export"
	"notary/internal/gap"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/store"
)

// loopbackHost is the Host these handler tests send. The records view is an
// ordinary GET, but every request still passes the Host guard, so a test that
// left httptest's default "example.com" in place would be refused with 403.
const loopbackHost = "127.0.0.1"

// request drives h once and returns the recorder. It is the one seam the route
// tests share: method, target, and the Host the guard sees.
func request(t *testing.T, h http.Handler, method, target, host string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = host
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// get asks h for target as a loopback browser would.
func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	return request(t, h, http.MethodGet, target, loopbackHost)
}

// renderedRow is one records-table row reduced to the fields the differential
// compares against the export path, plus the content state the page chose.
type renderedRow struct {
	seq   string
	id    string
	tier  string
	state string // "shown", "withheld", or "none"
	text  string // the shown content, html-unescaped; empty otherwise
}

// The row markup carries its machine-readable identity in data-* attributes,
// so the differential reads the page the way a client would key on it rather
// than guessing from presentation.
var (
	recordRowReMerge = regexp.MustCompile(`(?s)<tr class="record-row"[^>]*>.*?</tr>`)
	rowDataAttrRe    = regexp.MustCompile(`data-([a-z-]+)="([^"]*)"`)
	contentTextRe    = regexp.MustCompile(`<span class="content-text">([^<]*)</span>`)
	recordHrefRe     = regexp.MustCompile(`href="(/record\?[^"]*)"`)
)

// parseRows pulls the records rows out of a rendered page.
func parseRows(t *testing.T, body string) []renderedRow {
	t.Helper()
	var rows []renderedRow
	for _, block := range recordRowReMerge.FindAllString(body, -1) {
		attrs := map[string]string{}
		for _, m := range rowDataAttrRe.FindAllStringSubmatch(block, -1) {
			attrs[m[1]] = m[2]
		}
		r := renderedRow{seq: attrs["seq"], id: attrs["id"], tier: attrs["tier"], state: attrs["content"]}
		if m := contentTextRe.FindStringSubmatch(block); m != nil {
			r.text = html.UnescapeString(m[1])
		}
		rows = append(rows, r)
	}
	return rows
}

// rangeQuery builds the query a records-view request carries for a filter, so
// the page's parseFilter reproduces exactly the From/To/Scope/Reveal the test
// expects on the export side.
func rangeQuery(f filter) string {
	q := url.Values{}
	q.Set("from", f.From.Format(time.RFC3339))
	q.Set("to", f.To.Format(time.RFC3339))
	if f.Scope.UserID != "" {
		q.Set("user_id", f.Scope.UserID)
	}
	if f.Scope.AgentID != "" {
		q.Set("agent_id", f.Scope.AgentID)
	}
	if f.Scope.AppID != "" {
		q.Set("app_id", f.Scope.AppID)
	}
	if f.Scope.RunID != "" {
		q.Set("run_id", f.Scope.RunID)
	}
	if f.Reveal {
		q.Set("reveal", "1")
	}
	return q.Encode()
}

// TestEveryRouteAnswers pins that every route the console registers answers:
// the records view, the per-record, per-memory, gaps and verify views, and the
// assets.
func TestEveryRouteAnswers(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	for _, target := range []string{
		"/", "/record?id=fixture-observed", "/memory?id=mem-alpha", "/gaps", "/verify",
		"/assets/app.css", "/assets/htmx.min.js",
	} {
		rr := get(t, h, target)
		assert.Equal(t, http.StatusOK, rr.Code, "GET %s must answer 200", target)
	}
}

// TestRecordsViewMatchesTheExportPath is the differential: for the same range
// and scope, the rows the records view renders must equal -- id, seq, tier and
// withheld-or-shown content, in order -- the lines `notary export` writes.
//
// The expected side is parsed from the JSONL export itself, never re-derived
// from the records view, so the console cannot drift from the CLI.
func TestRecordsViewMatchesTheExportPath(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	cases := []struct {
		name   string
		reveal bool
		scope  record.Scope
	}{
		{"redacted", false, record.Scope{}},
		{"revealed", true, record.Scope{}},
		{"scoped-to-u1", false, record.Scope{UserID: "u1"}},
		{"scoped-to-a1-revealed", true, record.Scope{AgentID: "a1"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fl := defaultFilter()
			fl.Reveal = tc.reveal
			fl.Scope = tc.scope

			rr := get(t, h, "/?"+rangeQuery(fl))
			require.Equal(t, http.StatusOK, rr.Code)
			rows := parseRows(t, rr.Body.String())

			var buf strings.Builder
			_, err := export.New(f.ledger).Export(context.Background(), export.Request{
				From:             fl.From,
				To:               fl.To,
				Scope:            fl.Scope,
				IncludeSensitive: fl.Reveal,
			}, &buf)
			require.NoError(t, err)
			want := decodeLines(t, []byte(buf.String()))

			require.Len(t, rows, len(want), "one row per exported line")
			for i := range want {
				wantState := "none"
				wantText := ""
				if want[i].Content != nil {
					wantState, wantText = "shown", *want[i].Content
				} else if want[i].Redacted != "" {
					wantState = "withheld"
				}
				assert.Equal(t, strconv.FormatUint(want[i].Seq, 10), rows[i].seq, "row %d seq", i)
				assert.Equal(t, string(want[i].ID), rows[i].id, "row %d id", i)
				assert.Equal(t, want[i].Tier.String(), rows[i].tier, "row %d tier", i)
				assert.Equal(t, wantState, rows[i].state, "row %d content state", i)
				assert.Equal(t, wantText, rows[i].text, "row %d content text", i)
			}
		})
	}
}

// TestRecordsViewShowsTheTierBadgeForEachTier pins that a badge carries the
// tier name as TEXT as well as its colour class, so it survives colour-blindness
// and a greyscale printout.
func TestRecordsViewShowsTheTierBadgeForEachTier(t *testing.T) {
	f := newFixture(t)
	rr := get(t, f.server.Handler(), "/")
	require.Equal(t, http.StatusOK, rr.Code)

	body := rr.Body.String()
	for _, want := range []string{"Observed", "Reconstructed", "Internal"} {
		assert.Contains(t, body, want, "the badge text %q must appear", want)
	}
	for _, class := range []string{"tier-observed", "tier-reconstructed", "tier-internal"} {
		assert.Contains(t, body, class, "the badge class %q must appear", class)
	}
}

// TestRecordsViewEscapesAHostileID pins that an id is encoded exactly once in
// the rendered link, that no `../` path segment is produced, and that the id
// round-trips: parsing the rendered href returns the id, and the ledger finds
// the record by it.
//
// The /record route arrives in a later task, so the round-trip is asserted by
// following the href's own query value -- the id a browser would send back --
// through the ledger, which is the property the escaping exists to protect.
func TestRecordsViewEscapesAHostileID(t *testing.T) {
	f := newFixture(t)
	const hostile = "a/b#c d:e..f"
	appendRecord(t, f.ledger, contentRecord(t, hostile, fixtureAt(1),
		record.Scope{UserID: "u1"}, "mem-alpha", "hostile", false))

	rr := get(t, f.server.Handler(), "/")
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()

	// Find the record link for this row by its data-id, then its href.
	var href string
	for _, block := range recordRowReMerge.FindAllString(body, -1) {
		if strings.Contains(block, `data-id="`+hostile+`"`) {
			m := recordHrefRe.FindStringSubmatch(block)
			require.NotNil(t, m, "the hostile row must carry a /record link")
			href = m[1]
		}
	}
	require.NotEmpty(t, href, "the hostile record must render a row")

	assert.Contains(t, href, "%2F", "/ must be percent-encoded")
	assert.Contains(t, href, "%23", "# must be percent-encoded")
	assert.Contains(t, href, "%3A", ": must be percent-encoded")
	assert.Contains(t, href, "&#43;", "space must be encoded (QueryEscape's +, HTML-escaped)")
	assert.NotContains(t, href, "../", "no ../ path segment may be produced")

	// Round-trip: html-unescaping the rendered attribute yields the value a
	// browser would send, whose parsed id is the record id, and the ledger finds
	// that record by it.
	u, err := url.Parse(html.UnescapeString(href))
	require.NoError(t, err)
	got := u.Query().Get("id")
	require.Equal(t, hostile, got, "the id must survive the URL round-trip")
	rec, err := f.ledger.GetRecord(record.RecordID(got))
	require.NoError(t, err)
	assert.Equal(t, record.RecordID(hostile), rec.ID)
}

// TestRecordsViewLinksMemoryOnlyWhenTheRecordNamesOne pins the rendered-HTML
// behaviour that replaced the old pre-joined-MemoryHref assertion: a record
// whose subject names no memory (the store's memory_id DEFAULT "") renders NO
// /memory link at all, while a record that names one renders a link to it.
//
// It also asserts the unit-level fact behind it -- the no-memory row's
// Line.MemoryID is empty -- because that is what the template branches on.
func TestRecordsViewLinksMemoryOnlyWhenTheRecordNamesOne(t *testing.T) {
	f := newFixture(t)
	rr := get(t, f.server.Handler(), "/")
	require.Equal(t, http.StatusOK, rr.Code)

	var sawNoMemory, sawMemory bool
	for _, block := range recordRowReMerge.FindAllString(rr.Body.String(), -1) {
		switch {
		case strings.Contains(block, `data-id="fixture-internal"`):
			sawNoMemory = true
			assert.NotContains(t, block, "/memory?id=",
				"a record naming no memory must render no memory link")
		case strings.Contains(block, `data-id="fixture-observed"`):
			sawMemory = true
			assert.Contains(t, block, "/memory?id=mem-alpha",
				"a record naming a memory must link to it")
		}
	}
	require.True(t, sawNoMemory, "the fixture must contain the no-memory record")
	require.True(t, sawMemory, "the fixture must contain a memory-naming record")

	rows, err := f.server.loadRecords(defaultFilter())
	require.NoError(t, err)
	for _, r := range rows {
		if r.Line.ID == "fixture-internal" {
			assert.Empty(t, r.Line.MemoryID, "the no-memory record's Line.MemoryID must be empty")
		}
	}
}

// TestNonGETMethodsAreRefusedWithNoBody pins the method guard: anything but
// GET or HEAD is 405 with no body, so no request shape can carry a mutation.
func TestNonGETMethodsAreRefusedWithNoBody(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rr := request(t, h, method, "/", loopbackHost)
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code, "%s must be 405", method)
		assert.Empty(t, rr.Body.String(), "%s must carry no body", method)
	}
}

// TestANonLoopbackHostIsRefused pins the Host guard: a request whose Host is
// not loopback is refused, and the refusal body leaks nothing about the ledger.
func TestANonLoopbackHostIsRefused(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	for _, host := range []string{"evil.example", "example.com:4317", "10.0.0.5:4317"} {
		rr := request(t, h, http.MethodGet, "/", host)
		assert.Equal(t, http.StatusForbidden, rr.Code, "Host %q must be refused", host)
		assert.Empty(t, rr.Body.String(), "Host %q must carry no body", host)
		assert.NotContains(t, rr.Body.String(), "fixture-", "the refusal must name no record")
	}
}

// TestNoResponseCarriesACORSHeader pins that no response is readable
// cross-origin: Access-Control-Allow-Origin is never set.
func TestNoResponseCarriesACORSHeader(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	for _, target := range []string{"/", "/assets/app.css", "/assets/htmx.min.js", "/no-such-page", "/?from=2026-09-25T00:00:00Z&to=2026-09-20T00:00:00Z"} {
		rr := get(t, h, target)
		assert.Empty(t, rr.Header().Get("Access-Control-Allow-Origin"),
			"GET %s must not carry a CORS header", target)
	}
}

// TestPagesAreOffline pins that the console fetches nothing remote: no page a
// handler renders, and no app.css, contains an http:// or https:// URL.
//
// Every page a handler renders is fetched -- the records view, the per-record,
// per-memory, gaps and verify views, and the bad-filter 400 render -- and each
// body is asserted NON-EMPTY before the offline assertion, so an empty body
// cannot pass the check trivially. The vendored htmx.min.js is exempt by name --
// it is a pinned artefact whose digest is asserted separately and whose bytes
// must not change; htmx issues no request unless an hx- attribute asks for one,
// and none does. Asserting over it would fail on the script's own strings.
func TestPagesAreOffline(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	// A reversed range is the bad-filter 400 render.
	const badFilter = "/?from=2026-09-25T00:00:00Z&to=2026-09-20T00:00:00Z"
	pages := map[string]string{
		"/":                           get(t, h, "/").Body.String(),
		"/record?id=fixture-observed": get(t, h, "/record?id=fixture-observed").Body.String(),
		"/memory?id=mem-alpha":        get(t, h, "/memory?id=mem-alpha").Body.String(),
		"/gaps":                       get(t, h, "/gaps").Body.String(),
		"/verify":                     get(t, h, "/verify").Body.String(),
		badFilter:                     get(t, h, badFilter).Body.String(),
		"/assets/app.css":             get(t, h, "/assets/app.css").Body.String(),
	}
	for target, body := range pages {
		require.NotEmpty(t, body, "%s must render a non-empty body before it can be asserted offline", target)
		assert.NotContains(t, body, "http://", "%s must not reference a remote URL", target)
		assert.NotContains(t, body, "https://", "%s must not reference a remote URL", target)
	}
}

// TestAnEmptyRangeSaysSoInWords pins that an empty range is stated, naming the
// range it searched and how to widen it -- not a table that is silently blank.
func TestAnEmptyRangeSaysSoInWords(t *testing.T) {
	f := newFixture(t)
	rr := get(t, f.server.Handler(), "/?from=2020-01-01&to=2020-01-02")
	require.Equal(t, http.StatusOK, rr.Code)

	body := rr.Body.String()
	from := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	assert.Contains(t, body, from.Format(time.RFC3339), "the empty message must name the range it searched")
	assert.Contains(t, body, "widen", "the empty message must say how to widen it")
}

// TestAnEmptyLedgerRendersWithoutError pins that a ledger with no records at
// all still renders the front page rather than failing.
func TestAnEmptyLedgerRendersWithoutError(t *testing.T) {
	srv := newEmptyServer(t)
	rr := get(t, srv.Handler(), "/")
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), "No records", "an empty ledger must render its empty state")
}

// TestABadFilterRendersTheViewWithABanner pins that a usage error in the filter
// is rendered as a page with a banner naming the problem, at 400, and that the
// chain banner is still present.
func TestABadFilterRendersTheViewWithABanner(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	overCapFrom := serveFixedNow.Add(-export.DefaultMaxSpan - 24*time.Hour)
	cases := []struct {
		name   string
		query  string
		marker string
	}{
		{"reversed-range", "from=2026-09-25T00:00:00Z&to=2026-09-20T00:00:00Z", "before"},
		{"over-cap-span", "from=" + overCapFrom.Format(time.RFC3339) + "&to=" + serveFixedNow.Format(time.RFC3339), "exceeds"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := get(t, h, "/?"+tc.query)
			assert.Equal(t, http.StatusBadRequest, rr.Code, "a bad filter must render 400")

			body := rr.Body.String()
			assert.Contains(t, body, tc.marker, "the banner must name the problem")
			assert.Contains(t, body, "this tool's verification reported",
				"the bad-filter page must still carry the chain banner")
		})
	}
}

// TestListenBindsLoopbackOnly pins that Listen binds a literal loopback
// address and nothing else.
func TestListenBindsLoopbackOnly(t *testing.T) {
	f := newFixture(t)
	ln, err := f.server.Listen(0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ln.Close()) })

	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok, "the listener must be a TCP listener")
	assert.True(t, addr.IP.IsLoopback(), "the bound address must be loopback, got %s", addr.IP)
}

// TestListenReportsTheChosenPortForPortZero pins that port 0 asks the OS for a
// free port and the listener reports the real one.
func TestListenReportsTheChosenPortForPortZero(t *testing.T) {
	f := newFixture(t)
	ln, err := f.server.Listen(0)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ln.Close()) })

	addr := ln.Addr().(*net.TCPAddr)
	assert.NotZero(t, addr.Port, "port 0 must be resolved to a real port")
}

// writeFailingStore wraps the real store and fails the test the moment any
// route attempts a write. Reads delegate to the embedded store, so the Server
// sees the seeded ledger; a write reaches the wrapper and is reported.
type writeFailingStore struct {
	*store.SQLiteStore
	t *testing.T
}

func (w *writeFailingStore) PutRecord(rec record.Record) error {
	w.t.Errorf("PutRecord was called from a route: this console must never write (record %s)", rec.ID)
	return errors.New("serve: write attempted")
}

func (w *writeFailingStore) AppendChained(build func(prev record.Record, hasPrev bool) (record.Record, error)) error {
	w.t.Errorf("AppendChained was called from a route: this console must never write")
	return errors.New("serve: write attempted")
}

// sha256Of returns the hex SHA-256 of a file, or a sentinel when it is absent.
func sha256Of(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "<absent>"
	}
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestNoRouteWrites pins that no route writes anything. It builds its own
// fixture over a write-failing store -- the shared fixture's ledger is built
// over the plain store, so a wrapper there would never be reached -- and
// asserts neither the ledger file nor the gap log changed.
func TestNoRouteWrites(t *testing.T) {
	sg, pub := newTestSigner(t)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	// Seed over the PLAIN store, then hand the Server a ledger over the
	// write-failing wrapper so any write from a route is caught.
	l := ledger.New(st, sg, fixedClock)
	gapPath := filepath.Join(dir, "gaps.log")
	appendRecord(t, l, contentRecord(t, "w-record", fixtureAt(0),
		record.Scope{UserID: "u1", AgentID: "a1"}, "mem-1", "a note", false))

	dbPath := filepath.Join(dir, "ledger.db")
	beforeDB := sha256Of(t, dbPath)
	beforeGap := sha256Of(t, gapPath)

	wfs := &writeFailingStore{SQLiteStore: st, t: t}
	wl := ledger.New(wfs, sg, fixedClock)
	srv, err := New(Options{
		Ledger:      wl,
		Store:       wfs,
		GapLogPath:  gapPath,
		Keyring:     map[string]ed25519.PublicKey{sg.KeyID(): pub},
		KeyringPath: filepath.Join(dir, "trusted.keys"),
		Now:         fixedClock,
	})
	require.NoError(t, err)

	h := srv.Handler()
	for _, target := range []string{
		"/", "/assets/app.css", "/assets/htmx.min.js",
		"/record?id=w-record", "/memory?id=mem-1", "/gaps", "/verify",
	} {
		rr := get(t, h, target)
		require.Equal(t, http.StatusOK, rr.Code, "GET %s must answer", target)
	}

	assert.Equal(t, beforeDB, sha256Of(t, dbPath), "the ledger file must be byte-identical after every route")
	assert.Equal(t, beforeGap, sha256Of(t, gapPath), "the gap log must be byte-identical after every route")
}

// TestServeShutsDownOnContextCancel pins that Serve returns nil promptly once
// its context is cancelled.
func TestServeShutsDownOnContextCancel(t *testing.T) {
	f := newFixture(t)
	ln, err := f.server.Listen(0)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.server.Serve(ctx, ln) }()

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a clean shutdown must return nil")
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
}

// newEmptyServer builds a Server over a fresh, empty ledger: the records view
// over a ledger with no records at all.
func newEmptyServer(t *testing.T) *Server {
	t.Helper()
	sg, pub := newTestSigner(t)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	srv, err := New(Options{
		Ledger:      ledger.New(st, sg, fixedClock),
		Store:       st,
		GapLogPath:  filepath.Join(dir, "gaps.log"),
		Keyring:     map[string]ed25519.PublicKey{sg.KeyID(): pub},
		KeyringPath: filepath.Join(dir, "trusted.keys"),
		Now:         fixedClock,
	})
	require.NoError(t, err)
	return srv
}

// --- Task 5: the record, memory, gaps and verify views, and the reveal ---

// recordLinkRe matches a link to the per-record view (with its query) anywhere
// in a page, so a test can assert on the links a page generates to another view.
var recordLinkRe = regexp.MustCompile(`href="(/record\?[^"]*)"`)

// chainHashRe and chainSigRe pull the hash and signature the record page shows
// in its collapsed chain-fields block, so a reveal test can compare the two
// renders byte for byte.
var (
	chainHashRe = regexp.MustCompile(`<dd class="chain-hash">([0-9a-f]+)</dd>`)
	chainSigRe  = regexp.MustCompile(`<dd class="chain-signature">([0-9a-f]+)</dd>`)
)

// memoryPhrasingRe pulls the phrasing cell from a memory-view row.
var memoryPhrasingRe = regexp.MustCompile(`<td class="phrasing"><a href="[^"]*">([^<]*)</a></td>`)

// toggleLinkRe pulls the href and hx-get of the reveal toggle anchor -- the one
// link that carries BOTH attributes and an id. Its two capture groups are the
// rendered href and hx-get values.
var toggleLinkRe = regexp.MustCompile(`<a href="([^"]*)" hx-get="([^"]*)"`)

// fragmentOf returns the element htmx swaps for a view -- the element its
// reveal control targets (hx-select="#<id>") -- so a test asserts on the
// fragment a browser would actually swap, not only on the whole page.
func fragmentOf(t *testing.T, body, id string) string {
	t.Helper()
	re := regexp.MustCompile(`(?s)<div id="` + regexp.QuoteMeta(id) + `">.*</div>`)
	m := re.FindString(body)
	require.NotEmpty(t, m, "the %s view must render its htmx fragment #%s", id, id)
	return m
}

// memoryRowData is one memory-view row: the shared fields the records table
// already exposes, plus the phrasing sentence the memory view adds.
type memoryRowData struct {
	seq, id, tier, state, text, phrasing string
}

// parseMemoryRows pulls the memory-view rows out of a rendered page.
func parseMemoryRows(t *testing.T, body string) []memoryRowData {
	t.Helper()
	blocks := recordRowReMerge.FindAllString(body, -1)
	phrasings := memoryPhrasingRe.FindAllStringSubmatch(body, -1)
	require.Equal(t, len(blocks), len(phrasings), "every memory row must carry one phrasing cell")
	rows := make([]memoryRowData, 0, len(blocks))
	for i, block := range blocks {
		attrs := map[string]string{}
		for _, m := range rowDataAttrRe.FindAllStringSubmatch(block, -1) {
			attrs[m[1]] = m[2]
		}
		r := memoryRowData{
			seq:      attrs["seq"],
			id:       attrs["id"],
			tier:     attrs["tier"],
			state:    attrs["content"],
			phrasing: html.UnescapeString(phrasings[i][1]),
		}
		if m := contentTextRe.FindStringSubmatch(block); m != nil {
			r.text = html.UnescapeString(m[1])
		}
		rows = append(rows, r)
	}
	return rows
}

// TestRecordViewShowsTheClaimAndTheChainFields pins that /record renders the
// export.Phrase sentence, the tier name, both instants, the record hash, and
// the re-run command the page names for the check it does not itself perform.
func TestRecordViewShowsTheClaimAndTheChainFields(t *testing.T) {
	f := newFixture(t)
	rr := get(t, f.server.Handler(), "/record?id=fixture-observed")
	require.Equal(t, http.StatusOK, rr.Code)
	body := rr.Body.String()

	rec, err := f.ledger.GetRecord("fixture-observed")
	require.NoError(t, err)
	sentence, ok := export.Phrase(rec)
	require.True(t, ok, "the fixture record must be phraseable")
	line, err := export.Render(rec, false)
	require.NoError(t, err)

	assert.Contains(t, body, sentence, "the page must carry the phrased sentence")
	assert.Contains(t, body, badgeFor(line.Tier).Name, "the page must carry the tier name")
	assert.Contains(t, body, line.Hash, "the page must carry the record hash")
	assert.Contains(t, body, rec.At.Format("2006-01-02 15:04:05 MST"), "the page must carry At")
	assert.Contains(t, body, rec.RecordedAt.Format("2006-01-02 15:04:05 MST"), "the page must carry RecordedAt")
	assert.Contains(t, body, "notary verify", "the page must name the re-run command")
}

// TestRevealToggleHxGetCarriesTheEscapedID pins the path the href round-trip
// alone does not cover. html/template URL-encodes href but only HTML-escapes a
// custom attribute such as htmx's hx-get, so an id left raw for the escaper
// would start a URL fragment in hx-get and fetch a truncated id -- the same bug
// class that survives wherever a raw id rides in hx-get. The reveal toggle
// builds both attributes from the SAME pre-escaped string (the recordHref
// helper), so they are byte-identical and the id round-trips out of the hx-get a
// browser would actually send.
func TestRevealToggleHxGetCarriesTheEscapedID(t *testing.T) {
	f := newFixture(t)
	const hostile = "a/b#c d:e..f"
	appendRecord(t, f.ledger, contentRecord(t, hostile, fixtureAt(1),
		record.Scope{UserID: "u1"}, "mem-alpha", "hostile", false))

	rr := get(t, f.server.Handler(), "/record?id="+url.QueryEscape(hostile))
	require.Equal(t, http.StatusOK, rr.Code)

	m := toggleLinkRe.FindStringSubmatch(rr.Body.String())
	require.NotNil(t, m, "the record page must render a reveal toggle link")
	href, hxGet := m[1], m[2]
	assert.Equal(t, href, hxGet, "the href and the hx-get of one link must be byte-identical")

	// The value a browser sends is the rendered attribute HTML-unescaped.
	u, err := url.Parse(html.UnescapeString(hxGet))
	require.NoError(t, err)
	got := u.Query().Get("id")
	require.Equal(t, hostile, got, "the id must survive the hx-get round-trip")

	rec, err := f.ledger.GetRecord(record.RecordID(got))
	require.NoError(t, err)
	assert.Equal(t, record.RecordID(hostile), rec.ID)
}

// TestMemoryViewMatchesTheExplainPath is the differential for the memory view:
// for one memory, the page's rows must carry the same sentences, tiers and
// withheld-or-shown decisions that `notary explain --memory <id> --json`
// reports. The expected side is the CLI's own explain path run in-process, and
// it is cross-checked against the export.Render/export.Phrase projections
// explain itself uses, so the console's memory view cannot drift from explain.
func TestMemoryViewMatchesTheExplainPath(t *testing.T) {
	f := newFixture(t)
	rr := get(t, f.server.Handler(), "/memory?id=mem-alpha")
	require.Equal(t, http.StatusOK, rr.Code)
	rows := parseMemoryRows(t, rr.Body.String())

	// Drive the CLI's --memory --json path in-process.
	var buf bytes.Buffer
	_, err := explain.New(f.ledger).Explain(context.Background(),
		explain.Request{MemoryID: "mem-alpha", JSON: true}, &buf)
	require.NoError(t, err)

	var view struct {
		Records []struct {
			Seq      uint64  `json:"seq"`
			ID       string  `json:"id"`
			Tier     string  `json:"tier"`
			Sentence string  `json:"sentence"`
			Content  *string `json:"content"`
			Redacted string  `json:"redacted"`
		} `json:"records"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &view))
	require.NotEmpty(t, view.Records, "the fixture memory must have records")

	require.Len(t, rows, len(view.Records), "one row per explained record")
	for i, want := range view.Records {
		assert.Equal(t, strconv.FormatUint(want.Seq, 10), rows[i].seq, "row %d seq", i)
		assert.Equal(t, want.ID, rows[i].id, "row %d id", i)
		assert.Equal(t, want.Tier, rows[i].tier, "row %d tier", i)
		assert.Equal(t, want.Sentence, rows[i].phrasing, "row %d sentence", i)
		wantState := "none"
		if want.Content != nil {
			wantState = "shown"
		} else if want.Redacted != "" {
			wantState = "withheld"
		}
		assert.Equal(t, wantState, rows[i].state, "row %d content state", i)
	}

	// The same export.Line projections the page is built from, built directly:
	// the page and explain share these functions, so their shared fields agree.
	recs, err := f.ledger.ListRecordsByMemory("mem-alpha")
	require.NoError(t, err)
	require.Len(t, recs, len(view.Records))
	for i, rec := range recs {
		sentence, ok := export.Phrase(rec)
		require.True(t, ok)
		line, err := export.Render(rec, false)
		require.NoError(t, err)
		assert.Equal(t, sentence, view.Records[i].Sentence, "record %s sentence", rec.ID)
		assert.Equal(t, line.Tier.String(), view.Records[i].Tier, "record %s tier", rec.ID)
	}
}

// TestGapsViewMatchesTheSharedReport pins that /gaps renders the same report
// ledger.OutstandingGaps produces: every unreconciled entry's correlation id
// and detail, any gap-log integrity break, and "No outstanding gaps" on the
// intact fixture.
func TestGapsViewMatchesTheSharedReport(t *testing.T) {
	t.Run("intact", func(t *testing.T) {
		f := newFixture(t)
		rr := get(t, f.server.Handler(), "/gaps")
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Contains(t, rr.Body.String(), "No outstanding gaps")
	})

	t.Run("unreconciled", func(t *testing.T) {
		f := newFixture(t)
		writeGapLog(t, f.gapPath, gap.Entry{
			At:            serveFixedNow,
			Kind:          record.EventMemorySurfaced,
			Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
			CorrelationID: "corr-missing",
			Detail:        "memory store unreachable",
		})
		report, err := ledger.OutstandingGaps(f.store, f.gapPath)
		require.NoError(t, err)
		require.Len(t, report.Unreconciled, 1, "the fixture must yield one unreconciled entry")

		rr := get(t, f.server.Handler(), "/gaps")
		require.Equal(t, http.StatusOK, rr.Code)
		body := rr.Body.String()
		e := report.Unreconciled[0]
		assert.Contains(t, body, e.CorrelationID, "the page must name the correlation id")
		assert.Contains(t, body, e.Detail, "the page must carry the detail")
		assert.Contains(t, body, string(e.Kind), "the page must name the kind")
		assert.NotContains(t, body, "No outstanding gaps")
		// The scope cell renders only the dimensions the entry carried, the same
		// way the records and record views render scope: this entry carries user
		// u1 and agent a1, so app= and run= must not appear.
		assert.Contains(t, body, "user=u1 agent=a1", "the scope cell must render the carried dimensions")
		assert.NotContains(t, body, "app=", "an empty scope dimension must be omitted")
		assert.NotContains(t, body, "run=", "an empty scope dimension must be omitted")
	})

	t.Run("corrupt-log", func(t *testing.T) {
		f := newFixture(t)
		// An entry that matches a stored record (so nothing is unreconciled),
		// then its Detail is rewritten without recomputing its hash: the line
		// still decodes but no longer verifies -- an integrity break.
		writeGapLog(t, f.gapPath, gap.Entry{
			At:            serveFixedNow,
			Kind:          record.EventMemorySurfaced,
			Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
			CorrelationID: "fixture-observed",
			Detail:        "ok",
		})
		raw, err := os.ReadFile(f.gapPath)
		require.NoError(t, err)
		tampered := bytes.Replace(raw, []byte(`"detail":"ok"`), []byte(`"detail":"tampered-xyz"`), 1)
		require.NotEqual(t, raw, tampered, "the fixture detail must appear verbatim for the rewrite to bite")
		require.NoError(t, os.WriteFile(f.gapPath, tampered, 0o600))

		report, err := ledger.OutstandingGaps(f.store, f.gapPath)
		require.NoError(t, err)
		require.NotEmpty(t, report.Integrity, "the rewrite must produce an integrity break")

		rr := get(t, f.server.Handler(), "/gaps")
		require.Equal(t, http.StatusOK, rr.Code)
		body := rr.Body.String()
		assert.Contains(t, body, report.Integrity[0].Detail, "the page must carry the integrity break detail")
		assert.NotContains(t, body, "No outstanding gaps")
	})
}

// TestVerifyViewRendersThreeDistinctStates pins that /verify renders the three
// chain states distinguishably: clean (with the record count), broken (naming
// the record id and the field), and not verified (naming the keyring fix).
func TestVerifyViewRendersThreeDistinctStates(t *testing.T) {
	fClean := newFixture(t)
	fTampered := newFixture(t)

	// Change a record's event out of band, so its stored hash no longer
	// recomputes -- a "hash" break on "fixture-internal" (seq 3).
	tamperStoredEvent(t, filepath.Join(fTampered.dir, "ledger.db"),
		`UPDATE records SET event = 'memory_kept' WHERE seq = 3`)

	clean := get(t, fClean.server.Handler(), "/verify")
	require.Equal(t, http.StatusOK, clean.Code)
	cleanBody := clean.Body.String()
	assert.Contains(t, cleanBody, "4 records verified", "the clean state must report the record count")

	broken := get(t, fTampered.server.Handler(), "/verify")
	require.Equal(t, http.StatusOK, broken.Code)
	brokenBody := broken.Body.String()
	assert.Contains(t, brokenBody, "fixture-internal", "the broken state must name the record id")
	assert.Contains(t, brokenBody, "fixture-internal (seq 3): hash", "the broken state must name the field")

	unverified := newUnverifiedServer(t, fClean)
	nv := get(t, unverified.Handler(), "/verify")
	require.Equal(t, http.StatusOK, nv.Code)
	nvBody := nv.Body.String()
	assert.Contains(t, nvBody, "NOTARY_TRUSTED_KEYS_PATH", "the not-verified state must name the keyring fix")

	assert.NotEqual(t, cleanBody, brokenBody, "clean and broken must render differently")
	assert.NotEqual(t, cleanBody, nvBody, "clean and not-verified must render differently")
	assert.NotEqual(t, brokenBody, nvBody, "broken and not-verified must render differently")
}

// TestStoredTextCannotBecomeMarkup puts a script tag, a closing textarea tag
// and a newline into a memory's content, a memory id and a gap entry's Detail,
// and asserts every page that shows them renders them escaped -- on the whole
// page AND on the htmx fragment a reveal control would swap in, since the
// fragment is what a browser actually replaces.
func TestStoredTextCannotBecomeMarkup(t *testing.T) {
	f := newFixture(t)
	const payload = "<script>alert(1)</script>\n</textarea>"
	hostileMemory := "mem" + payload

	appendRecord(t, f.ledger, contentRecord(t, "hostile-record", fixtureAt(4),
		record.Scope{UserID: "u1"}, hostileMemory, payload, false))
	writeGapLog(t, f.gapPath, gap.Entry{
		At:            serveFixedNow,
		Kind:          record.EventMemorySurfaced,
		Scope:         record.Scope{UserID: "u1"},
		CorrelationID: "corr-hostile",
		Detail:        payload,
	})

	h := f.server.Handler()
	pages := []struct {
		target   string
		fragment string
	}{
		{"/", "records"},
		{"/record?id=hostile-record", "record"},
		{"/memory?id=" + url.QueryEscape(hostileMemory), "memory"},
		{"/gaps", "gaps"},
	}

	for _, tc := range pages {
		t.Run(tc.target, func(t *testing.T) {
			rr := get(t, h, tc.target)
			require.Equal(t, http.StatusOK, rr.Code)
			body := rr.Body.String()
			frag := fragmentOf(t, body, tc.fragment)

			for _, scope := range []struct {
				label string
				html  string
			}{
				{"page", body},
				{"fragment", frag},
			} {
				assert.NotContains(t, scope.html, "<script>alert(1)",
					"%s %s must not carry the raw script tag", tc.target, scope.label)
				assert.NotContains(t, scope.html, "</textarea>",
					"%s %s must not carry the raw close tag", tc.target, scope.label)
				assert.Contains(t, scope.html, "&lt;script&gt;alert(1)&lt;/script&gt;",
					"%s %s must carry the escaped script text", tc.target, scope.label)
				assert.Contains(t, scope.html, "&lt;/textarea&gt;",
					"%s %s must carry the escaped close text", tc.target, scope.label)
			}
		})
	}
}

// TestMemoryViewRefusesAnEmptyID pins that an empty memory id is a 400 whose
// body lists no record -- the guard that stops an empty filter from listing the
// whole ledger wearing one memory's clothes.
func TestMemoryViewRefusesAnEmptyID(t *testing.T) {
	f := newFixture(t)
	rr := get(t, f.server.Handler(), "/memory?id=")
	require.Equal(t, http.StatusBadRequest, rr.Code)
	body := rr.Body.String()
	require.NotEmpty(t, body)
	for _, id := range []string{
		"fixture-observed", "fixture-observed-plain", "fixture-reconstructed", "fixture-internal",
	} {
		assert.NotContains(t, body, id, "an empty memory id must list no record (%s)", id)
	}
	assert.Contains(t, body, "id is required", "the page must say an id is required")
}

// TestRevealIsPerViewAndLogged pins the reveal contract: the default render
// withholds, a revealing request shows the content and writes exactly ONE line
// to the Reveal writer naming the view, a link a page generates to another view
// never carries reveal (so following one re-redacts), and each view's own
// reveal toggle names that view.
func TestRevealIsPerViewAndLogged(t *testing.T) {
	f := newFixture(t)
	var reveal bytes.Buffer
	srv, err := New(Options{
		Ledger:      f.ledger,
		Store:       f.store,
		GapLogPath:  f.gapPath,
		Keyring:     map[string]ed25519.PublicKey{f.signer.KeyID(): f.pub},
		KeyringPath: f.server.keyringPath,
		Reveal:      &reveal,
		Now:         fixedClock,
	})
	require.NoError(t, err)
	h := srv.Handler()

	// Redacted by default; a non-revealing request writes no line.
	red := get(t, h, "/record?id=fixture-observed")
	require.Equal(t, http.StatusOK, red.Code)
	assert.Contains(t, red.Body.String(), "Content withheld — marked sensitive")
	assert.NotContains(t, red.Body.String(), "the api key is hunter2")
	assert.Empty(t, reveal.String(), "a non-revealing request must write no line")

	// A revealing request shows the content and writes exactly one line naming
	// the view and id.
	rev := get(t, h, "/record?id=fixture-observed&reveal=1")
	require.Equal(t, http.StatusOK, rev.Code)
	assert.Contains(t, rev.Body.String(), "the api key is hunter2")
	assert.Equal(t, 1, strings.Count(reveal.String(), "\n"), "exactly one line per revealing request")
	assert.Contains(t, reveal.String(), "record fixture-observed", "the line must name the view and id")

	// A /memory page's links to records carry NO reveal, so following one
	// re-redacts -- the reset is structural, not a timer.
	reveal.Reset()
	mem := get(t, h, "/memory?id=mem-alpha")
	require.Equal(t, http.StatusOK, mem.Code)
	links := recordLinkRe.FindAllStringSubmatch(mem.Body.String(), -1)
	require.NotEmpty(t, links, "the memory page must link to its records")
	for _, m := range links {
		assert.NotContains(t, m[1], "reveal", "a link to another view must never carry reveal (got %q)", m[1])
	}

	followed := get(t, h, links[0][1])
	require.Equal(t, http.StatusOK, followed.Code)
	assert.NotContains(t, followed.Body.String(), "the api key is hunter2", "following a link must re-redact")
	assert.Empty(t, reveal.String(), "following a link must not reveal")

	// The memory view's own reveal toggle works and names the memory.
	revMem := get(t, h, "/memory?id=mem-alpha&reveal=1")
	require.Equal(t, http.StatusOK, revMem.Code)
	assert.Contains(t, revMem.Body.String(), "the api key is hunter2")
	assert.Equal(t, 1, strings.Count(reveal.String(), "\n"))
	assert.Contains(t, reveal.String(), "memory mem-alpha", "the line must name the memory view")
}

// TestRevealChangesNoHash pins that reveal is a rendering decision and nothing
// more: the hash and signature the record page shows are identical with and
// without reveal.
func TestRevealChangesNoHash(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	red := get(t, h, "/record?id=fixture-observed").Body.String()
	rev := get(t, h, "/record?id=fixture-observed&reveal=1").Body.String()

	require.NotContains(t, red, "the api key is hunter2", "the default render must withhold")
	require.Contains(t, rev, "the api key is hunter2", "the revealing render must show the content")

	redHash := chainHashRe.FindStringSubmatch(red)
	revHash := chainHashRe.FindStringSubmatch(rev)
	require.NotNil(t, redHash, "the page must render the hash")
	require.NotNil(t, revHash)
	assert.NotEmpty(t, redHash[1])
	assert.Equal(t, redHash[1], revHash[1], "the hash must be identical with and without reveal")

	redSig := chainSigRe.FindStringSubmatch(red)
	revSig := chainSigRe.FindStringSubmatch(rev)
	require.NotNil(t, redSig, "the page must render the signature")
	require.NotNil(t, revSig)
	assert.Equal(t, redSig[1], revSig[1], "the signature must be identical with and without reveal")
}

// TestNoKeyMaterialInAnyPage is the key-material canary: the fixture's signing
// key bytes and their base64 encodings never appear in any rendered page.
func TestNoKeyMaterialInAnyPage(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	priv := ed25519.NewKeyFromSeed(serveTestSeed)
	secrets := []string{
		base64.StdEncoding.EncodeToString(serveTestSeed),
		string(serveTestSeed),
		base64.StdEncoding.EncodeToString(priv),
		string(priv),
	}

	for _, target := range []string{
		"/", "/record?id=fixture-observed", "/memory?id=mem-alpha", "/gaps", "/verify",
	} {
		body := get(t, h, target).Body.String()
		require.NotEmpty(t, body)
		for _, secret := range secrets {
			assert.NotContains(t, body, secret, "GET %s must never carry key material", target)
		}
	}
}

// revealToggleMarker is the class the shared "revealToggle" partial stamps on
// the paragraph wrapping its anchor, so a test can assert whether a view offers
// a reveal control without depending on the link's label. The records view's own
// control is its filter form's checkbox instead, marked by name="reveal".
const revealToggleMarker = `class="reveal-toggle"`

// TestRevealScopeIsPinned locks in which views reveal and log. Only the views
// that render memory content offer a reveal control and write the one-line
// reveal note: the records view, /record and /memory. /gaps and /verify render
// no memory content, so a request carrying reveal=1 to either must render NO
// reveal control and write NOTHING; and an error render of /record or /memory (a
// 400 or 404) has no content to reveal either, so it must offer no control. The
// scope was set by an earlier fix round but nothing tested it, so it could
// silently regress.
func TestRevealScopeIsPinned(t *testing.T) {
	f := newFixture(t)
	var reveal bytes.Buffer
	srv, err := New(Options{
		Ledger:      f.ledger,
		Store:       f.store,
		GapLogPath:  f.gapPath,
		Keyring:     map[string]ed25519.PublicKey{f.signer.KeyID(): f.pub},
		KeyringPath: f.server.keyringPath,
		Reveal:      &reveal,
		Now:         fixedClock,
	})
	require.NoError(t, err)
	h := srv.Handler()

	// Content-bearing views: a revealing request renders the view's own reveal
	// control and writes exactly ONE line naming the view.
	for _, tc := range []struct {
		name   string
		target string
		marker string
		view   string
	}{
		{"records", "/?reveal=1", `name="reveal"`, "records"},
		{"record", "/record?id=fixture-observed&reveal=1", revealToggleMarker, "record fixture-observed"},
		{"memory", "/memory?id=mem-alpha&reveal=1", revealToggleMarker, "memory mem-alpha"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reveal.Reset()
			rr := get(t, h, tc.target)
			require.Equal(t, http.StatusOK, rr.Code)
			assert.Contains(t, rr.Body.String(), tc.marker, "%s must offer its reveal control", tc.name)
			assert.Equal(t, 1, strings.Count(reveal.String(), "\n"), "exactly one reveal line per revealing request")
			assert.Contains(t, reveal.String(), tc.view, "the line must name the view")
		})
	}

	// Contentless views: a revealing request offers no control and writes
	// nothing -- a note there would be a false claim about content that is not on
	// the page.
	for _, target := range []string{"/gaps?reveal=1", "/verify?reveal=1"} {
		t.Run(target, func(t *testing.T) {
			reveal.Reset()
			rr := get(t, h, target)
			require.Equal(t, http.StatusOK, rr.Code)
			body := rr.Body.String()
			assert.NotContains(t, body, revealToggleMarker, "%s must offer no reveal control", target)
			assert.NotContains(t, body, "Reveal sensitive content", "%s must offer no reveal control", target)
			assert.Empty(t, reveal.String(), "%s must write nothing to the reveal log", target)
		})
	}

	// Error renders: the /record and /memory 400/404 pages carry no content, so
	// they must render no reveal control (the toggle would point at a dead URL
	// built from the empty id) and write nothing.
	for _, target := range []string{"/record?id=", "/record?id=no-such-record", "/memory?id="} {
		t.Run("error "+target, func(t *testing.T) {
			reveal.Reset()
			rr := get(t, h, target)
			assert.NotContains(t, rr.Body.String(), revealToggleMarker,
				"%s must render no reveal control on its error page", target)
			assert.Empty(t, reveal.String(), "%s must write nothing to the reveal log", target)
		})
	}
}

// TestRecordViewMissingAndUnknownID pins /record's 400-versus-404 split: an
// empty id is a 400 (a usage error naming an id as required), and an id that
// names no record is a 404 whose body names the missing id. These are different
// answers to different questions -- "you asked wrong" versus "there is no such
// record" -- and the branch that tells them apart is exactly the kind that can
// silently flip.
func TestRecordViewMissingAndUnknownID(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	empty := get(t, h, "/record?id=")
	require.Equal(t, http.StatusBadRequest, empty.Code, "an empty id must be 400")
	assert.Contains(t, empty.Body.String(), "id is required", "the 400 must say an id is required")

	const missing = "no-such-record"
	notFound := get(t, h, "/record?id="+missing)
	require.Equal(t, http.StatusNotFound, notFound.Code, "an id that names no record must be 404")
	assert.Contains(t, notFound.Body.String(), missing, "the 404 must name the missing id")
}
