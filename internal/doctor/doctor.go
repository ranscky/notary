// Package doctor answers one question: is this Notary deployment set up
// correctly, and if not, what exactly should the operator do? It resolves
// every fact the command line depends on -- the ledger and gap-log paths, the
// trusted keyring, the signing key, the Mem0 endpoint and key, the sensitivity
// rules, and the ledger's own chain state -- into a list of Findings. Each
// finding carries a severity, one plain sentence saying what was found, and
// the exact command that fixes it, because a diagnosis without the fix is half
// a tool.
//
// `notary doctor` prints the findings and exits non-zero when any of them is
// an error; the same findings are what `notary doctor --out` renders into
// setup.html. Doctor's report is a *setup* document, never part of the
// evidence report (design section 3, decision 6), which is why this package
// embeds its own page template instead of depending on the report renderer.
//
// Two properties are load-bearing:
//
//   - A check that cannot be run is reported as not run, never as a pass and
//     never as a failure. The chain check in particular is skipped when no
//     trusted keys are configured: sign.NewVerifier(nil) trusts no keys and
//     reports every record as a signature break, so running the check would
//     accuse a healthy ledger of tampering -- the worst output this tool could
//     produce.
//   - No finding ever carries key material. Keys are named by the variable or
//     path they live in, and by their public fingerprint when one is known.
package doctor

import (
	"crypto/ed25519"
	_ "embed" // the setup page template is embedded with go:embed
	"errors"
	"fmt"
	"html/template"
	"io"
	"time"

	"notary/config"
	"notary/internal/gap"
	"notary/internal/ledger"
	"notary/internal/sign"
	"notary/internal/store"
)

// Severity ranks a finding. It mirrors what the command exits on: Err is a
// broken deployment, everything else is not.
type Severity int

const (
	// OK reports that the check held.
	OK Severity = iota
	// Warn reports something the operator should know that does not make the
	// deployment unusable, and does not fail the command.
	Warn
	// Err reports a broken deployment; `notary doctor` exits non-zero.
	Err
)

// String returns the severity's lowercase label -- "ok", "warn", "error" --
// which the command's text output and setup.html both render.
func (s Severity) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Err:
		return "error"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// The check names. They are stable identifiers: the command's text output and
// setup.html render them, and tests select findings by them. CheckMem0BaseURL
// is the one check the command contributes itself, because its validator
// (parseUpstream) lives in package main and must not be re-implemented.
const (
	// CheckLedger is the NOTARY_DB_PATH / store.Open check.
	CheckLedger = "ledger"
	// CheckGapLog is the NOTARY_GAP_LOG_PATH writability check.
	CheckGapLog = "gap log"
	// CheckTrustedKeys is the NOTARY_TRUSTED_KEYS_PATH / sign.LoadTrustedKeys
	// check.
	CheckTrustedKeys = "trusted keys"
	// CheckSigningKey is the NOTARY_SIGNING_KEY / sign.NewSigner check.
	CheckSigningKey = "signing key"
	// CheckMem0BaseURL is the NOTARY_MEM0_BASE_URL check, run by the command.
	CheckMem0BaseURL = "mem0 base URL"
	// CheckMem0APIKey is the NOTARY_MEM0_API_KEY presence check.
	CheckMem0APIKey = "mem0 API key"
	// CheckSensitivityRules is the NOTARY_SENSITIVITY_RULES parse check.
	CheckSensitivityRules = "sensitivity rules"
	// CheckChain is the ledger's chain-state check (ledger.CollectBreaks).
	CheckChain = "chain"
	// CheckConfiguration is the check that there is a configuration at all.
	CheckConfiguration = "configuration"
)

// Finding is one check's result. Message is one plain sentence saying what was
// found; Fix is the exact command that fixes it, and is empty when nothing
// needs fixing. Neither field ever carries key material: a key is named by the
// variable or path it lives in, or by its public fingerprint.
type Finding struct {
	// Check names the check that produced this finding.
	Check string
	// Severity ranks the finding.
	Severity Severity
	// Message says what was found, in one plain sentence.
	Message string
	// Fix is the exact command that fixes what Message describes, or empty
	// when there is nothing to fix.
	Fix string
}

