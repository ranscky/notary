package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/record"
)

// reportFrom and reportTo bound a range that holds every fixture record:
// validCmdRecord stamps each one with verifyFixedNow, 2026-09-28T12:00:00Z.
const (
	reportFrom = "2026-09-28T00:00:00Z"
	reportTo   = "2026-09-29T00:00:00Z"
)

// newTestReportCmd builds a report command whose stdout and stderr are captured
// in SEPARATE buffers. Like newTestExplainCmd -- and unlike newTestVerifyCmd's
// single shared buffer -- the split is deliberate: report's summary goes to
// stdout, and a refusal must leave stdout empty rather than half-written, which
// one buffer could not pin.
func newTestReportCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := newReportCmd()
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd, out, errOut
}

// reportFixture appends recs to a fresh signed ledger and returns a config
// pointing at it, at the gap log beside it, and at a trusted-keys file holding
// the fixture's public key. The ledger is REAL -- the records are appended and
// signed through the common test signer -- so the command reads what the store
// actually holds rather than a stub.
func reportFixture(t *testing.T, recs []record.Record) *config.Config {
	t.Helper()
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	appendFixtureRecords(t, dbPath, sg, recs...)
	return &config.Config{
		DBPath:          dbPath,
		GapLogPath:      filepath.Join(dir, "notary-gaps.log"),
		TrustedKeysPath: writeTrustedKeys(t, pub),
	}
}

// reportRecords builds one valid record per memory id given, with distinct ids
// in Seq order: the ordinary slice a range report covers.
func reportRecords(t *testing.T, memoryIDs ...string) []record.Record {
	t.Helper()
	recs := make([]record.Record, 0, len(memoryIDs))
	for i, id := range memoryIDs {
		rec := validCmdRecord(t, record.RecordID(fmt.Sprintf("rec-%04d", i+1)))
		rec.Subject.MemoryID = id
		recs = append(recs, rec)
	}
	return recs
}

// reportFixtureLedger is reportFixture over one valid record per memory id.
func reportFixtureLedger(t *testing.T, memoryIDs ...string) *config.Config {
	t.Helper()
	return reportFixture(t, reportRecords(t, memoryIDs...))
}

// setReportFlags sets every flag named, failing the test when one does not
// exist, so a test cannot pass by setting a flag the command never declared.
func setReportFlags(t *testing.T, cmd *cobra.Command, flags map[string]string) {
	t.Helper()
	for name, value := range flags {
		require.NoError(t, cmd.Flags().Set(name, value), "report has no usable flag --%s", name)
	}
}

// readFileText reads path as a string, failing the test when it is absent.
func readFileText(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)
	return string(data)
}

// filesWithSuffix returns the names of the non-directory entries in dir that
// end in suffix, so a test can count the pages one kind produced without
// knowing their derived names.
func filesWithSuffix(t *testing.T, dir, suffix string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "reading %s", dir)
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), suffix) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// dirEntryNames returns the entries dir holds, so a test can assert a refusal
// changed nothing at all -- not even one directory entry.
func dirEntryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "reading %s", dir)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// ---------------------------------------------------------------------------
// Exactly one subject is required, and an empty --memory is its own refusal.
// ---------------------------------------------------------------------------

