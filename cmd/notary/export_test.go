package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/export"
	"notary/internal/ledger"
	"notary/internal/phrase"
	"notary/internal/record"
	"notary/internal/sign"
)

// stubParaphraser is a controllable export.Paraphraser for the CLI tests. It
// lets runExportWith exercise the paraphrase path with no provider, no network
// call and no API key -- the reason the client is hidden behind an interface.
type stubParaphraser struct {
	result phrase.Paraphrase
	err    error
	calls  int
}

func (s *stubParaphraser) Paraphrase(_ context.Context, _ []record.Record) (phrase.Paraphrase, error) {
	s.calls++
	if s.err != nil {
		return phrase.Paraphrase{}, s.err
	}
	return s.result, nil
}

// newTestExportCmd builds an export command whose stdout and stderr are captured
// in SEPARATE buffers. It does not mirror newTestReconcileCmd's single shared
// buffer, deliberately: the export contract is that stdout carries JSONL and
// nothing else, while operator prose ("no records", checkpoint notices) goes to
// stderr. A single buffer could not tell those apart, so it could not pin the
// distinction this task is built around (design §9; Review Focus 2).
func newTestExportCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	cmd := newExportCmd()
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd, out, errOut
}

// contentRecord builds a valid, appendable record carrying memory text, so a
// test can exercise the --include-sensitive path. Its reason is a genuine
// observed reason so Render can phrase it.
func contentRecord(t *testing.T, id record.RecordID, at time.Time, scope record.Scope, text string, sensitive bool) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"ok":true}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonReturnedBySearch, ev)
	require.NoError(t, err)
	rec := record.Record{
		ID:      id,
		At:      at,
		Event:   record.EventMemorySurfaced,
		Reason:  reason,
		Subject: record.Subject{Scope: scope, MemoryID: "mem-1", ContentHash: verifyHash(0x40)},
		Content: &record.Content{Text: text, Sensitive: sensitive},
	}
	require.NoError(t, rec.Validate())
	return rec
}

// exportLines parses stdout as JSONL, returning one export.Line per non-empty
// line. It fails the test on a malformed line, so a broken render cannot hide.
func exportLines(t *testing.T, out string) []export.Line {
	t.Helper()
	var lines []export.Line
	for _, ln := range strings.Split(out, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var l export.Line
		require.NoError(t, json.Unmarshal([]byte(ln), &l), "stdout line must be JSON: %s", ln)
		lines = append(lines, l)
	}
	return lines
}

// ---------------------------------------------------------------------------
// Flag validation: required --from, RFC3339 parsing, ordering, bounding.
// ---------------------------------------------------------------------------

// TestExportRequiresFrom pins that an unbounded export is refused: --from is
// required, and the error says so in plain language rather than exporting the
// whole ledger.
func TestExportRequiresFrom(t *testing.T) {
	_, _ = newVerifySigner(t)
	cmd, _, _ := newTestExportCmd(t)
	cfg := &config.Config{DBPath: filepath.Join(t.TempDir(), "ledger.db"), SigningKeyEnv: verifyKeyEnv}

	err := runExport(cmd, cfg)
	require.Error(t, err, "a missing --from must be refused")
	assert.Contains(t, err.Error(), "--from", "the error must name the flag")
	assert.Contains(t, err.Error(), "required", "the error must say it is required")
}

// TestExportRejectsMalformedFromNamingRFC3339 pins that a malformed --from is a
// plain-language error naming the expected format, not a silent zero-time read.
func TestExportRejectsMalformedFromNamingRFC3339(t *testing.T) {
	_, _ = newVerifySigner(t)
	cmd, _, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "September 28 2026"))
	cfg := &config.Config{DBPath: filepath.Join(t.TempDir(), "ledger.db"), SigningKeyEnv: verifyKeyEnv}

	err := runExport(cmd, cfg)
	require.Error(t, err, "a malformed --from must be refused")
	assert.Contains(t, err.Error(), "--from", "the error must name the flag")
	assert.Contains(t, err.Error(), "RFC3339", "the error must name the expected format")
}

