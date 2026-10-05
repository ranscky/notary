package serve

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/export"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// serveTestSeed is a fixed 32-byte ed25519 seed, mirroring the ledger, sign,
// interceptor and cmd/notary fixtures so every suite signs with the same
// deterministic identity and no key is ever generated.
var serveTestSeed = []byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

// serveTestKeyEnv names the environment variable these tests hold the base64
// seed in, following the sign package's KeySource shape.
const serveTestKeyEnv = "NOTARY_SERVE_TEST_KEY"

// serveFixedNow is the deterministic clock the fixture ledger and server are
// given, so every instant a view prints is stable across runs.
var serveFixedNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// fixedClock is the fixture's clock, in the shape ledger.New and Options.Now
// both expect.
func fixedClock() time.Time { return serveFixedNow }

// fixtureAt is the event time of the i-th seeded record: a distinct instant a
// day apart, all inside the default (last-30-days) window of serveFixedNow.
func fixtureAt(i int) time.Time { return serveFixedNow.AddDate(0, 0, i-8) }

// newTestSigner builds a Signer from serveTestSeed through the env KeySource and
// returns the matching public key, following newVerifySigner and newSigner
// exactly: base64 seed through a KeySource, and the public half derived from
// the seed rather than generated.
func newTestSigner(t *testing.T) (*sign.Signer, ed25519.PublicKey) {
	t.Helper()
	t.Setenv(serveTestKeyEnv, base64.StdEncoding.EncodeToString(serveTestSeed))
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: serveTestKeyEnv})
	require.NoError(t, err)
	pub := ed25519.NewKeyFromSeed(serveTestSeed).Public().(ed25519.PublicKey)
	return sg, pub
}

// hash32 builds a distinct, non-zero 32-byte hash from a seed byte.
func hash32(seed byte) record.Hash {
	var h record.Hash
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// appendRecord appends rec to l and fails the test on any error, so a fixture
// is either wholly built or the test stops.
func appendRecord(t *testing.T, l *ledger.Ledger, rec record.Record) {
	t.Helper()
	_, err := l.Append(rec)
	require.NoError(t, err)
}

// contentRecord builds a well-formed Observed record carrying content: the
// event is memory_surfaced with reason returned_by_search, so its sentence is
// "the memory was returned by a search". sensitive marks the content so a
// redaction test has something to withhold. It is the shared fixture for the
// content-carrying shape; a no-content record is built inline by newFixture.
func contentRecord(t *testing.T, id record.RecordID, at time.Time, scope record.Scope, memoryID, text string, sensitive bool) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"ok":true}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonReturnedBySearch, ev)
	require.NoError(t, err)
	rec := record.Record{
		ID:     id,
		At:     at,
		Event:  record.EventMemorySurfaced,
		Reason: reason,
		Subject: record.Subject{
			MemoryID:    memoryID,
			Scope:       scope,
			ContentHash: hash32(0x40),
		},
		Content: &record.Content{Text: text, Sensitive: sensitive},
	}
	require.NoError(t, rec.Validate())
	return rec
}

// fixture is the shared ledger + server a view test runs against. Tasks 3, 4
// and 5 reuse newFixture rather than redeclaring this or any helper above:
// in one package a second helper of the same name is a duplicate symbol.
type fixture struct {
	server  *Server
	ledger  *ledger.Ledger
	store   *store.SQLiteStore
	signer  *sign.Signer
	dir     string
	gapPath string
	pub     ed25519.PublicKey
}