// Fix commands. Each is a line an operator can run: `export X=<...>` sets the
// variable the check read (the angle-bracketed part is the operator's own
// value), and `notary ...` runs a command of ours. They are constants so a
// check's message and its fix cannot drift apart.
const (
	fixDBPath = "export " + config.EnvDBPath +
		"=<path where the ledger can be created, for example notary.db>"
	fixGapLogPath  = "export " + config.EnvGapLogPath + "=<path to a writable gap-log file>"
	fixTrustedKeys = "export " + config.EnvTrustedKeysPath +
		"=<path to a file of base64 ed25519 public keys, one per line>"
	fixSigningKey = "notary doctor --generate-key"
	fixMem0APIKey = "export " + config.EnvMem0APIKey + "=<your Mem0 API key>"
	fixRules      = "export " + config.EnvSensitivityRules +
		"=<path to a valid sensitivity-rules YAML file>"
	fixChain = "notary verify --verbose"
)

// Diagnose runs every check against cfg and env and returns one finding per
// check, in the design's order: the ledger, the gap log, the trusted keys, the
// signing key, the Mem0 API key, the sensitivity rules, and the chain.
//
// cfg is the configuration config.LoadFrom resolved from env; env is consulted
// directly for the settings a *config.Config deliberately does not carry --
// today the sensitivity rules path, which has no Config field. The signing key
// is the one check neither can answer from a value: config carries only the
// variable's name (SigningKeyEnv), so Diagnose asks sign.NewSigner exactly as
// a command does, and that loader's KeySourceEnv reads the process
// environment. The command builds env from os.Environ, so the two agree by
// construction.
//
// Diagnose opens the ledger and the gap log (store.Open and gap.Open both
// create a missing path, so "the ledger does not exist yet" is a healthy
// first-run state, not a finding) and closes them again. It returns no error:
// every failure is a finding, so nothing a caller forgets can hide a broken
// deployment. It never panics.
func Diagnose(cfg *config.Config, env map[string]string, now time.Time) []Finding {
	if cfg == nil {
		// The one input a caller can get wrong: report it rather than
		// dereferencing nil.
		return []Finding{{
			Check:    CheckConfiguration,
			Severity: Err,
			Message:  "no configuration was supplied, so no check could run",
			Fix:      "notary doctor",
		}}
	}

	ledgerFinding, st := openLedger(cfg)
	if st != nil {
		defer func() { _ = st.Close() }()
	}
	trustedKeysFinding, keyring := checkTrustedKeys(cfg)

	// The findings are assembled in the design's order: the ledger, the gap
	// log, the trusted keys, the signing key, the Mem0 API key, the rules, and
	// the chain. The Mem0 base URL -- the one check whose validator lives in
	// package main -- is contributed by the command, which slots it in after
	// the signing key.
	return []Finding{
		ledgerFinding,
		checkGapLog(cfg),
		trustedKeysFinding,
		checkSigningKey(cfg),
		checkMem0APIKey(cfg),
		checkSensitivityRules(env),
		checkChain(cfg, st, keyring, now),
	}
}

// openLedger opens cfg.DBPath through store.Open, the same call every command
// makes. A missing file is not a failure: store.Open creates it, so this check
// answers "can this path hold the ledger?". The returned store is open for the
// chain check that follows, and Diagnose closes it.
func openLedger(cfg *config.Config) (Finding, *store.SQLiteStore) {
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return Finding{
			Check:    CheckLedger,
			Severity: Err,
			Message:  fmt.Sprintf("the ledger at %s cannot be opened: %v", cfg.DBPath, err),
			Fix:      fixDBPath,
		}, nil
	}
	return Finding{
		Check:    CheckLedger,
		Severity: OK,
		Message:  fmt.Sprintf("the ledger at %s opens", cfg.DBPath),
	}, st
}

// checkGapLog opens cfg.GapLogPath for append -- the mode the gap log's own
// writer uses -- and closes it again. Opening, not stat'ing, is the point: a
// gap log on a read-only volume is present and unwritable, and it fails
// exactly when it matters, on the write that records an audit gap. A missing
// log is not a failure; gap.Open creates it, which is the state the first
// recorded gap expects.
func checkGapLog(cfg *config.Config) Finding {
	gl, err := gap.Open(cfg.GapLogPath)
	if err != nil {
		return Finding{
			Check:    CheckGapLog,
			Severity: Err,
			Message:  fmt.Sprintf("the gap log at %s cannot be opened for append: %v", cfg.GapLogPath, err),
			Fix:      fixGapLogPath,
		}
	}
	if cerr := gl.Close(); cerr != nil {
		return Finding{
			Check:    CheckGapLog,
			Severity: Err,
			Message:  fmt.Sprintf("the gap log at %s opened for append but could not be closed: %v", cfg.GapLogPath, cerr),
			Fix:      fixGapLogPath,
		}
	}
	return Finding{
		Check:    CheckGapLog,
		Severity: OK,
		Message:  fmt.Sprintf("the gap log at %s is writable", cfg.GapLogPath),
	}
}

