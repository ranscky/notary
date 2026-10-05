// Package serve is the read-only loopback dashboard over Notary's audit
// ledger: a small HTTP surface that renders the same records the CLI's
// `export`, `explain`, `gaps` and `verify` commands report, so a reviewer can
// read the trail in a browser without a copy in between.
//
// It is structurally read-only. It receives a *ledger.Ledger that the command
// builds with a nil signer, so Ledger.Append refuses and no code path here can
// write the chain; it never opens the gap log for append and never imports
// internal/phrase, so no handler can bill a language model. Every record it
// shows, in every view, is an export.Line produced by export.Render, which is
// the one owner of the tier label, the redaction decision and the phrasing
// sentence -- the console cannot drift from `notary export` because it renders
// through the same function.
//
// This file holds the Server and the dependencies it is constructed from; the
// data projections live in views.go, the routes and templates arrive in later
// tasks.
package serve

import (
	"io"
	"time"

	"notary/internal/ledger"
	"notary/internal/sign"
	"notary/internal/store"
)

// Options collects the dependencies and settings New builds a Server from.
//
// They are explicit values rather than package state, so a Server is fully
// described by what it was constructed with and no two Servers can share
// mutable configuration. The command supplies the real ledger, store, gap-log
// path, verifier and keyring path; a test supplies the same over a temp-dir
// ledger.
type Options struct {
	// Ledger is the read path over the audit trail. The command builds it with
	// a nil signer, which is what makes this package structurally read-only:
	// Append refuses, so no handler can write.
	Ledger *ledger.Ledger

	// Store is the underlying store, the read side of the same ledger. It is
	// carried separately so a later view can read what the ledger's own
	// methods do not expose (the chain entries the verify view walks).
	Store store.Store

	// GapLogPath names the gap log the gaps view reads (through gap.Read and
	// gap.Verify, never gap.Open, which would create and heal it).
	GapLogPath string

	// Verifier checks record signatures against the trusted keyring. A nil
	// verifier trusts nothing: the chain view then reports "not verified"
	// rather than a clean chain, because without a keyring nothing was
	// signature-checked.
	Verifier *sign.Verifier

	// KeyringPath names the trusted-keys file the banner's re-run command
	// should name. It is display-only: the verifier is what actually checks.
	KeyringPath string

	// Reveal is where the reveal toggle's one-line audit note is written
	// (§6: each revealing request writes one line, e.g. "serve: revealed
	// sensitive content for record <id>"). It is normalised to io.Discard when
	// nil, so a test that does not care about the note needs no writer.
	Reveal io.Writer

	// Now supplies the clock a view stamps itself with. It is normalised to
	// time.Now when nil, so a caller cannot disable the clock.
	Now func() time.Time
}

// Server renders the ledger as HTML over a small, fixed set of GET routes.
//
// Its fields are the construction inputs, held unexported and never mutated
// after New, so a Server is safe to read concurrently: every method reads
// through the ledger and builds a fresh view, and none writes anything.
type Server struct {
	ledger      *ledger.Ledger
	store       store.Store
	gapLogPath  string
	verifier    *sign.Verifier
	keyringPath string
	reveal      io.Writer
	now         func() time.Time
}

// New builds a Server from opts. It normalises the optional settings -- a nil
// Now becomes time.Now, a nil Reveal becomes io.Discard -- and stores the rest
// as given.
//
// It returns an error so a construction that cannot succeed can say why rather
// than leaving a Server that looks usable. This task has nothing to fail on
// yet (the templates arrive in a later task), so it is always nil now; the
// error return is the shape later work needs and library code must never panic
// to report a fault (.clinerules §4).
func New(opts Options) (*Server, error) {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	reveal := opts.Reveal
	if reveal == nil {
		reveal = io.Discard
	}
	return &Server{
		ledger:      opts.Ledger,
		store:       opts.Store,
		gapLogPath:  opts.GapLogPath,
		verifier:    opts.Verifier,
		keyringPath: opts.KeyringPath,
		reveal:      reveal,
		now:         now,
	}, nil
}