// TestReportCmdRequiresExactlyOneSubject pins the subject rule on both sides: a
// memory id and a range together are refused naming both, and neither given is
// refused saying how to give one. Both are usage errors, so both leave stdout
// empty and create no output directory.
func TestReportCmdRequiresExactlyOneSubject(t *testing.T) {
	cfg := reportFixtureLedger(t, "mem-1", "mem-2")
	out := filepath.Join(t.TempDir(), "report")

	t.Run("both", func(t *testing.T) {
		cmd, stdout, _ := newTestReportCmd(t)
		setReportFlags(t, cmd, map[string]string{
			"out": out, "memory": "mem-1", "from": reportFrom,
		})

		err := runReport(cmd, cfg)
		require.Error(t, err, "a memory id and a range together must be refused")
		assert.Contains(t, err.Error(), "mem-1", "the error must name the memory id given")
		assert.Contains(t, err.Error(), reportFrom, "the error must name the range given")
		assert.Contains(t, err.Error(), "not both", "the error must say the two are mutually exclusive")
		assert.Empty(t, stdout.String(), "a usage error must leave stdout clean")
		assert.NoDirExists(t, out, "a refused run must create no output directory")
	})

	t.Run("neither", func(t *testing.T) {
		cmd, stdout, _ := newTestReportCmd(t)
		setReportFlags(t, cmd, map[string]string{"out": out})

		err := runReport(cmd, cfg)
		require.Error(t, err, "no subject must be refused")
		assert.Contains(t, err.Error(), "--memory", "the error must show how to name one memory")
		assert.Contains(t, err.Error(), "--from", "the error must show how to name a range")
		assert.Empty(t, stdout.String(), "a usage error must leave stdout clean")
		assert.NoDirExists(t, out, "a refused run must create no output directory")
	})

	t.Run("only --to", func(t *testing.T) {
		cmd, stdout, _ := newTestReportCmd(t)
		setReportFlags(t, cmd, map[string]string{"out": out, "to": reportTo})

		err := runReport(cmd, cfg)
		require.Error(t, err, "an end with no start names no subject")
		assert.Contains(t, err.Error(), reportTo, "the error must name the --to that was given")
		assert.Contains(t, err.Error(), "--from", "the error must name the flag the range is missing")
		assert.Empty(t, stdout.String(), "a usage error must leave stdout clean")
		assert.NoDirExists(t, out, "a refused run must create no output directory")
	})

	// An explicitly empty --memory is refused with its OWN message, not the
	// neither-subject one: the store matches an empty memory_id -- the column's
	// DEFAULT -- so an empty filter would report every memory-less record in the
	// ledger as though it were one memory. The fixture plants exactly such a
	// record, so a command that passed the empty id through would report it.
	t.Run("an empty --memory", func(t *testing.T) {
		rec := validCmdRecord(t, "rec-nomem")
		rec.Subject.MemoryID = ""
		cfg := reportFixture(t, []record.Record{rec, validCmdRecord(t, "rec-0002")})
		out := filepath.Join(t.TempDir(), "report")

		cmd, stdout, _ := newTestReportCmd(t)
		setReportFlags(t, cmd, map[string]string{"out": out, "memory": ""})

		err := runReport(cmd, cfg)
		require.Error(t, err, "an explicitly empty --memory must be refused")
		assert.Contains(t, err.Error(), "--memory", "the error must name the flag")
		assert.Contains(t, err.Error(), "non-empty", "the error must say what the flag needs")
		assert.NotContains(t, err.Error(), "a subject is required",
			"an empty --memory must not be answered by the neither-subject message")
		assert.Empty(t, stdout.String(), "the empty filter must never reach the reader")
		assert.NoDirExists(t, out)
	})
}

// ---------------------------------------------------------------------------
// The request the renderer is handed: memory alone, a range with a scope, and
// the clock the default --to comes from.
// ---------------------------------------------------------------------------

// TestReportRequestFromDefaultsToNow pins the range's end: --from alone means
// "from then until now", resolved from the clock the command passes in rather
// than from a second reading of the environment, and the scope flags narrow it.
func TestReportRequestFromDefaultsToNow(t *testing.T) {
	fixed := time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)
	cmd, _, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{
		"from": reportFrom, "user-id": "u1", "agent-id": "a1",
	})

	req, err := reportRequest(cmd, func() time.Time { return fixed })
	require.NoError(t, err)
	assert.Equal(t, fixed, req.To, "--to must default to the clock the command handed in")
	assert.Equal(t, record.Scope{UserID: "u1", AgentID: "a1"}, req.Scope,
		"the four scope flags must reach the request, so the range is narrowed as export and replay narrow it")
	assert.True(t, req.Verify, "verification is the default; --no-verify is the opt-out")
	assert.Empty(t, req.MemoryID, "a range names no memory")
}

// TestReportRequestNoVerifyReachesTheRendererAsAFalseVerify pins Review Focus 5
// at the request: --no-verify is carried as Verify: false, never as an empty
// break list, because len(Breaks) == 0 cannot tell a clean chain from a check
// that never ran.
func TestReportRequestNoVerifyReachesTheRendererAsAFalseVerify(t *testing.T) {
	cmd, _, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{"from": reportFrom, "no-verify": "true"})

	req, err := reportRequest(cmd, time.Now)
	require.NoError(t, err)
	assert.False(t, req.Verify, "--no-verify must reach the renderer as Verify: false")
	assert.Empty(t, req.Breaks, "and with no breaks, which is exactly the ambiguity Verify resolves")
	assert.Zero(t, req.VerifiedAt, "the run's instant is set by the command, not the flag parser")
}

