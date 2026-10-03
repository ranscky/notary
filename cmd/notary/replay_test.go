package main

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/record"
)

// newTestReplayCmd builds a replay command whose stdout and stderr are captured
// in SEPARATE buffers. Like newTestExportCmd, and unlike newTestVerifyCmd's
// single shared buffer, the split is deliberate: replay's contract is that
// stdout carries JSONL and nothing else, while operator prose -- the counts, and
// a break report -- goes to stderr, exactly where export puts its prose. A
// single buffer could not tell those apart.
func newTestReplayCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := newReplayCmd()
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd, out, errOut
}

// replayRecord builds a valid record with an explicit RecordedAt, so a test can
// drive the as-of filter directly: Append stamps RecordedAt only when it is
// zero, so the value set here survives into the store. Scope narrows the record
// by its subject scope.
func replayRecord(t *testing.T, id record.RecordID, recordedAt time.Time, scope record.Scope) record.Record {
	t.Helper()
	rec := validCmdRecord(t, id)
	rec.Subject.Scope = scope
	rec.RecordedAt = recordedAt
	require.NoError(t, rec.Validate())
	return rec
}

// ---------------------------------------------------------------------------
// --at is required and never defaults to now (Review Focus 5).
// ---------------------------------------------------------------------------

// TestReplayMissingAtIsRefusedNotNow pins that a missing --at is a refusal, not
// a silent replay of "now". The fixture record is stamped at 2000-01-01, before
// any plausible wall clock, so a command that defaulted a missing --at to now
// would succeed and print it -- exactly the wrong-question footgun the flag
// exists to close. The read (and the store) are never reached.
func TestReplayMissingAtIsRefusedNotNow(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		replayRecord(t, "rec-0001", time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), record.Scope{UserID: "u1"}))

	cmd, out, _ := newTestReplayCmd(t)
	cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub)}

	err := runReplay(cmd, cfg)
	require.Error(t, err, "a missing --at must be refused, never treated as now")
	assert.Contains(t, err.Error(), "--at", "the error must name the flag")
	assert.Empty(t, out.String(), "no records may be replayed without an instant")
}

// TestReplayUnparseableAtIsRefusedNamingTheValue pins that a typo'd --at is a
// plain-language error naming the offending value, never a silent now.
func TestReplayUnparseableAtIsRefusedNamingTheValue(t *testing.T) {
	_, pub := newVerifySigner(t)
	cmd, out, _ := newTestReplayCmd(t)
	require.NoError(t, cmd.Flags().Set("at", "September 28 2026"))
	cfg := &config.Config{DBPath: filepath.Join(t.TempDir(), "ledger.db"), TrustedKeysPath: writeTrustedKeys(t, pub)}

	err := runReplay(cmd, cfg)
	require.Error(t, err, "an unparseable --at must be refused")
	assert.Contains(t, err.Error(), "--at", "the error must name the flag")
	assert.Contains(t, err.Error(), "September 28 2026", "the error must name the value")
	assert.Empty(t, out.String(), "no records may be replayed from an unparsed instant")
}

// ---------------------------------------------------------------------------
// The happy path: JSONL on stdout, counts on stderr, exit 0.
// ---------------------------------------------------------------------------

// TestReplayCleanLedgerWritesJSONLAndExitsZero pins the core success contract:
// the prefix recorded at or before --at renders one JSONL line per record on
// stdout (inclusive of the bound, and nothing recorded after it), stdout carries
// nothing else, and the command exits zero.
func TestReplayCleanLedgerWritesJSONLAndExitsZero(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	appendFixtureRecords(t, dbPath, sg,
		replayRecord(t, "rec-0001", t0, record.Scope{UserID: "u1"}),
		replayRecord(t, "rec-0002", t0.Add(time.Minute), record.Scope{UserID: "u1"}),
		replayRecord(t, "rec-0003", t0.Add(5*time.Minute), record.Scope{UserID: "u1"}))

	cmd, out, errOut := newTestReplayCmd(t)
	// T equals rec-0002's RecordedAt and falls before rec-0003's: the as-of view
	// is the inclusive prefix {seq 0, seq 1}.
	require.NoError(t, cmd.Flags().Set("at", t0.Add(time.Minute).Format(time.RFC3339)))
	cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub)}
	require.NoError(t, runReplay(cmd, cfg), "a clean prefix must exit zero")

	lines := exportLines(t, out.String())
	require.Len(t, lines, 2, "one JSONL line per record at or before --at; the later record is absent")
	assert.Equal(t, record.RecordID("rec-0001"), lines[0].ID)
	assert.Equal(t, record.RecordID("rec-0002"), lines[1].ID)
	assert.Contains(t, errOut.String(), "wrote 2 record(s)", "the counts are reported the way export reports them")
}

