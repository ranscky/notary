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
	"crypto/ed25519"
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
// path, keyring and keyring path; a test supplies the same over a temp-dir
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

	// Keyring is the trusted public keys a chain check verifies against. A nil
	// or EMPTY keyring means the check does not run at all: LoadTrustedKeys can
	// return an empty map for a rules-only or comment-only file, and a keyless
	// verifier reports every record as a signature break. New is the guarantor
	// -- it derives the verifier only when len(Keyring) > 0 -- so no caller can
	// hand the Server a verifier that trusts nothing. This is the guard
	// doctor.checkChain makes, for the same reason.
	Keyring map[string]ed25519.PublicKey

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
// Now becomes time.Now, a nil Reveal becomes io.Discard -- derives the chain
// verifier from opts.Keyring ONLY when the keyring holds at least one key, and
// stores the rest as given.
//
// Deriving the verifier here, and only for a non-empty keyring, is deliberate:
// it is what makes it impossible for a caller to hand the Server a verifier
// that trusts nothing. A keyless verifier reports every record as a signature
// break, so building one at all is exactly what would turn a perfectly healthy
// ledger into a chain "broken" banner. When the keyring is nil or empty,
// verifier stays nil, and loadChain takes its "not verified" path instead of
// accusing the ledger. This is the guard doctor.checkChain makes, for the same
// reason: the empty keyring is the dangerous one, and asking its length here
// makes THIS package the guarantor rather than an invariant of another.
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
	var verifier *sign.Verifier
	if len(opts.Keyring) > 0 {
		verifier = sign.NewVerifier(opts.Keyring)
	}
	return &Server{
		ledger:      opts.Ledger,
		store:       opts.Store,
		gapLogPath:  opts.GapLogPath,
		verifier:    verifier,
		keyringPath: opts.KeyringPath,
		reveal:      reveal,
		now:         now,
	}, nil
}
