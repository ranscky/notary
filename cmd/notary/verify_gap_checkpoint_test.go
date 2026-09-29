package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/gap"
	"notary/internal/record"
	"notary/internal/sign"
)

// buildMatchedGapLog records n valid, hash-chained gap entries at path, each
// matched to a stored record produced by buildLedger (same Kind, Scope, and
// CorrelationID), so the cross-check reports no "gap" break and the ONLY thing
// under test is the checkpoint. It closes the log so runVerify can read it.
func buildMatchedGapLog(t *testing.T, path string, n int) {
	t.Helper()
	g, err := gap.Open(path)
	require.NoError(t, err)
	for i := 0; i < n; i++ {
		require.NoError(t, g.Record(gap.Entry{
			At:            verifyFixedNow,
			Kind:          record.EventMemorySurfaced,
			Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
			CorrelationID: fmt.Sprintf("rec-%04d", i+1),
			Detail:        fmt.Sprintf("span %d", i),
		}))
	}
	require.NoError(t, g.Close())
}

// gapCheckpointConfig builds a config with a ledger, a trusted keyring, a gap
// log, and the signing key env, so runVerify can both read and write gap
// checkpoints in the test.
func gapCheckpointConfig(t *testing.T, dbPath, gapPath, pub string) *config.Config {
	t.Helper()
	return &config.Config{
		DBPath:          dbPath,
		TrustedKeysPath: pub,
		GapLogPath:      gapPath,
		SigningKeyEnv:   verifyKeyEnv,
	}
}

// TestVerifyCmdRegistersGapCheckpointFlags proves the two new flags exist on the
// command and default to empty, so behaviour with no flags is unchanged.
func TestVerifyCmdRegistersGapCheckpointFlags(t *testing.T) {
	cmd := newVerifyCmd()
	for _, name := range []string{"gap-checkpoint", "write-gap-checkpoint"} {
		f := cmd.Flags().Lookup(name)
		require.NotNil(t, f, "flag %q must be registered", name)
		assert.Equal(t, "", f.DefValue, "flag %q must default to empty", name)
	}
}

// TestRunVerifyGapCheckpointCleanExitsZero covers the happy path end to end:
// write a gap checkpoint, then verify a clean gap log against it. Both runs must
// exit zero and report success.
func TestRunVerifyGapCheckpointCleanExitsZero(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 3)
	gapPath := filepath.Join(dir, "gaps.log")
	buildMatchedGapLog(t, gapPath, 3)
	gcpPath := filepath.Join(dir, "gcp.json")

	cfg := gapCheckpointConfig(t, dbPath, gapPath, writeTrustedKeys(t, pub))

	// Write a gap checkpoint.
	cmd, buf := newTestVerifyCmd(t)
	require.NoError(t, cmd.Flags().Set("write-gap-checkpoint", gcpPath))
	require.NoError(t, runVerify(cmd, cfg, "", ""), "--write-gap-checkpoint must exit zero")
	assert.Contains(t, buf.String(), "wrote gap checkpoint", "the run must report the written checkpoint")

	// Verify the clean gap log against it.
	cmd2, buf2 := newTestVerifyCmd(t)
	require.NoError(t, cmd2.Flags().Set("gap-checkpoint", gcpPath))
	require.NoError(t, runVerify(cmd2, cfg, "", ""), "a clean gap log must exit zero")
	assert.Contains(t, buf2.String(), "gap checkpoint ok", "the run must confirm the checkpoint")
	assert.Contains(t, buf2.String(), "ok:", "the clean ledger must still report success")
}

// TestRunVerifyGapCheckpointDetectsDeletedFile is the whole point of the feature
// end to end: delete the entire gap log and `notary verify --gap-checkpoint`
// must name the truncation and exit non-zero, rather than printing "ok".
func TestRunVerifyGapCheckpointDetectsDeletedFile(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 3)
	gapPath := filepath.Join(dir, "gaps.log")
	buildMatchedGapLog(t, gapPath, 3)
	gcpPath := filepath.Join(dir, "gcp.json")

	cfg := gapCheckpointConfig(t, dbPath, gapPath, writeTrustedKeys(t, pub))

	cmd, _ := newTestVerifyCmd(t)
	require.NoError(t, cmd.Flags().Set("write-gap-checkpoint", gcpPath))
	require.NoError(t, runVerify(cmd, cfg, "", ""))

	// The hazard: deleting the whole gap log leaves nothing that the plain gap
	// checks can see -- gap.Read and gap.Verify both treat a missing file as an
	// empty, clean log.
	require.NoError(t, os.Remove(gapPath))

	cmd2, buf2 := newTestVerifyCmd(t)
	require.NoError(t, cmd2.Flags().Set("gap-checkpoint", gcpPath))
	err := runVerify(cmd2, cfg, "", "")
	require.Error(t, err, "a deleted gap log must exit non-zero")
	assert.ErrorIs(t, err, gap.ErrTruncated, "the error must wrap gap.ErrTruncated")
	assert.Contains(t, buf2.String(), "truncation", "the report must name the truncation")
	assert.NotContains(t, buf2.String(), "ok:", "a truncated gap log must never print ok")
}

