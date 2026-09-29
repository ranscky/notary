package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/gap"
	"notary/internal/record"
)

// newTestGapsCmd builds a gaps command whose output is captured in a buffer, so
// a test can assert on the printed report, not only on the returned error. It
// mirrors newTestVerifyCmd.
func newTestGapsCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := newGapsCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

// writeGapLog builds a gap log at path holding entries, in order, using the
// same gap.Open + Record path the existing tests use, and closes it before
// returning so runGaps reopens nothing. The caller never supplies chain fields:
// Record assigns Counter, PrevHash, and Hash.
func writeGapLog(t *testing.T, path string, entries ...gap.Entry) {
	t.Helper()
	gl, err := gap.Open(path)
	require.NoError(t, err)
	for _, e := range entries {
		require.NoError(t, gl.Record(e))
	}
	require.NoError(t, gl.Close())
}

// TestRunGapsNoLogIsZero locks the ordinary case the task calls out: a missing
// gap log means no gap ever occurred, so the command reports no outstanding
// gaps and exits zero. It also pins that no signing key and no keyring are
// needed: cfg carries neither, and the run must still succeed.
func TestRunGapsNoLogIsZero(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 2)

	cmd, buf := newTestGapsCmd(t)
	// No TrustedKeysPath and no SigningKeyEnv: the report must not need either.
	cfg := &config.Config{DBPath: dbPath, GapLogPath: filepath.Join(dir, "gaps.log")}

	require.NoError(t, runGaps(cmd, cfg), "a missing gap log must exit zero")
	assert.Contains(t, buf.String(), "no outstanding gaps")
}

// TestRunGapsAllReconciledIsZero covers the healthy logged case: every gap
// entry is accounted for by a stored record, so nothing is outstanding and the
// command exits zero.
func TestRunGapsAllReconciledIsZero(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	gapPath := filepath.Join(dir, "gaps.log")
	buildLedger(t, dbPath, sg, 2)

	writeGapLog(t, gapPath,
		gap.Entry{
			At: verifyFixedNow, Kind: record.EventMemorySurfaced,
			Scope: record.Scope{UserID: "u1", AgentID: "a1"}, CorrelationID: "rec-0001", Detail: "a",
		},
		gap.Entry{
			At: verifyFixedNow, Kind: record.EventMemorySurfaced,
			Scope: record.Scope{UserID: "u1", AgentID: "a1"}, CorrelationID: "rec-0002", Detail: "b",
		},
	)

	cmd, buf := newTestGapsCmd(t)
	cfg := &config.Config{DBPath: dbPath, GapLogPath: gapPath}

	require.NoError(t, runGaps(cmd, cfg), "all-reconciled gaps must exit zero")
	assert.Contains(t, buf.String(), "no outstanding gaps")
}

// TestRunGapsUnreconciledExitsNonZero is the command's reason to exist: a
// standing gap -- a recorded gap no stored record accounts for -- must make the
// command exit non-zero, and the report must name the missing record by its
// correlation ID together with the kind, the scope, and the detail.
func TestRunGapsUnreconciledExitsNonZero(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	gapPath := filepath.Join(dir, "gaps.log")
	buildLedger(t, dbPath, sg, 2)

	writeGapLog(t, gapPath, gap.Entry{
		At:            verifyFixedNow,
		Kind:          record.EventMemorySurfaced,
		Scope:         record.Scope{UserID: "u9", AgentID: "a9", AppID: "appX", RunID: "runY"},
		CorrelationID: "rec-9999",
		Detail:        "store write failed",
	})

	cmd, buf := newTestGapsCmd(t)
	cfg := &config.Config{DBPath: dbPath, GapLogPath: gapPath}

	err := runGaps(cmd, cfg)
	require.Error(t, err, "an unreconciled gap must exit non-zero")

	out := buf.String()
	assert.Contains(t, out, "rec-9999", "the report must name the missing record's ID")
	assert.Contains(t, out, string(record.EventMemorySurfaced), "the report must name the kind")
	assert.Contains(t, out, "user=u9", "the report must name the scope's user")
	assert.Contains(t, out, "agent=a9", "the report must name the scope's agent")
	assert.Contains(t, out, "app=appX", "the report must name the scope's app")
	assert.Contains(t, out, "run=runY", "the report must name the scope's run")
	assert.Contains(t, out, "store write failed", "the report must carry the gap's detail")
}

