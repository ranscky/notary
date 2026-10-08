package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/ledger"
	"notary/internal/serve"
	"notary/internal/sign"
	"notary/internal/store"
)

// serveDefaultPort is the port `notary serve` binds when --port is not given.
// It is a fixed, uncontended high port so a reviewer has a predictable URL to
// open; --port 0 asks the OS for any free port, which the command then prints.
const serveDefaultPort = 4317

// newServeCmd builds the `notary serve` subcommand: the read-only loopback
// dashboard over the audit ledger, the console a compliance lead can open in a
// browser instead of a terminal.
//
// It is the command that runs internal/serve. `--port` is its ONLY flag, and
// that is deliberate. The bind address is not configurable: the server listens
// on the literal loopback address 127.0.0.1 and nowhere else, so there is no
// --host and no --addr to misconfigure, and the loopback-only trust boundary
// (§8) is a property of the code rather than of a flag default. There is no
// --include-sensitive either, because the console's reveal is a per-view
// in-browser toggle (a query value on the request), not a process-wide mode;
// making it a start-up flag would make "reveal everything forever" a runtime
// posture, which is exactly what the design refuses. A test pins the flag set
// to exactly {port}, so any later flag is a failing test, not a silent
// addition.
//
// `notary serve` requires NEITHER a signing key NOR a keyring, like `explain`.
// It signs nothing -- it never loads a private key -- and it verifies nothing
// by itself: a keyring, when one is configured, only enables the chain
// verdict the banner shows (serve.Options.Keyring), and an empty keyring means
// that check simply does not run rather than reporting a healthy ledger as
// broken. So the only thing an operator must have is a ledger to read.
//
// The ledger is opened read-only in the STRUCTURAL sense: the command builds
// it with a nil signer (ledger.New(st, nil, nil)), so Ledger.Append refuses
// with "no signer configured". Read-only-ness is therefore a property of the
// construction and not of anyone remembering not to call Append -- the same
// structural trick `explain` uses, and the reason this command can never write
// the chain it displays.
//
// It runs until stopped: runServe installs a signal-aware context around
// whatever context cmd carries, so SIGINT/SIGTERM (Ctrl-C) shuts the dashboard
// down gracefully rather than killing the process. See runServe for why that
// wrapping is unconditional.
func newServeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Serve the read-only ledger dashboard on loopback",
		Long: "Serve runs a small, read-only HTTP dashboard over the audit ledger\n" +
			"on the loopback address 127.0.0.1, so a reviewer can read the trail in a\n" +
			"browser -- the same records `notary export`, `explain`, `gaps` and\n" +
			"`verify` report, rendered as pages -- without a copy in between.\n" +
			"\n" +
			"The records view defaults to the last 30 days and can be narrowed by\n" +
			"range and scope in the page itself; from there each record opens a\n" +
			"single-record page, each memory its own lifecycle, and the gaps and\n" +
			"chain-verification pages report what `notary gaps` and `notary verify`\n" +
			"report. Stored content marked sensitive is withheld by default and a\n" +
			"per-view toggle reveals it for that view only, writing one audit line to\n" +
			"stderr each time it does; revealing never changes a hash.\n" +
			"\n" +
			"It is structurally read-only: the ledger is opened with no signer, so it\n" +
			"can never append, and the server exposes only GET routes. It binds\n" +
			"127.0.0.1 and only 127.0.0.1 -- there is no host or address flag -- and\n" +
			"sets no CORS header, so a foreign page can neither reach nor read it.\n" +
			"\n" +
			"Serve needs neither a signing key nor a trusted-key file: it signs nothing\n" +
			"and verifies nothing on its own. When NOTARY_TRUSTED_KEYS_PATH is set, the\n" +
			"chain banner checks the ledger against those keys; when it is unset, the\n" +
			"banner simply says the chain was not verified rather than pretending.\n" +
			"\n" +
			"The only flag is --port. Press Ctrl-C to stop.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			return runServe(cmd, cfg)
		},
	}

	// The port is read back through cmd.Flags() in runServe, so there is no
	// package- or closure-level mutable state.
	cmd.Flags().Int("port", serveDefaultPort,
		"loopback port to serve on (0 asks the OS for a free one, which is then printed)")
	return cmd
}

