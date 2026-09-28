package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/ledger"
	"notary/internal/sign"
	"notary/internal/store"
)

// newVerifyCmd builds the `notary verify` subcommand: the auditor's command for
// answering "has anything been tampered with?". It walks the ledger in chain
// order, checks each record's hash, link, and signature, names the exact record
// and field of every break it finds, and exits non-zero if any record is
// damaged.
func newVerifyCmd() *cobra.Command {
	var checkpointPath string

	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify the integrity of the audit ledger",
		Long: "Verify walks the audit ledger in chain order and checks that each\n" +
			"record's hash recomputes, that it links to its predecessor, and that its\n" +
			"signature is valid under a trusted key. It names the exact record and field\n" +
			"that broke, and exits non-zero if any record is damaged.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// --checkpoint is declared here for Task 11, which owns its
			// behaviour; it is accepted and ignored for now.
			_ = checkpointPath

			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			return runVerify(cmd, cfg)
		},
	}

	cmd.Flags().StringVar(&checkpointPath, "checkpoint", "",
		"path to a signed head checkpoint (consumed by a later task; accepted and ignored here)")
	return cmd
}

// runVerify loads the trusted keyring, opens the ledger, and reports the result
// of verification to cmd's output. It returns a non-nil error -- and so a
// non-zero exit -- both when the ledger is damaged and when no trusted keys are
// configured, because "could not verify anything" must never look like
// "verified everything".
func runVerify(cmd *cobra.Command, cfg *config.Config) error {
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

	if verbose {
		// Print each record as it is checked. A read failure here is left for
		// Verify below to report as a decode break, so the two never disagree.
		if recs, rerr := l.Records(); rerr == nil {
			for _, r := range recs {
				fmt.Fprintf(out, "checking %s (seq %d)\n", r.ID, r.Seq)
			}
		}
	}

	breaks, err := l.Verify(sign.NewVerifier(keyring))
	if err != nil {
		return fmt.Errorf("verifying ledger: %w", err)
	}

	if len(breaks) == 0 {
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
		fmt.Fprintf(out, "record %s (seq %d): %s — %s\n", b.RecordID, b.Seq, b.Field, b.Detail)
	}
	return fmt.Errorf("verification failed: %d break(s) found", len(breaks))
}
