package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// version is the notary release version.
const version = "0.1.0-dev"

// newVersionCmd builds the `notary version` subcommand, which prints the
// version constant.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the notary version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintf(cmd.OutOrStdout(), "notary %s\n", version)
			return nil
		},
	}
}
