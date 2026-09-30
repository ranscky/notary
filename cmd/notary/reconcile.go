package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/ledger"
	"notary/internal/mem0"
	"notary/internal/reconcile"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// newReconcileCmd builds the `notary reconcile` subcommand: the operator's
// schedulable job for deriving the claims the ledger cannot observe directly.
// It folds the ledger into a worklist, asks Mem0 what actually became of each
// work item, and -- unless --dry-run -- appends each derived claim through
// ledger.Append, printing one line per claim.
//
// It is a one-shot pass, not a daemon (design spec §3, decision 1): a
// compliance pass should be a schedulable job with its own exit code and logs,
// not a hidden goroutine. The command writes SIGNED records, so a missing
// signing key is a refusal to start (spec §8), never a degraded mode.
//
// --since bounds the pass on each record's At -- the time the Mem0 event
// happened -- and deliberately NOT on RecordedAt: a record written late about
// an old event still belongs to that old event's window. The four scope flags
// (--user-id, --agent-id, --app-id, --run-id) restrict the pass to one entity
// scope; they are combinable and combined with AND. --dry-run computes and
// reports what the pass would claim and writes nothing.
//
// The reconciler is loud (spec §9.2): any store or Mem0 error exits non-zero,
// and no audit_gap is written on failure -- a gap entry means "a
// customer-visible operation happened that we failed to record", which is not
// this situation.
func newReconcileCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Derive and record the claims the ledger cannot observe directly",
		Long: "Reconcile folds the ledger into a worklist, asks Mem0 what became of each\n" +
			"work item (an unresolved add, a memory's presence in a listing, a removal),\n" +
			"and appends the claims those answers warrant through the signed chain,\n" +
			"printing one line per claim. It is a one-shot pass meant to be scheduled.\n" +
			"\n" +
			"Every pass is a pure function of the ledger and Mem0's current state and\n" +
			"keys its claims on their subject and reason kind -- the rule version,\n" +
			"where a rule justifies the inference -- never on the run, so re-running\n" +
			"against an unchanged store appends nothing. It is not fail-open: a store or\n" +
			"Mem0 error exits non-zero, and it writes no audit gap on failure.\n" +
			"\n" +
			"--since bounds the pass on each record's At (the Mem0 event time) and not\n" +
			"on RecordedAt: a record written late about an old event still belongs to\n" +
			"that old event's window. --user-id, --agent-id, --app-id and --run-id\n" +
			"restrict the pass to one entity scope and combine with AND. --dry-run\n" +
			"computes and reports what the pass would claim and writes nothing.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			return runReconcile(cmd, cfg)
		},
	}

	// A bound variable would be a second source of truth for a flag's value;
	// every flag is declared here and read back through cmd.Flags() in
	// runReconcile, so there is no package- or closure-level mutable state.
	cmd.Flags().String("since", "",
		"only consider records whose At (the Mem0 event time) is at or after this RFC3339 time")
	cmd.Flags().String("user-id", "", "restrict the pass to this Mem0 user id")
	cmd.Flags().String("agent-id", "", "restrict the pass to this Mem0 agent id")
	cmd.Flags().String("app-id", "", "restrict the pass to this Mem0 app id")
	cmd.Flags().String("run-id", "", "restrict the pass to this Mem0 run id")
	cmd.Flags().Bool("dry-run", false,
		"compute and report what the pass would claim, and write nothing")
	return cmd
}

