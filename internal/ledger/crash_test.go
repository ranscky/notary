package ledger_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // registers the "sqlite" driver for raw reads

	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// crashHelperTimeout bounds both the helper build and every helper run. The
// crash test dominates the ledger suite at roughly 50s (20 process kills plus a
// build); a hung helper -- a deadlock, a slow build, a stuck SIGKILL watchdog --
// would otherwise hang the whole test run rather than fail it. Four minutes is
// well above the observed cost of any single build or run, so it never trips a
// healthy suite, yet a stuck helper fails within a bounded time instead of
// hanging forever. On expiry exec.CommandContext kills the child, so the wait
// is bounded by this value; the failure message names the timeout rather than
// surfacing a bare "signal: killed".
const crashHelperTimeout = 4 * time.Minute

// crashKeyEnv names the environment variable the crashwriter helper reads its
// signing key from. The test sets it so the helper signs with the same key the
// test derives the public half of.
const crashKeyEnv = "NOTARY_CRASH_KEY"

// buildCrashWriter builds testdata/crashwriter into a temp dir and returns the
// binary path. The helper lives under testdata/ precisely so ordinary builds
// (go build ./...) ignore it; building it explicitly here is what runs it.
func buildCrashWriter(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "crashwriter")

	// Bound the build: a stuck `go build` must fail the test, not hang it.
	ctx, cancel := context.WithTimeout(context.Background(), crashHelperTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "./testdata/crashwriter")
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("building testdata/crashwriter timed out after %s; output:\n%s",
			crashHelperTimeout, out)
	}
	require.NoErrorf(t, err, "building testdata/crashwriter: %s", out)
	return bin
}

// crashSigner sets the helper's key environment variable from the fixed test
// seed and returns a matching Signer and its public key. The helper inherits the
// variable (exec.Command passes the parent environment through), so a signature
// the helper writes verifies under the public key returned here.
func crashSigner(t *testing.T) (*sign.Signer, ed25519.PublicKey) {
	t.Helper()
	t.Setenv(crashKeyEnv, base64.StdEncoding.EncodeToString(testSeed))
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: crashKeyEnv})
	require.NoError(t, err)
	require.NotNil(t, sg)
	pub := ed25519.NewKeyFromSeed(testSeed).Public().(ed25519.PublicKey)
	// The KeyID the helper will attribute its signatures to must be the
	// fingerprint of this exact public key.
	sum := sha256.Sum256(pub)
	require.Equal(t, base64.StdEncoding.EncodeToString(sum[:]), sg.KeyID(),
		"the helper's key ID must be the fingerprint of the test public key")
	return sg, pub
}

// runCrashWriter runs the helper against db in the given mode and returns the
// chain positions it printed. A crash mode is expected to end abnormally --
// exit-after by os.Exit(1), kill-mid by SIGKILL -- so a clean exit there is a
// failure, not a pass.
func runCrashWriter(t *testing.T, bin, db, mode string, n, target int) []int {
	t.Helper()
	args := []string{db, mode, strconv.Itoa(n)}
	if target >= 0 {
		args = append(args, strconv.Itoa(target))
	}

	// Bound the run: a hung helper (a deadlock, a stuck SIGKILL watchdog) must
	// fail this test within crashHelperTimeout, not hang the whole suite.
	ctx, cancel := context.WithTimeout(context.Background(), crashHelperTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, bin, args...).Output()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("crashwriter %s run timed out after %s without terminating -- "+
			"a hung helper must fail the test, not hang it; partial output:\n%s",
			mode, crashHelperTimeout, out)
	}

	if mode == "clean" {
		require.NoErrorf(t, err, "a clean run must exit 0; output:\n%s", out)
	} else {
		var ee *exec.ExitError
		require.ErrorAsf(t, err, &ee, "a crash run must terminate abnormally; output:\n%s", out)
	}
	return parseSeqs(t, out)
}

// parseSeqs reads the helper's stdout: one committed chain position per line.
func parseSeqs(t *testing.T, out []byte) []int {
	t.Helper()
	var seqs []int
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		seq, err := strconv.Atoi(line)
		require.NoErrorf(t, err, "helper stdout line %q is not a chain position", line)
		seqs = append(seqs, seq)
	}
	return seqs
}

