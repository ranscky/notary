package serve

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // registers the "sqlite" driver used for raw tampering

	"notary/internal/gap"
	"notary/internal/ledger"
	"notary/internal/record"
)

// writeGapLog writes entries to the gap log at path through gap.Open, the same
// path the ledger package's gap-log fixtures use. The caller never supplies
// chain fields: Record assigns Counter, PrevHash and Hash.
func writeGapLog(t *testing.T, path string, entries ...gap.Entry) {
	t.Helper()
	g, err := gap.Open(path)
	require.NoError(t, err)
	for _, e := range entries {
		require.NoError(t, g.Record(e))
	}
	require.NoError(t, g.Close())
}

// tamperStoredEvent opens the ledger file with a second connection and runs one
// statement against it -- the out-of-band edit an attacker with file access
// would make (testdata/tamper/README.md). Writing through raw SQL rather than
// through the ledger is the point: the store never sees this edit and cannot
// refuse it.
func tamperStoredEvent(t *testing.T, dbPath, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(stmt)
	require.NoError(t, err)
}

// newUnverifiedServer builds a second Server over f's ledger, store and gap log
// with NO verifier -- the configuration a command has when no trusted keyring
// loaded. Its chain view is the "could not verify" state. It is a helper
// because two tests need it; it deliberately shares f's ledger so it reads the
// very records the fixture signed.
func newUnverifiedServer(t *testing.T, f fixture) *Server {
	t.Helper()
	srv, err := New(Options{
		Ledger:      f.ledger,
		Store:       f.store,
		GapLogPath:  f.gapPath,
		KeyringPath: f.server.keyringPath,
		Now:         fixedClock,
	})
	require.NoError(t, err)
	return srv
}

// assertGapsMatchTheSharedReport pins that f's server renders exactly the report
// ledger.OutstandingGaps produces over the same store and gap log, field for
// field, and that OK is true exactly when both lists are empty.
func assertGapsMatchTheSharedReport(t *testing.T, f fixture, wantOK bool) {
	t.Helper()

	got, err := f.server.loadGaps()
	require.NoError(t, err)

	want, err := ledger.OutstandingGaps(f.store, f.gapPath)
	require.NoError(t, err)

	assert.Equal(t, want.Unreconciled, got.Unreconciled,
		"the view's unreconciled entries must equal the shared report's")
	assert.Equal(t, want.Integrity, got.Integrity,
		"the view's integrity breaks must equal the shared report's")
	assert.Equal(t, len(want.Unreconciled) == 0 && len(want.Integrity) == 0, got.OK,
		"OK must be the derived fact that BOTH lists are empty, never stored independently")
	assert.Equal(t, wantOK, got.OK, "the fixture must produce the expected OK")
	if wantOK {
		assert.Empty(t, got.Unreconciled, "the intact fixture has nothing unreconciled")
		assert.Empty(t, got.Integrity, "the intact fixture's gap log has no integrity breaks")
	}
}

// TestLoadGapsMatchesTheSharedReport is the differential for the gap view: for
// three gap-log states -- absent, an entry matching no record, and a rewritten
// line -- f.server.loadGaps() must equal ledger.OutstandingGaps(st, gapPath)
// field for field, so the page and `notary gaps` cannot disagree about the same
// log. OK must be true only for the intact one.
func TestLoadGapsMatchesTheSharedReport(t *testing.T) {
	t.Run("intact", func(t *testing.T) {
		f := newFixture(t)

		// No gap log is written: a missing log is an empty log, so nothing is
		// outstanding and OK is true.
		assertGapsMatchTheSharedReport(t, f, true)
	})

	t.Run("entry-matching-no-record", func(t *testing.T) {
		f := newFixture(t)

		// Same kind and scope as the fixture's records, so the ONLY thing
		// keeping this entry from being accounted for is its correlation ID.
		writeGapLog(t, f.gapPath, gap.Entry{
			At:            serveFixedNow,
			Kind:          record.EventMemorySurfaced,
			Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
			CorrelationID: "rec-missing",
			Detail:        "memory store unreachable",
		})

		assertGapsMatchTheSharedReport(t, f, false)
	})

	t.Run("corrupt-log", func(t *testing.T) {
		f := newFixture(t)

		// The entry matches a stored record (so nothing is unreconciled), then
		// its Detail is rewritten without recomputing its hash: the line still
		// decodes, so gap.Read reads it back, but its stored hash no longer
		// matches -- exactly the rewrite the cross-check cannot see and
		// gap.Verify must surface.
		writeGapLog(t, f.gapPath, gap.Entry{
			At:            serveFixedNow,
			Kind:          record.EventMemorySurfaced,
			Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
			CorrelationID: "fixture-observed",
			Detail:        "ok",
		})

		raw, err := os.ReadFile(f.gapPath)
		require.NoError(t, err)
		tampered := bytes.Replace(raw, []byte(`"detail":"ok"`), []byte(`"detail":"tampered"`), 1)
		require.NotEqual(t, raw, tampered, "the fixture detail must appear verbatim for the rewrite to bite")
		require.NoError(t, os.WriteFile(f.gapPath, tampered, 0o600))

		assertGapsMatchTheSharedReport(t, f, false)
	})
}