// TestReportRequestMemoryIsAlone pins the shape a memory subject must arrive
// in: no window and the zero scope. internal/report refuses a memory id
// combined with either -- a page must not assert a narrowing the read never
// applied -- so a command that passed one alongside --memory would fail inside
// the renderer instead of at its own flags.
func TestReportRequestMemoryIsAlone(t *testing.T) {
	cmd, _, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{"memory": "mem-1"})

	req, err := reportRequest(cmd, time.Now)
	require.NoError(t, err)
	assert.Equal(t, "mem-1", req.MemoryID)
	assert.True(t, req.From.IsZero() && req.To.IsZero(), "a memory subject must carry no window")
	assert.Equal(t, record.Scope{}, req.Scope, "a memory subject must carry the zero scope")
}

// TestReportCmdRefusesScopeFlagsWithMemory pins the other half of that rule
// where the operator meets it: the scope flags narrow a RANGE, so combining
// them with --memory is refused naming the flag that was given and the flags
// that would work instead -- before anything is read or written.
func TestReportCmdRefusesScopeFlagsWithMemory(t *testing.T) {
	cfg := reportFixtureLedger(t, "mem-1", "mem-2")

	// Every one of the four, because a refusal that covered only the first
	// would let --agent-id, --app-id or --run-id reach the renderer.
	for _, flag := range []string{"user-id", "agent-id", "app-id", "run-id"} {
		t.Run("--"+flag, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "report")
			cmd, stdout, _ := newTestReportCmd(t)
			setReportFlags(t, cmd, map[string]string{"out": out, "memory": "mem-1", flag: "u1"})

			err := runReport(cmd, cfg)
			require.Error(t, err, "--memory with --%s must be refused", flag)
			assert.Contains(t, err.Error(), "--"+flag, "the error must name the scope flag that was given")
			assert.Contains(t, err.Error(), "mem-1", "the error must name the memory id")
			assert.Contains(t, err.Error(), "--from", "the error must say how to narrow a range instead")
			assert.Empty(t, stdout.String(), "a usage error must leave stdout clean")
			assert.NoDirExists(t, out, "a refused run must create no output directory")
		})
	}
}

// TestReportCmdRejectsAReversedRange pins that a backwards window is refused
// before the ledger is opened, naming both instants -- the same shape the
// renderer's own validation refuses, reached at the flags the operator typed.
func TestReportCmdRejectsAReversedRange(t *testing.T) {
	cfg := reportFixtureLedger(t, "mem-1")
	out := filepath.Join(t.TempDir(), "report")

	cmd, stdout, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{"out": out, "from": reportTo, "to": reportFrom})

	err := runReport(cmd, cfg)
	require.Error(t, err, "a range that ends before it starts must be refused")
	assert.Contains(t, err.Error(), "before", "the error must say the range is backwards")
	assert.Contains(t, err.Error(), reportTo, "the error must name the instant given as --from")
	assert.Contains(t, err.Error(), reportFrom, "the error must name the instant given as --to")
	assert.Empty(t, stdout.String())
	assert.NoDirExists(t, out)
}

// ---------------------------------------------------------------------------
// --out: required, and refused when it already holds something (never implied).
// ---------------------------------------------------------------------------

// TestReportCmdRequiresOut pins that a report is never written somewhere the
// operator did not name.
func TestReportCmdRequiresOut(t *testing.T) {
	cfg := reportFixtureLedger(t, "mem-1")

	cmd, stdout, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{"from": reportFrom})

	err := runReport(cmd, cfg)
	require.Error(t, err, "a report with no --out must be refused")
	assert.Contains(t, err.Error(), "--out", "the error must name the flag")
	assert.Empty(t, stdout.String())
}

// TestReportCmdRefusesANonEmptyOutDirectory pins the footgun guard: a report
// generator that writes into whatever directory it is handed merges itself into
// the operator's files. The refusal leaves the directory byte-for-byte as it
// was -- no page, no subdirectory, no entry added.
func TestReportCmdRefusesANonEmptyOutDirectory(t *testing.T) {
	cfg := reportFixtureLedger(t, "mem-1", "mem-2")
	out := t.TempDir()
	keep := filepath.Join(out, "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("the operator's own file\n"), 0o600))
	before := dirEntryNames(t, out)

	cmd, stdout, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{"out": out, "from": reportFrom})

	err := runReport(cmd, cfg)
	require.Error(t, err, "a non-empty --out must be refused without --force")
	assert.Contains(t, err.Error(), out, "the error must name the directory")
	assert.Contains(t, err.Error(), "--force", "the error must name the way to accept it")
	assert.Empty(t, stdout.String(), "a refused run must leave stdout clean")
	assert.Equal(t, before, dirEntryNames(t, out), "the refusal must leave the directory untouched")
	assert.Equal(t, "the operator's own file\n", readFileText(t, keep),
		"and the files it already held must be byte-for-byte unchanged")
}