// runServe starts the dashboard and serves it until cmd's context is done.
//
// It opens the ledger, builds it with a nil signer -- the structural
// read-only guarantee, exactly as `explain` does -- loads the trusted keyring
// when one is configured, constructs the server, binds the loopback listener,
// prints the dashboard URL to stderr, and serves until the context is
// cancelled, at which point it shuts down gracefully and returns nil.
//
// The URL is printed with the port the LISTENER actually chose, not the flag,
// so `--port 0` reports the real port a browser should open. stdout is left
// untouched: the dashboard speaks HTTP, and nothing this command prints
// belongs on stdout.
//
// The context is made signal-aware here, unconditionally, because
// cmd.Context() is NOT: under Execute cobra sets a plain
// context.Background(), not one that reacts to signals, and the test seam
// sets its own context directly. So runServe wraps whatever context it is
// given with signal.NotifyContext (SIGINT/SIGTERM), falling back to a
// background context only when cmd.Context() is nil. That wrapping is the
// whole graceful-stop mechanism -- a signal cancels the context, Serve's
// ctx.Done() branch shuts the server down, and runServe returns nil -- which
// is why Ctrl-C is a clean stop rather than a hard kill. It is installed
// after the listener is up, so a start-up failure never touches the signal
// machinery, and before the URL is printed, so a reader who has seen the URL
// can already stop us. This mirrors `notary proxy`, the repository's other
// long-running command.
func runServe(cmd *cobra.Command, cfg *config.Config) error {
	port, err := cmd.Flags().GetInt("port")
	if err != nil {
		return fmt.Errorf("reading --port: %w", err)
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening ledger %s: %w", cfg.DBPath, err)
	}
	defer func() { _ = st.Close() }()

	// The ledger is opened for read: the command never appends, so it passes
	// no signer. Ledger.Append refuses without one, which is what makes the
	// whole console read-only by construction rather than by convention.
	l := ledger.New(st, nil, nil)

	keyring, err := loadServeKeyring(cfg.TrustedKeysPath)
	if err != nil {
		return err
	}

	srv, err := serve.New(serve.Options{
		Ledger:      l,
		Store:       st,
		GapLogPath:  cfg.GapLogPath,
		Keyring:     keyring,
		KeyringPath: cfg.TrustedKeysPath,
		Reveal:      cmd.ErrOrStderr(),
		// Now is left nil: the server normalises it to time.Now, so the
		// default records window is "now minus 30 days" and cannot be
		// accidentally disabled.
		Now: nil,
	})
	if err != nil {
		return fmt.Errorf("serve: building the console: %w", err)
	}

	ln, err := srv.Listen(port)
	if err != nil {
		return err
	}

	// The signal-aware context is the whole graceful-stop mechanism: SIGINT or
	// SIGTERM cancels it, and Serve's ctx.Done() branch then shuts the server
	// down and returns nil. It is installed unconditionally, because
	// cmd.Context() is NOT signal-aware -- under Execute cobra supplies a plain
	// context.Background(), and the test seam supplies its own -- so a nil
	// check would leave SIGINT to the runtime default and kill the process.
	// It is installed after the listener is up (a start-up failure never
	// touches the signal machinery) and before the URL is printed (so a reader
	// who has seen the URL can already stop us).
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Report the port the listener actually bound -- never the flag -- so
	// `--port 0` prints the real one a browser should open. The port is read
	// through net.SplitHostPort rather than asserting the listener's concrete
	// type to *net.TCPAddr: Listen returns a TCP listener today, but a type
	// assertion would panic on any other listener, and library code must never
	// panic. The line goes to stderr; stdout stays empty.
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("serve: reading the listener's port: %w", err)
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"notary serve: dashboard on http://127.0.0.1:%s (read-only; Ctrl-C to stop)\n", portStr)

	return srv.Serve(ctx, ln)
}

// loadServeKeyring loads the trusted public keys a chain check verifies
// against, from path. It returns a nil keyring, with no error, when path is
// empty -- no trusted keys are configured, so the check simply does not run
// and that is not a failure: unlike `notary verify`, serve's whole job does
// not depend on verifying, so an absent keyring is a "not verified" banner,
// not a refusal to start.
//
// When path is set, a load failure is a plain-language start-up error naming
// the path, because an operator who configured trusted keys must not silently
// run without them. If a load somehow succeeds with zero keys, the keyring is
// passed through as-is: serve.New is the single guarantor that an empty
// keyring never becomes a keyless verifier (it derives the verifier only when
// len(Keyring) > 0), so second-guessing it here would put the same guard in
// two places and eventually let the two drift.
func loadServeKeyring(path string) (map[string]ed25519.PublicKey, error) {
	if path == "" {
		return nil, nil
	}
	keyring, err := sign.LoadTrustedKeys(path)
	if err != nil {
		return nil, fmt.Errorf("serve: loading trusted keys from %s: %w", path, err)
	}
	return keyring, nil
}
