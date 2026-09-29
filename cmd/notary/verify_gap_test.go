package main

import (
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