// TestReportCmdForceWritesIntoANonEmptyOutDirectory pins that --force is the
// only thing that lets the report into a directory that holds something: it
// writes its pages beside what is there and deletes nothing.
func TestReportCmdForceWritesIntoANonEmptyOutDirectory(t *testing.T) {
	cfg := reportFixtureLedger(t, "mem-1", "mem-2")
	out := t.TempDir()
	keep := filepath.Join(out, "keep.txt")
	require.NoError(t, os.WriteFile(keep, []byte("the operator's own file\n"), 0o600))

	cmd, stdout, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{"out": out, "from": reportFrom, "force": "true"})

	require.NoError(t, runReport(cmd, cfg), "--force must let the report into a directory that holds files")
	assert.FileExists(t, filepath.Join(out, "index.html"))
	assert.Equal(t, "the operator's own file\n", readFileText(t, keep),
		"--force writes its pages and deletes nothing")
	assert.Contains(t, stdout.String(), "wrote", "the run must report what it wrote")
}

// TestReportCmdRefusesAnOutPathThatIsAFile pins the other unusable --out: a
// path that is a file cannot hold a report, and the refusal names it rather
// than failing later with a driver error.
func TestReportCmdRefusesAnOutPathThatIsAFile(t *testing.T) {
	cfg := reportFixtureLedger(t, "mem-1")
	out := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(out, []byte("x"), 0o600))

	cmd, stdout, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{"out": out, "from": reportFrom})

	err := runReport(cmd, cfg)
	require.Error(t, err, "--out that is a file must be refused")
	assert.Contains(t, err.Error(), out, "the error must name the path")
	assert.Empty(t, stdout.String())
	assert.Equal(t, "x", readFileText(t, out), "the file must be left as it was")
}

// ---------------------------------------------------------------------------
// The end-to-end render, and what the command says it wrote.
// ---------------------------------------------------------------------------

// TestReportCmdWritesThePagesIntoOut is the end-to-end contract against a real
// ledger: the four fixed files, one page per memory and one per record, and the
// summary reporting every count Result carries -- with Pages counting every
// file written, the two assets included.
func TestReportCmdWritesThePagesIntoOut(t *testing.T) {
	cfg := reportFixtureLedger(t, "mem-1", "mem-1", "mem-2")
	out := filepath.Join(t.TempDir(), "report")

	cmd, stdout, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{"out": out, "from": reportFrom, "to": reportTo})

	require.NoError(t, runReport(cmd, cfg))

	assert.FileExists(t, filepath.Join(out, "index.html"), "the index is the report's entry point")
	assert.FileExists(t, filepath.Join(out, "verify.html"), "the chain-state page is written in every run")
	assert.FileExists(t, filepath.Join(out, "assets", "report.css"))
	assert.FileExists(t, filepath.Join(out, "assets", "report.js"))
	assert.Len(t, filesWithSuffix(t, filepath.Join(out, "memory"), ".html"), 2, "one page per memory")
	assert.Len(t, filesWithSuffix(t, filepath.Join(out, "record"), ".html"), 3, "one page per record")

	// 2 memories + 3 records + the index + the chain-state page + the two
	// assets = 9 files, which is what Pages counts and what the operator is
	// holding.
	summary := stdout.String()
	assert.Contains(t, summary, "wrote 9 pages to "+out, "Pages counts every file written, assets included")
	assert.Contains(t, summary, "2 memories", "Memories must be reported")
	assert.Contains(t, summary, "3 records", "Records must be reported")
	assert.Contains(t, summary, "0 records with content withheld", "Redacted must be reported")
	assert.Contains(t, summary, "the chain state is clean",
		"an intact ledger's run must say so, with the artefact carrying the same state")

	// The slice really was read: the index carries both memories.
	index := readFileText(t, filepath.Join(out, "index.html"))
	assert.Contains(t, index, "mem-1")
	assert.Contains(t, index, "mem-2")
}

