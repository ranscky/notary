package serve

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/export"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/replay"
	"notary/internal/store"
)

// TestParseAsOfInstantZonesNameTheSameMoment pins the reason the form asks for a
// zone at all: an instant is a moment in time, and the same moment has different
// wall-clock readings in different zones. The two inputs below are the same
// instant, so they must produce the same value and echo the same UTC.
func TestParseAsOfInstantZonesNameTheSameMoment(t *testing.T) {
	utc, err := parseAsOfInstant("2026-09-28", "12:00", "UTC")
	require.NoError(t, err)

	offset, err := parseAsOfInstant("2026-09-28", "07:00", "-05:00")
	require.NoError(t, err)

	assert.True(t, utc.Equal(offset),
		"12:00 UTC and 07:00-05:00 are the same instant, got %s and %s", utc, offset)
	assert.Equal(t, "2026-09-28T12:00:00Z", utc.UTC().Format(time.RFC3339))
	assert.Equal(t, "2026-09-28T12:00:00Z", offset.UTC().Format(time.RFC3339),
		"the echoed UTC must agree whatever zone the reader chose")
}

// TestParseAsOfInstantRejectsAMissingZone pins that a time without a zone is
// refused rather than assumed. This is the page's whole reason for existing: a
// silent zone answers a question the reader did not ask.
func TestParseAsOfInstantRejectsAMissingZone(t *testing.T) {
	_, err := parseAsOfInstant("2026-09-28", "12:00", "")
	require.Error(t, err, "a submission with no zone must be refused, never defaulted")
	assert.Contains(t, err.Error(), "time zone", "the message must name the field that is missing")
}

// TestParseAsOfInstantNamesTheFieldItCouldNotParse pins that each refusal names
// the part the reader must fix, and that nothing here can pass for a valid
// instant.
func TestParseAsOfInstantNamesTheFieldItCouldNotParse(t *testing.T) {
	cases := []struct{ date, clock, zone, want string }{
		{"28/09/2026", "12:00", "UTC", "date"},
		{"2026-09-28T12:00", "12:00", "UTC", "date"},
		{"2026-09-28", "noon", "UTC", "time"},
		{"2026-09-28", "12:00", "Europe/London", "time zone"},
		{"2026-09-28", "12:00", "+5", "time zone"},
	}
	for _, tc := range cases {
		t.Run(tc.date+" "+tc.clock+" "+tc.zone, func(t *testing.T) {
			_, err := parseAsOfInstant(tc.date, tc.clock, tc.zone)
			require.Error(t, err, "this input must be refused")
			assert.Contains(t, err.Error(), tc.want,
				"the message must name the field that is wrong")
		})
	}
}

// asOfServer builds a console over a ledger whose records have DISTINCT
// RecordedAt instants, one second apart in the event time but exactly one
// nanosecond apart in the write time.
//
// The shared fixture cannot serve this: it writes every record at one clock, so
// an inclusive-boundary test over it would pass for the wrong reason. The form
// submits whole seconds, which is exactly why the second record is a nanosecond
// later -- the reader cannot name it, so it must be absent.
func asOfServer(t *testing.T) (*Server, time.Time) {
	t.Helper()
	sg, pub := newTestSigner(t)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	// The instant the form will actually be able to submit.
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	clock := at
	l := ledger.New(st, sg, func() time.Time { return clock })

	appendRecord(t, l, contentRecord(t, "as-of-at-the-instant", at.Add(-time.Hour),
		record.Scope{UserID: "u1"}, "mem-1", "the api key is hunter2", true))

	// A nanosecond later: indistinguishable in the form, distinguishable to
	// replay, and therefore the record that proves the boundary is > and not >=.
	clock = at.Add(time.Nanosecond)
	appendRecord(t, l, contentRecord(t, "as-of-a-nanosecond-later", at.Add(-time.Hour),
		record.Scope{UserID: "u1"}, "mem-1", "written a moment too late", false))

	srv, err := New(Options{
		Ledger:      l,
		Store:       st,
		GapLogPath:  filepath.Join(dir, "gaps.log"),
		Keyring:     map[string]ed25519.PublicKey{sg.KeyID(): pub},
		KeyringPath: filepath.Join(dir, "trusted.keys"),
		Now:         fixedClock,
	})
	require.NoError(t, err)
	return srv, at
}

