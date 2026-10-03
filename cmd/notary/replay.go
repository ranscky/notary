package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/replay"
	"notary/internal/sign"
	"notary/internal/store"
)

// newReplayCmd builds the `notary replay` subcommand: the auditor's command for
// answering "what did Notary know at instant T?". It reads the ledger as it
// stood at --at -- the records whose RecordedAt is at or before --at, in Seq
// order -- verifies that exact prefix before rendering it, then streams one
// JSONL object per record to stdout, byte-identical to an exported line.
//
// It is a knowledge-time view, not a world-state view: a Reconstructed claim
// written after --at is correctly absent. It renders through the same renderer
// and encoder `export` uses, so a replayed line and an exported line are the
// same bytes by construction.
//
// --at is required. An unparseable or missing value fails loudly and is never
// silently treated as "now", because a typo'd date silently meaning the present
// turns a point-in-time query into the wrong question. The four scope flags
// (--user-id, --agent-id, --app-id, --run-id) restrict the replay to one entity
// scope and compose with AND, exactly as export's do. --include-sensitive is the
// only way to render stored sensitive content; it changes only what is printed,
// never a hash, because rendering is a read path.
//
// There is deliberately no --from/--to (a single instant replaces the range), no
// --phrase (the paraphrase is export-only), no --checkpoint-out (a replay can
// attest nothing beyond its instant) and no --max-span (a span cap guards a
// range; an instant has none to cap).
func newReplayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Replay the ledger as it stood at an instant",
		Long: "Replay streams the audit ledger as it stood at an instant to stdout as\n" +
			"JSONL, one stable object per record: the records recorded at or before\n" +
			"--at, in chain order, verified as a chain prefix before they are written.\n" +
			"A replayed line is byte-identical to an exported one.\n" +
			"\n" +
			"--at is required: it is the knowledge instant, and a record belongs to it\n" +
			"when its RecordedAt is at or before --at, inclusive. An unparseable --at is\n" +
			"refused rather than silently treated as now, because a typo'd date would\n" +
			"answer a different question than the one asked. --user-id, --agent-id,\n" +
			"--app-id and --run-id restrict the replay to one entity scope and combine\n" +
			"with AND.\n" +
			"\n" +
			"By default, stored content marked sensitive is withheld and its line says\n" +
			"so; --include-sensitive prints it. The flag changes only what is printed --\n" +
			"never a hash -- because replay is a read path. A replay that finds a break\n" +
			"in the prefix it renders reports it on stderr and exits non-zero, the way\n" +
			"`notary verify` does for a damaged ledger.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			return runReplay(cmd, cfg)
		},
	}

	// Every flag is read back through cmd.Flags() in replayRequest, so there is
	// no package- or closure-level mutable state.
	cmd.Flags().String("at", "",
		"the knowledge instant as RFC3339 (required); replay shows the records recorded at or before it")
	cmd.Flags().Bool("include-sensitive", false,
		"print stored content marked sensitive instead of withholding it (changes only what is printed, never a hash)")
	cmd.Flags().String("user-id", "", "restrict the replay to this Mem0 user id")
	cmd.Flags().String("agent-id", "", "restrict the replay to this Mem0 agent id")
	cmd.Flags().String("app-id", "", "restrict the replay to this Mem0 app id")
	cmd.Flags().String("run-id", "", "restrict the replay to this Mem0 run id")
	return cmd
}

