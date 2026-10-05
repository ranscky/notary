package main

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // registers the "sqlite" driver used for raw tampering

	"notary/config"
	"notary/internal/gap"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// verifyTestSeed is a fixed 32-byte ed25519 seed, mirroring the ledger and sign
// packages' own test pattern so the command tests sign and verify
// deterministically -- no key is generated, and no key-generation helper is
// added to the library.
var verifyTestSeed = []byte{
	0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
	0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10,
	0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18,
	0x19, 0x1a, 0x1b, 0x1c, 0x1d, 0x1e, 0x1f, 0x20,
}

// verifyKeyEnv names the environment variable these tests hold signing material
// in. runVerify's writeCheckpoint resolves cfg.SigningKeyEnv to a variable
// *name*, so the tests point SigningKeyEnv here.
const verifyKeyEnv = "NOTARY_VERIFY_TEST_KEY"

// verifyFixedNow is the deterministic clock the ledger fixture is given.
var verifyFixedNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

// newVerifySigner builds a Signer from verifyTestSeed through the env KeySource
// and returns the matching public key, following the sign package's test pattern
// (base64 seed through a KeySource; NewSigner never invents a key).
func newVerifySigner(t *testing.T) (*sign.Signer, ed25519.PublicKey) {
	t.Helper()
	t.Setenv(verifyKeyEnv, base64.StdEncoding.EncodeToString(verifyTestSeed))
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: verifyKeyEnv})
	require.NoError(t, err)
	pub := ed25519.NewKeyFromSeed(verifyTestSeed).Public().(ed25519.PublicKey)
	return sg, pub
}

// writeTrustedKeys writes a one-line trusted-keys file holding pub -- the exact
// format sign.LoadTrustedKeys consumes -- and returns its path.
func writeTrustedKeys(t *testing.T, pub ed25519.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trusted.keys")
	require.NoError(t, os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644))
	return path
}

// buildLedger appends n valid records to a fresh SQLite ledger at dbPath and
// closes the store, so runVerify can reopen it cleanly.
func buildLedger(t *testing.T, dbPath string, sg *sign.Signer, n int) {
	t.Helper()
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	l := ledger.New(st, sg, func() time.Time { return verifyFixedNow })
	for i := 0; i < n; i++ {
		_, err := l.Append(validCmdRecord(t, record.RecordID(fmt.Sprintf("rec-%04d", i+1))))
		require.NoError(t, err)
	}
}

// validCmdRecord returns a well-formed record, matching the ledger package's own
// fixture, so the command tests run against real appended data.
func validCmdRecord(t *testing.T, id record.RecordID) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"ok":true}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonReturnedBySearch, ev)
	require.NoError(t, err)
	rec := record.Record{
		ID:     id,
		At:     verifyFixedNow,
		Event:  record.EventMemorySurfaced,
		Reason: reason,
		Subject: record.Subject{
			MemoryID:    "mem-1",
			Scope:       record.Scope{UserID: "u1", AgentID: "a1"},
			ContentHash: verifyHash(0x40),
		},
	}
	require.NoError(t, rec.Validate())
	return rec
}

// verifyHash builds a distinct, non-zero 32-byte hash from a seed byte.
func verifyHash(seed byte) record.Hash {
	var h record.Hash
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// execRawSQL runs one statement against dbPath on a separate connection, the way
// an out-of-band edit of the database file would; it mirrors the ledger
// package's execRaw helper.
func execRawSQL(t *testing.T, dbPath, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(stmt, args...)
	require.NoError(t, err)
}

// newTestVerifyCmd builds a verify command whose output is captured in a buffer,
// so a test can assert on the printed report, not only on the returned error.
func newTestVerifyCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := newVerifyCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

// TestRunVerifyCleanRunIsZero locks the command's core success contract: a clean
// ledger returns nil -- so the process exits 0 -- and reports "ok:".
func TestRunVerifyCleanRunIsZero(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	buildLedger(t, dbPath, sg, 3)

	cmd, buf := newTestVerifyCmd(t)
	cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub)}

	require.NoError(t, runVerify(cmd, cfg, "", ""), "a clean ledger must exit zero")
	assert.Contains(t, buf.String(), "ok:", "a clean run must report success")
}