// TestChainStateDistinguishesTheThreeStates is the load-bearing chain test: the
// three states must never collapse. A server with no verifier must report
// chainNotVerified with no breaks; a verified fixture must report chainClean; a
// verified but tampered fixture must report chainBroken naming the exact record
// id and field.
//
// Two fixtures are built -- one left intact, one tampered -- so the intact
// verdict is never read from a store a tamper already touched.
func TestChainStateDistinguishesTheThreeStates(t *testing.T) {
	fClean := newFixture(t)
	fTampered := newFixture(t)

	// Change a record's event out of band. The event is part of the record
	// digest, so the stored hash no longer recomputes -- a "hash" break on
	// exactly that record. seq 3 is the fixture's fourth record,
	// "fixture-internal".
	tamperStoredEvent(t, filepath.Join(fTampered.dir, "ledger.db"),
		`UPDATE records SET event = 'memory_kept' WHERE seq = 3`)

	unverified := newUnverifiedServer(t, fClean)

	t.Run("no verifier", func(t *testing.T) {
		view, err := unverified.loadChain()
		require.NoError(t, err)
		assert.Equal(t, chainNotVerified, view.State,
			"a nil verifier trusts no keys, so the check did not run")
		assert.Empty(t, view.Breaks, "no check ran, so there are no breaks")
		assert.Equal(t, serveFixedNow, view.AsOf, "AsOf is the instant the check would run")
		assert.Equal(t, fClean.server.keyringPath, view.KeyringPath,
			"the not-verified state must carry the keyring path it tells the operator to configure")
	})

	t.Run("intact verified", func(t *testing.T) {
		view, err := fClean.server.loadChain()
		require.NoError(t, err)
		assert.Equal(t, chainClean, view.State, "an intact fixture must verify clean")
		assert.Empty(t, view.Breaks, "an intact fixture has no breaks")
		assert.Equal(t, serveFixedNow, view.AsOf)
	})

	t.Run("tampered verified", func(t *testing.T) {
		view, err := fTampered.server.loadChain()
		require.NoError(t, err)
		assert.Equal(t, chainBroken, view.State, "an edited record must break the chain")
		require.Len(t, view.Breaks, 1, "editing one record's event must yield exactly one break")
		assert.Equal(t, record.RecordID("fixture-internal"), view.Breaks[0].RecordID,
			"the break must name the exact record id")
		assert.Equal(t, "hash", view.Breaks[0].Field,
			"editing the event breaks the record hash")
	})
}

// TestChainStateIsNeverCleanWithoutAKeyring pins the guarantee the third state
// exists for: a chain that could not be signature-checked must never render as
// verified. The no-verifier state carries no breaks AND is not chainClean, so
// "could not verify" can never be shown as "verified".
func TestChainStateIsNeverCleanWithoutAKeyring(t *testing.T) {
	f := newFixture(t)
	unverified := newUnverifiedServer(t, f)

	view, err := unverified.loadChain()
	require.NoError(t, err)

	assert.Empty(t, view.Breaks, "no check ran, so there are no breaks to show")
	assert.NotEqual(t, chainClean, view.State,
		"a chain that was never signature-checked must not render as verified")
	assert.Equal(t, chainNotVerified, view.State)
}

// TestChainStateTreatsAnEmptyKeyringAsNoKeyring pins the guarantee this fix
// round added to the package's contract: a Server built with a NON-nil but
// EMPTY keyring must report chainNotVerified with no breaks, NEVER chainBroken.
// A verifier over an empty keyring trusts no keys and reports every record of a
// perfectly healthy ledger as a signature break, so the empty keyring must take
// the same "the check did not run" path as a nil one.
//
// This test could not be written against the previous interface: Options took a
// *sign.Verifier, and sign.NewVerifier always returns a non-nil Verifier even
// for a nil or empty keyring, so New would have accepted sign.NewVerifier(empty)
// and loadChain would have accused the ledger. The guard now lives in New,
// which derives a verifier only when the keyring holds at least one key, so the
// dangerous verifier can no longer be constructed through this package at all.
//
// (The empty keyring is reachable only via an explicitly empty map: a
// comment-only keys file makes sign.LoadTrustedKeys return an error, not an
// empty map, so a real command would surface that error before New is reached.)
func TestChainStateTreatsAnEmptyKeyringAsNoKeyring(t *testing.T) {
	f := newFixture(t)

	cases := map[string]map[string]ed25519.PublicKey{
		"nil keyring":   nil,
		"empty keyring": {},
	}
	for name, keyring := range cases {
		t.Run(name, func(t *testing.T) {
			srv, err := New(Options{
				Ledger:      f.ledger,
				Store:       f.store,
				GapLogPath:  f.gapPath,
				Keyring:     keyring,
				KeyringPath: f.server.keyringPath,
				Now:         fixedClock,
			})
			require.NoError(t, err)

			view, err := srv.loadChain()
			require.NoError(t, err)
			assert.Equal(t, chainNotVerified, view.State)
			assert.Empty(t, view.Breaks, "a keyless check must never produce a break list")
			assert.NotEqual(t, chainBroken, view.State,
				"a keyless verifier must never accuse a healthy ledger of tampering")
		})
	}
}
