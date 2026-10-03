package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/export"
	"notary/internal/ledger"
	"notary/internal/phrase"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// newExportCmd builds the `notary export` subcommand: the auditor's command for
// rendering a range of the ledger as JSONL -- one stable object per record,
// streamed to stdout -- and, on request, writing a signed head checkpoint that
// `verify --checkpoint` already knows how to read.
//
// It is a read path. It never writes the ledger; its only output of its own is
// the optional checkpoint file, whose format is the one verify consumes, so a
// bulk export is immediately verifiable by a command that exists.
//
// --from is required: the range is bounded on each record's At -- the event
// time -- and NOT on RecordedAt, so a record written late about an old event
// still belongs to that old event's window (the same rule reconcile's --since
// uses). --to defaults to now. The four scope flags (--user-id, --agent-id,
// --app-id, --run-id) restrict the export to one entity scope and compose with
// AND, mirroring reconcile. --include-sensitive is the only way to render
// stored sensitive content; it changes what is printed and never a hash, because
// rendering is a read path (design §3 D4, §6). --max-span caps the range.
// --phrase adds an optional, display-only paraphrase from a language model: it
// is off by default, so no provider is billed unless it is asked for, and a
// phrasing failure degrades to a note rather than failing the export (design
// §8).
func newExportCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export a range of the ledger as JSONL",
		Long: "Export streams a range of the audit ledger to stdout as JSONL, one\n" +
			"stable object per record, and -- with --checkpoint-out -- writes a signed\n" +
			"checkpoint of the ledger head that `verify --checkpoint` reads back.\n" +
			"\n" +
			"The range is bounded on each record's At (the event time) and not on\n" +
			"RecordedAt, so a record written late about an old event still belongs to\n" +
			"that old event's window. --from is required and --to defaults to now: an\n" +
			"unbounded export of a large ledger is a footgun, and requiring the start\n" +
			"makes the cost explicit. --user-id, --agent-id, --app-id and --run-id\n" +
			"restrict the export to one entity scope and combine with AND.\n" +
			"\n" +
			"By default, stored content marked sensitive is withheld and its line says\n" +
			"so; --include-sensitive prints it. The flag changes only what is printed\n" +
			"-- never a hash -- because the export is a read path. A range with no\n" +
			"records is a success that says so on stderr, so 'no records' and 'broken\n" +
			"invocation' never look the same.\n" +
			"\n" +
			"--phrase adds a language-model paraphrase to each line, beside the record\n" +
			"and never instead of it. It is off by default, so the provider is only\n" +
			"billed for an export someone asked for, and it is configured from the\n" +
			"environment (" + config.EnvPhraseBaseURL + ", " + config.EnvPhraseModel + ",\n" +
			config.EnvPhraseAPIKey + "). A phrasing failure never fails the export: the\n" +
			"records are still written and the failure is reported on stderr.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			return runExport(cmd, cfg)
		},
	}

	// Every flag is declared here and read back through cmd.Flags() in
	// exportRequest, so there is no package- or closure-level mutable state.
	cmd.Flags().String("from", "",
		"start of the export range as RFC3339 (required); bounds each record's At, the event time")
	cmd.Flags().String("to", "",
		"end of the export range as RFC3339 (defaults to now); bounds each record's At")
	cmd.Flags().Bool("include-sensitive", false,
		"print stored content marked sensitive instead of withholding it (changes only what is printed, never a hash)")
	cmd.Flags().Bool("phrase", false,
		"add a display-only language-model paraphrase to each line, beside the record; a phrasing failure degrades to a note and never fails the export")
	cmd.Flags().String("checkpoint-out", "",
		"path to write a signed head checkpoint to, in the format the verify --checkpoint command reads")
	cmd.Flags().Duration("max-span", export.DefaultMaxSpan,
		"maximum span of the export range")
	cmd.Flags().String("user-id", "", "restrict the export to this Mem0 user id")
	cmd.Flags().String("agent-id", "", "restrict the export to this Mem0 agent id")
	cmd.Flags().String("app-id", "", "restrict the export to this Mem0 app id")
	cmd.Flags().String("run-id", "", "restrict the export to this Mem0 run id")
	return cmd
}

