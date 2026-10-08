package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"notary/config"
)

// skipEnsureLedgerAnnotation, set to "true" in a command's Annotations, tells
// the root command's PersistentPreRunE not to create the ledger file (and its
// parent directories) before that command runs.
//
// `notary doctor` is the command that carries it. Doctor exists to report
// whether NOTARY_DB_PATH can hold the ledger, so creating the file first would
// hide exactly the failure it is there to name (the pre-run's error would
// replace doctor's findings), and it would make `doctor --generate-key` --
// which promises to write no file -- write one. An annotation, rather than a
// comparison against the command's name here, says WHY a command is exempt at
// the command that declares the exemption.
const skipEnsureLedgerAnnotation = "notary.skip-ensure-ledger"

// newRootCmd builds the notary root command. It declares the persistent
// --verbose and --config flags and, before any subcommand runs, ensures the
// ledger file exists on disk -- unless the subcommand opts out with
// skipEnsureLedgerAnnotation, which `doctor` does.
func newRootCmd() *cobra.Command {
	var (
		verbose    bool
		configPath string
	)

	cmd := &cobra.Command{
		Use:   "notary",
		Short: "Tamper-evident audit trail for agent memory decisions",
		Long: "Notary records why an agent memory was kept, dropped, or surfaced,\n" +
			"and produces a signed, tamper-evident audit trail of those decisions.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			if cmd.Annotations[skipEnsureLedgerAnnotation] == "true" {
				return nil
			}
			if err := ensureLedgerFile(cfg.DBPath); err != nil {
				return err
			}
			return nil
		},
	}

	cmd.PersistentFlags().BoolVar(&verbose, "verbose", false, "enable verbose output")
	cmd.PersistentFlags().StringVar(&configPath, "config", "", "path to the configuration file")

	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newVerifyCmd())
	cmd.AddCommand(newGapsCmd())
	cmd.AddCommand(newReconcileCmd())
	cmd.AddCommand(newExportCmd())
	cmd.AddCommand(newReplayCmd())
	cmd.AddCommand(newExplainCmd())
	cmd.AddCommand(newProxyCmd())
	cmd.AddCommand(newDoctorCmd())
	cmd.AddCommand(newServeCmd())

	return cmd
}

// ensureLedgerFile creates the SQLite ledger file (and its parent directory)
// when it does not already exist, then closes it. It is a no-op when the file
// is already present.
func ensureLedgerFile(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking ledger file %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating ledger directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("creating ledger file %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return fmt.Errorf("creating ledger file %s: %w", path, err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("closing ledger file %s: %w", path, err)
	}
	return nil
}