// TestRunVerifyTruncationExitsNonZero locks the exit-code contract that defines
// this task: a truncated chain must make runVerify return an error wrapping
// ledger.ErrTruncated, so the process exits non-zero. It builds a chain, writes a
// real checkpoint, deletes the tail through a raw second connection, then calls
// runVerify with --checkpoint. Deleting this fold is the silent regression the
// task warns about -- a truncated ledger would audit as passing.
func TestRunVerifyTruncationExitsNonZero(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 6)
	cpPath := filepath.Join(dir, "cp.json")

	cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub), SigningKeyEnv: verifyKeyEnv}

	// Write a genuine signed checkpoint for the head.
	cmd, _ := newTestVerifyCmd(t)
	require.NoError(t, runVerify(cmd, cfg, "", cpPath))

	// Remove the tail out of band.
	execRawSQL(t, dbPath, `DELETE FROM records WHERE seq >= 4`)

	cmd2, buf := newTestVerifyCmd(t)
	err := runVerify(cmd2, cfg, cpPath, "")
	require.Error(t, err, "a truncated chain must exit non-zero")
	assert.ErrorIs(t, err, ledger.ErrTruncated, "the error must wrap ErrTruncated")
	assert.Contains(t, buf.String(), "truncation", "the report must name the truncation")
	assert.Contains(t, err.Error(), "verification failed", "the error must state the failure")
}

// TestRunVerifyMangledCheckpointIsCheckpointError proves a checkpoint whose
// signature has been tampered with reads as "your checkpoint is bad", never as
// "tamper detected": the run errors, but the error must NOT wrap ErrTruncated.
func TestRunVerifyMangledCheckpointIsCheckpointError(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 6)
	cpPath := filepath.Join(dir, "cp.json")

	cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub), SigningKeyEnv: verifyKeyEnv}

	cmd, _ := newTestVerifyCmd(t)
	require.NoError(t, runVerify(cmd, cfg, "", cpPath))

	// Mangle the signature field, leaving the JSON itself well-formed.
	data, err := os.ReadFile(cpPath)
	require.NoError(t, err)
	cp, err := sign.UnmarshalCheckpoint(data)
	require.NoError(t, err)
	cp.Signature[0] ^= 0xFF
	tampered, err := sign.MarshalCheckpoint(cp)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(cpPath, tampered, 0o644))

	cmd2, _ := newTestVerifyCmd(t)
	verr := runVerify(cmd2, cfg, cpPath, "")
	require.Error(t, verr, "a mangled checkpoint must exit non-zero")
	assert.NotErrorIs(t, verr, ledger.ErrTruncated, "a bad checkpoint must not read as tamper")
	assert.Contains(t, verr.Error(), "cannot be trusted", "the error must name the checkpoint as the problem")
}

// TestRunVerifyNoTrustedKeysErrors closes the Task 10 invariant parked in cmd/:
// with no trusted keys configured, "could not verify anything" must never look
// like "verified everything", so runVerify returns an error (exit non-zero).
func TestRunVerifyNoTrustedKeysErrors(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	buildLedger(t, dbPath, sg, 2)

	t.Run("no path configured", func(t *testing.T) {
		cmd, _ := newTestVerifyCmd(t)
		cfg := &config.Config{DBPath: dbPath} // TrustedKeysPath empty
		err := runVerify(cmd, cfg, "", "")
		require.Error(t, err, "no trusted keys must not report success")
	})

	t.Run("path set but file holds no keys", func(t *testing.T) {
		empty := filepath.Join(t.TempDir(), "empty.keys")
		require.NoError(t, os.WriteFile(empty, []byte("\n"), 0o644))
		cmd, _ := newTestVerifyCmd(t)
		cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: empty}
		err := runVerify(cmd, cfg, "", "")
		require.Error(t, err, "an empty keyring must not report success")
	})
}

// TestRunVerifyWriteCheckpointWritesParsableFile covers --write-checkpoint: it
// must exit zero, write the file, and the file must re-parse through the
// checkpoint codec back to the attested head. It also confirms the freshly
// written checkpoint then verifies cleanly against the same ledger.
func TestRunVerifyWriteCheckpointWritesParsableFile(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 4)
	cpPath := filepath.Join(dir, "cp.json")

	cmd, buf := newTestVerifyCmd(t)
	cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub), SigningKeyEnv: verifyKeyEnv}

	require.NoError(t, runVerify(cmd, cfg, "", cpPath), "--write-checkpoint must exit zero")
	assert.Contains(t, buf.String(), "wrote checkpoint", "the run must report the written checkpoint")

	data, err := os.ReadFile(cpPath)
	require.NoError(t, err, "the checkpoint file must have been written")
	cp, err := sign.UnmarshalCheckpoint(data)
	require.NoError(t, err, "the written checkpoint must re-parse")
	assert.Equal(t, uint64(3), cp.Seq, "the checkpoint must attest to the head, seq 3")

	// The freshly written checkpoint verifies cleanly against the same ledger.
	cmd2, buf2 := newTestVerifyCmd(t)
	require.NoError(t, runVerify(cmd2, cfg, cpPath, ""))
	assert.Contains(t, buf2.String(), "checkpoint ok")
}

