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

// newRootCmd builds the notary root command. It declares the persistent
// --verbose and --config flags and, before any subcommand runs, ensures the
// ledger file exists on disk.
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
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
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