// TestExportRejectsToBeforeFrom pins that a reversed range is an error, never a
// silent empty export.
func TestExportRejectsToBeforeFrom(t *testing.T) {
	_, _ = newVerifySigner(t)
	cmd, _, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-02-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2026-01-01T00:00:00Z"))
	cfg := &config.Config{DBPath: filepath.Join(t.TempDir(), "ledger.db"), SigningKeyEnv: verifyKeyEnv}

	err := runExport(cmd, cfg)
	require.Error(t, err, "a reversed range must be refused")
	assert.Contains(t, err.Error(), "--to", "the error must name the end of the range")
	assert.Contains(t, err.Error(), "before", "the error must state the ordering problem")
}

// TestExportRejectsSpanOverMaxSpan pins the bounding default and that --max-span
// overrides it, so a compliance export errors rather than running unbounded.
func TestExportRejectsSpanOverMaxSpan(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1"}))
	cfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}

	t.Run("span over the default is refused", func(t *testing.T) {
		cmd, _, _ := newTestExportCmd(t)
		require.NoError(t, cmd.Flags().Set("from", "2020-01-01T00:00:00Z"))
		require.NoError(t, cmd.Flags().Set("to", "2026-01-01T00:00:00Z"))
		err := runExport(cmd, cfg)
		require.Error(t, err, "a span over the default cap must be refused")
		assert.Contains(t, err.Error(), "--max-span", "the error must name the cap")
	})

	t.Run("--max-span widens the cap", func(t *testing.T) {
		cmd, _, _ := newTestExportCmd(t)
		require.NoError(t, cmd.Flags().Set("from", "2020-01-01T00:00:00Z"))
		require.NoError(t, cmd.Flags().Set("to", "2026-01-01T00:00:00Z"))
		require.NoError(t, cmd.Flags().Set("max-span", "87600h")) // 10 years
		require.NoError(t, runExport(cmd, cfg), "a widened cap must allow the range")
	})
}

// TestExportRequiresASigningKey pins the refusal behaviour (design §9, Task 8
// obligation 1): the checkpoint exists to be verifiable, so a missing signing
// key is a refusal to start rather than a silent downgrade.
func TestExportRequiresASigningKey(t *testing.T) {
	cmd, _, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	cfg := &config.Config{DBPath: filepath.Join(t.TempDir(), "ledger.db"), SigningKeyEnv: ""}

	err := runExport(cmd, cfg)
	require.Error(t, err, "no signing key must refuse to start")
	assert.Contains(t, err.Error(), "signing key", "the error must name the missing key")
}

// ---------------------------------------------------------------------------
// The empty-range distinction (Review Focus 2): exit 0 AND say so in words.
// ---------------------------------------------------------------------------

// TestExportEmptyRangeSucceedsAndSaysSo pins that a range with no records exits
// zero and says so on stderr, so "no records" and "broken invocation" never look
// the same to an operator.
func TestExportEmptyRangeSucceedsAndSaysSo(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	// The one record sits in September 2026; the window below excludes it.
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1"}))

	cmd, out, errOut := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2000-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2000-02-01T00:00:00Z"))
	cfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}

	require.NoError(t, runExport(cmd, cfg), "an empty range is a success")
	assert.Empty(t, exportLines(t, out.String()), "an empty range writes no JSONL to stdout")
	assert.Contains(t, errOut.String(), "no records",
		"the operator must be told, in words, that the range held nothing")
}

// ---------------------------------------------------------------------------
// The happy path: JSONL on stdout, one line per record, streaming, read-only.
// ---------------------------------------------------------------------------

