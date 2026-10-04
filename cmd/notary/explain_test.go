package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/record"
	"notary/internal/store"
)

// newTestExplainCmd builds an explain command whose stdout and stderr are
// captured in SEPARATE buffers. Like newTestExportCmd and newTestReplayCmd --
// and unlike newTestVerifyCmd's single shared buffer -- the split is
// deliberate: explain's contract is that stdout carries the view (prose or
// JSON) and nothing else, so a single buffer could not pin that an error path
// leaves stdout clean.
func newTestExplainCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := newExplainCmd()
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd, out, errOut
}

// setExplainRecordID feeds the <record-id> positional argument to a command
// built by newTestExplainCmd. runExplain reads the positional through
// cmd.Flags().Args(), and ParseFlags is what populates those args without
// going through Execute -- which would re-load config from the environment.
func setExplainRecordID(t *testing.T, cmd *cobra.Command, id string) {
	t.Helper()
	require.NoError(t, cmd.ParseFlags([]string{id}))
}

// explainFixture builds a real on-disk ledger holding two records for memory
// "mem-1" (the shared validCmdRecord fixture), signed through the common test
// signer, and returns its path. explain needs no key of its own, but the ledger
// it reads does -- the fixture signs it exactly as the other commands' fixtures
// do.
func explainFixture(t *testing.T) string {
	t.Helper()
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		validCmdRecord(t, "rec-0001"),
		validCmdRecord(t, "rec-0002"))
	return dbPath
}

// ---------------------------------------------------------------------------
// Exactly one subject is required (Review Focus 1).
// ---------------------------------------------------------------------------

// TestExplainRejectsBothSubjects pins that a <record-id> and --memory together
// are a usage error naming both, before any read. The package resolves a
// both-set request (RecordID wins); refusing it is the CLI's job.
func TestExplainRejectsBothSubjects(t *testing.T) {
	dbPath := explainFixture(t)
	cmd, out, _ := newTestExplainCmd(t)
	setExplainRecordID(t, cmd, "rec-0001")
	require.NoError(t, cmd.Flags().Set("memory", "mem-1"))
	cfg := &config.Config{DBPath: dbPath}

	err := runExplain(cmd, cfg)
	require.Error(t, err, "a record id and --memory together must be refused")
	assert.Contains(t, err.Error(), "rec-0001", "the error must name the record id given")
	assert.Contains(t, err.Error(), "mem-1", "the error must name the memory id given")
	assert.Contains(t, err.Error(), "not both", "the error must say they are mutually exclusive")
	assert.Empty(t, out.String(), "a usage error must leave stdout clean")
}

// TestExplainRequiresASubject pins the other half of the mutual exclusion:
// neither subject given is a usage error that says how to give one. It never
// reaches the store.
func TestExplainRequiresASubject(t *testing.T) {
	cmd, out, _ := newTestExplainCmd(t)
	cfg := &config.Config{DBPath: filepath.Join(t.TempDir(), "ledger.db")}

	err := runExplain(cmd, cfg)
	require.Error(t, err, "no subject must be refused")
	assert.Contains(t, err.Error(), "<record-id>", "the error must show how to give a record id")
	assert.Contains(t, err.Error(), "--memory", "the error must show how to give a memory id")
	assert.Empty(t, out.String(), "a usage error must leave stdout clean")
}

// TestExplainRejectsEmptyMemoryRatherThanReadingTheWholeLedger pins Review Focus
// 1's sharp edge: --memory "" is refused with its OWN message, not the
// neither-subject one, and never reaches the reader. The fixture plants a
// record whose memory id is the empty-string default -- exactly the rows an
// empty filter would sweep up -- so a command that wrongly passed the empty id
// to the reader would print that record and this test would catch it.
func TestExplainRejectsEmptyMemoryRatherThanReadingTheWholeLedger(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	keyless := validCmdRecord(t, "rec-nomem")
	keyless.Subject.MemoryID = "" // the column's DEFAULT: what an empty filter matches
	appendFixtureRecords(t, dbPath, sg, keyless, validCmdRecord(t, "rec-mem1"))

	cmd, out, _ := newTestExplainCmd(t)
	require.NoError(t, cmd.Flags().Set("memory", ""))
	cfg := &config.Config{DBPath: dbPath}

	err := runExplain(cmd, cfg)
	require.Error(t, err, "an explicitly empty --memory must be refused")
	assert.Contains(t, err.Error(), "--memory", "the error must name the flag")
	assert.Contains(t, err.Error(), "non-empty", "the error must say what the flag needs")
	assert.NotContains(t, err.Error(), "a subject is required",
		"an empty --memory must not be answered by the neither-subject message")
	assert.Empty(t, out.String(),
		"the empty filter must never reach the reader and print the whole ledger")
}

// ---------------------------------------------------------------------------
// A memory id with no records exits non-zero and is distinct from a read
// failure (Review Focus 2).
// ---------------------------------------------------------------------------