// TestReportCmdMemoryRendersThatMemoryAlone pins the memory subject end to end:
// the slice is that memory's records and nothing else, so a command that
// quietly widened the read would be visible in the pages and in the counts.
func TestReportCmdMemoryRendersThatMemoryAlone(t *testing.T) {
	cfg := reportFixture(t, reportRecords(t, "mem-1", "mem-2", "mem-1"))
	out := filepath.Join(t.TempDir(), "report")

	cmd, stdout, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{"out": out, "memory": "mem-1"})

	require.NoError(t, runReport(cmd, cfg))

	assert.Len(t, filesWithSuffix(t, filepath.Join(out, "memory"), ".html"), 1, "one memory page")
	assert.Len(t, filesWithSuffix(t, filepath.Join(out, "record"), ".html"), 2,
		"only mem-1's two records")
	index := readFileText(t, filepath.Join(out, "index.html"))
	assert.Contains(t, index, "mem-1")
	assert.NotContains(t, index, "mem-2", "the index must describe the slice that was read")
	assert.Contains(t, stdout.String(), "1 memory,", "Memories counts the pages written")
	assert.Contains(t, stdout.String(), "2 records")
}

// TestReportCmdNarrowsARangeByScope pins that the scope flags reach the read:
// the same ledger gives two records unfiltered and one when narrowed, and the
// page carries the narrowing it applied.
func TestReportCmdNarrowsARangeByScope(t *testing.T) {
	rec1 := validCmdRecord(t, "rec-0001") // scope u1/a1
	rec2 := validCmdRecord(t, "rec-0002")
	rec2.Subject.Scope = record.Scope{UserID: "u2", AgentID: "a2"}
	cfg := reportFixture(t, []record.Record{rec1, rec2})

	run := func(t *testing.T, flags map[string]string) (string, string) {
		t.Helper()
		out := filepath.Join(t.TempDir(), "report")
		flags["out"] = out
		flags["from"] = reportFrom
		cmd, stdout, _ := newTestReportCmd(t)
		setReportFlags(t, cmd, flags)
		require.NoError(t, runReport(cmd, cfg))
		return out, stdout.String()
	}

	unfiltered, unfilteredOut := run(t, map[string]string{})
	assert.Len(t, filesWithSuffix(t, filepath.Join(unfiltered, "record"), ".html"), 2,
		"the un-narrowed range holds both records")

	narrowed, narrowedSummary := run(t, map[string]string{"user-id": "u1"})
	assert.Len(t, filesWithSuffix(t, filepath.Join(narrowed, "record"), ".html"), 1,
		"--user-id u1 must narrow the range to the one record in that scope")
	assert.Contains(t, narrowedSummary, "1 record,")
	assert.Contains(t, unfilteredOut, "2 records")
}

// TestReportCmdReportsWithheldContent pins --include-sensitive at the command:
// the default withholds a sensitive record's text and the summary says how many
// records were withheld; the flag prints it and changes no hash.
func TestReportCmdReportsWithheldContent(t *testing.T) {
	const text = "the memory text the rules marked sensitive"
	rec := contentRecord(t, "rec-0001", verifyFixedNow, record.Scope{UserID: "u1"}, text, true)
	cfg := reportFixture(t, []record.Record{rec})

	run := func(t *testing.T, includeSensitive bool) (string, string) {
		t.Helper()
		out := filepath.Join(t.TempDir(), "report")
		flags := map[string]string{"out": out, "from": reportFrom}
		if includeSensitive {
			flags["include-sensitive"] = "true"
		}
		cmd, stdout, _ := newTestReportCmd(t)
		setReportFlags(t, cmd, flags)
		require.NoError(t, runReport(cmd, cfg))
		return out, stdout.String()
	}

	withheldDir, withheldSummary := run(t, false)
	assert.Contains(t, withheldSummary, "1 record with content withheld",
		"the default run must report what it withheld")
	assert.NotContains(t, reportText(t, withheldDir), text,
		"a sensitive record's text must be absent from every generated file")

	shownDir, shownSummary := run(t, true)
	assert.Contains(t, shownSummary, "0 records with content withheld")
	assert.Contains(t, reportText(t, shownDir), text,
		"--include-sensitive must print the stored text")

	assert.Equal(t, reportHashes(t, withheldDir), reportHashes(t, shownDir),
		"--include-sensitive changes only what is printed, never a hash")
}

// ---------------------------------------------------------------------------
// Verification: what --no-verify means, and what a missing keyring means.
// ---------------------------------------------------------------------------

