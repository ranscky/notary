package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/gap"
	"notary/internal/ledger"
	"notary/internal/record"
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
// the gap log and the store, so no sign.NewSigner or sign.NewVerifier is on
// the path. The gap log is read through the package-level gap.Read and
// gap.Verify, which take a path and never open the file for append -- gap.Open
// would, and it would additionally heal a torn tail by writing a newline. An
// inspection command that mutated the file it inspects would corrupt the
// evidence it exists to show.
func runGaps(cmd *cobra.Command, cfg *config.Config) error {
	out := cmd.OutOrStdout()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening ledger %s: %w", cfg.DBPath, err)
	}
	defer func() { _ = st.Close() }()

	// Read the gap log read-only. A missing file is an empty log, not an error:
	// no gap log ever written means no gap ever occurred.
	gapEntries, err := gap.Read(cfg.GapLogPath)
	if err != nil {
		return fmt.Errorf("reading gap log %s: %w", cfg.GapLogPath, err)
	}

	// Load the stored records the cross-check needs, the same way `notary
	// verify` does: only the rows that decode, since a row that cannot decode
	// has no identity to match a gap against.
	var records []record.Record
	if len(gapEntries) > 0 {
		seqEntries, serr := st.SeqEntries()
		if serr != nil {
			return fmt.Errorf("reading ledger records for gap check: %w", serr)
		}
		records = make([]record.Record, 0, len(seqEntries))
		for _, se := range seqEntries {
			if se.DecodeErr == nil {
				records = append(records, se.Rec)
			}
		}
	}

	// "Unreconciled" is decided by ledger.GapBreaks -- the SAME function
	// `notary verify` uses, never a re-derived match. A second, independent
	// notion of "unreconciled" could drift and leave the two commands
	// disagreeing about the same log.
	breaks := ledger.GapBreaks(gapEntries, records)

	// Surface the gap log's own integrity breaks. gap.Read above silently skips
	// a line that fails to decode (and checks no hashes), so a corrupt log
	// would produce an incomplete list that still looked healthy; gap.Verify
	// reports exactly those breaks. A missing or empty log yields none, so a
	// system that never logged a gap behaves as before.
	integrityBreaks, verr := gap.Verify(cfg.GapLogPath)
	if verr != nil {
		return fmt.Errorf("verifying gap log %s: %w", cfg.GapLogPath, verr)
	}

	if len(breaks) == 0 && len(integrityBreaks) == 0 {
		fmt.Fprintln(out, "no outstanding gaps")
		return nil
	}

	// Index the gap entries by Counter so the report can print each
	// unreconciled entry's own fields -- its kind, scope, correlation ID, time,
	// and the detail explaining why the gap occurred. This join only reads the
	// entry back for printing; which entries are unreconciled is still decided
	// by GapBreaks, which carries the entry's Counter in Break.Seq.
	byCounter := make(map[uint64]gap.Entry, len(gapEntries))
	for _, e := range gapEntries {
		byCounter[e.Counter] = e
	}

	if len(breaks) > 0 {
		fmt.Fprintf(out, "%d unreconciled gap(s):\n", len(breaks))
		for _, b := range breaks {
			e, ok := byCounter[b.Seq]
			if !ok {
				// Unreachable in practice: GapBreaks emits one break per gap
				// entry, keyed by that entry's Counter. Fall back to the break
				// itself so a gap is never silently dropped from the report.
				fmt.Fprintf(out, "  gap entry %d: correlation=%s — %s\n", b.Seq, b.RecordID, b.Detail)
				continue
			}
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
	case len(breaks) > 0 && len(integrityBreaks) > 0:
		return fmt.Errorf("gaps: %d unreconciled gap(s) and %d gap-log integrity break(s)",
			len(breaks), len(integrityBreaks))
	case len(breaks) > 0:
		return fmt.Errorf("gaps: %d unreconciled gap(s)", len(breaks))
	default:
		return fmt.Errorf("gaps: %d gap-log integrity break(s)", len(integrityBreaks))
	}
}