// checkTrustedKeys loads cfg.TrustedKeysPath through sign.LoadTrustedKeys and
// returns the keyring the chain check needs. No configured path is a warning,
// not an error -- a deployment that never verifies has nothing to load yet --
// and it is deliberately NOT papered over with an empty keyring, which would
// make every signature unverifiable (see checkChain).
func checkTrustedKeys(cfg *config.Config) (Finding, map[string]ed25519.PublicKey) {
	if cfg.TrustedKeysPath == "" {
		return Finding{
			Check:    CheckTrustedKeys,
			Severity: Warn,
			Message: "no trusted keys file is configured (" + config.EnvTrustedKeysPath +
				" is unset), so no record's signature can be checked",
			Fix: fixTrustedKeys,
		}, nil
	}
	keyring, err := sign.LoadTrustedKeys(cfg.TrustedKeysPath)
	if err != nil {
		return Finding{
			Check:    CheckTrustedKeys,
			Severity: Err,
			Message:  fmt.Sprintf("the trusted keys at %s do not load: %v", cfg.TrustedKeysPath, err),
			Fix:      fixTrustedKeys,
		}, nil
	}
	return Finding{
		Check:    CheckTrustedKeys,
		Severity: OK,
		Message:  fmt.Sprintf("the trusted keys at %s load (%d key(s))", cfg.TrustedKeysPath, len(keyring)),
	}, keyring
}

// checkSigningKey asks sign.NewSigner for the key cfg names -- the same loader
// every command uses -- and reports the answer. A key that is absent is a
// warning, not an error: it is "not needed for the read paths" (verify, gaps,
// export, replay, explain), and the finding names the command that creates
// one. A key that is present but does not load is an error: commands that
// write signed records would refuse to start. The key's public fingerprint is
// the only thing about it that is ever printed.
//
// The loader's KeySourceEnv reads the PROCESS environment; see Diagnose for
// why that is the honest source for this one check.
func checkSigningKey(cfg *config.Config) Finding {
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: cfg.SigningKeyEnv})
	switch {
	case err == nil:
		return Finding{
			Check:    CheckSigningKey,
			Severity: OK,
			Message:  fmt.Sprintf("the signing key in %s loads (key id %s)", cfg.SigningKeyEnv, sg.KeyID()),
		}
	case errors.Is(err, sign.ErrNoKeyConfigured):
		return Finding{
			Check:    CheckSigningKey,
			Severity: Warn,
			Message: fmt.Sprintf(
				"no signing key is configured (%v); it is not needed for the read paths, but commands that write signed records (reconcile, proxy) refuse to start without one",
				err),
			Fix: fixSigningKey,
		}
	default:
		return Finding{
			Check:    CheckSigningKey,
			Severity: Err,
			Message:  fmt.Sprintf("the signing key in %s does not load: %v", cfg.SigningKeyEnv, err),
			Fix:      fixSigningKey,
		}
	}
}

// checkMem0APIKey reports the key's presence only -- never its value. It is
// required by the commands that make a Mem0 call (reconcile); `notary proxy`
// forwards the caller's own Authorization header and needs none, so an absent
// key is a warning.
func checkMem0APIKey(cfg *config.Config) Finding {
	if cfg.Mem0APIKey == "" {
		return Finding{
			Check:    CheckMem0APIKey,
			Severity: Warn,
			Message: "no Mem0 API key is configured (" + config.EnvMem0APIKey +
				" is unset); it is required only by commands that call Mem0 (reconcile), and notary proxy forwards the caller's own credential",
			Fix: fixMem0APIKey,
		}
	}
	return Finding{
		Check:    CheckMem0APIKey,
		Severity: OK,
		Message:  "a Mem0 API key is configured (" + config.EnvMem0APIKey + " is set)",
	}
}

// checkSensitivityRules parses the rules file when one is configured, through
// the same loader the write paths use (config.LoadSensitivityRules). No rules
// is not a failure: rules mark content sensitive at write time, and a
// deployment that records no content needs none. A configured file that does
// not parse is an error, because an operator who wrote rules expects them in
// force.
func checkSensitivityRules(env map[string]string) Finding {
	path := env[config.EnvSensitivityRules]
	if path == "" {
		return Finding{
			Check:    CheckSensitivityRules,
			Severity: OK,
			Message: "no sensitivity rules are configured (" + config.EnvSensitivityRules +
				" is unset), so no content is marked sensitive at write time",
		}
	}
	if _, err := config.LoadSensitivityRules(path); err != nil {
		return Finding{
			Check:    CheckSensitivityRules,
			Severity: Err,
			Message:  fmt.Sprintf("the sensitivity rules at %s do not load: %v", path, err),
			Fix:      fixRules,
		}
	}
	return Finding{
		Check:    CheckSensitivityRules,
		Severity: OK,
		Message:  fmt.Sprintf("the sensitivity rules at %s load", path),
	}
}

