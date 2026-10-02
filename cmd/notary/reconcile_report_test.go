package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/mem0"
	"notary/internal/reconcile"
	"notary/internal/record"
)

// TestReconcileReportsAppendedSeparatelyFromDerived is the regression test for
// the reporting defect the live probe found against real Mem0.
//
// The summary counted DERIVED claims rather than appended records, so a re-run
// printed "wrote 2 claim(s)" while appending nothing at all -- ledger.Append had
// deduplicated them on the idempotency key. On a tool whose whole scheduling
// story rests on idempotency, an operator could not tell whether the ledger had
// changed.
//
// The pass runs twice against one ledger. The second run must report the claims
// as already present, and must not append.
func TestReconcileReportsAppendedSeparatelyFromDerived(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-add-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1"}))

	baseURL := eventStatusServer(t, mem0.EventStatusResponse{
		ID: "evt-1", Status: "SUCCEEDED", Results: []mem0.EventResult{{ID: "mem-1"}},
	})
	cfg := &config.Config{
		DBPath: dbPath, Mem0APIKey: "test-key", Mem0BaseURL: baseURL,
		SigningKeyEnv: verifyKeyEnv, ReconcileMode: reconcile.ReconcileCommand,
	}

	// First run: both derived claims are new, so both are appended.
	first, firstBuf := newTestReconcileCmd(t)
	require.NoError(t, runReconcile(first, cfg), "the first pass must succeed")

	// Pin the APPENDED half. Without these, a mutation that reported every claim
	// as already present would pass this test and TestReconcileWritesClaims
	// alike -- the latter's only output assertion is that the report names
	// "evt-1", which the claim's own ID satisfies either way.
	firstOut := firstBuf.String()
	assert.Contains(t, firstOut, "appended  add_resolved",
		"the first run must report the resolved add as appended")
	assert.Contains(t, firstOut, "2 appended",
		"the first run appended both claims")
	assert.Contains(t, firstOut, "0 already present",
		"nothing was already present on the first run")
	_, _, rowsAfterFirst := ledgerSnapshot(t, dbPath)
	require.Equal(t, 3, rowsAfterFirst,
		"the fixture row plus both derived claims; a different count would make the assertions below mean something else")

	// Second run: the same claims are derived again and deduplicated.
	second, buf := newTestReconcileCmd(t)
	require.NoError(t, runReconcile(second, cfg), "a re-run must succeed")
	out := buf.String()

	assert.Contains(t, out, "already present",
		"a re-run must report its claims as already present, not as written")
	assert.Contains(t, out, "0 appended",
		"nothing was appended on the re-run, so the summary must say so")

	_, _, rowsAfterSecond := ledgerSnapshot(t, dbPath)
	assert.Equal(t, rowsAfterFirst, rowsAfterSecond,
		"the re-run must not append; the report exists to make that visible")
}