// TestExportWritesJSONLToStdout pins that a covered range renders one JSONL
// object per record on stdout, that stdout carries nothing else, and that the
// export writes nothing to the ledger (it is a read path).
func TestExportWritesJSONLToStdout(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1"}),
		addRequestedRecord(t, "r-2", "evt-2", verifyFixedNow.Add(time.Minute), record.Scope{UserID: "u1"}))

	headBefore, okBefore, rowsBefore := ledgerSnapshot(t, dbPath)
	require.True(t, okBefore)

	cmd, out, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2027-01-01T00:00:00Z"))
	cfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}
	require.NoError(t, runExport(cmd, cfg))

	lines := exportLines(t, out.String())
	require.Len(t, lines, 2, "one JSONL line per record in the range")
	assert.Equal(t, record.RecordID("r-1"), lines[0].ID)
	assert.Equal(t, record.RecordID("r-2"), lines[1].ID)
	assert.NotEmpty(t, lines[0].Phrasing, "each line carries its phrasing beside the structured fields")

	headAfter, okAfter, rowsAfter := ledgerSnapshot(t, dbPath)
	require.True(t, okAfter)
	assert.Equal(t, headBefore.Seq, headAfter.Seq, "export is a read path: it must not change the head")
	assert.Equal(t, headBefore.Hash, headAfter.Hash, "export must not change the head hash")
	assert.Equal(t, rowsBefore, rowsAfter, "export must not append any row")
}

// TestExportScopeFlagsNarrowTheRange pins that the scope flags restrict the
// export to matching records, composing the same way reconcile's do.
func TestExportScopeFlagsNarrowTheRange(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-u1", "evt-u1", verifyFixedNow, record.Scope{UserID: "u1"}),
		addRequestedRecord(t, "r-u2", "evt-u2", verifyFixedNow, record.Scope{UserID: "u2"}))

	cmd, out, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2027-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("user-id", "u1"))
	cfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}
	require.NoError(t, runExport(cmd, cfg))

	lines := exportLines(t, out.String())
	require.Len(t, lines, 1, "only the matching scope must be exported")
	assert.Equal(t, record.RecordID("r-u1"), lines[0].ID)
	assert.Equal(t, "u1", lines[0].Scope.UserID)
}

// TestExportScopeFlagsComposeWithAnd pins the second half of the scope contract:
// the four flags combine with AND, not OR. Two records share the user id but
// carry different agent ids; narrowing on BOTH must keep exactly the one that
// matches both, so a pass that ORed the dimensions would wrongly return two.
func TestExportScopeFlagsComposeWithAnd(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-a1", "evt-a1", verifyFixedNow, record.Scope{UserID: "u1", AgentID: "a1"}),
		addRequestedRecord(t, "r-a2", "evt-a2", verifyFixedNow, record.Scope{UserID: "u1", AgentID: "a2"}))

	cmd, out, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2027-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("user-id", "u1"))
	require.NoError(t, cmd.Flags().Set("agent-id", "a1"))
	cfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}
	require.NoError(t, runExport(cmd, cfg))

	lines := exportLines(t, out.String())
	require.Len(t, lines, 1, "the scope flags must combine with AND, not OR")
	assert.Equal(t, record.RecordID("r-a1"), lines[0].ID)
}

// TestExportIncludeSensitiveControlsRedaction pins that --include-sensitive
// reaches the renderer: by default sensitive text is withheld and the line says
// so; with the flag the text is shown.
func TestExportIncludeSensitiveControlsRedaction(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		contentRecord(t, "r-secret", verifyFixedNow, record.Scope{UserID: "u1"}, "the secret", true))
	cfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}

	cmd, out, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2027-01-01T00:00:00Z"))
	require.NoError(t, runExport(cmd, cfg))

	redacted := exportLines(t, out.String())
	require.Len(t, redacted, 1)
	assert.Equal(t, "sensitive", redacted[0].Redacted, "the withheld line must state it was redacted")
	assert.Nil(t, redacted[0].Content, "sensitive text must not be rendered by default")

	cmd2, out2, _ := newTestExportCmd(t)
	require.NoError(t, cmd2.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd2.Flags().Set("to", "2027-01-01T00:00:00Z"))
	require.NoError(t, cmd2.Flags().Set("include-sensitive", "true"))
	require.NoError(t, runExport(cmd2, cfg))

	shown := exportLines(t, out2.String())
	require.Len(t, shown, 1)
	require.NotNil(t, shown[0].Content, "--include-sensitive must render the text")
	assert.Equal(t, "the secret", *shown[0].Content)
	assert.Empty(t, shown[0].Redacted, "a shown line must not claim redaction")
}

