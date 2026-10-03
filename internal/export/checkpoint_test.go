package export_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"fmt"
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

// checkpointFixedNow is the deterministic clock the checkpoint tests stamp a
// checkpoint with, so its At is reproducible.
var checkpointFixedNow = time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC)

// checkpointFixture builds a real ledger -- a temp SQLite store, a signer over
// a fixed seed, a fixed clock -- with n appended records, and returns the
// signer's verifier and the database path too, so a test can verify against a
// checkpoint and can truncate the tail out of band.
func checkpointFixture(t *testing.T, n int) (*ledger.Ledger, *sign.Signer, *sign.Verifier, string) {
	t.Helper()
	t.Setenv("NOTARY_EXPORT_CHECKPOINT_KEY", base64.StdEncoding.EncodeToString(redactionTestSeed))
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_EXPORT_CHECKPOINT_KEY"})
	require.NoError(t, err)
	pub := ed25519.NewKeyFromSeed(redactionTestSeed).Public().(ed25519.PublicKey)

	path := filepath.Join(t.TempDir(), "ledger.db")
	st, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	l := ledger.New(st, sg, func() time.Time { return checkpointFixedNow })
	for i := 0; i < n; i++ {
		_, err := l.Append(buildRecord(t, record.RecordID(fmt.Sprintf("cp-rec-%04d", i+1)),
			rangeFrom.Add(time.Duration(i)*time.Hour), nil))
		require.NoError(t, err)
	}
	v := sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})
	return l, sg, v, path
}

// execRawSQL opens the ledger file with a second connection and runs one
// statement. Tampering through raw SQL rather than through the ledger is the
// point: it simulates an out-of-band edit of the database file.
func execRawSQL(t *testing.T, path, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(stmt, args...)
	require.NoError(t, err)
}

// TestExportedCheckpointIsAcceptedByVerify proves the artifact an export writes
// is immediately usable by the command that already exists to read it: the file
// is byte-for-byte sign.MarshalCheckpoint's canonical encoding (the exact format
// loadCheckpoint in cmd/notary decodes), and the ledger verifies clean against
// the decoded checkpoint.
//
// It also pins the whole point of the checkpoint travelling with the export: it
// attests the ledger HEAD, not the last record in the exported range.
func TestExportedCheckpointIsAcceptedByVerify(t *testing.T) {
	l, sg, v, _ := checkpointFixture(t, 3)

	// A record whose event time is AFTER the exported range: it is the ledger
	// HEAD, but it is NOT in the range. A checkpoint that attested the range's
	// last record would attest seq 2; the head is seq 3. The exported lines are
	// three; the checkpoint must still attest four records of history.
	_, err := l.Append(buildRecord(t, "cp-after-range", rangeTo.Add(24*time.Hour), nil))
	require.NoError(t, err)

	cpPath := filepath.Join(t.TempDir(), "head.checkpoint")
	e := export.New(l,
		export.WithSigner(sg),
		export.WithClock(func() time.Time { return checkpointFixedNow }))

	var buf bytes.Buffer
	res, err := e.Export(context.Background(),
		export.Request{From: rangeFrom, To: rangeTo, CheckpointOut: cpPath}, &buf)
	require.NoError(t, err)

	assert.Equal(t, 3, res.Records, "only the three in-range records are exported")
	require.NotNil(t, res.Checkpoint, "an export with CheckpointOut must set Result.Checkpoint")
	assert.Equal(t, uint64(3), res.Checkpoint.Seq,
		"the checkpoint attests the ledger HEAD (seq 3), not the range's last record (seq 2)")

	// The file on disk is exactly the canonical checkpoint encoding, not a
	// second shape invented for the exporter. Two artifacts with one name and
	// two formats is the failure mode this asserts against.
	onDisk, err := os.ReadFile(cpPath)
	require.NoError(t, err)
	want, err := sign.MarshalCheckpoint(*res.Checkpoint)
	require.NoError(t, err)
	assert.Equal(t, want, onDisk, "the written file must be sign.MarshalCheckpoint's format")

	// Decode the file the way verify's loader does, then verify against it.
	cp, err := sign.UnmarshalCheckpoint(onDisk)
	require.NoError(t, err, "the exported checkpoint must be readable by verify's loader")

	breaks, err := l.VerifyAgainstCheckpoint(cp, v)
	require.NoError(t, err, "a fresh export's checkpoint must be accepted by verify")
	assert.Empty(t, breaks)
}