// runReconcile validates the configured mode, resolves the run's Window from
// cmd's flags, builds the signer, ledger and Mem0 client from cfg, runs one
// pass, and -- unless --dry-run -- appends each derived claim through
// ledger.Append, reporting the result to cmd's output.
//
// It returns a non-nil error -- and so a non-zero exit -- for any failure: an
// unimplemented mode, an unparseable flag, a missing signing key, a missing
// Mem0 API key, a store or Mem0 error, or a failed append. It writes no
// audit_gap on failure (spec §9.2), and it never panics.
func runReconcile(cmd *cobra.Command, cfg *config.Config) error {
	out := cmd.OutOrStdout()

	// The mode is validated first: a mode this build does not implement must be
	// rejected before anything else is touched. Validate -- not Valid -- is the
	// implementation check that refuses the reserved in-process mode (spec §3,
	// decision 1).
	if err := cfg.ReconcileMode.Validate(); err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}

	window, err := reconcileWindow(cmd)
	if err != nil {
		return err
	}

	// A missing signing key is a hard failure at config time (spec §8): this
	// command writes SIGNED records, so it refuses to start rather than write
	// unsigned ones. NewSigner never generates a key, so an unset or empty
	// variable fails here too, naming the variable it looked for.
	if cfg.SigningKeyEnv == "" {
		return fmt.Errorf(
			"no signing key is configured, so reconcile cannot sign the records it writes: "+
				"set %s to base64-encoded ed25519 key material (reconcile refuses to start "+
				"without it because it writes signed records)",
			config.DefaultSigningKeyEnv)
	}
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: cfg.SigningKeyEnv})
	if err != nil {
		return fmt.Errorf("loading signing key from %s: %w", cfg.SigningKeyEnv, err)
	}

	// A missing Mem0 API key fails here with an error that NAMES the variable,
	// rather than surfacing a bare HTTP 401 from inside the client. Notary has
	// no .env loader, so the error tells the operator the key is read from the
	// environment.
	if cfg.Mem0APIKey == "" {
		return fmt.Errorf(
			"no Mem0 API key is configured: set %s in the process environment "+
				"(Notary reads the key from the environment; there is no .env loader)",
			config.EnvMem0APIKey)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening ledger %s: %w", cfg.DBPath, err)
	}
	defer func() { _ = st.Close() }()

	l := ledger.New(st, sg, nil)
	rc := reconcile.New(l, mem0.NewClient(cfg.Mem0BaseURL, cfg.Mem0APIKey, nil))

	// cmd.Context() is nil for a command not run through Execute (the test seam
	// constructs one directly), so fall back to a background context rather
	// than handing a nil context to the HTTP client.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	claims, err := rc.Reconcile(ctx, window)
	if err != nil {
		// Spec §9.2: the reconciler is loud, and it writes NO audit_gap. A gap
		// entry means a customer-visible operation we failed to record, which
		// is not this situation; inventing one would blur the gap log's
		// meaning. Return the wrapped error so the process exits non-zero.
		return fmt.Errorf("reconciling: %w", err)
	}

	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return fmt.Errorf("reading --dry-run: %w", err)
	}

	if dryRun {
		// --dry-run is why Reconcile returns records instead of writing them:
		// the whole pass is computed, reported, and thrown away.
		for _, rec := range claims {
			fmt.Fprintf(out, "would claim %s %s\n", rec.Event, rec.ID)
		}
		fmt.Fprintf(out, "reconcile: dry run: %d claim(s) pending; nothing written\n", len(claims))
		return nil
	}

	for i, rec := range claims {
		id, aerr := l.Append(rec)
		if aerr != nil {
			// Each append is its own transaction, so a mid-pass failure leaves
			// a consistent chain; no rollback and no audit_gap is written. i is
			// how many claims this pass already appended before the failure, so
			// the error reports how far the pass got rather than naming only the
			// claim that failed.
			return fmt.Errorf("appending claim %s (after %d appended): %w", rec.ID, i, aerr)
		}
		fmt.Fprintf(out, "claimed %s %s\n", rec.Event, id)
	}

	if len(claims) == 0 {
		fmt.Fprintln(out, "reconcile: no claims to write; nothing pending")
		return nil
	}
	fmt.Fprintf(out, "reconcile: wrote %d claim(s)\n", len(claims))
	return nil
}

// reconcileWindow resolves the run's Window from cmd's flags. --since is parsed
// as RFC3339 against the event time (Window.Since excludes records whose At
// precedes it); the four scope flags become the Window's Scope, whose zero
// fields are no filter, so combining them yields an AND across the dimensions.
func reconcileWindow(cmd *cobra.Command) (reconcile.Window, error) {
	var w reconcile.Window

	since, err := cmd.Flags().GetString("since")
	if err != nil {
		return reconcile.Window{}, fmt.Errorf("reading --since: %w", err)
	}
	if since != "" {
		t, perr := time.Parse(time.RFC3339, since)
		if perr != nil {
			return reconcile.Window{}, fmt.Errorf(
				"parsing --since %q as RFC3339 (for example 2026-09-28T12:00:00Z): %w", since, perr)
		}
		w.Since = t
	}

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
			return reconcile.Window{}, fmt.Errorf("reading --%s: %w", dim.flag, gerr)
		}
		*dim.dst = v
	}
	w.Scope = scope

	return w, nil
}