// newFixture builds the shared fixture: a temp-dir SQLite ledger signed with a
// fixed key, seeded with one record per fact the console must show, and a
// *Server built through New over it.
//
// The seeded set (all At inside the default last-30-days window) is:
//
//   - "fixture-observed"       Observed, sensitive content "the api key is
//     hunter2", memory "mem-alpha", scope u1/a1.
//   - "fixture-observed-plain" Observed, plain content "a benign note", memory
//     "mem-alpha", scope u1/a1.
//   - "fixture-reconstructed"  Reconstructed, NO content, memory "mem-beta",
//     scope u1.
//   - "fixture-internal"       Internal, NO content, memory "" (the store's
//     memory_id DEFAULT), scope u2.
//
// So the set covers all three tiers, a sensitive record, a content-free
// record, and a record whose subject names no memory.
func newFixture(t *testing.T) fixture {
	t.Helper()
	sg, pub := newTestSigner(t)

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	l := ledger.New(st, sg, fixedClock)
	gapPath := filepath.Join(dir, "gaps.log")

	appendRecord(t, l, contentRecord(t, "fixture-observed", fixtureAt(0),
		record.Scope{UserID: "u1", AgentID: "a1"}, "mem-alpha", "the api key is hunter2", true))
	appendRecord(t, l, contentRecord(t, "fixture-observed-plain", fixtureAt(1),
		record.Scope{UserID: "u1", AgentID: "a1"}, "mem-alpha", "a benign note", false))

	rev, err := record.NewReconstructedEvidence([]record.RecordID{"fixture-observed"}, "fixture-rule", "v1", 0)
	require.NoError(t, err)
	rreason, err := record.NewReconstructedReason(record.ReasonAbsentFromSearch, rev)
	require.NoError(t, err)
	appendRecord(t, l, record.Record{
		ID:      "fixture-reconstructed",
		At:      fixtureAt(2),
		Event:   record.EventMemoryDropped,
		Reason:  rreason,
		Subject: record.Subject{MemoryID: "mem-beta", Scope: record.Scope{UserID: "u1"}, ContentHash: hash32(0x42)},
	})

	note, err := record.NewInternalNote("fixture internal note")
	require.NoError(t, err)
	ireason, err := record.NewInternalReason(record.ReasonRemovedByMem0, note)
	require.NoError(t, err)
	appendRecord(t, l, record.Record{
		ID:      "fixture-internal",
		At:      fixtureAt(3),
		Event:   record.EventMemoryDropped,
		Reason:  ireason,
		Subject: record.Subject{Scope: record.Scope{UserID: "u2"}, ContentHash: hash32(0x43)},
	})

	// A keyring the chain view can verify against, and the file the banner
	// names as the re-run command's --keyring. The map is keyed by the signer's
	// own KeyID, so the verifier New derives from it trusts exactly the key the
	// fixture signs with.
	keyringPath := filepath.Join(dir, "trusted.keys")
	require.NoError(t, os.WriteFile(keyringPath,
		[]byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644))

	srv, err := New(Options{
		Ledger:      l,
		Store:       st,
		GapLogPath:  gapPath,
		Keyring:     map[string]ed25519.PublicKey{sg.KeyID(): pub},
		KeyringPath: keyringPath,
		Reveal:      io.Discard,
		Now:         fixedClock,
	})
	require.NoError(t, err)

	return fixture{server: srv, ledger: l, store: st, signer: sg, dir: dir, gapPath: gapPath, pub: pub}
}

// defaultFilter is the filter a first visit produces: the default window of the
// fixture clock, no scope, redacted.
func defaultFilter() filter {
	return filter{From: serveFixedNow.AddDate(0, 0, -30), To: serveFixedNow}
}

// decodeLines parses the JSONL an export writes into the Lines it encoded.
func decodeLines(t *testing.T, data []byte) []export.Line {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(data))
	var lines []export.Line
	for {
		var line export.Line
		err := dec.Decode(&line)
		if errors.Is(err, io.EOF) {
			return lines
		}
		require.NoError(t, err)
		lines = append(lines, line)
	}
}

// TestLoadRecordsMatchesTheExportPath is the differential: for the same range
// and scope, the console's rows must equal, field for field, the lines the
// CLI's own export path writes. It is what proves the console cannot drift
// from `notary export`, for both the redacted and the revealed render and for
// a narrowed scope.
func TestLoadRecordsMatchesTheExportPath(t *testing.T) {
	f := newFixture(t)

	cases := []struct {
		name   string
		reveal bool
		scope  record.Scope
	}{
		{"redacted", false, record.Scope{}},
		{"revealed", true, record.Scope{}},
		// Scope narrowing runs after the range read, exactly as the exporter
		// narrows, so a scoped request must also agree -- including a scope
		// that matches only some of the fixture's records.
		{"scoped-to-u1", false, record.Scope{UserID: "u1"}},
		{"scoped-to-a1-revealed", true, record.Scope{AgentID: "a1"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fl := defaultFilter()
			fl.Reveal = tc.reveal
			fl.Scope = tc.scope

			rows, err := f.server.loadRecords(fl)
			require.NoError(t, err)

			var buf bytes.Buffer
			_, err = export.New(f.ledger).Export(context.Background(), export.Request{
				From:             fl.From,
				To:               fl.To,
				Scope:            fl.Scope,
				IncludeSensitive: fl.Reveal,
			}, &buf)
			require.NoError(t, err)
			want := decodeLines(t, buf.Bytes())

			require.Len(t, rows, len(want), "row count must match the export's line count")
			for i := range want {
				assert.Equal(t, want[i], rows[i].Line,
					"row %d (%s) must equal the export line", i, rows[i].Line.ID)
			}
		})
	}
}

// TestLoadRecordsPreservesLedgerOrder pins that the rows are in the ledger's
// read order (At ascending), not re-sorted by the view.
func TestLoadRecordsPreservesLedgerOrder(t *testing.T) {
	f := newFixture(t)

	rows, err := f.server.loadRecords(defaultFilter())
	require.NoError(t, err)

	got := make([]record.RecordID, len(rows))
	for i, r := range rows {
		got[i] = r.Line.ID
	}
	want := []record.RecordID{
		"fixture-observed",
		"fixture-observed-plain",
		"fixture-reconstructed",
		"fixture-internal",
	}
	assert.Equal(t, want, got)
}

