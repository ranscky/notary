package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/ledger"
	"notary/internal/mem0"
	"notary/internal/reconcile"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// newTestReconcileCmd builds a reconcile command whose output is captured in a
// buffer, so a test can assert on the printed report, not only on the returned
// error. It mirrors newTestVerifyCmd / newTestGapsCmd.
func newTestReconcileCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := newReconcileCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

// eventStatusServer starts an httptest server that answers every event-status
// request with resp, so a command test can drive the reconciler without a
// network or a real API key. It follows the conventions in internal/reconcile:
// no test touches the real Mem0 API.
func eventStatusServer(t *testing.T, resp mem0.EventStatusResponse) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// errorStatusServer starts an httptest server that answers every request with a
// non-2xx status, so a test can prove a Mem0 outage fails the pass loudly.
func errorStatusServer(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"mem0 outage"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// addRequestedRecord builds a valid add_requested record whose Observed
// evidence is the AddPayload the interceptor writes, so the reconciler's fold
// treats it as an unresolved add -- the cheapest way to give a pass something
// to do, and so the strongest way to prove a pass wrote nothing.
func addRequestedRecord(t *testing.T, id, eventID string, at time.Time, scope record.Scope) record.Record {
	t.Helper()
	payload, err := json.Marshal(mem0.AddPayload{EventID: eventID, Status: "PENDING"})
	require.NoError(t, err)
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, payload)
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonAddAcknowledged, ev)
	require.NoError(t, err)
	rec := record.Record{
		ID:      record.RecordID(id),
		At:      at,
		Event:   record.EventAddRequested,
		Reason:  reason,
		Subject: record.Subject{Scope: scope, ContentHash: verifyHash(0x11)},
	}
	require.NoError(t, rec.Validate())
	return rec
}

// appendFixtureRecords appends recs to a fresh SQLite ledger at dbPath and
// closes the store, so the command under test reopens it cleanly. RecordedAt is
// only stamped when the record leaves it zero, so a fixture that sets it keeps
// it.
func appendFixtureRecords(t *testing.T, dbPath string, sg *sign.Signer, recs ...record.Record) {
	t.Helper()
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	l := ledger.New(st, sg, func() time.Time { return verifyFixedNow })
	for _, rec := range recs {
		_, err := l.Append(rec)
		require.NoError(t, err)
	}
}

// ledgerSnapshot opens dbPath read-only and returns its head record (ok false
// when empty) and the number of stored rows, so a test can prove a pass wrote
// nothing.
func ledgerSnapshot(t *testing.T, dbPath string) (record.Record, bool, int) {
	t.Helper()
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	head, ok, err := st.Head()
	require.NoError(t, err)
	entries, err := st.SeqEntries()
	require.NoError(t, err)
	return head, ok, len(entries)
}

// ---------------------------------------------------------------------------
// Spec §8: a missing signing key is a refusal to start, not a degraded mode.
// ---------------------------------------------------------------------------

// TestReconcileRequiresASigningKey pins that config time with no signing key is
// a hard failure. The command writes SIGNED records, so it must refuse to start
// rather than sign with an absent (or throwaway) key.
func TestReconcileRequiresASigningKey(t *testing.T) {
	base := &config.Config{
		DBPath:        filepath.Join(t.TempDir(), "ledger.db"),
		Mem0APIKey:    "test-key",
		Mem0BaseURL:   "http://127.0.0.1:0", // never contacted: the key check comes first
		ReconcileMode: reconcile.ReconcileCommand,
	}

	t.Run("no signing key variable configured", func(t *testing.T) {
		cmd, _ := newTestReconcileCmd(t)
		cfg := *base
		cfg.SigningKeyEnv = ""
		err := runReconcile(cmd, &cfg)
		require.Error(t, err, "no signing key must refuse to start")
		assert.Contains(t, err.Error(), "signing key")
	})

	t.Run("signing key variable unset", func(t *testing.T) {
		cmd, _ := newTestReconcileCmd(t)
		cfg := *base
		cfg.SigningKeyEnv = "NOTARY_RECONCILE_TEST_UNSET_KEY"
		t.Setenv("NOTARY_RECONCILE_TEST_UNSET_KEY", "")
		err := runReconcile(cmd, &cfg)
		require.Error(t, err, "an unset signing key must refuse to start")
		assert.Contains(t, err.Error(), "NOTARY_RECONCILE_TEST_UNSET_KEY",
			"the error must name the missing key variable")
	})
}