// TestReportCmdNoVerifyRendersAsNotVerified pins Review Focus 5 through the
// command. The fixture is an INTACT ledger with a usable keyring, so "not
// verified" here can only be the flag's doing -- and the page must say no
// verification ran rather than showing an empty break list as a pass. The
// keyring is also pointed nowhere in the second half: --no-verify spares the
// walk and the keys it would need, and a run that loaded them anyway would fail
// there.
func TestReportCmdNoVerifyRendersAsNotVerified(t *testing.T) {
	cfg := reportFixtureLedger(t, "mem-1", "mem-2")

	// Control: the ledger is intact, so a page that read "clean" would be
	// saying the walk happened.
	verifyCmd, vbuf := newTestVerifyCmd(t)
	require.NoError(t, runVerify(verifyCmd, cfg, "", ""))
	assert.Contains(t, vbuf.String(), "ok:", "the fixture ledger must be intact")

	out := filepath.Join(t.TempDir(), "report")
	cmd, stdout, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{"out": out, "from": reportFrom, "no-verify": "true"})
	require.NoError(t, runReport(cmd, cfg), "--no-verify is a success, not a failure")

	page := readFileText(t, filepath.Join(out, "verify.html"))
	assert.Contains(t, page, "Chain state: not verified",
		"the chain-state page must exist and say which state it is in")
	assert.Contains(t, page, "No verification ran for this report")
	assert.NotContains(t, page, "Chain state: clean", "a skipped check must never render as clean")
	assert.NotContains(t, page, "collected no break",
		"and never as a passing check in different words")
	assert.Contains(t, stdout.String(), "the chain state is not verified",
		"the command's own summary must say the same thing the page does")

	t.Run("no keyring is needed at all", func(t *testing.T) {
		cfg := reportFixtureLedger(t, "mem-1")
		cfg.TrustedKeysPath = filepath.Join(t.TempDir(), "no-such-keyring")
		out := filepath.Join(t.TempDir(), "report")

		cmd, _, _ := newTestReportCmd(t)
		setReportFlags(t, cmd, map[string]string{"out": out, "from": reportFrom, "no-verify": "true"})

		require.NoError(t, runReport(cmd, cfg),
			"--no-verify must not load the trusted keys, so an unusable keyring cannot fail it")
		assert.FileExists(t, filepath.Join(out, "verify.html"))
	})
}

// TestReportCmdRefusesAVerifiedRunWithNoTrustedKeys pins that the chain state
// is never silently downgraded: without a keyring and without --no-verify, the
// report refuses rather than writing one that claims nothing and letting the
// operator believe it was checked. The error names both ways forward.
func TestReportCmdRefusesAVerifiedRunWithNoTrustedKeys(t *testing.T) {
	cfg := reportFixtureLedger(t, "mem-1")
	cfg.TrustedKeysPath = ""
	out := filepath.Join(t.TempDir(), "report")

	cmd, stdout, _ := newTestReportCmd(t)
	setReportFlags(t, cmd, map[string]string{"out": out, "from": reportFrom})

	err := runReport(cmd, cfg)
	require.Error(t, err, "a run that cannot compute the chain state must not pretend it did")
	assert.Contains(t, err.Error(), config.EnvTrustedKeysPath,
		"the error must name the variable that fixes it")
	assert.Contains(t, err.Error(), "--no-verify",
		"the error must name the way to ask for a report without the chain state")
	assert.Empty(t, stdout.String(), "a refused run must leave stdout clean")
	assert.NoDirExists(t, out, "a refused run must write nothing")
}

// ---------------------------------------------------------------------------
// The help text: the two facts a reader needs BEFORE they email the folder.
// ---------------------------------------------------------------------------

// TestReportCmdHelpNamesTheSetupPageAndTheEvidenceRule pins both facts the
// brief requires the help to carry, checked for substance rather than for
// wording: where the setup page lives (spec §5 -- `notary doctor --out`, the
// whole discoverability cost of keeping that page on doctor), and that this
// report's EVIDENCE blocks are not filtered by --include-sensitive (spec §12.5
// -- reconcile's observed payloads are whole upstream objects and can carry
// memory text the sensitivity rules never marked, so the flag narrows the
// content the report prints and not the record's own bytes).
//
// The second half pins the flag set in BOTH directions -- the eleven flags the
// design lists and nothing else -- so a stray flag cannot appear unnoticed.
func TestReportCmdHelpNamesTheSetupPageAndTheEvidenceRule(t *testing.T) {
	cmd, out, _ := newTestReportCmd(t)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())

	help := out.String()
	assert.Contains(t, help, "notary doctor --out",
		"the help must name where the setup page lives")
	assert.Contains(t, help, "evidence",
		"the help must name the evidence block the disclosure is about")
	assert.Contains(t, strings.ToLower(help), "not filtered",
		"the help must say the block is not filtered by the sensitivity flag")
	assert.Contains(t, help, "--include-sensitive",
		"and name the flag it is not filtered by")
	assert.Contains(t, help, "memory text",
		"and say what can reach it: memory text the sensitivity rules never marked")

	// The flags, read from a fresh command: Execute adds Cobra's own --help,
	// which is not one of this command's flags.
	fresh := newReportCmd()
	var got []string
	fresh.Flags().VisitAll(func(f *pflag.Flag) { got = append(got, f.Name) })
	sort.Strings(got)
	assert.Equal(t, []string{
		"agent-id", "app-id", "force", "from", "include-sensitive", "memory",
		"no-verify", "out", "run-id", "to", "user-id",
	}, got, "the flag set is the design's list, in both directions")
}

