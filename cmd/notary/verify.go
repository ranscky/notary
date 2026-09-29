package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/gap"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// newVerifyCmd builds the `notary verify` subcommand: the auditor's command for
// answering "has anything been tampered with?". It walks the ledger in chain
// order, checks each record's hash, link, and signature, names the exact record
// and field of every break it finds, and exits non-zero if any record is
// damaged.
//
// With --checkpoint it additionally checks the chain against a signed head
// checkpoint, catching truncation -- a tamper a plain chain walk cannot see,
// because deleting the tail leaves a self-consistent remainder. With
// --write-checkpoint it signs a fresh checkpoint for the current head.
func newVerifyCmd() *cobra.Command {
	var (
		checkpointPath      string
		writeCheckpointPath string
	)

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify the integrity of the audit ledger",
		Long: "Verify walks the audit ledger in chain order and checks that each\n" +
			"record's hash recomputes, that it links to its predecessor, and that its\n" +
			"signature is valid under a trusted key. It names the exact record and field\n" +
			"that broke, and exits non-zero if any record is damaged.\n" +
			"\n" +
			"With --checkpoint it also verifies the chain against a signed head\n" +
			"checkpoint, detecting truncation (a shortened or rewritten tail), which a\n" +
			"plain chain walk cannot see. With --write-checkpoint it signs a fresh\n" +
			"checkpoint for the current head.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			return runVerify(cmd, cfg, checkpointPath, writeCheckpointPath)
		},
	}

	cmd.Flags().StringVar(&checkpointPath, "checkpoint", "",
		"path to a signed head checkpoint to verify the ledger against, detecting truncation")
	cmd.Flags().StringVar(&writeCheckpointPath, "write-checkpoint", "",
		"path to write a fresh signed head checkpoint of the current ledger head")
	return cmd
}