// checkChain asks the shared break collection whether the ledger and its gap
// log are intact, using the same function `notary verify` uses, so doctor and
// verify cannot give two answers to one question.
//
// The chain check is SKIPPED, not run, when no trusted keyring is available.
// sign.NewVerifier(nil) trusts no keys, and ledger.Verify reports a signature
// break for any key the verifier does not trust, so a keyless run would report
// every record in a perfectly healthy ledger as a break. That is a false
// accusation of tampering, so the check does not run at all: the finding says
// it was not checked and why, and it is a warning rather than an error, since
// an unconfigured keyring is a setup gap, not proof of damage.
//
// now stamps the finding with the moment the whole-ledger check was taken: the
// chain's state is a property of the whole ledger at one instant, and a report
// that says which instant is a report a reader can reason about.
func checkChain(cfg *config.Config, st *store.SQLiteStore, keyring map[string]ed25519.PublicKey, now time.Time) Finding {
	checkedAt := now.UTC().Format(time.RFC3339)

	switch {
	case st == nil:
		return Finding{
			Check:    CheckChain,
			Severity: Warn,
			Message:  "the chain was not checked: the ledger could not be opened",
			Fix:      fixDBPath,
		}
	case keyring == nil:
		return Finding{
			Check:    CheckChain,
			Severity: Warn,
			Message: "the chain was not checked: no trusted keys are configured (" +
				config.EnvTrustedKeysPath + " is unset or did not load), and a verifier with no trusted keys " +
				"reports every record as a signature break, which would accuse a healthy ledger of tampering",
			Fix: fixTrustedKeys,
		}
	}

	// Verification only reads: the ledger needs no signer here, exactly as in
	// `notary verify`.
	l := ledger.New(st, nil, nil)
	breaks, err := ledger.CollectBreaks(l, st, cfg.GapLogPath, sign.NewVerifier(keyring))
	switch {
	case err != nil:
		return Finding{
			Check:    CheckChain,
			Severity: Err,
			Message:  fmt.Sprintf("the chain could not be checked: %v", err),
			Fix:      fixChain,
		}
	case len(breaks) > 0:
		return Finding{
			Check:    CheckChain,
			Severity: Err,
			Message: fmt.Sprintf(
				"the ledger or its gap log shows %d integrity break(s) as of %s: the ledger may have been tampered with (run `notary verify --verbose` to see each one)",
				len(breaks), checkedAt),
			Fix: fixChain,
		}
	default:
		return Finding{
			Check:    CheckChain,
			Severity: OK,
			Message:  fmt.Sprintf("the ledger's chain and gap log show no breaks as of %s", checkedAt),
		}
	}
}

//go:embed setup.html
var setupHTML string

// setupPage is the setup.html template's data.
type setupPage struct {
	Findings []Finding
	// OKs, Warns and Errs are the severity counts the page's summary line
	// renders.
	OKs   int
	Warns int
	Errs  int
}

// RenderSetup writes the setup page -- the same findings `notary doctor`
// prints, rendered as one standalone HTML file -- to w. The page is written by
// doctor's own embedded template rather than by the report renderer, so a
// setup page and an evidence page stay different documents and this package
// ships without the report dependency (design section 3, decision 6).
//
// Every field of every finding is rendered through html/template, so a path or
// message containing markup is escaped rather than executed. No finding ever
// carries key material, so no page can either. RenderSetup never panics: a nil
// writer and a template failure are wrapped errors.
func RenderSetup(findings []Finding, w io.Writer) error {
	if w == nil {
		return errors.New("doctor: rendering the setup page: no writer")
	}

	// The template is parsed per call. Parsing once into package state would
	// be mutable global state shared across callers; the page is rendered once
	// per doctor run, so the parse cost is irrelevant.
	tmpl, err := template.New("setup.html").Parse(setupHTML)
	if err != nil {
		return fmt.Errorf("doctor: parsing the setup page template: %w", err)
	}

	page := setupPage{Findings: findings}
	for _, f := range findings {
		switch f.Severity {
		case Err:
			page.Errs++
		case Warn:
			page.Warns++
		default:
			page.OKs++
		}
	}

	if err := tmpl.Execute(w, page); err != nil {
		return fmt.Errorf("doctor: rendering the setup page: %w", err)
	}
	return nil
}
