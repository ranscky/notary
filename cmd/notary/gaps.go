package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/ledger"
	"notary/internal/store"
)

// newGapsCmd builds the `notary gaps` subcommand: the operator's command for
// answering "is anything outstanding?". A standing gap -- a span of activity
// recorded as an audit gap that no record in the ledger accounts for -- is
// otherwise visible only as a line of `notary verify` output; this lists the
// outstanding gaps and names the record each one stands in for.
//
// It is report-only. Reconciliating a gap needs the missing record, and a gap
// entry deliberately persists only the event kind, the scope, the correlation
// ID, and a description -- never the record payload -- so a CLI process has
// nothing to replay from. Reconciliation stays a library concern.
func newGapsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gaps",
		Short: "Report outstanding audit gaps",
		Long: "Gaps reports every audit gap in the gap log that no record in the\n" +
			"ledger accounts for -- work that was recorded as a gap and never\n" +
			"reconciled back into the trail. For each it names the event kind, the\n" +
			"scope, the correlation ID (the missing record's ID), when the gap\n" +
			"happened, and why.\n" +
			"\n" +
			"It also reports any break in the gap log's own hash chain. The read that\n" +
			"builds the list above silently skips a line it cannot decode, so a\n" +
			"corrupt log could otherwise produce an incomplete list that still looked\n" +
			"healthy; the integrity breaks make the report honest about its own\n" +
			"completeness.\n" +
			"\n" +
			"Gaps is read-only: it never opens the gap log for append, never heals a\n" +
			"torn tail, and never reconciles anything. It exits non-zero whenever\n" +
			"there is anything to report, so it can gate a pipeline.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			return runGaps(cmd, cfg)
		},
	}
}

// runGaps reports the ledger's outstanding audit gaps to cmd's output and
// returns a non-nil error -- and so a non-zero exit -- whenever there is
// anything to report: an unreconciled gap, or a break in the gap log's own
// integrity. A clean report prints a single "no outstanding gaps" line and
// returns nil. An operational failure (an unreadable store or gap log) is an
// error too.
//
// It requires no signing key and no keyring: it is a read-only statement about
// the gap log and the store, so no sign.NewSigner or sign.NewVerifier is on the
// path. The work of deciding what is outstanding lives in
// ledger.OutstandingGaps, the one definition shared with the serve console; it
// reads the log through the package-level gap.Read and gap.Verify, which take a
// path and never open the file for append -- gap.Open would, and it would
// additionally heal a torn tail by writing a newline. An inspection command
// that mutated the file it inspects would corrupt the evidence it exists to
// show.
func runGaps(cmd *cobra.Command, cfg *config.Config) error {
	out := cmd.OutOrStdout()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening ledger %s: %w", cfg.DBPath, err)
	}
	defer func() { _ = st.Close() }()

	// One definition of "outstanding", shared with the serve console:
	// ledger.OutstandingGaps decides it -- and wraps each stage's failure with
	// the text `notary gaps` has always reported -- so the command and the
	// console cannot disagree about the same log.
	report, err := ledger.OutstandingGaps(st, cfg.GapLogPath)
	if err != nil {
		return err
	}

	unreconciled := report.Unreconciled
	integrityBreaks := report.Integrity

	if len(unreconciled) == 0 && len(integrityBreaks) == 0 {
		fmt.Fprintln(out, "no outstanding gaps")
		return nil
	}

	if len(unreconciled) > 0 {
		fmt.Fprintf(out, "%d unreconciled gap(s):\n", len(unreconciled))
		for _, e := range unreconciled {
			fmt.Fprintf(out, "  gap entry %d: kind=%s scope={user=%s agent=%s app=%s run=%s} correlation=%s at=%s\n",
				e.Counter, e.Kind, e.Scope.UserID, e.Scope.AgentID, e.Scope.AppID, e.Scope.RunID,
				e.CorrelationID, e.At.UTC().Format(time.RFC3339))
			if e.Detail != "" {
				fmt.Fprintf(out, "    detail: %s\n", e.Detail)
			}
		}
	}

	if len(integrityBreaks) > 0 {
		fmt.Fprintf(out, "%d gap-log integrity break(s):\n", len(integrityBreaks))
		fmt.Fprintln(out, "  the gap log's hash chain, counter continuity, or decodability is broken;")
		fmt.Fprintln(out, "  because the read that builds the list above skips undecodable lines, that")
		fmt.Fprintln(out, "  list may be incomplete -- these breaks name what could not be read or verified:")
		for _, b := range integrityBreaks {
			fmt.Fprintf(out, "  line %d: %s — %s\n", b.Line, b.Field, b.Detail)
		}
	}

	switch {
	case len(unreconciled) > 0 && len(integrityBreaks) > 0:
		return fmt.Errorf("gaps: %d unreconciled gap(s) and %d gap-log integrity break(s)",
			len(unreconciled), len(integrityBreaks))
	case len(unreconciled) > 0:
		return fmt.Errorf("gaps: %d unreconciled gap(s)", len(unreconciled))
	default:
		return fmt.Errorf("gaps: %d gap-log integrity break(s)", len(integrityBreaks))
	}
}