// TestRunGapsCorruptLineSurfacesIntegrityBreak proves requirement 4: gap.Read
// silently skips a line it cannot decode, so a corrupt log would otherwise
// produce an incomplete list that looked healthy. The command must surface the
// gap log's own integrity breaks and exit non-zero. The one good entry is
// reconciled, so the only reason for the non-zero exit is the corrupt line --
// and it must NOT be reported as an unreconciled gap.
func TestRunGapsCorruptLineSurfacesIntegrityBreak(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	gapPath := filepath.Join(dir, "gaps.log")
	buildLedger(t, dbPath, sg, 1)

	writeGapLog(t, gapPath, gap.Entry{
		At: verifyFixedNow, Kind: record.EventMemorySurfaced,
		Scope: record.Scope{UserID: "u1", AgentID: "a1"}, CorrelationID: "rec-0001", Detail: "ok",
	})
	// Append a line that cannot decode, in place: line 2 is corrupt.
	f, err := os.OpenFile(gapPath, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("this is not json\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	cmd, buf := newTestGapsCmd(t)
	cfg := &config.Config{DBPath: dbPath, GapLogPath: gapPath}

	rerr := runGaps(cmd, cfg)
	require.Error(t, rerr, "a corrupt gap-log line must exit non-zero")

	out := buf.String()
	assert.Contains(t, out, "integrity", "the report must surface the integrity break")
	assert.Contains(t, out, "decode", "the break must name the undecodable line")
	assert.Contains(t, out, "line 2", "the break must locate the bad line")
	assert.NotContains(t, out, "unreconciled", "the reconciled good entry must not be flagged")
}

// TestRunGapsIsReadOnlyOnGapLog is requirement 1's negative control. gap.Open
// opens for append AND heals a torn tail by writing a newline; an inspection
// command built on it would mutate the very file it inspects. runGaps must use
// gap.Read / gap.Verify, which never write. The second case strips the trailing
// newline so the file is torn -- the exact input on which a write-happy
// implementation mutates the bytes -- and asserts they are unchanged.
func TestRunGapsIsReadOnlyOnGapLog(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 1)

	t.Run("newline-terminated log is unchanged", func(t *testing.T) {
		gapPath := filepath.Join(dir, "gaps-ok.log")
		writeGapLog(t, gapPath, gap.Entry{
			At: verifyFixedNow, Kind: record.EventMemorySurfaced,
			Scope: record.Scope{UserID: "u1", AgentID: "a1"}, CorrelationID: "rec-0001", Detail: "ok",
		})
		before, err := os.ReadFile(gapPath)
		require.NoError(t, err)

		cmd, _ := newTestGapsCmd(t)
		cfg := &config.Config{DBPath: dbPath, GapLogPath: gapPath}
		_ = runGaps(cmd, cfg) // the error is irrelevant; the bytes must not change

		after, err := os.ReadFile(gapPath)
		require.NoError(t, err)
		assert.Equal(t, before, after, "an inspection must not modify the gap log")
	})

	t.Run("torn, non-newline-terminated log is unchanged", func(t *testing.T) {
		gapPath := filepath.Join(dir, "gaps-torn.log")
		writeGapLog(t, gapPath, gap.Entry{
			At: verifyFixedNow, Kind: record.EventMemorySurfaced,
			Scope: record.Scope{UserID: "u1", AgentID: "a1"}, CorrelationID: "rec-0001", Detail: "ok",
		})
		// Strip the terminating newline. gap.Open would heal this by appending
		// one; gap.Read must leave it exactly as it found it.
		raw, err := os.ReadFile(gapPath)
		require.NoError(t, err)
		require.Equal(t, byte('\n'), raw[len(raw)-1], "the fixture must end in a newline before stripping")
		require.NoError(t, os.WriteFile(gapPath, raw[:len(raw)-1], 0o600))
		before, err := os.ReadFile(gapPath)
		require.NoError(t, err)

		cmd, _ := newTestGapsCmd(t)
		cfg := &config.Config{DBPath: dbPath, GapLogPath: gapPath}
		_ = runGaps(cmd, cfg)

		after, err := os.ReadFile(gapPath)
		require.NoError(t, err)
		assert.Equal(t, before, after, "a torn log must not be healed by an inspection")
	})
}