// TestExportToDefaultsToNow pins that --to is optional and defaults to now: the
// invocation succeeds with only --from, and a record in the past appears.
func TestExportToDefaultsToNow(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1"}))

	cmd, out, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	cfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}
	require.NoError(t, runExport(cmd, cfg), "omitting --to must succeed by defaulting to now")
	assert.Len(t, exportLines(t, out.String()), 1, "a record before now must fall in the default range")
}

// ---------------------------------------------------------------------------
// Range semantics (obligation 3): bound on At, never on RecordedAt.
// ---------------------------------------------------------------------------

// TestExportBoundsOnAtNotRecordedAt pins the flag semantics a future reader will
// look for: the range bounds each record's At -- the event time -- and NOT its
// RecordedAt. The record's At is in January but it was WRITTEN in October; a
// January window must still include it, because filtering on RecordedAt would
// wrongly drop it.
func TestExportBoundsOnAtNotRecordedAt(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")

	january := time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)
	october := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	rec := addRequestedRecord(t, "r-late", "evt-late", january, record.Scope{UserID: "u1"})
	rec.RecordedAt = october
	appendFixtureRecords(t, dbPath, sg, rec)

	cmd, out, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2026-01-31T00:00:00Z"))
	cfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}
	require.NoError(t, runExport(cmd, cfg))

	lines := exportLines(t, out.String())
	require.Len(t, lines, 1, "a record whose At is in-window must appear even when written later")
	assert.Equal(t, record.RecordID("r-late"), lines[0].ID)
	assert.True(t, lines[0].At.Equal(january), "the line's at is the event time, not RecordedAt")
}

// ---------------------------------------------------------------------------
// Help must be self-describing.
// ---------------------------------------------------------------------------

// TestExportHelpListsItsFlags pins that --help names every flag the command
// reads, including --phrase (Task 10). --sensitivity-rules is still deliberately
// absent: rules mark content at *write* time and no CLI command writes records,
// so the rules are an application-facing path configured by
// NOTARY_SENSITIVITY_RULES.
func TestExportHelpListsItsFlags(t *testing.T) {
	cmd, out, _ := newTestExportCmd(t)
	cmd.SetArgs([]string{"--help"})
	require.NoError(t, cmd.Execute())

	help := out.String()
	for _, flag := range []string{
		"--from", "--to", "--include-sensitive", "--checkpoint-out", "--max-span",
		"--user-id", "--agent-id", "--app-id", "--run-id", "--phrase",
	} {
		assert.Contains(t, help, flag, "help must list %s", flag)
	}
	assert.NotContains(t, help, "--sensitivity-rules", "the rules flag has nothing to attach to")
}

// ---------------------------------------------------------------------------
// Phrasing: opt-in, and a failure degrades rather than failing the export.
// ---------------------------------------------------------------------------

// TestPhraseIsOffByDefaultAtTheCLI pins that an export without --phrase never
// asks the paraphraser for anything, even when one is available.
func TestPhraseIsOffByDefaultAtTheCLI(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1"}))

	cmd, out, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2027-01-01T00:00:00Z"))
	cfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}

	fp := &stubParaphraser{result: phrase.Paraphrase{Text: "unused"}}
	require.NoError(t, runExportWith(cmd, cfg, fp))

	assert.Equal(t, 0, fp.calls, "without --phrase the paraphraser must never be called")
	lines := exportLines(t, out.String())
	require.Len(t, lines, 1)
	assert.Nil(t, lines[0].Paraphrase, "no paraphrase appears without --phrase")
}

// TestPhraseAddsTheParaphraseAtTheCLI pins the happy path through the command:
// --phrase attaches the provider's paraphrase to the line, beside the record.
func TestPhraseAddsTheParaphraseAtTheCLI(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1"}))

	cmd, out, errOut := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2027-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("phrase", "true"))
	cfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}

	fp := &stubParaphraser{result: phrase.Paraphrase{Text: "an add was acknowledged", Model: "test-model"}}
	require.NoError(t, runExportWith(cmd, cfg, fp))
	assert.Equal(t, 1, fp.calls)

	lines := exportLines(t, out.String())
	require.Len(t, lines, 1)
	require.NotNil(t, lines[0].Paraphrase)
	assert.Equal(t, "an add was acknowledged", lines[0].Paraphrase.Text)
	assert.NotEmpty(t, lines[0].Phrasing, "the deterministic phrasing still sits beside the paraphrase")
	assert.NotContains(t, errOut.String(), "failed", "a successful paraphrase is not reported as a failure")
}