// ---------------------------------------------------------------------------
// --dry-run: compute and report what the pass WOULD claim; write nothing.
// ---------------------------------------------------------------------------

// TestReconcileDryRunAppendsNothing is the whole point of --dry-run. It gives
// the pass exactly one deriveable claim (an unresolved add the httptest Mem0
// resolves) and asserts that a --dry-run leaves the ledger's HEAD and row count
// -- and so its Seq -- unchanged. Asserting the report names the would-be claim
// keeps this non-vacuous: a pass that derived nothing could leak nothing.
func TestReconcileDryRunAppendsNothing(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-add-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1", AgentID: "a1"}))

	baseURL := eventStatusServer(t, mem0.EventStatusResponse{
		ID: "evt-1", Status: "SUCCEEDED", Results: []mem0.EventResult{{ID: "mem-1"}},
	})

	headBefore, okBefore, rowsBefore := ledgerSnapshot(t, dbPath)
	require.True(t, okBefore)

	cmd, buf := newTestReconcileCmd(t)
	require.NoError(t, cmd.Flags().Set("dry-run", "true"))
	cfg := &config.Config{DBPath: dbPath, Mem0APIKey: "test-key", Mem0BaseURL: baseURL, SigningKeyEnv: verifyKeyEnv, ReconcileMode: reconcile.ReconcileCommand}
	require.NoError(t, runReconcile(cmd, cfg), "a dry run must not fail")

	headAfter, okAfter, rowsAfter := ledgerSnapshot(t, dbPath)
	require.True(t, okAfter)
	assert.Equal(t, headBefore.Seq, headAfter.Seq, "a dry run must not change the head Seq")
	assert.Equal(t, headBefore.Hash, headAfter.Hash, "a dry run must not change the head hash")
	assert.Equal(t, rowsBefore, rowsAfter, "a dry run must not append any row")

	out := buf.String()
	assert.Contains(t, out, "pending", "a dry run must frame the derived claims as pending, not written")
	assert.Contains(t, out, "evt-1", "the report must name the claim the pass would write")
}

// TestReconcileWritesClaims is the positive control for
// TestReconcileDryRunAppendsNothing: the same setup, without --dry-run, must
// append exactly the derived claim. Without this, the dry-run test could pass
// simply because the reconciler derived nothing.
func TestReconcileWritesClaims(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-add-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1"}))

	baseURL := eventStatusServer(t, mem0.EventStatusResponse{
		ID: "evt-1", Status: "SUCCEEDED", Results: []mem0.EventResult{{ID: "mem-1"}},
	})

	_, _, rowsBefore := ledgerSnapshot(t, dbPath)

	cmd, buf := newTestReconcileCmd(t)
	cfg := &config.Config{DBPath: dbPath, Mem0APIKey: "test-key", Mem0BaseURL: baseURL, SigningKeyEnv: verifyKeyEnv, ReconcileMode: reconcile.ReconcileCommand}
	require.NoError(t, runReconcile(cmd, cfg), "a real pass must succeed")

	_, _, rowsAfter := ledgerSnapshot(t, dbPath)
	assert.Equal(t, rowsBefore+1, rowsAfter, "a real pass must append the derived claim")
	assert.Contains(t, buf.String(), "evt-1", "the report must name the claim it wrote")
}

// ---------------------------------------------------------------------------
// --since filters on At (the Mem0 EVENT time), never on RecordedAt.
// ---------------------------------------------------------------------------

