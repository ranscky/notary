package ledger_test

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // registers the "sqlite" driver used for raw tampering

	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// verifyFixture builds a fresh SQLite-backed ledger with n appended records and
// a verifier trusting the ledger's signer. It returns the ledger, the verifier,
// and the database path so a test can tamper with the raw file.
func verifyFixture(t *testing.T, n int) (*ledger.Ledger, *sign.Verifier, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger.db")
	st, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	sg, pub := newSigner(t)
	l := ledger.New(st, sg, func() time.Time { return fixedNow })
	for i := 0; i < n; i++ {
		_, err := l.Append(validRecord(t, record.RecordID(fmt.Sprintf("rec-%04d", i+1))))
		require.NoError(t, err)
	}
	v := sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})
	return l, v, path
}

// execRaw opens the ledger file with a second connection and runs one statement.
// Tampering through raw SQL rather than through the ledger is the point: it
// simulates an out-of-band edit of the database file.
func execRaw(t *testing.T, path, stmt string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(stmt, args...)
	require.NoError(t, err)
}

// TestVerifyEmptyStore covers Review Focus #1: an empty store must report an
// empty break list and no error, never panic.
func TestVerifyEmptyStore(t *testing.T) {
	l, v, _ := verifyFixture(t, 0)

	breaks, err := l.Verify(v)
	require.NoError(t, err, "Verify on an empty store must not error")
	assert.Empty(t, breaks, "Verify on an empty store must report no breaks")

	// A nil verifier must not panic either: an empty store has nothing to check.
	assert.NotPanics(t, func() {
		breaks, err := l.Verify(nil)
		require.NoError(t, err)
		assert.Empty(t, breaks)
	})
}

// TestVerifyCleanChain proves a well-formed chain verifies with no breaks.
func TestVerifyCleanChain(t *testing.T) {
	l, v, _ := verifyFixture(t, 5)

	breaks, err := l.Verify(v)
	require.NoError(t, err)
	assert.Empty(t, breaks, "a clean chain must verify with no breaks")
}

// TestVerifyDetectsEventTamper covers Review Focus #3: a record edited directly
// in SQLite must be named by its exact ID and field.
func TestVerifyDetectsEventTamper(t *testing.T) {
	l, v, path := verifyFixture(t, 5)

	execRaw(t, path, `UPDATE records SET event = 'memory_kept' WHERE seq = 3`)

	breaks, err := l.Verify(v)
	require.NoError(t, err)
	require.Len(t, breaks, 1, "changing one record's event must yield exactly one break")
	assert.Equal(t, uint64(3), breaks[0].Seq)
	assert.Equal(t, "hash", breaks[0].Field)
	assert.Equal(t, record.RecordID("rec-0004"), breaks[0].RecordID,
		"the break must name the exact record ID")
}

// TestVerifyDetectsPrevHashTamper proves a broken chain link is reported against
// the record whose prev_hash no longer matches its predecessor.
func TestVerifyDetectsPrevHashTamper(t *testing.T) {
	l, v, path := verifyFixture(t, 5)

	execRaw(t, path, `UPDATE records SET prev_hash = ? WHERE seq = 4`, bytes.Repeat([]byte{0xAA}, 32))

	breaks, err := l.Verify(v)
	require.NoError(t, err)
	require.NotEmpty(t, breaks)

	var named bool
	for _, b := range breaks {
		assert.Equal(t, uint64(4), b.Seq, "only the tampered record may break")
		if b.Field == "prev_hash" {
			named = true
		}
	}
	assert.True(t, named, "corrupting prev_hash must be reported as a prev_hash break, got %+v", breaks)
}

// TestVerifyDetectsSignatureTamper proves a flipped signature byte is reported
// against the signed record.
func TestVerifyDetectsSignatureTamper(t *testing.T) {
	l, v, path := verifyFixture(t, 5)

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	var sig []byte
	require.NoError(t, db.QueryRow(`SELECT signature FROM records WHERE seq = 2`).Scan(&sig))
	require.NotEmpty(t, sig)
	sig[0] ^= 0xFF
	_, err = db.Exec(`UPDATE records SET signature = ? WHERE seq = 2`, sig)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	breaks, err := l.Verify(v)
	require.NoError(t, err)
	require.Len(t, breaks, 1, "a flipped signature byte must yield exactly one break")
	assert.Equal(t, uint64(2), breaks[0].Seq)
	assert.Equal(t, "signature", breaks[0].Field)
	assert.Contains(t, breaks[0].Detail, "invalid signature")
}

// TestVerifyUnknownKey proves a chain signed under a key the verifier does not
// trust is reported as a signature break with an unknown-key detail, distinct
// from an invalid signature.
func TestVerifyUnknownKey(t *testing.T) {
	l, _, _ := verifyFixture(t, 3)

	otherPub := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x42}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	v := sign.NewVerifier(map[string]ed25519.PublicKey{"not-the-signer": otherPub})

	breaks, err := l.Verify(v)
	require.NoError(t, err)
	require.Len(t, breaks, 3, "every record signed under an untrusted key must break")
	for _, b := range breaks {
		assert.Equal(t, "signature", b.Field)
		assert.Contains(t, b.Detail, "unknown key", "unknown-key and bad-signature must differ")
		assert.NotContains(t, b.Detail, "invalid signature")
	}
}

// TestVerifyReportsDecodeBreak proves a row whose stored Reason no longer decodes
// (a SQLite-level tamper) surfaces as a decode break rather than an error.
func TestVerifyReportsDecodeBreak(t *testing.T) {
	l, v, path := verifyFixture(t, 3)

	execRaw(t, path, `UPDATE records SET reason_payload = ? WHERE seq = 1`, []byte("not a valid reason"))

	breaks, err := l.Verify(v)
	require.NoError(t, err, "a decode failure must be reported as a break, not an error")
	require.Len(t, breaks, 1)
	assert.Equal(t, "decode", breaks[0].Field)
}

// TestVerifyIndependentOfAtWindow is the trap the brief warns about: the chain
// must be walked by Seq, not through ListRecords' At window. A record whose At
// is far in the past (committed at Append time, so the hash covers it) must
// still be verified. A walk that read the chain through an At window would omit
// that record and then report a seq gap.
func TestVerifyIndependentOfAtWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.db")
	st, err := store.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })

	sg, pub := newSigner(t)
	l := ledger.New(st, sg, func() time.Time { return fixedNow })

	ats := []time.Time{
		time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), // far in the past, interior record
		time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC),
		time.Date(2040, 6, 1, 0, 0, 0, 0, time.UTC), // far in the future
		time.Date(2026, 9, 28, 11, 0, 0, 0, time.UTC),
	}
	for i, at := range ats {
		rec := validRecord(t, record.RecordID(fmt.Sprintf("rec-%04d", i+1)))
		rec.At = at
		_, err := l.Append(rec)
		require.NoError(t, err)
	}

	v := sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})
	breaks, err := l.Verify(v)
	require.NoError(t, err)
	assert.Empty(t, breaks,
		"a record with an odd At must still be verified; an At-windowed walk would drop it and report a seq gap")

	// Prove the far-past record is genuinely visited, not merely unnoticed:
	// tamper it and require the break to name it.
	execRaw(t, path, `UPDATE records SET event = 'memory_kept' WHERE seq = 1`)
	breaks, err = l.Verify(v)
	require.NoError(t, err)
	require.Len(t, breaks, 1)
	assert.Equal(t, uint64(1), breaks[0].Seq, "the far-past record must be examined")
	assert.Equal(t, "hash", breaks[0].Field)
}