// TestParaphraseFailureStillExitsZero is Review Focus 5 at the CLI level: a
// phrasing outage must not change the exit code -- the operator keeps their
// audit output -- but it must be reported on stderr rather than swallowed.
func TestParaphraseFailureStillExitsZero(t *testing.T) {
	sg, _ := newVerifySigner(t)
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg,
		addRequestedRecord(t, "r-1", "evt-1", verifyFixedNow, record.Scope{UserID: "u1"}))

	cmd, out, errOut := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2027-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("phrase", "true"))
	cfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}

	fp := &stubParaphraser{err: errors.New("phrase: dial tcp: connection refused")}
	require.NoError(t, runExportWith(cmd, cfg, fp),
		"a phrasing outage must not change the exit code: the export still succeeds")

	lines := exportLines(t, out.String())
	require.Len(t, lines, 1, "the record is still exported")
	assert.Nil(t, lines[0].Paraphrase, "a failed paraphrase leaves no paraphrase object")
	assert.Contains(t, errOut.String(), "paraphrase", "the operator is told on stderr that the paraphrase failed")
	assert.Contains(t, errOut.String(), "connection refused", "and told why")
}

// TestExportPhraseRequiresProviderConfig pins that --phrase with no provider
// configured refuses to start, naming the variable it looked for, rather than
// silently exporting without the paraphrase the operator asked for.
func TestExportPhraseRequiresProviderConfig(t *testing.T) {
	_, _ = newVerifySigner(t)
	t.Setenv(config.EnvPhraseBaseURL, "")

	cmd, _, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2027-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("phrase", "true"))
	cfg := &config.Config{DBPath: filepath.Join(t.TempDir(), "ledger.db"), SigningKeyEnv: verifyKeyEnv}

	err := runExportWith(cmd, cfg, nil)
	require.Error(t, err, "--phrase with no provider configured must refuse rather than silently skip")
	assert.Contains(t, err.Error(), config.EnvPhraseBaseURL, "the error must name the missing variable")
}

// TestNewPhraseParaphraserRequiresModel pins the second refusal branch of the
// phrase client's construction: with a base URL set but no model, --phrase
// cannot be honoured, so building the client refuses and names the missing
// variable rather than sending a request with an empty model. The base-URL
// branch is covered by TestExportPhraseRequiresProviderConfig, which returns
// before this branch; this is the model branch it never reaches.
func TestNewPhraseParaphraserRequiresModel(t *testing.T) {
	t.Setenv(config.EnvPhraseBaseURL, "https://example.invalid/v1")
	t.Setenv(config.EnvPhraseModel, "")

	p, err := newPhraseParaphraser()
	require.Error(t, err, "a base URL without a model must refuse rather than send an empty model")
	assert.Nil(t, p, "no client must be built when the model is missing")
	assert.Contains(t, err.Error(), config.EnvPhraseModel, "the error must name the missing variable")
}

// ---------------------------------------------------------------------------
// Checkpoint round trip through the REAL verify command (obligation 2).
// ---------------------------------------------------------------------------