// runExport validates the flags, builds the signer and a read-only ledger from
// cfg, exports the range through export.Exporter, and reports the result.
//
// It returns a non-nil error -- and so a non-zero exit -- for any failure: a
// missing or malformed --from, a reversed or over-long range, a missing signing
// key, --phrase with no provider configured, a store read failure, a render
// failure, or a checkpoint write failure. A range with no records is a success
// and prints a line on stderr saying so.
//
// A missing signing key is a hard failure (design §9), the same refusal
// reconcile makes: export exists to produce a signed checkpoint that can be
// verified, so it refuses to start rather than silently downgrade. NewSigner
// never invents a key, so an unset or empty variable fails here too, naming the
// variable it looked for.
//
// It delegates to runExportWith with no injected paraphraser, so --phrase builds
// the real phrase.Client from the environment.
func runExport(cmd *cobra.Command, cfg *config.Config) error {
	return runExportWith(cmd, cfg, nil)
}

// runExportWith is runExport with an injectable paraphraser. A nil p means
// "build the real client from the environment if --phrase asks for one"; a
// non-nil p is used as given, which is how a test exercises the paraphrase path
// with a fake -- no provider, no network, no key. It is the single seam the
// paraphrase wiring passes through.
func runExportWith(cmd *cobra.Command, cfg *config.Config, p export.Paraphraser) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	now := time.Now

	req, err := exportRequest(cmd, now)
	if err != nil {
		return err
	}

	// Supply the real phrasing client only when --phrase was asked for, and only
	// if a test has not already injected one. Building it before the signer and
	// the store means a --phrase with no provider fails fast, naming the
	// variable, rather than after opening the ledger.
	if req.Phrase && p == nil {
		p, err = newPhraseParaphraser()
		if err != nil {
			return err
		}
	}

	if cfg.SigningKeyEnv == "" {
		return fmt.Errorf(
			"no signing key is configured, so export cannot sign the checkpoint --checkpoint-out "+
				"may write: set %s to base64-encoded ed25519 key material (export refuses to start "+
				"without it because it writes signed checkpoints)",
			config.DefaultSigningKeyEnv)
	}
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: cfg.SigningKeyEnv})
	if err != nil {
		return fmt.Errorf("loading signing key from %s: %w", cfg.SigningKeyEnv, err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening ledger %s: %w", cfg.DBPath, err)
	}
	defer func() { _ = st.Close() }()

	// The ledger is opened for read: export never appends, so it passes no
	// signer. The signer a checkpoint needs is supplied to the EXPORTER, not to
	// the ledger, which keeps this command structurally a read path.
	l := ledger.New(st, nil, nil)
	ex := export.New(l, export.WithSigner(sg), export.WithClock(now), export.WithParaphraser(p))

	// cmd.Context() is nil for a command not run through Execute (the test seam
	// constructs one directly), so fall back to a background context.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	res, err := ex.Export(ctx, req, out)
	if err != nil {
		return err
	}

	// All operator prose goes to stderr, so stdout carries JSONL and nothing
	// else -- an export can be redirected to a file without editing. The
	// empty-range case says so IN WORDS, so "no records" never reads as a
	// broken invocation (design §9, Review Focus 2).
	if res.Records == 0 {
		fmt.Fprintf(errOut, "export: no records in the range %s to %s\n",
			req.From.Format(time.RFC3339), req.To.Format(time.RFC3339))
	} else {
		fmt.Fprintf(errOut, "export: wrote %d record(s) to stdout\n", res.Records)
	}
	// A phrasing failure degrades the prose, never the evidence: the records are
	// already written, the exit code stays zero, and the operator is told on
	// stderr -- with WHY -- so the loss is reported rather than hidden.
	if res.ParaphraseFailed {
		fmt.Fprintf(errOut, "export: paraphrase failed: %s\n", res.ParaphraseError)
	}
	if res.Checkpoint != nil {
		fmt.Fprintf(errOut, "export: wrote checkpoint for seq %d to %s\n",
			res.Checkpoint.Seq, req.CheckpointOut)
	}
	return nil
}