// storedBlob is one row's chain-integrity columns read straight from SQLite,
// before any decoding. Reading the raw bytes is what lets the test see a NULL,
// empty, or all-zero column that a decoded record might mask.
type storedBlob struct {
	seq      int64
	hash     []byte
	prevHash []byte
	sig      []byte
}

// readRawBlobs reads every row's hash, prev_hash, and signature columns.
func readRawBlobs(t *testing.T, path string) []storedBlob {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()

	rows, err := db.Query(`SELECT seq, hash, prev_hash, signature FROM records ORDER BY seq ASC`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var out []storedBlob
	for rows.Next() {
		var b storedBlob
		require.NoError(t, rows.Scan(&b.seq, &b.hash, &b.prevHash, &b.sig))
		out = append(out, b)
	}
	require.NoError(t, rows.Err())
	return out
}

// assertNoPartialRow is the explicit "no partial entry" assertion: no row may
// carry a hash, prev_hash, or signature that is absent, empty, or all-zero. A
// half-built record -- one whose hash was never computed or whose signature was
// never written -- would show up as one of those, so this is the check that a
// transaction boundary held.
//
// The one documented exemption is the genesis record's prev_hash: it links to
// record.GenesisHash, the all-zero sentinel by design, so at seq 0 the all-zero
// value is required rather than forbidden. It returns the number of rows seen.
func assertNoPartialRow(t *testing.T, path string) int {
	t.Helper()
	blobs := readRawBlobs(t, path)
	zero32 := make([]byte, 32)
	zeroSig := make([]byte, ed25519.SignatureSize)
	for _, b := range blobs {
		require.Equalf(t, 32, len(b.hash),
			"seq %d: hash must be present and 32 bytes (not NULL or empty)", b.seq)
		assert.NotEqualf(t, zero32, b.hash,
			"seq %d: hash must not be all-zero -- that is a record whose hash was never computed", b.seq)

		require.Equalf(t, 32, len(b.prevHash),
			"seq %d: prev_hash must be present and 32 bytes (not NULL or empty)", b.seq)
		if b.seq == 0 {
			assert.Equalf(t, zero32, b.prevHash,
				"seq 0: prev_hash must be the genesis sentinel, not a real link")
		} else {
			assert.NotEqualf(t, zero32, b.prevHash,
				"seq %d: prev_hash must not be all-zero -- that is a record never linked to its predecessor", b.seq)
		}

		require.Equalf(t, ed25519.SignatureSize, len(b.sig),
			"seq %d: signature must be a full ed25519 signature (not NULL or empty)", b.seq)
		assert.NotEqualf(t, zeroSig, b.sig,
			"seq %d: signature must not be all-zero -- that is a record that was never signed", b.seq)
	}
	return len(blobs)
}

// assertChainIntact asserts the invariants a crash must preserve: Verify reports
// no breaks, every row decodes, positions are contiguous from 0 with no gaps,
// and each stored hash recomputes over its own stored record.
func assertChainIntact(t *testing.T, l *ledger.Ledger, st store.Store, v *sign.Verifier) {
	t.Helper()

	breaks, err := l.Verify(v)
	require.NoError(t, err, "Verify must not error on a crashed ledger")
	assert.Emptyf(t, breaks, "a mid-write crash must leave no chain breaks, got %+v", breaks)

	entries, err := st.SeqEntries()
	require.NoError(t, err, "reading the chain in seq order")
	for i := range entries {
		e := entries[i]
		require.NoErrorf(t, e.DecodeErr, "row at position %d (id %s) must decode", e.Seq, e.ID)
		require.Equalf(t, uint64(i), e.Seq,
			"chain positions must be contiguous from 0 with no gaps; row %d is at seq %d", i, e.Seq)

		want, herr := record.ComputeHash(e.Rec)
		require.NoErrorf(t, herr, "recomputing hash for seq %d", e.Seq)
		assert.Equalf(t, want, e.Rec.Hash,
			"seq %d: the stored hash must equal ComputeHash over the stored record", e.Seq)
	}
}

// assertHeadConsistent asserts the head matches the shape of the surviving
// chain: absent when empty, and otherwise exactly the last present position.
func assertHeadConsistent(t *testing.T, l *ledger.Ledger, present int) {
	t.Helper()
	head, ok, err := l.Head()
	require.NoError(t, err)
	if present == 0 {
		assert.False(t, ok, "an empty ledger must report no head")
		return
	}
	require.True(t, ok, "a non-empty ledger must report a head")
	assert.Equalf(t, uint64(present-1), head.Seq,
		"the head must be the last fully-committed record, with no gap before it")
}

// TestCrashMidWriteLeavesChainIntact is the property this task exists to prove:
// because Ledger.Append reads the head, hashes, signs, and inserts inside one
// transaction, a process killed mid-append leaves the ledger either with the
// record fully present or not present at all -- never a partial or corrupt link.
//
// The test cannot force a single, precise death instant, so it tries hard: it
// drives the helper repeatedly across several crash points and record counts
// (against a fresh database each time) and, after every run, asserts the
// invariants rather than any particular surviving count. A single lucky run
// therefore cannot make it pass vacuously.
func TestCrashMidWriteLeavesChainIntact(t *testing.T) {
	bin := buildCrashWriter(t)
	sg, pub := crashSigner(t)
	v := sign.NewVerifier(map[string]ed25519.PublicKey{sg.KeyID(): pub})

	// Each case dies at a different moment relative to the write path. Targets
	// are deliberately well below n so the watchdog always fires while appends
	// remain -- i.e. the process dies with the writer still in flight.
	cases := []struct {
		name   string
		mode   string
		n      int
		target int
	}{
		{"exit-after-last-commit", "exit-after", 8, -1},
		{"kill-mid-second-append", "kill-mid", 64, 1},
		{"kill-mid-early", "kill-mid", 64, 2},
		{"kill-mid-quarter", "kill-mid", 64, 16},
		{"kill-mid-half", "kill-mid", 64, 32},
	}

	// Four repeats per crash point: the death instant is scheduled by the OS
	// and cannot be pinned, so repetition is what turns one lucky survivor into
	// a claim about the boundary. Across the five cases that is 20 genuine
	// process kills, each with its own surviving prefix to check.
	const repeats = 4

	for _, tc := range cases {
		tc := tc
		for iter := 0; iter < repeats; iter++ {
			iter := iter
			t.Run(fmt.Sprintf("%s/iter-%d", tc.name, iter), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "crash.db")
				printed := runCrashWriter(t, bin, path, tc.mode, tc.n, tc.target)

				// (1) The explicit no-partial-row assertion, at the raw byte
				// level, before anything is decoded.
				present := assertNoPartialRow(t, path)

				// Durability: every record the writer reported as committed
				// (printed only after Append returned) must still be present.
				// A printed record that vanished would be a lost audit entry.
				require.GreaterOrEqualf(t, present, len(printed),
					"every committed (printed) record must survive the crash")

				// (2) Reopen the store and verify the chain in full.
				st, err := store.Open(path)
				require.NoError(t, err)
				l := ledger.New(st, nil, nil)

				assertChainIntact(t, l, st, v)
				assertHeadConsistent(t, l, present)
				require.NoError(t, st.Close())
			})
		}
	}

	// A checkpoint taken before the crash must still be satisfied afterwards:
	// a crash that only appends can lengthen the chain but never shorten it, so
	// it must not read as truncation -- the one failure a checkpoint exists to
	// expose.
	t.Run("checkpoint-taken-before-the-crash-reports-no-truncation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cp.db")

		// Establish a base chain, then sign a checkpoint attesting to its head.
		runCrashWriter(t, bin, path, "clean", 6, -1)
		base, err := store.Open(path)
		require.NoError(t, err)
		baseLedger := ledger.New(base, sg, func() time.Time { return fixedNow })
		cp, err := baseLedger.Checkpoint(sg, fixedNow)
		require.NoError(t, err)
		require.Equal(t, uint64(5), cp.Seq, "the checkpoint must attest to the base head")
		require.NoError(t, base.Close())

		// Crash a writer mid-append on top of the checkpointed chain.
		runCrashWriter(t, bin, path, "kill-mid", 64, 16)

		st, err := store.Open(path)
		require.NoError(t, err)
		defer func() { require.NoError(t, st.Close()) }()
		l := ledger.New(st, nil, nil)

		trunc, terr := l.VerifyAgainstCheckpoint(cp, v)
		require.NoError(t, terr, "a crash that appends must not read as truncation")
		assert.Emptyf(t, trunc, "no truncation break may be reported, got %+v", trunc)

		assertChainIntact(t, l, st, v)
	})
}