// TestExplainMemoryWithNoRecordsExitsNonZeroNamingTheID pins the CLI's
// ownership of the no-records failure: a memory the ledger has never seen is a
// non-zero exit that names the id searched for and says it had no records, not
// a quiet empty success -- and the package wrote nothing, so stdout is clean.
func TestExplainMemoryWithNoRecordsExitsNonZeroNamingTheID(t *testing.T) {
	dbPath := explainFixture(t) // holds mem-1 only
	cmd, out, _ := newTestExplainCmd(t)
	require.NoError(t, cmd.Flags().Set("memory", "mem-absent"))
	cfg := &config.Config{DBPath: dbPath}

	err := runExplain(cmd, cfg)
	require.Error(t, err, "a memory with no records must exit non-zero")
	assert.Contains(t, err.Error(), "mem-absent", "the error must name the memory searched for")
	assert.Contains(t, err.Error(), "no records", "the error must say the memory had no records")
	assert.NotErrorIs(t, err, store.ErrNotFound, "an empty memory is not a missing record")
	assert.Empty(t, out.String(), "the package wrote nothing; stdout must stay clean")
}

// TestExplainReadFailureIsDistinctFromAnEmptyMemory pins the other side of the
// distinction: a genuine read failure -- here a stored row whose content_hash
// has been corrupted so it cannot be rebuilt -- is reported as a read failure
// and not as "no records", so the two never look alike.
func TestExplainReadFailureIsDistinctFromAnEmptyMemory(t *testing.T) {
	dbPath := explainFixture(t)
	// Corrupt a stored row out of band so decoding it fails; the store's
	// ListRecordsByMemory aborts on an undecodable row rather than skipping it.
	execRawSQL(t, dbPath, `UPDATE records SET content_hash = x'00' WHERE id = 'rec-0001'`)

	cmd, out, _ := newTestExplainCmd(t)
	require.NoError(t, cmd.Flags().Set("memory", "mem-1"))
	cfg := &config.Config{DBPath: dbPath}

	err := runExplain(cmd, cfg)
	require.Error(t, err, "an undecodable row must fail the read")
	assert.NotContains(t, err.Error(), "no records",
		"a genuine read failure must not be reported as an empty memory")
	assert.Contains(t, strings.ToLower(err.Error()), "content_hash",
		"the read failure must name what could not be read")
	assert.NotErrorIs(t, err, store.ErrNotFound, "a read failure is not a missing record")
	assert.Empty(t, out.String(), "nothing is written when the read fails")
}

// ---------------------------------------------------------------------------
// A record id that does not exist exits non-zero, named, as "no such record".
// ---------------------------------------------------------------------------

// TestExplainMissingRecordExitsNonZeroNamingTheID pins that an unknown record
// id is told apart from a read failure by store.ErrNotFound in the error's
// chain, so the message says there is no such record.
func TestExplainMissingRecordExitsNonZeroNamingTheID(t *testing.T) {
	dbPath := explainFixture(t)
	cmd, out, _ := newTestExplainCmd(t)
	setExplainRecordID(t, cmd, "rec-nope")
	cfg := &config.Config{DBPath: dbPath}

	err := runExplain(cmd, cfg)
	require.Error(t, err, "a record id that does not exist must exit non-zero")
	assert.Contains(t, err.Error(), "rec-nope", "the error must name the record id")
	assert.Contains(t, err.Error(), "no record", "the message must say there is no such record")
	assert.ErrorIs(t, err, store.ErrNotFound,
		"the not-found case is detected through the wrapped store.ErrNotFound")
	assert.Empty(t, out.String(), "a missing record writes nothing to stdout")
}

// ---------------------------------------------------------------------------
// The happy paths: prose and JSON both to stdout, exit 0.
// ---------------------------------------------------------------------------

// TestExplainRecordPrintsProseAndExitsZero pins the core single-record success
// contract: a clean record renders prose on stdout, names the record and its
// memory, writes nothing to stderr, and exits zero.
func TestExplainRecordPrintsProseAndExitsZero(t *testing.T) {
	dbPath := explainFixture(t)
	cmd, out, errOut := newTestExplainCmd(t)
	setExplainRecordID(t, cmd, "rec-0001")
	cfg := &config.Config{DBPath: dbPath}

	require.NoError(t, runExplain(cmd, cfg), "explaining an existing record must succeed")

	view := out.String()
	assert.Contains(t, view, "rec-0001", "the prose must name the record it explains")
	assert.Contains(t, view, "mem-1", "the prose must name the record's memory")
	assert.False(t, strings.HasPrefix(strings.TrimSpace(view), "{"),
		"the default view is prose, not JSON")
	assert.Empty(t, errOut.String(), "the view is the whole output; explain writes no chatter")
}