// TestExportCheckpointRoundTripThroughVerify exercises the actual seam that makes
// a checkpoint worth having: export with --checkpoint-out, then run the REAL
// verify --checkpoint (through newVerifyCmd/runVerify) and assert it accepts the
// file; then truncate the ledger's tail and assert the SAME command now fails by
// wrapping ledger.ErrTruncated.
func TestExportCheckpointRoundTripThroughVerify(t *testing.T) {
	sg, pub := newVerifySigner(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	buildLedger(t, dbPath, sg, 4) // seq 0..3
	cpPath := filepath.Join(dir, "cp.json")

	// Export the range, writing the signed head checkpoint through the real flag.
	cmd, _, _ := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2027-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("checkpoint-out", cpPath))
	exportCfg := &config.Config{DBPath: dbPath, SigningKeyEnv: verifyKeyEnv}
	require.NoError(t, runExport(cmd, exportCfg), "export with --checkpoint-out must succeed")

	// The REAL verify command accepts the file the export produced. runVerify's
	// third argument is exactly what `--checkpoint` feeds it.
	verifyCfg := &config.Config{DBPath: dbPath, TrustedKeysPath: writeTrustedKeys(t, pub)}
	vcmd, vbuf := newTestVerifyCmd(t)
	require.NoError(t, runVerify(vcmd, verifyCfg, cpPath, ""),
		"verify --checkpoint must accept the checkpoint the export wrote")
	assert.Contains(t, vbuf.String(), "checkpoint ok", "verify must confirm the checkpoint")

	// Truncate the ledger's tail out of band -- the tamper a plain chain walk
	// cannot see, and the reason the checkpoint exists.
	execRawSQL(t, dbPath, `DELETE FROM records WHERE seq >= 3`)

	vcmd2, vbuf2 := newTestVerifyCmd(t)
	err := runVerify(vcmd2, verifyCfg, cpPath, "")
	require.Error(t, err, "the same verify must now fail on the shortened tail")
	assert.ErrorIs(t, err, ledger.ErrTruncated, "the failure must wrap ledger.ErrTruncated")
	assert.Contains(t, vbuf2.String(), "truncation", "the report must name the truncation")
}

// exportCanarySeed is a fixed seed, distinct from every other test seed, so the
// material used by the export leak canary below is unmistakable in any output.
var exportCanarySeed = []byte{
	0xa1, 0xb2, 0xc3, 0xd4, 0xe5, 0xf6, 0x07, 0x18,
	0x29, 0x3a, 0x4b, 0x5c, 0x6d, 0x7e, 0x8f, 0x90,
	0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef,
	0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10,
}

// TestExportNeverPrintsSigningKeyMaterial is the end-to-end canary design §10
// requires: no export output, in any mode, contains signing key material. It
// uses the natural configuration -- the material placed IN NOTARY_SIGNING_KEY
// and the config loaded from the environment -- and drives the mode most likely
// to echo key state, --checkpoint-out. The material must appear in NEITHER
// stdout (the JSONL) NOR stderr (the operator prose, plus any returned error
// the CLI would print there), and the export must actually succeed and emit a
// line, or the canary would be asserting over nothing.
func TestExportNeverPrintsSigningKeyMaterial(t *testing.T) {
	material := base64.StdEncoding.EncodeToString(exportCanarySeed)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")

	// The ledger is signed with the canary key...
	t.Setenv("NOTARY_EXPORT_CANARY_KEY", material)
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_EXPORT_CANARY_KEY"})
	require.NoError(t, err)
	buildLedger(t, dbPath, sg, 3)

	// ...and the natural configuration holds the material IN NOTARY_SIGNING_KEY.
	t.Setenv("NOTARY_SIGNING_KEY", material)
	t.Setenv("NOTARY_DB_PATH", dbPath)
	t.Setenv("NOTARY_GAP_LOG_PATH", filepath.Join(dir, "gaps.log"))
	cfg, err := config.Load()
	require.NoError(t, err)
	require.Equal(t, config.DefaultSigningKeyEnv, cfg.SigningKeyEnv,
		"SigningKeyEnv must be the variable NAME, never the material")

	cmd, out, errOut := newTestExportCmd(t)
	require.NoError(t, cmd.Flags().Set("from", "2026-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("to", "2027-01-01T00:00:00Z"))
	require.NoError(t, cmd.Flags().Set("checkpoint-out", filepath.Join(dir, "cp.json")))

	runErr := runExport(cmd, cfg)

	stdout := out.String()
	stderr := errOut.String()
	if runErr != nil {
		stderr += runErr.Error()
	}
	assert.NotContains(t, stdout, material, "key material must never reach stdout")
	assert.NotContains(t, stderr, material, "key material must never reach stderr")

	require.NoError(t, runErr, "the export must succeed so the canary exercises a real write")
	assert.NotEmpty(t, stdout, "the export must actually emit JSONL, or the canary guards nothing")
}