// verifyCanarySeed is a fixed seed, distinct from verifyTestSeed, so the
// material used by the leak canary below is unmistakable in any output.
var verifyCanarySeed = []byte{
	0xde, 0xad, 0xbe, 0xef, 0x11, 0x22, 0x33, 0x44,
	0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc,
	0xdd, 0xee, 0xff, 0x00, 0x0f, 0xf0, 0x3c, 0xc3,
	0x5a, 0xa5, 0x6b, 0xb6, 0x7c, 0xc7, 0x8d, 0xd8,
}

// TestRunVerifyNeverPrintsSigningKeyMaterial is the end-to-end canary for the
// leak. It uses the natural configuration -- the key material placed IN
// NOTARY_SIGNING_KEY and the config loaded from the environment -- and drives
// `--write-checkpoint`. The material must appear in NEITHER stdout NOR the
// returned error (which is what the CLI prints to stderr), and the write must
// actually succeed.
//
// Before the fix, config.LoadFrom copied the material's VALUE into
// cfg.SigningKeyEnv, and both cmd/notary/verify.go and internal/sign echoed it,
// so the material reached stderr AND the key could never load. This test fails
// against that code and passes once SigningKeyEnv is a name again.
func TestRunVerifyNeverPrintsSigningKeyMaterial(t *testing.T) {
	material := base64.StdEncoding.EncodeToString(verifyCanarySeed)
	pub := ed25519.NewKeyFromSeed(verifyCanarySeed).Public().(ed25519.PublicKey)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	trustedPath := filepath.Join(dir, "trusted.keys")
	cpPath := filepath.Join(dir, "cp.json")
	gapPath := filepath.Join(dir, "gaps.log")

	// The ledger is signed with the canary key, and the trusted-keys file holds
	// its public key, so the config below is a coherent, healthy setup.
	t.Setenv("NOTARY_CANARY_SIGN_KEY", material)
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_CANARY_SIGN_KEY"})
	require.NoError(t, err)
	buildLedger(t, dbPath, sg, 3)
	require.NoError(t, os.WriteFile(trustedPath, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644))

	// The natural configuration: material in NOTARY_SIGNING_KEY, config from env.
	t.Setenv("NOTARY_SIGNING_KEY", material)
	t.Setenv("NOTARY_DB_PATH", dbPath)
	t.Setenv("NOTARY_TRUSTED_KEYS_PATH", trustedPath)
	t.Setenv("NOTARY_GAP_LOG_PATH", gapPath)
	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, config.DefaultSigningKeyEnv, cfg.SigningKeyEnv,
		"SigningKeyEnv must be the variable NAME, never the material")

	cmd, buf := newTestVerifyCmd(t)
	werr := runVerify(cmd, cfg, "", cpPath)

	stdout := buf.String()
	stderr := ""
	if werr != nil {
		stderr = werr.Error()
	}
	assert.NotContains(t, stdout, material, "key material must never reach stdout")
	assert.NotContains(t, stderr, material, "key material must never reach stderr")

	require.NoError(t, werr, "with the material in NOTARY_SIGNING_KEY the checkpoint must be written")
	_, statErr := os.Stat(cpPath)
	require.NoError(t, statErr, "the checkpoint file must exist")
}

// verifyBreakIdentity is one break reduced to what two answers must agree on:
// the record it names (empty when it names none), the chain position, and the
// field that broke. The differential below compares these identities rather
// than the wording of either rendering, so a cosmetic change to `verify`'s text
// does not fail it.
type verifyBreakIdentity struct {
	recordID string
	seq      uint64
	field    string
}

// verifyBreakLine matches the two shapes runVerify prints a break in -- "record
// <id> (seq <n>): <field> — ..." and "seq <n>: <field> — ..." -- capturing the
// identity and ignoring the Detail, which the differential does not compare.
var verifyBreakLine = regexp.MustCompile(`^(?:record (\S+) \()?seq (\d+)\)?: (\S+) — `)

// verifyBreakIdentities parses the break lines out of a verify run's output.
// The success line is skipped; any other line must match the break shape, and
// the test fails on it rather than skipping, so a change to how a break is
// printed fails loudly instead of silently leaving nothing to compare.
func verifyBreakIdentities(t *testing.T, out string) []verifyBreakIdentity {
	t.Helper()
	var ids []verifyBreakIdentity
	for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" || strings.HasPrefix(line, "ok:") {
			continue
		}
		m := verifyBreakLine.FindStringSubmatch(line)
		require.NotNil(t, m, "unrecognised verify output line %q", line)
		seq, err := strconv.ParseUint(m[2], 10, 64)
		require.NoError(t, err, "the printed seq %q must be a number", m[2])
		ids = append(ids, verifyBreakIdentity{recordID: m[1], seq: seq, field: m[3]})
	}
	return ids
}