// TestExplainJSONPrintsJSONAndExitsZero pins the machine-readable mode: --json
// writes one JSON object carrying the record to stdout, and nothing else.
func TestExplainJSONPrintsJSONAndExitsZero(t *testing.T) {
	dbPath := explainFixture(t)
	cmd, out, errOut := newTestExplainCmd(t)
	setExplainRecordID(t, cmd, "rec-0001")
	require.NoError(t, cmd.Flags().Set("json", "true"))
	cfg := &config.Config{DBPath: dbPath}

	require.NoError(t, runExplain(cmd, cfg))
	assert.Empty(t, errOut.String(), "the view goes to stdout; stderr stays clean")

	var view struct {
		Records []struct {
			ID string `json:"id"`
		} `json:"records"`
	}
	require.NoError(t, json.Unmarshal(out.Bytes(), &view), "stdout must be one JSON object")
	require.Len(t, view.Records, 1, "the record view is exactly one record")
	assert.Equal(t, "rec-0001", view.Records[0].ID)
}

// TestExplainMemoryPrintsTimelineInSeqOrder pins the memory view through the
// CLI: --memory renders the memory's records as a timeline in Seq order on
// stdout, with nothing on stderr.
func TestExplainMemoryPrintsTimelineInSeqOrder(t *testing.T) {
	dbPath := explainFixture(t)
	cmd, out, errOut := newTestExplainCmd(t)
	require.NoError(t, cmd.Flags().Set("memory", "mem-1"))
	cfg := &config.Config{DBPath: dbPath}

	require.NoError(t, runExplain(cmd, cfg))
	view := out.String()
	assert.Contains(t, view, "mem-1", "the timeline must name the memory")
	assert.Less(t, strings.Index(view, "rec-0001"), strings.Index(view, "rec-0002"),
		"the timeline must render in Seq order")
	assert.Empty(t, errOut.String())
}

// ---------------------------------------------------------------------------
// --include-sensitive controls redaction (Review Focus 5).
// ---------------------------------------------------------------------------

// TestExplainIncludeSensitiveControlsRedaction pins that --include-sensitive
// reaches the renderer: sensitive content is withheld by default and the view
// says so, and the flag prints it.
func TestExplainIncludeSensitiveControlsRedaction(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		contentRecord(t, "rec-secret", verifyFixedNow, record.Scope{UserID: "u1"}, "the secret value", true))
	cfg := &config.Config{DBPath: dbPath}

	cmd, out, _ := newTestExplainCmd(t)
	setExplainRecordID(t, cmd, "rec-secret")
	require.NoError(t, runExplain(cmd, cfg))
	withheld := out.String()
	assert.NotContains(t, withheld, "the secret value", "sensitive text must be withheld by default")
	assert.Contains(t, strings.ToLower(withheld), "withheld",
		"the view must say the content was withheld, not stay silent")

	cmd2, out2, _ := newTestExplainCmd(t)
	setExplainRecordID(t, cmd2, "rec-secret")
	require.NoError(t, cmd2.Flags().Set("include-sensitive", "true"))
	require.NoError(t, runExplain(cmd2, cfg))
	assert.Contains(t, out2.String(), "the secret value", "--include-sensitive must render the text")
}

// ---------------------------------------------------------------------------
// The documented surface, and only it.
// ---------------------------------------------------------------------------

// TestExplainHasOnlyTheDocumentedFlags pins the deliberate omissions: explain
// offers --memory, --json and --include-sensitive, and neither exposes nor
// names --phrase, --checkpoint-out, or the four scope flags.
func TestExplainHasOnlyTheDocumentedFlags(t *testing.T) {
	cmd := newExplainCmd()

	for _, want := range []string{"memory", "json", "include-sensitive"} {
		assert.NotNil(t, cmd.Flags().Lookup(want), "--%s must be an explain flag", want)
	}
	for _, forbidden := range []string{
		"phrase", "checkpoint-out", "from", "to", "max-span",
		"user-id", "agent-id", "app-id", "run-id",
	} {
		assert.Nil(t, cmd.Flags().Lookup(forbidden), "--%s must not exist on explain", forbidden)
	}

	usage := cmd.UsageString() + cmd.Long
	for _, forbidden := range []string{
		"--phrase", "--checkpoint-out",
		"--user-id", "--agent-id", "--app-id", "--run-id",
	} {
		assert.NotContains(t, usage, forbidden, "explain's help must not hint at %s", forbidden)
	}
}

// TestExplainAcceptsAtMostOneRecordID pins cobra.MaximumNArgs(1): no positional
// is allowed (a subject may come from --memory), one is allowed, and more than
// one is refused.
func TestExplainAcceptsAtMostOneRecordID(t *testing.T) {
	cmd := newExplainCmd()
	require.NoError(t, cmd.Args(cmd, nil), "no positional is allowed; --memory may name the subject")
	require.NoError(t, cmd.Args(cmd, []string{"rec-0001"}), "one record id is allowed")
	require.Error(t, cmd.Args(cmd, []string{"a", "b"}), "more than one record id must be refused")
}
