package serve

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
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

	"notary/internal/export"
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

// TestEveryRouteAnswers pins that the two routes this task registers answer.
func TestEveryRouteAnswers(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	for _, target := range []string{"/", "/assets/app.css", "/assets/htmx.min.js"} {
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

	assert.Contains(t, href, "%2f", "/ must be percent-encoded")
	assert.Contains(t, href, "%23", "# must be percent-encoded")
	assert.Contains(t, href, "%3a", ": must be percent-encoded")
	assert.Contains(t, href, "%20", "space must be percent-encoded")
	assert.NotContains(t, href, "../", "no ../ path segment may be produced")

	// Round-trip: the rendered href, parsed, carries back the record id, and
	// the ledger finds that record by it.
	u, err := url.Parse(href)
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
// The vendored htmx.min.js is exempt by name -- it is a pinned artefact whose
// digest is asserted separately and whose bytes must not change; htmx issues no
// request unless an hx- attribute asks for one, and none does.
func TestPagesAreOffline(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	pages := map[string]string{
		"/":               get(t, h, "/").Body.String(),
		"/assets/app.css": get(t, h, "/assets/app.css").Body.String(),
	}
	for target, body := range pages {
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
	for _, target := range []string{"/", "/assets/app.css", "/assets/htmx.min.js"} {
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