// TestCheckpointDetectsATruncatedTail proves the checkpoint the export carried
// catches the one tamper a hash chain cannot see. The tail is deleted through
// raw SQL (an out-of-band edit); the self-consistent remainder still passes a
// plain chain walk, and only the exported checkpoint reports the truncation.
//
// The tail -- not a middle row -- is truncated deliberately: a removed middle
// row breaks the chain and a plain walk sees it, but a removed TAIL does not,
// and that is the case a checkpoint exists for.
func TestCheckpointDetectsATruncatedTail(t *testing.T) {
	l, sg, v, dbPath := checkpointFixture(t, 5)

	cpPath := filepath.Join(t.TempDir(), "head.checkpoint")
	e := export.New(l,
		export.WithSigner(sg),
		export.WithClock(func() time.Time { return checkpointFixedNow }))

	var buf bytes.Buffer
	_, err := e.Export(context.Background(),
		export.Request{From: rangeFrom, To: rangeTo, CheckpointOut: cpPath}, &buf)
	require.NoError(t, err)

	onDisk, err := os.ReadFile(cpPath)
	require.NoError(t, err)
	cp, err := sign.UnmarshalCheckpoint(onDisk)
	require.NoError(t, err)

	// Delete the tail: records at seq 3 and 4 (the last two) simply vanish.
	execRawSQL(t, dbPath, `DELETE FROM records WHERE seq >= 3`)

	// (a) The hazard: the remainder is self-consistent, so a plain chain walk
	// stays clean. This is exactly why a hash chain alone cannot detect a
	// removed tail.
	breaks, err := l.Verify(v)
	require.NoError(t, err)
	assert.Empty(t, breaks, "plain Verify cannot see a removed tail: it must stay clean")

	// (b) The checkpoint the export carried catches it.
	trunc, terr := l.VerifyAgainstCheckpoint(cp, v)
	require.Error(t, terr, "the exported checkpoint must catch the truncated tail")
	assert.ErrorIs(t, terr, ledger.ErrTruncated, "the error must wrap ErrTruncated")
	require.Len(t, trunc, 1, "a truncated chain yields exactly one truncation break")
	assert.Equal(t, "truncation", trunc[0].Field)
}

// TestCheckpointOutWithoutSignerFailsLoudly pins the fail-loud rule: a request
// that asks for a checkpoint with no signer configured must error rather than
// write an unsigned file. An unsigned artifact named "checkpoint" looks like
// evidence and is not, so producing nothing is strictly safer.
func TestCheckpointOutWithoutSignerFailsLoudly(t *testing.T) {
	l, _, _, _ := checkpointFixture(t, 3)

	cpPath := filepath.Join(t.TempDir(), "head.checkpoint")
	e := export.New(l) // no WithSigner

	var buf bytes.Buffer
	res, err := e.Export(context.Background(),
		export.Request{From: rangeFrom, To: rangeTo, CheckpointOut: cpPath}, &buf)
	require.Error(t, err, "a checkpoint request with no signer must fail")
	assert.Nil(t, res.Checkpoint, "no checkpoint is reported when none could be signed")
	_, statErr := os.Stat(cpPath)
	assert.True(t, os.IsNotExist(statErr), "no checkpoint file may be written without a signer")
}

// TestExportWithoutCheckpointOutWritesNoFile pins that checkpointing is opt-in:
// an ordinary export touches no file and reports no checkpoint.
func TestExportWithoutCheckpointOutWritesNoFile(t *testing.T) {
	l, sg, _, _ := checkpointFixture(t, 3)

	e := export.New(l, export.WithSigner(sg), export.WithClock(func() time.Time { return checkpointFixedNow }))

	var buf bytes.Buffer
	res, err := e.Export(context.Background(), export.Request{From: rangeFrom, To: rangeTo}, &buf)
	require.NoError(t, err)
	assert.Nil(t, res.Checkpoint, "no CheckpointOut means no checkpoint")
	assert.Contains(t, buf.String(), `"id":"cp-rec-0001"`, "the records still export")
}