// collectBreaksForAnswer answers the same question through the shared
// collection, configuring itself exactly as runVerify does: the same trusted
// keys file and the same database, read without a signer.
func collectBreaksForAnswer(t *testing.T, cfg *config.Config) []ledger.Break {
	t.Helper()
	keyring, err := sign.LoadTrustedKeys(cfg.TrustedKeysPath)
	require.NoError(t, err)
	st, err := store.Open(cfg.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	breaks, err := ledger.CollectBreaks(ledger.New(st, nil, nil), st, cfg.GapLogPath, sign.NewVerifier(keyring))
	require.NoError(t, err)
	return breaks
}

// TestCollectBreaksAgreesWithVerify is the differential guard spec section 3,
// decision 8 demands, and the proof that extracting the shared collection
// changed no answer: over three fixtures -- an intact ledger, a ledger with an
// edited record, and a ledger whose gap log holds an entry matching no record
// -- `ledger.CollectBreaks` must find exactly what `notary verify` finds, break
// for break, as identities: the count, the record ids, the chain positions, and
// the fields.
//
// The third fixture is Review Focus 1's case, the one ledger.Verify alone
// cannot see, so it is what makes the extraction necessary rather than
// decorative. Transitivity is the point: CollectBreaks answers here as the
// command does, and the report renders CollectBreaks' answer.
func TestCollectBreaksAgreesWithVerify(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T, dbPath, gapPath string, sg *sign.Signer)
	}{
		{
			name: "intact ledger",
			build: func(t *testing.T, dbPath, _ string, sg *sign.Signer) {
				buildLedger(t, dbPath, sg, 3)
			},
		},
		{
			name: "edited record",
			build: func(t *testing.T, dbPath, _ string, sg *sign.Signer) {
				buildLedger(t, dbPath, sg, 5)
				execRawSQL(t, dbPath, `UPDATE records SET event = 'memory_kept' WHERE seq = 3`)
			},
		},
		{
			// Every record is intact, so only the gap cross-check can see
			// this: the case ledger.Verify alone reports as clean.
			name: "gap entry matching no record",
			build: func(t *testing.T, dbPath, gapPath string, sg *sign.Signer) {
				buildLedger(t, dbPath, sg, 3)
				g, err := gap.Open(gapPath)
				require.NoError(t, err)
				require.NoError(t, g.Record(gap.Entry{
					At:            verifyFixedNow,
					Kind:          record.EventAuditGap,
					Scope:         record.Scope{UserID: "u1", AgentID: "a1"},
					CorrelationID: "rec-missing",
					Detail:        "memory store unreachable",
				}))
				require.NoError(t, g.Close())
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sg, pub := newVerifySigner(t)
			dir := t.TempDir()
			dbPath := filepath.Join(dir, "ledger.db")
			gapPath := filepath.Join(dir, "gaps.log")
			tc.build(t, dbPath, gapPath, sg)

			cfg := &config.Config{
				DBPath:          dbPath,
				TrustedKeysPath: writeTrustedKeys(t, pub),
				GapLogPath:      gapPath,
			}

			// The command's answer: what it printed, and its exit status.
			cmd, buf := newTestVerifyCmd(t)
			verr := runVerify(cmd, cfg, "", "")
			printed := verifyBreakIdentities(t, buf.String())

			// The shared collection's answer, over the same ledger.
			collected := collectBreaksForAnswer(t, cfg)

			require.Len(t, printed, len(collected),
				"the command and CollectBreaks must find the same number of breaks")
			var got []verifyBreakIdentity
			for _, b := range collected {
				got = append(got, verifyBreakIdentity{
					recordID: string(b.RecordID),
					seq:      b.Seq,
					field:    b.Field,
				})
			}
			assert.Equal(t, printed, got,
				"each printed break must be the same break CollectBreaks returned, in the same order")

			// And the same verdict: a non-zero exit if and only if a break
			// exists.
			if len(collected) == 0 {
				assert.NoError(t, verr, "no breaks must exit zero")
				assert.Contains(t, buf.String(), "ok:", "a clean run must say so")
			} else {
				assert.Error(t, verr, "a broken ledger must exit non-zero")
			}
		})
	}
}