// TestRunVerifyGapCheckpointMangledIsCheckpointError proves an unusable gap
// checkpoint reads as "your checkpoint is bad", never as a truncation: the run
// errors, but the error must NOT wrap gap.ErrTruncated. This mirrors the ledger
// --checkpoint handling exactly.
func TestRunVerifyGapCheckpointMangledIsCheckpointError(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 3)
	gapPath := filepath.Join(dir, "gaps.log")
	buildMatchedGapLog(t, gapPath, 3)
	gcpPath := filepath.Join(dir, "gcp.json")

	cfg := gapCheckpointConfig(t, dbPath, gapPath, writeTrustedKeys(t, pub))

	cmd, _ := newTestVerifyCmd(t)
	require.NoError(t, cmd.Flags().Set("write-gap-checkpoint", gcpPath))
	require.NoError(t, runVerify(cmd, cfg, "", ""))

	// Mangle the checkpoint's signature field, leaving the JSON well-formed.
	cp, err := loadCheckpoint(gcpPath)
	require.NoError(t, err)
	cp.Signature[0] ^= 0xFF
	tampered, err := sign.MarshalCheckpoint(cp)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(gcpPath, tampered, 0o644))

	cmd2, buf2 := newTestVerifyCmd(t)
	require.NoError(t, cmd2.Flags().Set("gap-checkpoint", gcpPath))
	verr := runVerify(cmd2, cfg, "", "")
	require.Error(t, verr, "a mangled gap checkpoint must exit non-zero")
	assert.NotErrorIs(t, verr, gap.ErrTruncated, "a bad checkpoint must not read as tamper")
	assert.Contains(t, verr.Error(), "cannot be trusted", "the error must name the checkpoint as the problem")
	assert.NotContains(t, buf2.String(), "ok:", "a bad checkpoint must never print ok")
}

// TestRunVerifyWriteGapCheckpointRefusesBrokenLog locks the write guard: a fresh
// gap checkpoint must not be written for a gap log we already know is broken --
// signing an attestation for a log that failed its own checks is exactly the
// silent failure the guard exists to prevent.
func TestRunVerifyWriteGapCheckpointRefusesBrokenLog(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 3) // rec-0001..rec-0003
	gapPath := filepath.Join(dir, "gaps.log")

	// A gap log whose entries match no stored record: the cross-check reports a
	// "gap" break, so the log is known broken.
	g, err := gap.Open(gapPath)
	require.NoError(t, err)
	require.NoError(t, g.Record(gap.Entry{
		At:            verifyFixedNow,
		Kind:          record.EventMemorySurfaced,
		Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
		CorrelationID: "rec-missing",
		Detail:        "span",
	}))
	require.NoError(t, g.Close())

	gcpPath := filepath.Join(dir, "gcp.json")
	cfg := gapCheckpointConfig(t, dbPath, gapPath, writeTrustedKeys(t, pub))

	cmd, _ := newTestVerifyCmd(t)
	require.NoError(t, cmd.Flags().Set("write-gap-checkpoint", gcpPath))
	err = runVerify(cmd, cfg, "", "")
	require.Error(t, err, "writing a gap checkpoint for a broken log must exit non-zero")
	assert.Contains(t, err.Error(), "refusing to write a gap checkpoint", "the error must name the refusal")
	_, statErr := os.Stat(gcpPath)
	assert.True(t, os.IsNotExist(statErr), "no checkpoint file must be written for a broken log")
}

// TestRunVerifyWriteGapCheckpointMissingLogLeavesNoFile locks the no-side-effect
// contract: --write-gap-checkpoint against a gap log that does not exist must
// fail AND must not create the file. gap.Open opens with O_CREATE, so opening
// the absent path as a side effect of a write that then fails would manufacture
// an empty gap log -- turning the healthy "no gap log ever existed" state into
// the semantically different "an empty gap log exists" state, and stranding a
// stray file behind.
func TestRunVerifyWriteGapCheckpointMissingLogLeavesNoFile(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 3)

	// No gap log is ever created at this path.
	gapPath := filepath.Join(dir, "gaps.log")
	gcpPath := filepath.Join(dir, "gcp.json")
	cfg := gapCheckpointConfig(t, dbPath, gapPath, writeTrustedKeys(t, pub))

	cmd, _ := newTestVerifyCmd(t)
	require.NoError(t, cmd.Flags().Set("write-gap-checkpoint", gcpPath))
	err := runVerify(cmd, cfg, "", "")
	require.Error(t, err, "writing a gap checkpoint for a missing gap log must exit non-zero")

	_, statErr := os.Stat(gapPath)
	assert.True(t, os.IsNotExist(statErr),
		"--write-gap-checkpoint must not create the gap log as a side effect of a failed write")
	_, cpStatErr := os.Stat(gcpPath)
	assert.True(t, os.IsNotExist(cpStatErr),
		"no checkpoint file must be written when the gap log is missing")
}