// TestLoadRecordRefusesAnEmptyID pins that a missing record id is a usage
// error, returned BEFORE the store is read, so an empty filter can never list
// the ledger.
func TestLoadRecordRefusesAnEmptyID(t *testing.T) {
	f := newFixture(t)

	_, err := f.server.loadRecord("", false)
	assert.ErrorIs(t, err, ErrMissingID)
}

// TestLoadMemoryRefusesAnEmptyID pins that an empty memory id is a usage error,
// returned before the store is read -- the guard explain also owns, because the
// store matches the empty memory_id column's DEFAULT.
func TestLoadMemoryRefusesAnEmptyID(t *testing.T) {
	f := newFixture(t)

	_, err := f.server.loadMemory("", false)
	assert.ErrorIs(t, err, ErrMissingID)
}

// TestLoadRecordReportsAMissingIDAsNotFound pins that an unknown record id
// stays distinguishable from a usage error: store.ErrNotFound flows through the
// error chain.
func TestLoadRecordReportsAMissingIDAsNotFound(t *testing.T) {
	f := newFixture(t)

	_, err := f.server.loadRecord("no-such-record", false)
	require.Error(t, err)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.NotErrorIs(t, err, ErrMissingID)
}

// TestBadgeForNamesEachTier pins the exact name and class each tier maps to.
func TestBadgeForNamesEachTier(t *testing.T) {
	cases := []struct {
		tier record.VisibilityTier
		want tierBadge
	}{
		{record.Observed, tierBadge{Name: "Observed", Class: "tier-observed"}},
		{record.Reconstructed, tierBadge{Name: "Reconstructed", Class: "tier-reconstructed"}},
		{record.Internal, tierBadge{Name: "Internal", Class: "tier-internal"}},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, badgeFor(tc.tier), "tier %s", tc.tier)
	}
}

// TestMemoryHrefIsEmptyWhenTheRecordNamesNoMemory pins that a record whose
// subject names no memory (the store's memory_id DEFAULT "") gets no memory
// link, while every other record does.
func TestMemoryHrefIsEmptyWhenTheRecordNamesNoMemory(t *testing.T) {
	f := newFixture(t)

	rows, err := f.server.loadRecords(defaultFilter())
	require.NoError(t, err)
	require.NotEmpty(t, rows)

	var sawNoMemory bool
	for _, r := range rows {
		if r.Line.ID == "fixture-internal" {
			sawNoMemory = true
			assert.Empty(t, r.MemoryHref, "a record naming no memory must have no memory href")
			continue
		}
		assert.NotEmpty(t, r.MemoryHref, "record %s names a memory and must link to it", r.Line.ID)
	}
	require.True(t, sawNoMemory, "the fixture must contain the no-memory record")
}

// TestLoadRecordAndMemoryRedactByDefaultAndRevealOnRequest pins the one
// redaction rule: a sensitive record's text is absent and Redacted is
// "sensitive" by default, present with reveal, and NO hash field differs
// between the two renders because redaction was never in the hash.
func TestLoadRecordAndMemoryRedactByDefaultAndRevealOnRequest(t *testing.T) {
	f := newFixture(t)

	redacted, err := f.server.loadRecord("fixture-observed", false)
	require.NoError(t, err)
	assert.Equal(t, "sensitive", redacted.Line.Redacted)
	assert.Nil(t, redacted.Line.Content, "sensitive content must be withheld by default")

	revealed, err := f.server.loadRecord("fixture-observed", true)
	require.NoError(t, err)
	assert.Empty(t, revealed.Line.Redacted)
	require.NotNil(t, revealed.Line.Content, "reveal must show the content")
	assert.Equal(t, "the api key is hunter2", *revealed.Line.Content)

	// No hash-bearing field may differ between the two renders.
	assert.Equal(t, redacted.Line.PrevHash, revealed.Line.PrevHash)
	assert.Equal(t, redacted.Line.Hash, revealed.Line.Hash)
	assert.Equal(t, redacted.Line.Signature, revealed.Line.Signature)

	// The memory view behaves the same way for the same record.
	redactedMem, err := f.server.loadMemory("mem-alpha", false)
	require.NoError(t, err)
	revealedMem, err := f.server.loadMemory("mem-alpha", true)
	require.NoError(t, err)
	require.Len(t, redactedMem, len(revealedMem))

	sensitiveRow := -1
	for i, r := range redactedMem {
		if r.Line.ID == "fixture-observed" {
			sensitiveRow = i
		}
	}
	require.NotEqual(t, -1, sensitiveRow, "the memory view must contain the sensitive record")
	assert.Equal(t, "sensitive", redactedMem[sensitiveRow].Line.Redacted)
	assert.Nil(t, redactedMem[sensitiveRow].Line.Content)
	require.NotNil(t, revealedMem[sensitiveRow].Line.Content)
	assert.Equal(t, "the api key is hunter2", *revealedMem[sensitiveRow].Line.Content)
	assert.Equal(t, redactedMem[sensitiveRow].Line.Hash, revealedMem[sensitiveRow].Line.Hash)
}

