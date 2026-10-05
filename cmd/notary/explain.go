package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/explain"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/store"
)

// newExplainCmd builds the `notary explain` subcommand: the auditor's
// single-subject view, in prose for a compliance lead who must not read code.
// It answers one question -- why a single record was written, or what happened
// to a single memory over its life -- and nothing else.
//
// There are exactly two ways to name the subject: a <record-id> positional
// argument, or --memory <mem0-id>. Exactly one is required; giving both is a
// usage error naming both, and giving neither is a usage error that says how to
// give one. A --memory given as the empty string is refused in its own right,
// before any read: the store matches an empty memory_id -- the column's DEFAULT --
// so an empty filter would quietly read the whole ledger wearing a memory's
// clothes. None of this is enforced with Cobra's declarative mutual-exclusion
// helper, so the message can say which of the two was given, which is missing,
// and what to do.
//
// It is a read path: it verifies nothing and writes no file. So -- unlike
// export, which signs a checkpoint, and replay, which verifies a prefix -- it
// needs NEITHER a signing key NOR a keyring of trusted keys, and it neither
// signs nor verifies anything. Its only output is the view, which goes to
// stdout in both modes; stdout is kept clean so `notary explain --json` can be
// piped straight into a consumer.
//
// There is deliberately no --phrase (the single-subject sentence is
// export.Phrase's deterministic wording, shared with the range view; explain
// offers no language-model paraphrase, design §7), no --checkpoint-out (a
// single subject attests nothing that needs a signed head), and none of the
// four scope flags -- --user-id, --agent-id, --app-id or --run-id -- because
// the subject is one record or one memory, never a scope-bounded range. The
// help text names none of them.
func newExplainCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "explain [<record-id>]",
		Short: "Explain one record's reason or one memory's lifecycle",
		Long: "Explain answers one question about the audit ledger as prose: why a\n" +
			"single record was written, or what happened to a single memory over its\n" +
			"life. Name the subject either with a <record-id> argument or with\n" +
			"--memory <mem0-id>; exactly one is required, and both together is an\n" +
			"error. A --memory given as the empty string is refused rather than read.\n" +
			"\n" +
			"The memory view prints one line per record, in chain order, each line\n" +
			"carrying the record's event, reason, tier and both of its instants --\n" +
			"when the event happened and when Notary wrote the claim -- then the\n" +
			"claim's sentence and the content, or the fact that it was withheld.\n" +
			"\n" +
			"The view is prose by default. --json prints the same records, in the same\n" +
			"order, as one small JSON object instead -- explain's own shape, not the\n" +
			"range view's export line.\n" +
			"\n" +
			"By default, stored content marked sensitive is withheld and the view says\n" +
			"so; --include-sensitive prints it. The flag changes only what is printed --\n" +
			"never a hash -- because explain is a read path. Explain verifies nothing and\n" +
			"signs nothing, so it needs no signing key and no trusted-key file: point it\n" +
			"at a ledger and it explains it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			return runExplain(cmd, cfg)
		},
	}

	// Every flag is read back through cmd.Flags() in explainRequest, so there
	// is no package- or closure-level mutable state.
	cmd.Flags().String("memory", "",
		"explain this Mem0 memory id's lifecycle instead of a single record")
	cmd.Flags().Bool("json", false,
		"print the view as one JSON object instead of prose")
	cmd.Flags().Bool("include-sensitive", false,
		"print stored content marked sensitive instead of withholding it (changes only what is printed, never a hash)")
	return cmd
}