// TestReconcileSinceFiltersOnAtNotRecordedAt pins the flag's semantics. The
// load-bearing record is the one whose At is in-window but whose RecordedAt
// precedes --since: filtering on RecordedAt would wrongly drop it, so it must
// survive and its claim must appear. The mirror record -- At before --since but
// recorded after -- must not appear, because the work item predates the window.
func TestReconcileSinceFiltersOnAtNotRecordedAt(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	scope := record.Scope{UserID: "u1"}

	before := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	since := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	after := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	// new: the event is in-window, but the record was WRITTEN before --since.
	newRec := addRequestedRecord(t, "r-new", "evt-new", after, scope)
	newRec.RecordedAt = before
	// old: the event predates the window, but the record was WRITTEN after it.
	oldRec := addRequestedRecord(t, "r-old", "evt-old", before, scope)
	oldRec.RecordedAt = after
	appendFixtureRecords(t, dbPath, sg, newRec, oldRec)

	baseURL := eventStatusServer(t, mem0.EventStatusResponse{
		ID: "x", Status: "SUCCEEDED", Results: []mem0.EventResult{{ID: "mem-x"}},
	})

	_, _, rowsBefore := ledgerSnapshot(t, dbPath)

	cmd, buf := newTestReconcileCmd(t)
	require.NoError(t, cmd.Flags().Set("since", since.Format(time.RFC3339)))
	cfg := &config.Config{DBPath: dbPath, Mem0APIKey: "test-key", Mem0BaseURL: baseURL, SigningKeyEnv: verifyKeyEnv, ReconcileMode: reconcile.ReconcileCommand}
	require.NoError(t, runReconcile(cmd, cfg))

	out := buf.String()
	assert.Contains(t, out, "evt-new",
		"a record whose At is in-window must be reconciled even when it was written before --since")
	assert.NotContains(t, out, "evt-old",
		"a record whose At precedes --since must be excluded even when it was written after --since")

	_, _, rowsAfter := ledgerSnapshot(t, dbPath)
	assert.Equal(t, rowsBefore+1, rowsAfter, "only the in-window add's claim must be appended")
}

// TestReconcileScopeFlagsRestrictThePass pins that the scope flags restrict the
// pass: a --user-id that matches no record's scope must produce no claim, and
// so write nothing.
func TestReconcileScopeFlagsRestrictThePass(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-add-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1"}))

	// If the scope filter is ignored, the reconciler would resolve evt-1 and
	// write a claim; an unroutable base URL makes any Mem0 call fail loudly.
	cmd, buf := newTestReconcileCmd(t)
	require.NoError(t, cmd.Flags().Set("user-id", "someone-else"))
	cfg := &config.Config{DBPath: dbPath, Mem0APIKey: "test-key", Mem0BaseURL: "http://127.0.0.1:0", SigningKeyEnv: verifyKeyEnv, ReconcileMode: reconcile.ReconcileCommand}
	require.NoError(t, runReconcile(cmd, cfg), "an out-of-scope pass is a clean no-op")

	_, _, rowsAfter := ledgerSnapshot(t, dbPath)
	assert.Equal(t, 1, rowsAfter, "an out-of-scope pass must write nothing")
	assert.NotContains(t, buf.String(), "evt-1")
}

// ---------------------------------------------------------------------------
// Help must be self-describing: every flag the pass reads is listed.
// ---------------------------------------------------------------------------

// TestReconcileHelpListsItsFlags pins that --help names all six flags, so the
// command's surface is discoverable (the reason the design chose four explicit
// scope flags over an ambiguous --scope).
func TestReconcileHelpListsItsFlags(t *testing.T) {
	cmd, buf := newTestReconcileCmd(t)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())

	out := buf.String()
	for _, flag := range []string{"--since", "--user-id", "--agent-id", "--app-id", "--run-id", "--dry-run"} {
		assert.Contains(t, out, flag, "help must list %s", flag)
	}
}

// ---------------------------------------------------------------------------
// Legibility and failure behaviour.
// ---------------------------------------------------------------------------