// TestParseFilterDefaultsToTheLastThirtyDays pins the default window and the
// other defaults: no scope, redacted. It also checks the reveal flag and the
// scope fields are read from the query.
func TestParseFilterDefaultsToTheLastThirtyDays(t *testing.T) {
	f := newFixture(t)

	fl, err := f.server.parseFilter(url.Values{})
	require.NoError(t, err)
	assert.Equal(t, serveFixedNow.Add(-30*24*time.Hour), fl.From)
	assert.Equal(t, serveFixedNow, fl.To)
	assert.Equal(t, record.Scope{}, fl.Scope)
	assert.False(t, fl.Reveal)

	fl, err = f.server.parseFilter(url.Values{
		"reveal":   {"1"},
		"user_id":  {"u1"},
		"agent_id": {"a1"},
	})
	require.NoError(t, err)
	assert.True(t, fl.Reveal)
	assert.Equal(t, record.Scope{UserID: "u1", AgentID: "a1"}, fl.Scope)
}

// TestParseFilterAcceptsDateOnlyBoundsAtDayEdges pins that a bare date is a day
// edge: a date-only from is 00:00:00Z, and a date-only to is the END of that
// day, so a record at 23:59:59Z that day is included. It also pins that an
// RFC3339 bound is accepted.
func TestParseFilterAcceptsDateOnlyBoundsAtDayEdges(t *testing.T) {
	f := newFixture(t)

	fl, err := f.server.parseFilter(url.Values{"from": {"2026-09-20"}, "to": {"2026-09-20"}})
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC), fl.From)
	assert.Equal(t, time.Date(2026, 9, 20, 23, 59, 59, 999999999, time.UTC), fl.To)

	// A record at the very last second of the named day must be inside the
	// date-only window, which is what "end of that day" means to the store's
	// inclusive ListRecords.
	appendRecord(t, f.ledger, contentRecord(t, "fixture-late",
		time.Date(2026, 9, 20, 23, 59, 59, 0, time.UTC),
		record.Scope{UserID: "u1"}, "mem-alpha", "late", false))
	rows, err := f.server.loadRecords(fl)
	require.NoError(t, err)
	ids := make([]record.RecordID, len(rows))
	for i, r := range rows {
		ids[i] = r.Line.ID
	}
	assert.Contains(t, ids, record.RecordID("fixture-late"))

	// An RFC3339 bound is parsed as the instant it names.
	rfc, err := f.server.parseFilter(url.Values{
		"from": {"2026-09-20T01:00:00Z"},
		"to":   {"2026-09-21T01:00:00Z"},
	})
	require.NoError(t, err)
	assert.Equal(t, time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC), rfc.From)
	assert.Equal(t, time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC), rfc.To)
}

// TestParseFilterRejectsAReversedRange pins that a to before a from is an
// error naming both bounds.
func TestParseFilterRejectsAReversedRange(t *testing.T) {
	f := newFixture(t)

	_, err := f.server.parseFilter(url.Values{
		"from": {"2026-09-25T00:00:00Z"},
		"to":   {"2026-09-20T00:00:00Z"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "2026-09-25")
	assert.Contains(t, err.Error(), "2026-09-20")
}

// TestParseFilterRejectsASpanOverTheCap pins that a span wider than the cap
// export applies is an error naming that same cap, export.DefaultMaxSpan -- so
// the console's limit and the CLI's cannot drift.
func TestParseFilterRejectsASpanOverTheCap(t *testing.T) {
	f := newFixture(t)

	from := serveFixedNow.Add(-export.DefaultMaxSpan - 24*time.Hour)
	_, err := f.server.parseFilter(url.Values{
		"from": {from.Format(time.RFC3339)},
		"to":   {serveFixedNow.Format(time.RFC3339)},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), export.DefaultMaxSpan.String())

	// A span exactly at the cap is allowed, so the cap is inclusive.
	atCap := serveFixedNow.Add(-export.DefaultMaxSpan)
	fl, err := f.server.parseFilter(url.Values{
		"from": {atCap.Format(time.RFC3339)},
		"to":   {serveFixedNow.Format(time.RFC3339)},
	})
	require.NoError(t, err)
	assert.Equal(t, atCap, fl.From)
}
