// Command notary is the CLI entrypoint for the Notary agent-memory audit ledger.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "notary: %v\n", err)
		os.Exit(1)
	}
}