// TestReconcileMissingMem0APIKeyNamesVariable pins that a missing
// NOTARY_MEM0_API_KEY fails with an error that NAMES the variable, rather than
// surfacing a bare HTTP 401 from deep inside the client. This is the first
// command to consume cfg.Mem0APIKey, so the failure must be legible.
func TestReconcileMissingMem0APIKeyNamesVariable(t *testing.T) {
	sg, _ := newVerifySigner(t) // sets the signing-key environment variable
	require.NotNil(t, sg)

	cmd, _ := newTestReconcileCmd(t)
	cfg := &config.Config{
		DBPath:        filepath.Join(t.TempDir(), "ledger.db"),
		SigningKeyEnv: verifyKeyEnv,
		Mem0APIKey:    "",
		Mem0BaseURL:   "http://127.0.0.1:0",
		ReconcileMode: reconcile.ReconcileCommand,
	}
	err := runReconcile(cmd, cfg)
	require.Error(t, err, "a missing Mem0 API key must fail")
	assert.Contains(t, err.Error(), config.EnvMem0APIKey, "the error must name NOTARY_MEM0_API_KEY")
}

// TestReconcileRejectsInProcessMode pins that the reserved, unimplemented
// in-process mode is rejected through ReconcileMode.Validate, not silently run.
func TestReconcileRejectsInProcessMode(t *testing.T) {
	sg, _ := newVerifySigner(t)
	require.NotNil(t, sg)

	cmd, _ := newTestReconcileCmd(t)
	cfg := &config.Config{
		DBPath:        filepath.Join(t.TempDir(), "ledger.db"),
		SigningKeyEnv: verifyKeyEnv,
		Mem0APIKey:    "test-key",
		Mem0BaseURL:   "http://127.0.0.1:0",
		ReconcileMode: reconcile.ReconcileInProcess,
	}
	err := runReconcile(cmd, cfg)
	require.Error(t, err, "the reserved in-process mode must be rejected")
	assert.ErrorIs(t, err, reconcile.ErrReconcileInProcessUnimplemented,
		"the rejection must wrap the mode's sentinel so it is matchable")
}

// TestReconcileFailureWritesNoGapAndLeavesChain pins spec §9.2: on any error
// the pass exits non-zero and writes NO audit_gap. A gap entry means "a
// customer-visible operation happened that we failed to record", which is not
// this situation; inventing one would blur the gap log's meaning. The chain
// must be exactly as it was.
func TestReconcileFailureWritesNoGapAndLeavesChain(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-add-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1"}))

	headBefore, okBefore, rowsBefore := ledgerSnapshot(t, dbPath)
	require.True(t, okBefore)

	baseURL := errorStatusServer(t, http.StatusInternalServerError)

	cmd, _ := newTestReconcileCmd(t)
	cfg := &config.Config{DBPath: dbPath, Mem0APIKey: "test-key", Mem0BaseURL: baseURL, SigningKeyEnv: verifyKeyEnv, ReconcileMode: reconcile.ReconcileCommand}
	err := runReconcile(cmd, cfg)
	require.Error(t, err, "a Mem0 outage must exit non-zero (spec §9.2)")

	headAfter, okAfter, rowsAfter := ledgerSnapshot(t, dbPath)
	require.True(t, okAfter)
	assert.Equal(t, headBefore.Seq, headAfter.Seq, "a failed pass must not change the head Seq")
	assert.Equal(t, headBefore.Hash, headAfter.Hash, "a failed pass must not change the head hash")
	assert.Equal(t, rowsBefore, rowsAfter, "a failed pass must not append any record")

	// And, specifically, no audit_gap was written to the chain.
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()
	entries, err := st.SeqEntries()
	require.NoError(t, err)
	for _, e := range entries {
		if e.DecodeErr == nil {
			assert.NotEqual(t, record.EventAuditGap, e.Rec.Event,
				"a failed pass must not invent an audit_gap (spec §9.2)")
		}
	}
}