// TestReplayEmptyViewSucceedsAndSaysSo pins Review Focus 2: an instant before
// any record is a success that says so on stderr, so "nothing was known yet"
// and "broken invocation" never look the same.
func TestReplayEmptyViewSucceedsAndSaysSo(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		replayRecord(t, "rec-0001", time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC), record.Scope{UserID: "u1"}))

	cmd, out, errOut := newTestReplayCmd(t)
	require.NoError(t, cmd.Flags().Set("at", "2000-01-01T00:00:00Z"))
	cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub)}
	require.NoError(t, runReplay(cmd, cfg), "an empty view is a success")

	assert.Empty(t, exportLines(t, out.String()), "an empty view writes no JSONL to stdout")
	assert.Contains(t, errOut.String(), "no records", "the operator must be told, in words, that nothing was known yet")
}

// ---------------------------------------------------------------------------
// A hole in the prefix: report the breaks, then exit non-zero (verify's model).
// ---------------------------------------------------------------------------

// TestReplayHoleInPrefixReportsBreaksAndExitsNonZero is the CLI half of the
// phase's falsifier. A record stamped with an earlier RecordedAt than its
// predecessor (a backwards clock step) puts that predecessor outside T while its
// successor is inside, so the as-of set has a hole. Replay must render the
// prefix it read AND surface the break, exiting non-zero the way `verify` does:
// the report is written before the non-zero exit, and the error names it.
func TestReplayHoleInPrefixReportsBreaksAndExitsNonZero(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	appendFixtureRecords(t, dbPath, sg,
		replayRecord(t, "rec-0001", t0, record.Scope{UserID: "u1"}),                    // seq 0
		replayRecord(t, "rec-0002", t0.Add(1*time.Minute), record.Scope{UserID: "u1"}), // seq 1
		replayRecord(t, "rec-0003", t0.Add(5*time.Minute), record.Scope{UserID: "u1"}), // seq 2 -- after T
		replayRecord(t, "rec-0004", t0.Add(2*time.Minute), record.Scope{UserID: "u1"})) // seq 3 -- before its predecessor

	cmd, out, errOut := newTestReplayCmd(t)
	// T is after seq 3's time but before seq 2's, so the as-of set is {0,1,3}.
	require.NoError(t, cmd.Flags().Set("at", t0.Add(3*time.Minute).Format(time.RFC3339)))
	cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub)}

	err := runReplay(cmd, cfg)
	require.Error(t, err, "a hole in the prefix must exit non-zero, matching verify")
	assert.Contains(t, err.Error(), "break", "the error must name the break(s)")
	// verify's convention: the report is written BEFORE the non-zero exit.
	assert.Contains(t, errOut.String(), "expected seq 2",
		"the break report must name the missing seq, written before the non-zero exit")
	assert.Len(t, exportLines(t, out.String()), 3, "the prefix records are still written to stdout")
}

// ---------------------------------------------------------------------------
// The scope flags filter as export's do.
// ---------------------------------------------------------------------------

// TestReplayScopeFlagsNarrowTheView pins that --user-id reaches the scope filter:
// only the record whose subject scope matches is replayed.
func TestReplayScopeFlagsNarrowTheView(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	appendFixtureRecords(t, dbPath, sg,
		replayRecord(t, "rec-u1", t0, record.Scope{UserID: "u1"}),
		replayRecord(t, "rec-u2", t0, record.Scope{UserID: "u2"}))

	cmd, out, _ := newTestReplayCmd(t)
	require.NoError(t, cmd.Flags().Set("at", t0.Format(time.RFC3339)))
	require.NoError(t, cmd.Flags().Set("user-id", "u1"))
	cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub)}
	require.NoError(t, runReplay(cmd, cfg))

	lines := exportLines(t, out.String())
	require.Len(t, lines, 1, "only the matching scope must be replayed")
	assert.Equal(t, record.RecordID("rec-u1"), lines[0].ID)
	assert.Equal(t, "u1", lines[0].Scope.UserID)
}

