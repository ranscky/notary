package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/gap"
	"notary/internal/record"
)

// TestRunVerifySurfacesUnmatchedGap covers Step 5's unmatched case end to end:
// a gap-log entry whose (Kind, Scope, CorrelationID) matches no stored record
// must make `notary verify` exit non-zero and name the gap.
//
// NOTE: the matched case cannot be tested until Task 15 gives gaps a
// corresponding record, so it is revisited in Task 19.
func TestRunVerifySurfacesUnmatchedGap(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 2)

	gapPath := filepath.Join(dir, "gaps.log")
	g, err := gap.Open(gapPath)
	require.NoError(t, err)
	require.NoError(t, g.Record(gap.Entry{
		At:            time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC),
		Kind:          record.EventAuditGap,
		Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
		CorrelationID: "rec-missing",
		Detail:        "memory store unreachable",
	}))
	require.NoError(t, g.Close())

	cmd, buf := newTestVerifyCmd(t)
	cfg := &config.Config{
		DBPath:          dbPath,
		TrustedKeysPath: writeTrustedKeys(t, pub),
		GapLogPath:      gapPath,
	}

	err = runVerify(cmd, cfg, "", "")
	require.Error(t, err, "an unmatched gap must exit non-zero")
	assert.Contains(t, buf.String(), "gap", "the report must name the gap break")
	assert.Contains(t, buf.String(), "rec-missing", "the report must name the correlation ID")
}

// TestRunVerifyNoGapLogIsUnchanged locks that a run with no gap log behaves
// exactly as before: no gap breaks, and a clean ledger still verifies.
func TestRunVerifyNoGapLogIsUnchanged(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	buildLedger(t, dbPath, sg, 2)

	cmd, buf := newTestVerifyCmd(t)
	cfg := &config.Config{
		DBPath:          dbPath,
		TrustedKeysPath: writeTrustedKeys(t, pub),
		GapLogPath:      filepath.Join(t.TempDir(), "absent-gaps.log"),
	}

	require.NoError(t, runVerify(cmd, cfg, "", ""), "an absent gap log must not add breaks")
	assert.Contains(t, buf.String(), "ok:", "the clean ledger must still report success")
}

// TestRunVerifySurfacesGapIntegrityBreak covers the integrity half of the gap
// check end to end: a gap-log line that has been tampered with -- its JSON
// still decodes, but its stored hash no longer recomputes -- must make
// `notary verify` exit non-zero.
//
// Before this change nothing in the production tree called gap.Verify, and
// gap.Read silently accepts a line whose JSON still decodes (it only skips
// lines that fail to decode), so the tamper was invisible to the auditor's
// command.
//
// The two gap entries are deliberately matched to stored records (same Kind,
// Scope, and CorrelationID), so the cross-check reports no "gap" break: the
// ONLY thing that can catch the tamper is gap.Verify, which is what this test
// pins.
func TestRunVerifySurfacesGapIntegrityBreak(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 2) // rec-0001, rec-0002

	// Two valid, chain-linked gap entries, each matched to a stored record by
	// (Kind, Scope, CorrelationID).
	gapPath := filepath.Join(dir, "gaps.log")
	g, err := gap.Open(gapPath)
	require.NoError(t, err)
	for _, id := range []string{"rec-0001", "rec-0002"} {
		require.NoError(t, g.Record(gap.Entry{
			At:            verifyFixedNow,
			Kind:          record.EventMemorySurfaced,
			Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
			CorrelationID: id,
			Detail:        "span " + id,
		}))
	}
	require.NoError(t, g.Close())

	cfg := &config.Config{
		DBPath:          dbPath,
		TrustedKeysPath: writeTrustedKeys(t, pub),
		GapLogPath:      gapPath,
	}

	// The intact, matched log verifies clean: no gap break, exit zero. This is
	// what makes the corrupted-log failure below attributable to the integrity
	// break alone and not to an unmatched gap.
	cmd, buf := newTestVerifyCmd(t)
	require.NoError(t, runVerify(cmd, cfg, "", ""), "an intact matched gap log must verify clean")
	require.Contains(t, buf.String(), "ok:")

	// Tamper with the first line: flip a character inside its Detail value, so
	// the line is still valid JSON and still decodes.
	orig, err := os.ReadFile(gapPath)
	require.NoError(t, err)
	tampered := bytes.Replace(orig, []byte(`"span rec-0001"`), []byte(`"span rec-000Z"`), 1)
	require.NotEqual(t, string(orig), string(tampered), "the Detail value to tamper must exist in the log")
	require.NoError(t, os.WriteFile(gapPath, tampered, 0o600))

	// Pre-fix proof, asserted cheaply rather than only stated in a comment:
	// gap.Read returns no error on the tampered file -- the line still decodes,
	// and gap.Read does not check hashes -- so the existing cross-check alone
	// never reports the tamper. Only gap.Verify catches it.
	entries, rerr := gap.Read(gapPath)
	require.NoError(t, rerr, "gap.Read must not error on the tampered file")
	require.Len(t, entries, 2, "gap.Read decodes the tampered line and reports no problem")

	// gap.Verify does catch it: asserted directly so the test fails for the
	// right reason if the runVerify wiring below regresses.
	gapBreaks, verr := gap.Verify(gapPath)
	require.NoError(t, verr)
	require.NotEmpty(t, gapBreaks, "gap.Verify must report the tamper")

	// Post-fix: runVerify surfaces the break and exits non-zero.
	cmd2, buf2 := newTestVerifyCmd(t)
	verr2 := runVerify(cmd2, cfg, "", "")
	require.Error(t, verr2, "a tampered gap line must exit non-zero")
	assert.Contains(t, buf2.String(), "gap log line 1", "the report must locate the break by line")
	assert.Contains(t, verr2.Error(), "verification failed", "the error must state the failure")
}