// ---------------------------------------------------------------------------
// The hazard this task exists to settle: a report never creates a ledger.
// ---------------------------------------------------------------------------

// TestReportCmdNeverCreatesAMissingLedger is the hazard guard, driven through
// the ROOT command so the pre-run hook is in the picture.
//
// Two mechanisms have to hold, and each is the other's falsification:
//
//   - the root pre-run's ensureLedgerFile must not run for report (the
//     exemption `doctor` also carries), or a mistyped path would have its
//     directory and an empty ledger file created before report ever looked; and
//   - report must establish that the ledger exists before opening it, because
//     store.Open -- and the sqlite driver under it -- CREATES a missing
//     database. Without that check a mistyped path that already has a parent
//     directory would grow an empty ledger, the report would render zero
//     memories over it, and the run would exit 0: an authoritative-looking
//     compliance artefact describing nothing, over a path the operator
//     mistyped, that no reader could tell from a real empty ledger.
func TestReportCmdNeverCreatesAMissingLedger(t *testing.T) {
	cases := []struct {
		name   string
		layout func(dir string) (dbPath string, mustNotExist []string)
	}{
		{
			name: "the ledger file is missing",
			layout: func(dir string) (string, []string) {
				dbPath := filepath.Join(dir, "ledger.db")
				return dbPath, []string{dbPath}
			},
		},
		{
			name: "the ledger's directory is missing too",
			layout: func(dir string) (string, []string) {
				dbPath := filepath.Join(dir, "sub", "ledger.db")
				return dbPath, []string{dbPath, filepath.Join(dir, "sub")}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			dbPath, mustNotExist := tc.layout(dir)
			t.Setenv(config.EnvDBPath, dbPath)
			t.Setenv(config.EnvGapLogPath, filepath.Join(dir, "notary-gaps.log"))
			t.Setenv(config.EnvTrustedKeysPath, "")
			out := filepath.Join(dir, "report")

			stdout, err := executeRootCmd(t, "report",
				"--out", out, "--from", reportFrom, "--no-verify")

			require.Error(t, err, "a ledger that is not there must be refused, not created")
			assert.Contains(t, err.Error(), dbPath, "the error must name the path that was looked for")
			assert.NotContains(t, err.Error(), "creating ledger",
				"the root pre-run must not have run: report carries the exemption")
			for _, path := range mustNotExist {
				_, statErr := os.Stat(path)
				assert.ErrorIs(t, statErr, os.ErrNotExist,
					"a report must create no ledger and no directory for one: %s", path)
			}
			assert.NoDirExists(t, out, "and it must write no report for a ledger it could not read")
			assert.Empty(t, stdout, "the failure must leave stdout clean")
		})
	}
}

// ---------------------------------------------------------------------------
// The differential: a real tampered ledger, both commands, one answer.
// ---------------------------------------------------------------------------

// reportBreakRow matches one row of verify.html's break table: the chain
// position, the record cell (a link or a bare <code>), the field, and what the
// check found.
var reportBreakRow = regexp.MustCompile(
	`(?s)<tr><td>(\d+)</td><td>(.*?)</td><td><code>(.*?)</code></td><td>(.*?)</td></tr>`)

// reportBreakRecordID reads the record id out of a break row's record cell.
var reportBreakRecordID = regexp.MustCompile(`<code>(.*?)</code>`)