// asOfBody submits the form and returns the rendered page.
func asOfBody(t *testing.T, srv *Server, date, clock, zone string, extra url.Values) string {
	t.Helper()
	q := url.Values{}
	q.Set("date", date)
	q.Set("time", clock)
	if zone != "" {
		q.Set("zone", zone)
	}
	for k, vs := range extra {
		for _, v := range vs {
			q.Add(k, v)
		}
	}
	rr := get(t, srv.Handler(), "/asof?"+q.Encode())
	require.Equal(t, 200, rr.Code, "the as-of page must answer 200 for %s", q.Encode())
	return rr.Body.String()
}

// TestAsOfBoundaryIsInclusive pins the rule the page rests on: a record written
// AT the instant is in the view, and one written a nanosecond after it is not.
func TestAsOfBoundaryIsInclusive(t *testing.T) {
	srv, at := asOfServer(t)

	body := asOfBody(t, srv, "2026-09-28", "12:00:00", "UTC", nil)
	assert.Contains(t, body, "as-of-at-the-instant",
		"a record whose RecordedAt equals the instant must be shown")
	assert.NotContains(t, body, "as-of-a-nanosecond-later",
		"a record written after the instant must be absent")

	// A later instant includes it, so the absence above is the boundary and not a
	// broken read.
	later := asOfBody(t, srv, "2026-09-28", "12:00:02", "UTC", nil)
	assert.Contains(t, later, "as-of-a-nanosecond-later", "a later instant must include the record")
	assert.Contains(t, later, "Showing what Notary had recorded by 2026-09-28T12:00:02Z",
		"the echo must name the instant in UTC as RFC3339 (%s is the fixture's own instant)", at.Format(time.RFC3339))
}

// TestAsOfWithNoKeyringSaysSoAndRunsNothing pins the state the CLI also refuses:
// without trusted keys a replay cannot be verified, so the page says so in words
// and renders no records at all.
func TestAsOfWithNoKeyringSaysSoAndRunsNothing(t *testing.T) {
	f := newFixture(t)
	srv := newUnverifiedServer(t, f)

	body := asOfBody(t, srv, "2026-09-28", "12:00:00", "UTC", nil)
	assert.Contains(t, body, "No trusted keys are configured",
		"the page must say, in the console's own words, that nothing was checked")
	assert.NotContains(t, body, "fixture-observed",
		"no record may be shown when no replay could be verified")
}

// rowIDs pulls the record ids out of a rendered table, in page order.
func rowIDs(rows []renderedRow) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.id)
	}
	return ids
}

// TestAsOfMatchesReplay is the differential: for the same request on the same
// fixture, the page must show exactly the records replay.Replay writes, in the
// same order. The console is a renderer of replay's output, and this is what
// holds it to that.
func TestAsOfMatchesReplay(t *testing.T) {
	f := newFixture(t)
	at := serveFixedNow

	body := asOfBody(t, f.server, "2026-09-28", "12:00:00", "UTC", nil)
	got := rowIDs(parseRows(t, body))

	var buf bytes.Buffer
	_, err := replay.New(f.ledger, f.server.verifier).Replay(context.Background(),
		replay.Request{At: at}, &buf)
	require.NoError(t, err)

	var want []string
	dec := json.NewDecoder(&buf)
	for {
		var line export.Line
		err := dec.Decode(&line)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)
		want = append(want, string(line.ID))
	}

	require.NotEmpty(t, want, "the fixture must replay something for this to mean anything")
	assert.Equal(t, want, got, "the page must show exactly what replay.Replay returns, in order")
}

// TestAsOfMissingAndGarbageInstantsExplainThemselves pins that a request with no
// usable instant is a message about the reader's input -- never a 500, never a
// silent default to now, and never a replay run on a guess.
func TestAsOfMissingAndGarbageInstantsExplainThemselves(t *testing.T) {
	f := newFixture(t)

	// Nothing submitted: the form, and no error.
	empty := get(t, f.server.Handler(), "/asof")
	require.Equal(t, http.StatusOK, empty.Code)
	assert.Contains(t, empty.Body.String(), "As of")
	assert.NotContains(t, empty.Body.String(), "Could not read that moment",
		"an untouched form must not be shown as an error")

	for _, query := range []string{
		"?date=2026-09-28&time=12:00",           // no zone
		"?date=2026-09-28&time=12:00&zone=nope", // unparseable zone
		"?date=28/09/2026&time=12:00&zone=UTC",  // unparseable date
		"?date=2026-09-28&time=noon&zone=UTC",   // unparseable time
		"?date=&time=&zone=&user_id=u1",         // a scope but no instant
	} {
		rr := get(t, f.server.Handler(), "/asof"+query)
		assert.Equal(t, http.StatusBadRequest, rr.Code,
			"%s must be a validation message, not a 500", query)
		assert.Contains(t, rr.Body.String(), "Could not read that moment",
			"%s must say what is wrong", query)
		assert.NotContains(t, rr.Body.String(), "fixture-observed",
			"%s must not replay anything", query)
	}
}

