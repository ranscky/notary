package serve

import (
	"time"

	"notary/config"
	"notary/internal/record"
)

// overviewData is the front page's payload: the page shell -- which carries the
// chain verdict every page already shows -- plus the four things the Overview adds.
type overviewData struct {
	pageData

	// Card is the verdict, derived from the page's chainView and nothing else.
	Card overviewCard
	// Facts is the ledger at a glance. It is zero when Empty is true.
	Facts overviewFacts
	// Empty reports a ledger with no records at all, which shows the first-run
	// state instead of Facts -- a row of zeroes reads as a finding, and "0 records"
	// is not the same statement as "nothing has been recorded yet".
	Empty bool
	// Note is the forwardable text the first-run state offers.
	Note string
	// Gaps counts the moments Notary did not record.
	Gaps int
	// FactsUnavailable reports that the ledger's figures could not be read. The
	// page then states that in words rather than showing numbers it cannot
	// support, and still shows the verdict -- the figures are a summarisation, the
	// verdict is the answer.
	FactsUnavailable bool
	// GapsUnavailable reports that the gap count could not be read. Like
	// FactsUnavailable it degrades one line of the page, not the page.
	GapsUnavailable bool
}

// overviewCard is the front page's verdict: exactly three states, plus the run
// that produced it. The state is set from chainView.State, so the card cannot
// classify a ledger the banner would classify differently.
type overviewCard struct {
	Intact      bool
	NotVerified bool
	Broken      bool
	// Breaks is how many the shared verdict found; it is meaningful only when
	// Broken is set.
	Breaks int
	// AsOf is the instant of the run behind the verdict. Every state carries it,
	// because a verdict without its run is the claim this console exists not to
	// make.
	AsOf time.Time
}

// overviewFacts is the ledger at a glance.
type overviewFacts struct {
	Count    int
	Earliest time.Time
	Latest   time.Time
	Tiers    []tierFact
}

// tierFact is one tier's count, carrying the plain label and the badge class that
// draws its shape. The label is the reader's word for the tier; the ledger's own
// identifiers stay in the CLI and the records table.
type tierFact struct {
	Label string
	Class string
	Count int
}

// newOverviewData builds the front page from the verdict the handler already took.
//
// It performs no verification of its own. The card is the chainView, the gap line
// is loadGaps -- the same read `notary gaps` reports, so the two cannot disagree --
// and the facts are the one aggregate read the store exposes.
//
// It returns no error, deliberately. A failure to read the FIGURES is not a
// failure to answer the page's question: the verdict is already known by the time
// this is called, so the page degrades one section at a time and keeps the verdict.
// Only the chainView itself can fail the page, and that happens in the handler
// before this runs.
func (s *Server) newOverviewData(chain chainView) overviewData {
	data := overviewData{
		Card: overviewCard{
			Intact:      chain.State == chainClean,
			NotVerified: chain.State == chainNotVerified,
			Broken:      chain.State == chainBroken,
			Breaks:      len(chain.Breaks),
			AsOf:        chain.AsOf,
		},
	}

	// A row edited behind the ledger's back -- a tier that is none of the three,
	// say -- makes LedgerStats fail. The reader is told the counts could not be
	// read; they are never shown the storage error, and the figures are absent
	// rather than zeroed, because "0 records" is a claim this console cannot make
	// about a ledger it could not count.
	stats, statsErr := s.store.LedgerStats()
	if statsErr != nil {
		data.FactsUnavailable = true
	} else if stats.Count == 0 {
		data.Empty = true
		data.Note = firstRunNote(s.ledgerPath)
	} else {
		data.Facts = overviewFacts{
			Count:    stats.Count,
			Earliest: stats.Earliest,
			Latest:   stats.Latest,
			Tiers: []tierFact{
				{Label: "Seen directly", Class: "tier-observed", Count: stats.ByTier[record.Observed]},
				{Label: "Inferred", Class: "tier-reconstructed", Count: stats.ByTier[record.Reconstructed]},
				{Label: "Not visible to Notary", Class: "tier-internal", Count: stats.ByTier[record.Internal]},
			},
		}
	}

	if gaps, gapsErr := s.loadGaps(); gapsErr != nil {
		data.GapsUnavailable = true
	} else {
		data.Gaps = len(gaps.Unreconciled)
	}

	return data
}

// firstRunNote is the text a reviewer selects and forwards to the engineer who
// will connect the application.
//
// It is plain text rather than prose in the page because its job is to be sent,
// and it names this deployment's own ledger file rather than generic advice --
// vague instructions are what make a first-run page useless. This is the one place
// in the console that names environment variables, which is deliberate: the note's
// reader is the engineer, not the reviewer who forwards it.
//
// The variable NAMES come from package config rather than being spelled here, so a
// rename there cannot leave a note telling an engineer to set a variable the
// product no longer reads. The note carries no key material: it names the signing
// variable, and never reads it.
func firstRunNote(ledgerPath string) string {
	path := ledgerPath
	if path == "" {
		path = "the file this console is reading (set " + config.EnvDBPath + ")"
	}
	return `Notary is running, but nothing is recording into this ledger yet.

To connect it:
  1. Point your application's memory calls at Notary -- either construct the
     library interceptor, or set the application's Mem0 base URL to "notary proxy".
  2. Make sure it writes to this ledger: ` + config.EnvDBPath + `=` + path + `
  3. Signing is what lets the trail be checked rather than just displayed. Set
     ` + config.DefaultSigningKeyEnv + `, write the public half with
     "notary doctor --generate-key --trusted-keys-out <path>", then point
     ` + config.EnvTrustedKeysPath + ` at that file.

Full details: docs/OPERATIONS.md`
}