// reportBreakIdentities reads the breaks out of verify.html as the same
// identities verifyBreakIdentities reads out of `notary verify`'s own output,
// so the two answers can be compared as breaks rather than as formatted text.
func reportBreakIdentities(t *testing.T, page string) []verifyBreakIdentity {
	t.Helper()
	var ids []verifyBreakIdentity
	for _, m := range reportBreakRow.FindAllStringSubmatch(page, -1) {
		seq, err := strconv.ParseUint(m[1], 10, 64)
		require.NoError(t, err, "the row's chain position %q must be a number", m[1])
		var recordID string
		if code := reportBreakRecordID.FindStringSubmatch(m[2]); code != nil {
			recordID = code[1]
		}
		ids = append(ids, verifyBreakIdentity{recordID: recordID, seq: seq, field: m[3]})
	}
	return ids
}

// reportHashHex matches one 64-hex-character hash, the one length every hash in
// a record carries.
var reportHashHex = regexp.MustCompile(`\b[0-9a-f]{64}\b`)

// reportText is every generated file's text, so a test can assert on a page
// kind it cannot name.
func reportText(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	require.NoError(t, filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		b.Write(data)
		return nil
	}))
	return b.String()
}

// reportHashes is every hash the report printed, sorted, so two runs can be
// compared without caring which page each hash came from.
func reportHashes(t *testing.T, dir string) []string {
	t.Helper()
	hashes := reportHashHex.FindAllString(reportText(t, dir), -1)
	sort.Strings(hashes)
	return hashes
}

// TestReportCmdNamesTheSameBreakAsVerify is spec §10's differential and Task
// 4's carried action: over a REAL ledger with a REAL tampered record -- not a
// synthetic ledger.Break -- verify.html must name the same record id and the
// same field that `notary verify` reports for the same ledger, break for break,
// in the same order, and the two commands must agree on the verdict.
//
// Both read the one definition of "intact" (ledger.CollectBreaks): verify
// prints the collection's answer, the report renders it. If the page and the
// command ever disagree by a field name, this is the test that says so.
func TestReportCmdNamesTheSameBreakAsVerify(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 5)
	// Two tampers of the kinds the chain cannot hide, so the differential
	// compares more than one row and more than one field:
	//
	//   - seq 3's stored event is rewritten in place, leaving its hash -- and
	//     so the "hash" check -- over the record as it was written;
	//   - seq 1's row is deleted, which leaves seq 2 out of position ("seq")
	//     and no longer linked to a surviving predecessor ("prev_hash").
	execRawSQL(t, dbPath, `UPDATE records SET event = 'memory_kept' WHERE seq = 3`)
	execRawSQL(t, dbPath, `DELETE FROM records WHERE seq = 1`)

	cfg := &config.Config{
		DBPath:          dbPath,
		GapLogPath:      filepath.Join(dir, "notary-gaps.log"),
		TrustedKeysPath: writeTrustedKeys(t, pub),
	}

	// `notary verify`'s answer.
	verifyCmd, vbuf := newTestVerifyCmd(t)
	verifyErr := runVerify(verifyCmd, cfg, "", "")
	require.Error(t, verifyErr, "a tampered ledger must exit non-zero from verify")
	printed := verifyBreakIdentities(t, vbuf.String())
	// The fixture's own answer, pinned so the differential cannot pass
	// vacuously on two empty lists: the tampered record's hash, and the two
	// breaks a removed interior row produces.
	assert.Equal(t, []verifyBreakIdentity{
		{recordID: "rec-0003", seq: 2, field: "prev_hash"},
		{recordID: "rec-0003", seq: 2, field: "seq"},
		{recordID: "rec-0004", seq: 3, field: "hash"},
	}, printed, "the fixture must produce the real breaks the tamper causes")

	// `notary report`'s answer, over the same ledger and the same keyring.
	out := filepath.Join(dir, "report")
	reportCmd, stdout, _ := newTestReportCmd(t)
	setReportFlags(t, reportCmd, map[string]string{"out": out, "from": reportFrom, "to": reportTo})
	reportErr := runReport(reportCmd, cfg)
	require.Error(t, reportErr,
		"a broken chain must exit non-zero even though the pages are written: the exit code is an interface too")
	assert.Contains(t, reportErr.Error(), strconv.Itoa(len(printed)),
		"the error must name how many breaks the run found")

	rendered := reportBreakIdentities(t, readFileText(t, filepath.Join(out, "verify.html")))
	assert.Equal(t, printed, rendered,
		"verify.html must name the same breaks `notary verify` names: the same record ids, "+
			"chain positions and fields, in the same order")

	// The command's own summary carries the same state the page does, and the
	// record the break names is the tampered one in both.
	assert.Contains(t, stdout.String(), "the chain state is broken")
	assert.Contains(t, stdout.String(), strconv.Itoa(len(printed)))
}