// TestAsOfEchoesTheSameInstantForTwoZones pins that the echo reports the moment,
// not the wall clock the reader typed: 12:00 UTC and 07:00-05:00 are one
// instant, so both must echo the same UTC RFC3339.
func TestAsOfEchoesTheSameInstantForTwoZones(t *testing.T) {
	f := newFixture(t)

	utc := asOfBody(t, f.server, "2026-09-28", "12:00:00", "UTC", nil)
	offset := asOfBody(t, f.server, "2026-09-28", "07:00:00", "-05:00", nil)

	assert.Contains(t, utc, "Showing what Notary had recorded by 2026-09-28T12:00:00Z")
	assert.Contains(t, offset, "Showing what Notary had recorded by 2026-09-28T12:00:00Z",
		"a different zone naming the same moment must echo the same instant")
	assert.Equal(t, rowIDs(parseRows(t, utc)), rowIDs(parseRows(t, offset)),
		"both zones name one moment, so they must show one set of records")
}

// TestAsOfEscapesHostileInput pins that what the reader typed is rendered as
// text: in the form's echo, in a validation message that quotes it back, and in
// the reproduce line, which is the one place a value is meant to be copied into a
// shell.
func TestAsOfEscapesHostileInput(t *testing.T) {
	f := newFixture(t)
	const payload = `<script>alert(1)</script>`

	// In a scope value, on a successful request.
	body := asOfBody(t, f.server, "2026-09-28", "12:00:00", "UTC",
		url.Values{"user_id": {payload}})
	assert.NotContains(t, body, "<script>alert(1)", "a scope value must never become markup")
	assert.Contains(t, body, "&lt;script&gt;", "it must appear escaped instead")

	// In the date, on a refused request: the message quotes the input back.
	rr := get(t, f.server.Handler(),
		"/asof?date="+url.QueryEscape(payload)+"&time=12:00&zone=UTC")
	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.NotContains(t, rr.Body.String(), "<script>alert(1)")
	assert.Contains(t, rr.Body.String(), "&lt;script&gt;")
}

// TestAsOfRedactsSensitiveContentUntilRevealed pins that the page inherits the
// read path's redaction: withheld by default, shown only with this view's own
// toggle.
func TestAsOfRedactsSensitiveContentUntilRevealed(t *testing.T) {
	f := newFixture(t)

	plain := asOfBody(t, f.server, "2026-09-28", "12:00:00", "UTC", nil)
	assert.Contains(t, plain, "Content withheld — marked sensitive",
		"the withheld state must be labelled, not left blank")
	assert.NotContains(t, plain, "the api key is hunter2",
		"sensitive content must be withheld by default")

	revealed := asOfBody(t, f.server, "2026-09-28", "12:00:00", "UTC",
		url.Values{"reveal": {"1"}})
	assert.Contains(t, revealed, "the api key is hunter2",
		"the view's own toggle must reveal it")
}

// TestAsOfScopeFiltersComposeWithAND pins that the four scope fields narrow
// together: a record matching only one of two given dimensions is not shown.
func TestAsOfScopeFiltersComposeWithAND(t *testing.T) {
	f := newFixture(t)

	// The fixture: two records at u1/a1, one at u1 with no agent, one at u2.
	both := asOfBody(t, f.server, "2026-09-28", "12:00:00", "UTC",
		url.Values{"user_id": {"u1"}, "agent_id": {"a1"}})
	assert.Equal(t, []string{"fixture-observed", "fixture-observed-plain"}, rowIDs(parseRows(t, both)),
		"only records matching BOTH dimensions may be shown")

	// u1 alone is wider, so the reconstructed record (u1, no agent) joins.
	userOnly := asOfBody(t, f.server, "2026-09-28", "12:00:00", "UTC",
		url.Values{"user_id": {"u1"}})
	assert.Contains(t, rowIDs(parseRows(t, userOnly)), "fixture-reconstructed",
		"one dimension alone must not narrow as far as two")

	// A dimension that nothing matches yields nothing, not everything.
	none := asOfBody(t, f.server, "2026-09-28", "12:00:00", "UTC",
		url.Values{"user_id": {"u1"}, "app_id": {"nobody"}})
	assert.Empty(t, parseRows(t, none), "an unmatched dimension must exclude, not be ignored")
}
