package serve

import (
	"fmt"
	"time"

	"notary/internal/gap"
	"notary/internal/ledger"
)

// gapsView is the gap-report page's data: the two independent ways a ledger can
// be missing audit coverage, and whether either is present.
//
// The two lists are kept apart exactly as ledger.GapReport keeps them --
// Unreconciled is the log's relationship to the store, Integrity is the log's
// own internal consistency -- because the page prints them differently and one
// can be non-empty while the other is. OK is DERIVED here, never stored on the
// report: it is true only when BOTH lists are empty, so a single non-empty list
// (an unreconciled gap, or a corrupt line) makes the page say "not ok".
// Deriving it in the loader rather than in the template is what keeps "ok" from
// ever quietly meaning "one of the two was fine".
type gapsView struct {
	Unreconciled []gap.Entry
	Integrity    []gap.Break
	OK           bool
}

// loadGaps returns the gap report over the server's store and gap log.
//
// Membership is decided by ledger.OutstandingGaps -- the SAME function `notary
// gaps` runs -- and by nothing else here: this view never re-derives
// "unreconciled" or re-walks the gap log, because a second notion of either
// could drift and leave the page and the command disagreeing about the same
// log. loadGaps only projects that report into the view and derives OK. A
// failure from OutstandingGaps is returned wrapped, since a partial report is
// not an answer to the question that was asked.
func (s *Server) loadGaps() (gapsView, error) {
	report, err := ledger.OutstandingGaps(s.store, s.gapLogPath)
	if err != nil {
		return gapsView{}, fmt.Errorf("serve: outstanding gaps: %w", err)
	}
	return gapsView{
		Unreconciled: report.Unreconciled,
		Integrity:    report.Integrity,
		OK:           len(report.Unreconciled) == 0 && len(report.Integrity) == 0,
	}, nil
}

// chainState is the chain banner's verdict: one of exactly three, and the three
// must never collapse.
//
// The distinction chainNotVerified exists for is the whole point. A ledger can
// be "clean" (checked, nothing wrong), "broken" (checked, damage found), or
// "not verified" (the check could not run at all). There is deliberately no
// boolean and no fourth value, because the tempting pair -- clean vs broken --
// would force "not verified" to be rendered as one or the other, and rendering
// it as clean is the worst output this tool could produce: it would tell a
// reviewer a ledger was checked when nothing was.
type chainState int

const (
	// chainNotVerified is the verdict when no trusted keys are configured, so
	// the signature check did not run. sign.NewVerifier(nil) trusts no keys and
	// ledger.Verify reports a signature break for every key it does not trust,
	// so running the check would report every record in a perfectly healthy
	// ledger as a break -- a false accusation of tampering. The check is
	// therefore not run at all, and this state records that honestly. It is the
	// same reasoning doctor.checkChain uses to skip its own chain check.
	chainNotVerified chainState = iota
	// chainClean is the verdict when the check ran and found no breaks.
	chainClean
	// chainBroken is the verdict when the check ran and found breaks.
	chainBroken
)

// chainView is the chain banner's data: the verdict, the instant it was taken,
// the breaks when there are any, and the keyring path a "not verified" banner
// names as the fix.
//
// AsOf is the moment the check ran -- s.now() at render time -- so the banner
// is never a stale snapshot: a page loaded now reports the chain as it is now,
// which matters because the chain can change between two page views. KeyringPath
// is carried in EVERY state, including the two that verified, because the state
// that most needs it is chainNotVerified, whose whole job is to name the
// trusted-keys file the operator must configure.
type chainView struct {
	State       chainState
	AsOf        time.Time
	Breaks      []ledger.Break
	KeyringPath string
}

// loadChain reports the ledger's chain state for the banner.
//
// It has exactly three outcomes and never lets them collapse:
//
//   - With no verifier it returns chainNotVerified WITHOUT running the check.
//     It does not call ledger.CollectBreaks at all, because a verifier that
//     trusts no keys -- which is what "no verifier" means here -- would report
//     every record in a healthy ledger as a signature break. Not running the
//     check is the only way to avoid accusing an intact ledger of tampering,
//     and this state says plainly that the check did not run. Breaks is left
//     empty: there is nothing to show, and an empty list under a state that
//     means "unknown" must never be read as "nothing wrong".
//   - With a verifier it runs ledger.CollectBreaks -- the SAME function `notary
//     verify` runs -- so the banner and the command cannot give two answers to
//     one question. No breaks is chainClean; any break is chainBroken carrying
//     them. The breaks are returned as CollectBreaks produced them, so the page
//     names the same record, sequence and field `notary verify` names.
//
// AsOf is the instant the check ran, so the banner is fresh, never a snapshot.
// loadChain is called per request -- a whole-chain walk per page view -- which
// is a recorded cost of this console, not a surprise. A CollectBreaks failure is
// not a verdict: it is returned wrapped, and Breaks stays empty so a caller
// cannot mistake a partial read for a clean chain.
func (s *Server) loadChain() (chainView, error) {
	asOf := s.now()

	if s.verifier == nil {
		return chainView{
			State:       chainNotVerified,
			AsOf:        asOf,
			KeyringPath: s.keyringPath,
		}, nil
	}

	breaks, err := ledger.CollectBreaks(s.ledger, s.store, s.gapLogPath, s.verifier)
	if err != nil {
		return chainView{}, fmt.Errorf("serve: collect chain breaks: %w", err)
	}

	state := chainClean
	if len(breaks) > 0 {
		state = chainBroken
	}
	return chainView{
		State:       state,
		AsOf:        asOf,
		Breaks:      breaks,
		KeyringPath: s.keyringPath,
	}, nil
}