// exportRequest resolves an export.Request from cmd's flags. --from is parsed
// as RFC3339 and is required; --to defaults to now when unset. The four scope
// flags become the request Scope, whose zero fields are no filter, so combining
// them yields an AND across the dimensions -- the same rule reconcile uses.
// --include-sensitive and --phrase map to their Request fields; the range
// ordering and the --max-span cap are enforced by export.Request's own
// validation, so they are not restated here.
func exportRequest(cmd *cobra.Command, now func() time.Time) (export.Request, error) {
	var r export.Request

	from, err := cmd.Flags().GetString("from")
	if err != nil {
		return export.Request{}, fmt.Errorf("reading --from: %w", err)
	}
	if from == "" {
		return export.Request{}, fmt.Errorf(
			"export: --from is required: give the start of the range as RFC3339 " +
				"(for example 2026-09-28T12:00:00Z); exporting an unbounded ledger is a footgun")
	}
	fromTime, err := time.Parse(time.RFC3339, from)
	if err != nil {
		return export.Request{}, fmt.Errorf(
			"export: parsing --from %q as RFC3339 (for example 2026-09-28T12:00:00Z): %w", from, err)
	}
	r.From = fromTime

	to, err := cmd.Flags().GetString("to")
	if err != nil {
		return export.Request{}, fmt.Errorf("reading --to: %w", err)
	}
	if to == "" {
		r.To = now()
	} else {
		toTime, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return export.Request{}, fmt.Errorf(
				"export: parsing --to %q as RFC3339 (for example 2026-09-28T12:00:00Z): %w", to, err)
		}
		r.To = toTime
	}

	includeSensitive, err := cmd.Flags().GetBool("include-sensitive")
	if err != nil {
		return export.Request{}, fmt.Errorf("reading --include-sensitive: %w", err)
	}
	r.IncludeSensitive = includeSensitive

	phraseOn, err := cmd.Flags().GetBool("phrase")
	if err != nil {
		return export.Request{}, fmt.Errorf("reading --phrase: %w", err)
	}
	r.Phrase = phraseOn

	checkpointOut, err := cmd.Flags().GetString("checkpoint-out")
	if err != nil {
		return export.Request{}, fmt.Errorf("reading --checkpoint-out: %w", err)
	}
	r.CheckpointOut = checkpointOut

	maxSpan, err := cmd.Flags().GetDuration("max-span")
	if err != nil {
		return export.Request{}, fmt.Errorf("reading --max-span: %w", err)
	}
	r.MaxSpan = maxSpan

	var scope record.Scope
	for _, dim := range []struct {
		flag string
		dst  *string
	}{
		{"user-id", &scope.UserID},
		{"agent-id", &scope.AgentID},
		{"app-id", &scope.AppID},
		{"run-id", &scope.RunID},
	} {
		v, gerr := cmd.Flags().GetString(dim.flag)
		if gerr != nil {
			return export.Request{}, fmt.Errorf("reading --%s: %w", dim.flag, gerr)
		}
		*dim.dst = v
	}
	r.Scope = scope

	return r, nil
}

// newPhraseParaphraser builds the real phrasing client from the environment,
// reading the three phrase variables directly rather than through config.Config:
// the design keeps the API key out of any rendered config by giving it no
// Config field (design D10). A missing base URL or model is a refusal to start,
// naming the variable, because --phrase was asked for and cannot be honoured
// without it. The key is not required here: a self-hosted compatible endpoint
// may need none, and the client sends whatever is set.
func newPhraseParaphraser() (export.Paraphraser, error) {
	baseURL := os.Getenv(config.EnvPhraseBaseURL)
	if baseURL == "" {
		return nil, fmt.Errorf(
			"export: --phrase needs a provider: set %s to the OpenAI-compatible chat-completions base URL",
			config.EnvPhraseBaseURL)
	}
	model := os.Getenv(config.EnvPhraseModel)
	if model == "" {
		return nil, fmt.Errorf("export: --phrase needs a model: set %s", config.EnvPhraseModel)
	}
	return phrase.NewClient(baseURL, os.Getenv(config.EnvPhraseAPIKey), model, nil), nil
}