// runVerify loads the trusted keyring, opens the ledger, and reports the result
// of verification to cmd's output. It returns a non-nil error -- and so a
// non-zero exit -- both when the ledger is damaged and when no trusted keys are
// configured, because "could not verify anything" must never look like
// "verified everything". A truncation detected against --checkpoint exits
// non-zero too.
func runVerify(cmd *cobra.Command, cfg *config.Config, checkpointPath, writeCheckpointPath string) error {
	out := cmd.OutOrStdout()
	verbose, _ := cmd.Flags().GetBool("verbose")

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

	// Verify only reads; it needs no signer.
	l := ledger.New(st, nil, nil)
	verifier := sign.NewVerifier(keyring)

	if verbose {
		// Print each stored row as it is checked, including one that fails to
		// decode (Verify below reports it). The store is read here through the
		// same Seq-ordered accessor Verify itself uses.
		if entries, eerr := st.SeqEntries(); eerr == nil {
			for _, e := range entries {
				fmt.Fprintf(out, "checking %s (seq %d)\n", e.ID, e.Seq)
			}
		}
	}

	breaks, err := l.Verify(verifier)
	if err != nil {
		return fmt.Errorf("verifying ledger: %w", err)
	}

	// Cross-check the gap log against the store: a gap entry whose
	// (Kind, Scope, CorrelationID) matches no stored record reports work that
	// left no audit trail, and is surfaced as a "gap" break so the run exits
	// non-zero. The "matched" case -- a gap later reconciled back into the
	// ledger -- cannot be exercised until gaps have a corresponding record
	// (Task 15); it is revisited in Task 19.
	gapEntries, gerr := gap.Read(cfg.GapLogPath)
	if gerr != nil {
		return fmt.Errorf("reading gap log %s: %w", cfg.GapLogPath, gerr)
	}
	if len(gapEntries) > 0 {
		seqEntries, serr := st.SeqEntries()
		if serr != nil {
			return fmt.Errorf("reading ledger records for gap check: %w", serr)
		}
		records := make([]record.Record, 0, len(seqEntries))
		for _, se := range seqEntries {
			if se.DecodeErr == nil {
				records = append(records, se.Rec)
			}
		}
		breaks = append(breaks, ledger.GapBreaks(gapEntries, records)...)
	}

	// Check the gap log's own hash chain. gap.Read above only returns decodable
	// entries -- it silently skips a line that fails to decode and checks no
	// hashes -- so a corrupt, rewritten, or reordered gap-log line, the evidence
	// an attacker would most want to erase, is invisible to the cross-check.
	// gap.Verify reports exactly those breaks, so they exit non-zero here. A
	// missing or empty log yields no breaks and no error, so a system that never
	// logged a gap behaves exactly as before.
	gapIntegrityBreaks, gverr := gap.Verify(cfg.GapLogPath)
	if gverr != nil {
		return fmt.Errorf("verifying gap log %s: %w", cfg.GapLogPath, gverr)
	}
	breaks = append(breaks, ledger.GapIntegrityBreaks(gapIntegrityBreaks)...)

	// --write-checkpoint emits a fresh signed checkpoint. It happens only after
	// the plain walk succeeds, so we never sign an attestation for a chain we
	// have already found broken.
	if writeCheckpointPath != "" {
		if len(breaks) != 0 {
			return fmt.Errorf(
				"refusing to write a checkpoint for %s: the ledger is already broken (%d break(s))",
				writeCheckpointPath, len(breaks))
		}
		if werr := writeCheckpoint(out, cfg, l, writeCheckpointPath); werr != nil {
			return werr
		}
	}

	// --checkpoint runs the second check: does the chain still reach the signed
	// head?
	var truncErr error
	if checkpointPath != "" {
		cp, cerr := loadCheckpoint(checkpointPath)
		if cerr != nil {
			return cerr
		}

		truncBreaks, terr := l.VerifyAgainstCheckpoint(cp, verifier)
		if terr != nil && !errors.Is(terr, ledger.ErrTruncated) {
			// The checkpoint itself is unusable -- an unknown key, a bad
			// signature, or a read failure. That is not evidence about the
			// ledger and must not be reported as a truncation break.
			return fmt.Errorf("checkpoint %s cannot be trusted: %w", checkpointPath, terr)
		}
		truncErr = terr
		breaks = append(breaks, truncBreaks...)
		if terr == nil {
			fmt.Fprintf(out, "checkpoint ok: the chain still reaches seq %d at %x\n", cp.Seq, cp.Hash[:])
		}
	}

	// Report success only when there is BOTH no break AND no truncation error.
	// truncation() today always pairs ErrTruncated with exactly one break, so a
	// set truncErr implies a non-empty breaks; but were a future change ever to
	// return ErrTruncated with no break, an unguarded "len(breaks) == 0" return
	// would print "ok: N records verified" and exit 0 with truncErr set -- the
	// exact silent failure this command exists to prevent. Requiring truncErr ==
	// nil pins that invariant.
	if len(breaks) == 0 && truncErr == nil {
		n := 0
		if head, ok, herr := l.Head(); herr != nil {
			return fmt.Errorf("reading ledger head: %w", herr)
		} else if ok {
			n = int(head.Seq) + 1
		}
		fmt.Fprintf(out, "ok: %d records verified\n", n)
		return nil
	}

	for _, b := range breaks {
		if b.RecordID != "" {
			fmt.Fprintf(out, "record %s (seq %d): %s — %s\n", b.RecordID, b.Seq, b.Field, b.Detail)
		} else {
			fmt.Fprintf(out, "seq %d: %s — %s\n", b.Seq, b.Field, b.Detail)
		}
	}
	if truncErr != nil {
		return fmt.Errorf("verification failed: %d break(s) found, including truncation: %w",
			len(breaks), truncErr)
	}
	return fmt.Errorf("verification failed: %d break(s) found", len(breaks))
}

// loadCheckpoint reads and decodes a checkpoint written by
// sign.MarshalCheckpoint. Malformed input is an error, so a corrupt checkpoint
// file fails loudly rather than being silently treated as absent.
func loadCheckpoint(path string) (sign.Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return sign.Checkpoint{}, fmt.Errorf("reading checkpoint %s: %w", path, err)
	}
	cp, err := sign.UnmarshalCheckpoint(data)
	if err != nil {
		return sign.Checkpoint{}, fmt.Errorf("parsing checkpoint %s: %w", path, err)
	}
	return cp, nil
}

// writeCheckpoint signs a checkpoint for the ledger's current head with the
// configured signing key and writes its canonical JSON to path. It loads the
// signer from cfg.SigningKeyEnv, so writing a checkpoint requires the same key
// the ledger was written with.
func writeCheckpoint(out io.Writer, cfg *config.Config, l *ledger.Ledger, path string) error {
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: cfg.SigningKeyEnv})
	if err != nil {
		return fmt.Errorf("loading signing key from %s: %w", cfg.SigningKeyEnv, err)
	}
	cp, err := l.Checkpoint(sg, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("creating checkpoint: %w", err)
	}
	data, err := sign.MarshalCheckpoint(cp)
	if err != nil {
		return fmt.Errorf("marshalling checkpoint: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing checkpoint %s: %w", path, err)
	}
	fmt.Fprintf(out, "wrote checkpoint for seq %d at %x to %s\n", cp.Seq, cp.Hash[:], path)
	return nil
}