// runExplain resolves the subject, opens the ledger read-only, and renders the
// single subject to cmd's stdout.
//
// It returns a non-nil error -- and so a non-zero exit -- for a usage error (no
// subject, both subjects, or an explicitly empty --memory), a store open
// failure, a read or render failure, a record id with no such record, and a
// --memory whose memory has no records at all. That last case is the CLI's to
// own: internal/explain deliberately returns it as a clean, empty success
// writing nothing to out, and this command is the single owner that turns it
// into the non-zero exit and the "no records" message -- so "this memory never
// appears" never reads as a quiet success.
//
// A record id that does not exist is told apart from a genuine read failure by
// store.ErrNotFound in the error's chain (ledger.GetRecord wraps it), so the
// message blames the subject, not the disk.
//
// It is a read path: the ledger is opened with no signer (explain never
// appends) and nothing is verified (explain has no keyring), matching its
// having no signing-key or trusted-keys requirement.
func runExplain(cmd *cobra.Command, cfg *config.Config) error {
	out := cmd.OutOrStdout()

	req, err := explainRequest(cmd)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening ledger %s: %w", cfg.DBPath, err)
	}
	defer func() { _ = st.Close() }()

	// The ledger is opened for read: explain never appends, so it passes no
	// signer, and it verifies nothing, so it needs no keyring. That structural
	// read-only-ness is why this command requires no signing key and no trusted
	// keys, unlike export and replay.
	l := ledger.New(st, nil, nil)

	// cmd.Context() is nil for a command not run through Execute (the test seam
	// constructs one directly), so fall back to a background context, as export
	// and replay do.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	res, err := explain.New(l).Explain(ctx, req, out)
	if err != nil {
		// A missing record id is not a read failure: tell them apart by the
		// store.ErrNotFound the read wraps, so the message names the absent
		// subject rather than blaming the read.
		if req.RecordID != "" && errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("explain: no record with id %s: %w", req.RecordID, err)
		}
		return err
	}

	// An empty memory view is a success to the package but a failure to the
	// operator, and the package wrote nothing to stdout for it -- so this
	// non-zero exit contradicts no output. Name the subject that turned up
	// empty, scoped to the view that was asked for, so the message can never
	// name an empty one: the memory view names the memory. A zero-record record
	// view is unreachable today -- a successful record read yields exactly one
	// record -- but if a Reader ever returned an empty slice for a RecordID
	// request, that path names the record id rather than printing "no records
	// found for memory " with no subject at all.
	if res.Records == 0 {
		if req.RecordID != "" {
			return fmt.Errorf("explain: no records found for record %s", req.RecordID)
		}
		return fmt.Errorf("explain: no records found for memory %s", req.MemoryID)
	}
	return nil
}

// explainRequest resolves an explain.Request from cmd's argument and flags: the
// single positional <record-id>, or --memory, plus --json and
// --include-sensitive. Exactly one subject is required, and the mutual
// exclusion is enforced here, explicitly, rather than with Cobra's declarative
// helper, so the message can say which of the two was given, which is missing,
// and what to do.
//
// The order of the checks is deliberate. A --memory given as the empty string
// is its OWN refusal, so it is never answered by "neither subject given" and
// never reaches the reader: cmd.Flags().Changed distinguishes "absent" from
// "given as empty", which GetString alone cannot, and an empty filter would
// match an empty memory_id -- the store's DEFAULT -- listing the whole ledger as
// though it were one memory.
func explainRequest(cmd *cobra.Command) (explain.Request, error) {
	var r explain.Request

	// The positional record id, if the command was given one. Its count is
	// bounded by cobra.MaximumNArgs(1) at the command boundary, so args is
	// either empty or holds exactly the one record id.
	var recordID string
	if args := cmd.Flags().Args(); len(args) > 0 {
		recordID = args[0]
	}

	memoryGiven := cmd.Flags().Changed("memory")
	memory, err := cmd.Flags().GetString("memory")
	if err != nil {
		return explain.Request{}, fmt.Errorf("reading --memory: %w", err)
	}

	switch {
	case recordID != "" && memoryGiven:
		return explain.Request{}, fmt.Errorf(
			"explain: give either a <record-id> argument or --memory <mem0-id>, not both "+
				"(a record id %q and --memory %q were both given)", recordID, memory)
	case recordID == "" && !memoryGiven:
		return explain.Request{}, fmt.Errorf(
			"explain: a subject is required: give a <record-id> argument or --memory <mem0-id>")
	case memoryGiven && memory == "":
		return explain.Request{}, fmt.Errorf(
			"explain: --memory needs a non-empty Mem0 memory id; an empty one would " +
				"list every record whose memory id is unset, not one memory")
	}

	r.RecordID = record.RecordID(recordID)
	r.MemoryID = memory

	jsonMode, err := cmd.Flags().GetBool("json")
	if err != nil {
		return explain.Request{}, fmt.Errorf("reading --json: %w", err)
	}
	r.JSON = jsonMode

	includeSensitive, err := cmd.Flags().GetBool("include-sensitive")
	if err != nil {
		return explain.Request{}, fmt.Errorf("reading --include-sensitive: %w", err)
	}
	r.IncludeSensitive = includeSensitive

	return r, nil
}
