package serve

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/ledger"
	"notary/internal/store"
)

// TestOverviewCardStatesAreDistinct pins the three states the front page can be
// in, and that each says something the other two cannot.
//
// The integrity of this page is that a reader can tell "checked and fine" from
// "checked and broken" from "not checked at all". If any two rendered the same,
// the page would be answering the wrong question -- and the failure mode that
// matters is the third one collapsing into the first.
func TestOverviewCardStatesAreDistinct(t *testing.T) {
	fClean := newFixture(t)
	fTampered := newFixture(t)

	// The same out-of-band edit the /verify tests use: change a stored event so
	// the record's hash no longer recomputes.
	tamperStoredEvent(t, filepath.Join(fTampered.dir, "ledger.db"),
		`UPDATE records SET event = 'memory_kept' WHERE seq = 3`)

	intact := get(t, fClean.server.Handler(), "/")
	require.Equal(t, http.StatusOK, intact.Code)
	intactBody := intact.Body.String()

	broken := get(t, fTampered.server.Handler(), "/")
	require.Equal(t, http.StatusOK, broken.Code)
	brokenBody := broken.Body.String()

	unverified := newUnverifiedServer(t, fClean)
	unverifiedBody := get(t, unverified.Handler(), "/").Body.String()

	assert.Contains(t, intactBody, "No tampering found",
		"the intact state must give its verdict in words a reviewer can act on")
	assert.Contains(t, brokenBody, "break(s) found",
		"the broken state must say how many breaks")
	assert.Contains(t, unverifiedBody, "No trusted keys are configured",
		"the unverified state must say that signatures were not checked")

	// Each state must also be visibly different from the other two, not merely
	// carry a different sentence somewhere.
	assert.NotEqual(t, intactBody, brokenBody, "intact and broken must not render alike")
	assert.NotEqual(t, intactBody, unverifiedBody, "intact and not-verified must not render alike")
	assert.NotEqual(t, brokenBody, unverifiedBody, "broken and not-verified must not render alike")

	// Every state carries the command that reproduces it. A verdict a reader
	// cannot re-run is a claim, not a result.
	for name, body := range map[string]string{
		"intact": intactBody, "broken": brokenBody, "not verified": unverifiedBody,
	} {
		assert.Contains(t, body, "notary verify", "the %s card must carry the re-run command", name)
	}

	// The broken card must route the reader to the detail rather than leaving
	// them to find it.
	assert.Contains(t, brokenBody, `href="/verify"`, "the broken card must link to the chain page")
}

// TestOverviewBreakCountMatchesTheChainVerdict is the differential: the number the
// card states must be the number the shared verdict reports. The card is a second
// presentation of one answer, never a second answer.
func TestOverviewBreakCountMatchesTheChainVerdict(t *testing.T) {
	f := newFixture(t)
	tamperStoredEvent(t, filepath.Join(f.dir, "ledger.db"),
		`UPDATE records SET event = 'memory_kept' WHERE seq = 3`)

	verdict, err := f.server.loadChain()
	require.NoError(t, err)
	require.Equal(t, chainBroken, verdict.State, "the fixture must be broken for this test to mean anything")
	require.NotEmpty(t, verdict.Breaks)

	body := get(t, f.server.Handler(), "/").Body.String()
	assert.Contains(t, body, fmt.Sprintf("%d break(s) found", len(verdict.Breaks)),
		"the card must state the count the shared verdict computed, not one of its own")
}

// TestOverviewLedgerFactsComeFromTheLedger pins the facts section against the
// ledger itself: the count, the span and the tier split are the ledger's, and the
// three tiers are named in words rather than by their ledger identifiers.
func TestOverviewLedgerFactsComeFromTheLedger(t *testing.T) {
	f := newFixture(t)

	stats, err := f.store.LedgerStats()
	require.NoError(t, err)
	require.Equal(t, 4, stats.Count, "the fixture seeds four records")

	body := get(t, f.server.Handler(), "/").Body.String()

	assert.Contains(t, body, "4 records", "the count must be the ledger's own")
	assert.Contains(t, body, "Seen directly", "the observed tier is named in words")
	assert.Contains(t, body, "Inferred", "the reconstructed tier is named in words")
	assert.Contains(t, body, "Not visible to Notary", "the internal tier is named in words")

	// The ledger's own identifiers must not leak into a reader-facing label,
	// though the commands named beside the figures may appear in code text.
	assert.NotContains(t, body, ">Reconstructed<", "the page must not label a tier with its wire name")
}

// TestOverviewEmptyLedgerShowsTheFirstRunNote pins the state a reviewer is most
// likely to meet first: a console on a machine nobody has connected yet.
//
// It must explain itself and offer something actionable, and it must NOT render an
// empty facts table -- a row of zeroes reads as a finding, and "0 records" is not
// the same statement as "nothing has been recorded yet".
func TestOverviewEmptyLedgerShowsTheFirstRunNote(t *testing.T) {
	srv := newEmptyServer(t)

	rr := get(t, srv.Handler(), "/")
	require.Equal(t, http.StatusOK, rr.Code, "an empty ledger must still render the front page")
	body := rr.Body.String()

	assert.Contains(t, body, "nothing is recording into this ledger yet",
		"the page must say why it is empty in plain words")
	assert.Contains(t, body, "NOTARY_DB_PATH", "the note must name the ledger path to set")
	assert.Contains(t, body, "notary proxy", "the note must name how to connect an application")
	assert.Contains(t, body, "notary doctor", "the note must name how to produce the key pair")

	assert.NotContains(t, body, "Seen directly",
		"an empty ledger must show the first-run state, not a facts table of zeroes")
}

// TestOverviewNoteEscapesAHostileLedgerPath pins the one value the Overview
// interpolates into text a reader will copy and send on.
//
// The ledger path comes from configuration, so it is not trusted input: markup in
// it must render as literal text inside the note. A forwarded note that executed
// something would be the worst possible place for it, because the note's whole
// purpose is to travel out of the browser.
func TestOverviewNoteEscapesAHostileLedgerPath(t *testing.T) {
	const hostile = `<script>alert(1)</script>.db`

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	srv, err := New(Options{
		Ledger:     ledger.New(st, nil, fixedClock),
		Store:      st,
		GapLogPath: filepath.Join(dir, "gaps.log"),
		LedgerPath: hostile,
		Now:        fixedClock,
	})
	require.NoError(t, err)

	body := get(t, srv.Handler(), "/").Body.String()
	require.Contains(t, body, "nothing is recording into this ledger yet",
		"the empty state must render for this test to mean anything")

	assert.NotContains(t, body, hostile, "the configured path must not reach the page as markup")
	assert.NotContains(t, body, "<script>alert(1)", "no script element may be built from a configured value")
	assert.Contains(t, body, "&lt;script&gt;", "the path must render as escaped text")
}