// runReplay resolves the request, loads the trusted keyring, opens the ledger
// read-only, and replays the verified prefix to cmd's output.
//
// It returns a non-nil error -- and so a non-zero exit -- for any failure: a
// missing or malformed --at, no trusted keys (matching runVerify: "could not
// verify anything" must never look like "verified everything"), a store read
// failure, a render or write failure, and any integrity break in the prefix.
//
// It follows verify for its trusted-keys and verifier construction, and export
// for its streams and its counts: stdout carries JSONL and nothing else, and all
// operator prose -- the counts, and the break report -- goes to stderr. On a
// break it matches verify's convention (Step 1): the report is written BEFORE
// the non-zero exit. verify writes its report to cmd.OutOrStdout(); replay
// routes the same human-readable report to stderr instead, so that stdout stays
// the JSONL stream the read path promises, exactly as export's prose does.
func runReplay(cmd *cobra.Command, cfg *config.Config) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()

	req, err := replayRequest(cmd)
	if err != nil {
		return err
	}

	if cfg.TrustedKeysPath == "" {
		return fmt.Errorf(
			"no trusted keys are configured, so nothing can be verified: set %s to a file "+
				"of base64-encoded ed25519 public keys, one per line",
			config.EnvTrustedKeysPath)
	}
	keyring, err := sign.LoadTrustedKeys(cfg.TrustedKeysPath)
	if err != nil {
		return fmt.Errorf("loading trusted keys: %w", err)
	}
	if len(keyring) == 0 {
		return fmt.Errorf(
			"no trusted keys were found in %s, so nothing can be verified: add at least one "+
				"base64-encoded ed25519 public key, one per line",
			cfg.TrustedKeysPath)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening ledger %s: %w", cfg.DBPath, err)
	}
	defer func() { _ = st.Close() }()

	// Replay reads; it appends nothing, so the ledger is opened without a signer
	// -- the verifier is what a prefix check needs, and it comes from the trusted
	// keyring, exactly as verify builds it.
	l := ledger.New(st, nil, nil)
	verifier := sign.NewVerifier(keyring)

	// cmd.Context() is nil for a command not run through Execute (the test seam
	// constructs one directly), so fall back to a background context, as export
	// does.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	res, err := replay.New(l, verifier).Replay(ctx, req, out)
	if err != nil {
		return err
	}

	// A clean prefix is a success: report the counts on stderr the way export
	// does, leaving stdout as JSONL and nothing else. The empty view says so IN
	// WORDS, so "nothing was known yet" never reads as a broken invocation.
	if len(res.Breaks) == 0 {
		if res.Records == 0 {
			fmt.Fprintf(errOut, "replay: no records recorded at or before %s\n", req.At.Format(time.RFC3339))
		} else {
			fmt.Fprintf(errOut, "replay: wrote %d record(s) to stdout (%d redacted)\n", res.Records, res.Redacted)
		}
		return nil
	}

	// A break in the prefix is not silent. Report each break, then exit non-zero:
	// the report is written BEFORE the non-zero exit, verify's convention. The
	// line format is verify's, so the two commands name a break the same way.
	for _, b := range res.Breaks {
		if b.RecordID != "" {
			fmt.Fprintf(errOut, "record %s (seq %d): %s — %s\n", b.RecordID, b.Seq, b.Field, b.Detail)
		} else {
			fmt.Fprintf(errOut, "seq %d: %s — %s\n", b.Seq, b.Field, b.Detail)
		}
	}
	return fmt.Errorf("replay: %d break(s) found in the prefix as of %s",
		len(res.Breaks), req.At.Format(time.RFC3339))
}

// replayRequest resolves a replay.Request from cmd's flags. --at is parsed as
// RFC3339 and is required; an unparseable or missing value is an error, never a
// silent "now". The four scope flags become the request Scope, whose zero fields
// are no filter, so combining them yields an AND across the dimensions -- the
// same rule export uses. --include-sensitive maps to its Request field.
//
// replayRequest consults no clock: unlike export's --to, there is no default
// instant to fall back to, which is what keeps a typo'd --at from becoming the
// present.
func replayRequest(cmd *cobra.Command) (replay.Request, error) {
	var r replay.Request

	at, err := cmd.Flags().GetString("at")
	if err != nil {
		return replay.Request{}, fmt.Errorf("reading --at: %w", err)
	}
	if at == "" {
		return replay.Request{}, fmt.Errorf(
			"replay: --at is required: give the knowledge instant as RFC3339 " +
				"(for example 2026-09-28T12:00:00Z); replay has no default instant because " +
				"silently replaying 'now' for a missing one answers the wrong question")
	}
	atTime, err := time.Parse(time.RFC3339, at)
	if err != nil {
		return replay.Request{}, fmt.Errorf(
			"replay: parsing --at %q as RFC3339 (for example 2026-09-28T12:00:00Z): %w", at, err)
	}
	r.At = atTime

	includeSensitive, err := cmd.Flags().GetBool("include-sensitive")
	if err != nil {
		return replay.Request{}, fmt.Errorf("reading --include-sensitive: %w", err)
	}
	r.IncludeSensitive = includeSensitive

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
			return replay.Request{}, fmt.Errorf("reading --%s: %w", dim.flag, gerr)
		}
		*dim.dst = v
	}
	r.Scope = scope

	return r, nil
}