// TestReplayScopeFlagsComposeWithAnd pins the second half of the scope contract:
// the four flags combine with AND, not OR, exactly as export's do.
func TestReplayScopeFlagsComposeWithAnd(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	appendFixtureRecords(t, dbPath, sg,
		replayRecord(t, "rec-a1", t0, record.Scope{UserID: "u1", AgentID: "a1"}),
		replayRecord(t, "rec-a2", t0, record.Scope{UserID: "u1", AgentID: "a2"}))

	cmd, out, _ := newTestReplayCmd(t)
	require.NoError(t, cmd.Flags().Set("at", t0.Format(time.RFC3339)))
	require.NoError(t, cmd.Flags().Set("user-id", "u1"))
	require.NoError(t, cmd.Flags().Set("agent-id", "a1"))
	cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub)}
	require.NoError(t, runReplay(cmd, cfg))

	lines := exportLines(t, out.String())
	require.Len(t, lines, 1, "the scope flags must combine with AND, not OR")
	assert.Equal(t, record.RecordID("rec-a1"), lines[0].ID)
}

// TestReplayIncludeSensitiveControlsRedaction pins that --include-sensitive
// reaches the renderer and changes only what is printed, never a hash.
func TestReplayIncludeSensitiveControlsRedaction(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	t0 := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	rec := contentRecord(t, "rec-secret", t0, record.Scope{UserID: "u1"}, "the secret", true)
	rec.RecordedAt = t0
	appendFixtureRecords(t, dbPath, sg, rec)

	cfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub)}

	cmd, out, _ := newTestReplayCmd(t)
	require.NoError(t, cmd.Flags().Set("at", t0.Format(time.RFC3339)))
	require.NoError(t, runReplay(cmd, cfg))
	redacted := exportLines(t, out.String())
	require.Len(t, redacted, 1)
	assert.Equal(t, "sensitive", redacted[0].Redacted, "the withheld line must state it was redacted")
	assert.Nil(t, redacted[0].Content, "sensitive text must not be rendered by default")

	cmd2, out2, _ := newTestReplayCmd(t)
	require.NoError(t, cmd2.Flags().Set("at", t0.Format(time.RFC3339)))
	require.NoError(t, cmd2.Flags().Set("include-sensitive", "true"))
	require.NoError(t, runReplay(cmd2, cfg))
	shown := exportLines(t, out2.String())
	require.Len(t, shown, 1)
	require.NotNil(t, shown[0].Content, "--include-sensitive must render the text")
	assert.Equal(t, "the secret", *shown[0].Content)
	assert.Empty(t, shown[0].Redacted, "a shown line must not claim redaction")
}

// ---------------------------------------------------------------------------
// The documented surface, and only it.
// ---------------------------------------------------------------------------

// TestReplayHasOnlyTheDocumentedFlags pins the deliberate omissions: replay
// offers --at, --include-sensitive and the four scope flags, and the help text
// must not hint at --from, --to, --phrase, --checkpoint-out or --max-span.
func TestReplayHasOnlyTheDocumentedFlags(t *testing.T) {
	cmd := newReplayCmd()

	for _, want := range []string{"at", "include-sensitive", "user-id", "agent-id", "app-id", "run-id"} {
		assert.NotNil(t, cmd.Flags().Lookup(want), "--%s must be a replay flag", want)
	}
	for _, forbidden := range []string{"from", "to", "phrase", "checkpoint-out", "max-span"} {
		assert.Nil(t, cmd.Flags().Lookup(forbidden), "--%s must not exist on replay", forbidden)
	}

	usage := cmd.UsageString() + cmd.Long
	for _, forbidden := range []string{"--from", "--to", "--phrase", "--checkpoint-out", "--max-span"} {
		assert.NotContains(t, usage, forbidden, "replay's help must not hint at %s", forbidden)
	}
}
