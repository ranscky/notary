package doctor

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "modernc.org/sqlite" // registers the "sqlite" driver the tamper fixture uses

	"notary/config"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// doctorKeyEnv names the variable the deployment-under-test's signing key is
// asked for. It is deliberately NOT config.DefaultSigningKeyEnv: Diagnose asks
// sign.NewSigner, whose KeySourceEnv reads the *process* environment, so the
// tests point cfg.SigningKeyEnv at a variable the tests control and leave it
// unset unless a case is about the signing key -- no ambient NOTARY_SIGNING_KEY
// can then make a case pass or fail by accident.
const doctorKeyEnv = "NOTARY_DOCTOR_TEST_KEY"

// doctorFixtureKeyEnv names the variable the fixture ledger's own signer is
// loaded from. Keeping it distinct from doctorKeyEnv lets a test sign a real
// ledger while the deployment it diagnoses still has no signing key configured.
const doctorFixtureKeyEnv = "NOTARY_DOCTOR_FIXTURE_KEY"

// doctorTestSeed is a fixed 32-byte ed25519 seed, mirroring the ledger and
// verify tests' pattern: the fixtures sign and verify deterministically, and
// no key-generation helper is added to the library.
var doctorTestSeed = []byte{
	0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28,
	0x29, 0x2a, 0x2b, 0x2c, 0x2d, 0x2e, 0x2f, 0x30,
	0x31, 0x32, 0x33, 0x34, 0x35, 0x36, 0x37, 0x38,
	0x39, 0x3a, 0x3b, 0x3c, 0x3d, 0x3e, 0x3f, 0x40,
}

// doctorFixedNow is the deterministic clock Diagnose is given: the chain's
// state is a whole-ledger property sampled at a moment, and the tests pin the
// instant rather than tolerating a moving one.
var doctorFixedNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

// doctorValidRules is a minimal well-formed sensitivity rules document.
const doctorValidRules = `rules:
  - name: health-data
    match:
      metadata:
        category: health
`

// doctorMalformedRules does not parse as YAML, so LoadSensitivityRules rejects
// it with a parse error naming the file.
const doctorMalformedRules = "rules: [unclosed\n"

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// fixtureSigner loads the fixture ledger's signing key through the env
// KeySource and returns the matching public key, following the sign package's
// own test pattern.
func fixtureSigner(t *testing.T) (*sign.Signer, ed25519.PublicKey) {
	t.Helper()
	t.Setenv(doctorFixtureKeyEnv, base64.StdEncoding.EncodeToString(doctorTestSeed))
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: doctorFixtureKeyEnv})
	require.NoError(t, err)
	pub := ed25519.NewKeyFromSeed(doctorTestSeed).Public().(ed25519.PublicKey)
	return sg, pub
}

// doctorHash builds a distinct, non-zero 32-byte hash from a seed byte.
func doctorHash(seed byte) record.Hash {
	var h record.Hash
	for i := range h {
		h[i] = seed + byte(i)
	}
	return h
}

// doctorRecord returns a well-formed record, mirroring the ledger package's
// validRecord fixture, so the checks run against real appended data.
func doctorRecord(t *testing.T, id record.RecordID) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"ok":true}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonReturnedBySearch, ev)
	require.NoError(t, err)
	rec := record.Record{
		ID:     id,
		At:     doctorFixedNow,
		Event:  record.EventMemorySurfaced,
		Reason: reason,
		Subject: record.Subject{
			MemoryID:    "mem-1",
			Scope:       record.Scope{UserID: "u1", AgentID: "a1"},
			ContentHash: doctorHash(0x40),
		},
	}
	require.NoError(t, rec.Validate())
	return rec
}

// buildLedger appends n valid signed records to a fresh SQLite ledger at
// dbPath, closes the store, and returns the public key the chain was signed
// with.
func buildLedger(t *testing.T, dbPath string, n int) ed25519.PublicKey {
	t.Helper()
	sg, pub := fixtureSigner(t)
	st, err := store.Open(dbPath)
	require.NoError(t, err)
	l := ledger.New(st, sg, func() time.Time { return doctorFixedNow })
	for i := 0; i < n; i++ {
		_, err := l.Append(doctorRecord(t, record.RecordID(fmt.Sprintf("rec-%04d", i+1))))
		require.NoError(t, err)
	}
	require.NoError(t, st.Close())
	return pub
}

// writeTrustedKeys writes a one-line trusted-keys file holding pub -- the
// exact format sign.LoadTrustedKeys consumes -- and returns its path.
func writeTrustedKeys(t *testing.T, pub ed25519.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trusted.keys")
	require.NoError(t, os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(pub)+"\n"), 0o644))
	return path
}

// writeFile writes content to a fresh file under a per-test temp directory and
// returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// tamper runs one statement against dbPath on a separate connection, the way
// an out-of-band edit of the database file would. The statement matches the
// tamper the ledger and verify tests use: a record whose stored event no
// longer matches its stored hash.
func tamper(t *testing.T, dbPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(`UPDATE records SET event = 'memory_kept' WHERE seq = 0`)
	require.NoError(t, err)
}

// healthyEnv returns a fully configured, healthy environment: a three-record
// signed ledger whose trusted keys file holds the matching public key, a
// writable gap log, a valid rules file, a Mem0 API key, and a signing key that
// loads.
func healthyEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "ledger.db")
	pub := buildLedger(t, dbPath, 3)
	t.Setenv(doctorKeyEnv, base64.StdEncoding.EncodeToString(doctorTestSeed))
	return map[string]string{
		config.EnvDBPath:           dbPath,
		config.EnvGapLogPath:       filepath.Join(dir, "notary-gaps.log"),
		config.EnvTrustedKeysPath:  writeTrustedKeys(t, pub),
		config.EnvMem0APIKey:       "test-mem0-key",
		config.EnvSensitivityRules: writeFile(t, "rules.yaml", doctorValidRules),
	}
}

// diagnose resolves env the way the command does -- through config.LoadFrom --
// points the signing key check at the test's own variable, and runs Diagnose.
func diagnose(t *testing.T, env map[string]string) []Finding {
	t.Helper()
	cfg, err := config.LoadFrom(env)
	require.NoError(t, err)
	cfg.SigningKeyEnv = doctorKeyEnv
	return Diagnose(cfg, env, doctorFixedNow)
}

// findingFor returns the finding for check, failing when the check did not
// produce one -- a check that silently vanished must not pass.
func findingFor(t *testing.T, findings []Finding, check string) Finding {
	t.Helper()
	for _, f := range findings {
		if f.Check == check {
			return f
		}
	}
	t.Fatalf("no finding for check %q; findings: %+v", check, findings)
	return Finding{}
}

// assertFixIsRunnable pins the report's contract: a finding that needs fixing
// names an exact command, not a description of one. "Runnable" is defined
// mechanically as a line starting with `export ` (set the variable it names)
// or `notary ` (run the command that repairs it).
func assertFixIsRunnable(t *testing.T, f Finding) {
	t.Helper()
	require.NotEmpty(t, f.Fix, "the %s finding for %q must name the command that fixes it", f.Severity, f.Check)
	assert.True(t,
		strings.HasPrefix(f.Fix, "export ") || strings.HasPrefix(f.Fix, "notary "),
		"the fix for %q must be a runnable command, got %q", f.Check, f.Fix)
}

// diagnoseChecks is the design's check list, in the order Diagnose reports it.
var diagnoseChecks = []string{
	CheckLedger,
	CheckGapLog,
	CheckTrustedKeys,
	CheckSigningKey,
	CheckMem0APIKey,
	CheckSensitivityRules,
	CheckChain,
}

// ---------------------------------------------------------------------------
// The healthy answer
// ---------------------------------------------------------------------------

// TestDiagnoseReportsEachCheck is the healthy-environment contract: every
// check in the design's list produces exactly one finding, in the design's
// order, and a fully configured deployment produces no error and nothing to
// fix.
func TestDiagnoseReportsEachCheck(t *testing.T) {
	findings := diagnose(t, healthyEnv(t))

	require.Len(t, findings, len(diagnoseChecks), "one finding per check, no more and no fewer")
	for i, check := range diagnoseChecks {
		assert.Equal(t, check, findings[i].Check, "finding %d must be the %q check", i, check)
	}
	for _, f := range findings {
		assert.Equal(t, OK, f.Severity, "a healthy environment must report %q as ok, got %q", f.Check, f.Message)
		assert.Empty(t, f.Fix, "a check that held has nothing to fix")
	}
}

// ---------------------------------------------------------------------------
// Each broken check
// ---------------------------------------------------------------------------

// TestDiagnoseReportsEachBrokenCheck is the table: one case per check, each
// breaking exactly one fact in an otherwise healthy environment and asserting
// that the finding is an error and that its Fix names a runnable command.
// The case names mirror the brief's test names.
func TestDiagnoseReportsEachBrokenCheck(t *testing.T) {
	cases := []struct {
		name        string
		breakIt     func(t *testing.T, env map[string]string)
		check       string
		fixContains string
	}{
		{
			name: "DiagnoseReportsAMissingLedger",
			breakIt: func(t *testing.T, env map[string]string) {
				// A path whose parent is a regular file: the ledger cannot be
				// created there, so store.Open fails.
				file := filepath.Join(t.TempDir(), "not-a-directory")
				require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
				env[config.EnvDBPath] = filepath.Join(file, "ledger.db")
			},
			check:       CheckLedger,
			fixContains: config.EnvDBPath,
		},
		{
			name: "DiagnoseReportsAnUnwritableGapLog",
			breakIt: func(t *testing.T, env map[string]string) {
				// A directory is never writable as a file: the append-open the
				// check performs fails exactly as it would on a read-only
				// volume, without depending on permission bits a root-run test
				// could ignore.
				dir := filepath.Join(t.TempDir(), "gap-log-is-a-directory")
				require.NoError(t, os.Mkdir(dir, 0o755))
				env[config.EnvGapLogPath] = dir
			},
			check:       CheckGapLog,
			fixContains: config.EnvGapLogPath,
		},
		{
			name: "DiagnoseReportsAMalformedTrustedKeysFile",
			breakIt: func(t *testing.T, env map[string]string) {
				env[config.EnvTrustedKeysPath] = writeFile(t, "trusted.keys", "not base64 at all !!!\n")
			},
			check:       CheckTrustedKeys,
			fixContains: config.EnvTrustedKeysPath,
		},
		{
			name: "DiagnoseReportsAMalformedSigningKey",
			breakIt: func(t *testing.T, _ map[string]string) {
				t.Setenv(doctorKeyEnv, "not base64 at all !!!")
			},
			check:       CheckSigningKey,
			fixContains: "notary doctor --generate-key",
		},
		{
			name: "DiagnoseReportsAnUnparseableRulesFile",
			breakIt: func(t *testing.T, env map[string]string) {
				env[config.EnvSensitivityRules] = writeFile(t, "rules.yaml", doctorMalformedRules)
			},
			check:       CheckSensitivityRules,
			fixContains: config.EnvSensitivityRules,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := healthyEnv(t)
			tc.breakIt(t, env)

			findings := diagnose(t, env)

			f := findingFor(t, findings, tc.check)
			assert.Equal(t, Err, f.Severity, "%s must be an error, got %q (%s)", tc.check, f.Message, f.Severity)
			assertFixIsRunnable(t, f)
			assert.Contains(t, f.Fix, tc.fixContains, "the fix must name what to set or run")
			assert.NotEmpty(t, f.Message, "every finding must say what was found")
		})
	}
}

// TestDiagnoseReportsABrokenChain reuses the tamper fixture: a record edited
// out of band must make the chain check an error whose fix tells the operator
// to see the breaks.
func TestDiagnoseReportsABrokenChain(t *testing.T) {
	env := healthyEnv(t)
	tamper(t, env[config.EnvDBPath])

	findings := diagnose(t, env)

	f := findingFor(t, findings, CheckChain)
	assert.Equal(t, Err, f.Severity, "a tampered record must be reported as an error, got %q", f.Message)
	assertFixIsRunnable(t, f)
	assert.Contains(t, f.Fix, "notary verify")
	assert.Contains(t, f.Message, "break", "the finding must say what was found")
}

// ---------------------------------------------------------------------------
// The checks that must not be mistaken for failures
// ---------------------------------------------------------------------------

// TestDiagnoseDoesNotFailOnAMissingSigningKeyForReadPaths pins the spec's
// wording: with no signing key configured the finding is informational -- not
// an error -- and it says the key is not needed for the read paths, while
// still naming the command that produces one.
func TestDiagnoseDoesNotFailOnAMissingSigningKeyForReadPaths(t *testing.T) {
	env := healthyEnv(t)
	t.Setenv(doctorKeyEnv, "") // the deployment has no signing key

	findings := diagnose(t, env)

	f := findingFor(t, findings, CheckSigningKey)
	assert.NotEqual(t, Err, f.Severity, "a missing signing key must not be an error")
	assert.Contains(t, f.Message, "not needed for the read paths")
	assertFixIsRunnable(t, f)
	assert.Contains(t, f.Fix, "notary doctor --generate-key",
		"the finding must name the command that creates the missing key")
}

// TestDiagnoseSkipsTheChainCheckWithoutTrustedKeys is the false-alarm canary.
// sign.NewVerifier(nil) trusts no keys and reports EVERY record as a signature
// break, so a chain check run without trusted keys would turn a perfectly
// healthy ledger into a report of thousands of breaks -- a false accusation of
// tampering, the worst output this tool could produce. With no trusted keys
// configured the chain check must not run at all: the finding says it was not
// checked and why, and the run reports no error.
func TestDiagnoseSkipsTheChainCheckWithoutTrustedKeys(t *testing.T) {
	env := healthyEnv(t)
	delete(env, config.EnvTrustedKeysPath) // NOTARY_TRUSTED_KEYS_PATH unset

	findings := diagnose(t, env)

	for _, f := range findings {
		assert.NotEqual(t, Err, f.Severity,
			"a healthy ledger with no trusted keys must report no error (finding %q: %s)", f.Check, f.Message)
		assert.NotContains(t, f.Message, "integrity break",
			"no finding may report a break the keyless run would fabricate (finding %q: %s)", f.Check, f.Message)
	}

	chain := findingFor(t, findings, CheckChain)
	assert.Equal(t, Warn, chain.Severity)
	assert.True(t, strings.HasPrefix(chain.Message, "the chain was not checked"),
		"the finding must say the chain was not checked, got %q", chain.Message)
	assertFixIsRunnable(t, chain)
}

// TestDiagnoseNeverEchoesKeyMaterial is the canary for the guardrail: the
// generated or configured key material must never appear in a finding -- and
// findings are the only thing the text output and setup.html render.
func TestDiagnoseNeverEchoesKeyMaterial(t *testing.T) {
	env := healthyEnv(t)
	material := base64.StdEncoding.EncodeToString(doctorTestSeed)

	findings := diagnose(t, env)
	require.NotEmpty(t, findings)

	signingKey := findingFor(t, findings, CheckSigningKey)
	assert.Equal(t, OK, signingKey.Severity,
		"the fixture's key must load, or this canary proves nothing: %s", signingKey.Message)
	for _, f := range findings {
		assert.NotContains(t, f.Message, material, "finding %q echoes key material", f.Check)
		assert.NotContains(t, f.Fix, material, "finding %q echoes key material in its fix", f.Check)
	}
}

// TestDiagnoseHandlesANilConfig pins the no-panic rule for the one input a
// caller can get wrong: Diagnose with no configuration returns a finding
// saying so rather than dereferencing nil.
func TestDiagnoseHandlesANilConfig(t *testing.T) {
	findings := Diagnose(nil, nil, doctorFixedNow)

	require.NotEmpty(t, findings)
	assert.Equal(t, Err, findings[0].Severity)
	assertFixIsRunnable(t, findings[0])
}

// ---------------------------------------------------------------------------
// The setup page
// ---------------------------------------------------------------------------

// TestRenderSetupWritesAnEscapedPage pins that the page carries every
// finding's check, message and fix, and that a finding's text is HTML-escaped
// rather than executed: the message is data, never markup.
func TestRenderSetupWritesAnEscapedPage(t *testing.T) {
	findings := []Finding{
		{
			Check:    CheckLedger,
			Severity: Err,
			Message:  "the ledger <script>alert(1)</script> cannot be opened",
			Fix:      "export " + config.EnvDBPath + "=/tmp/ledger.db",
		},
		{Check: CheckChain, Severity: OK, Message: "the chain and gap log show no breaks"},
	}

	var buf bytes.Buffer
	require.NoError(t, RenderSetup(findings, &buf))
	page := buf.String()

	assert.Contains(t, page, "<!DOCTYPE html>", "the page must be a self-contained HTML document")
	assert.Contains(t, page, "Notary setup", "the page must say what it is")
	assert.Contains(t, page, CheckLedger)
	assert.Contains(t, page, CheckChain)
	assert.Contains(t, page, "cannot be opened")
	assert.Contains(t, page, "export "+config.EnvDBPath+"=/tmp/ledger.db")
	assert.Contains(t, page, "error", "each finding must show its severity")
	assert.Contains(t, page, "warn", "the page must label every severity, including absences")
	assert.Contains(t, page, `class="severity error"`,
		"the severity must render as its label, not its number: Severity.String is what the class carries")
	assert.Contains(t, page, `class="severity ok"`)
	assert.NotContains(t, page, "<script>",
		"a finding's message must be escaped: stored text can never become markup")
	assert.Contains(t, page, "&lt;script&gt;")
}

// TestRenderSetupRejectsANilWriter pins the no-panic rule: a nil writer is a
// wrapped error, not a nil-pointer dereference inside the template engine.
func TestRenderSetupRejectsANilWriter(t *testing.T) {
	assert.Error(t, RenderSetup(nil, nil))
}

// TestRenderSetupAcceptsNoFindings pins that an empty diagnosis still renders
// a valid page rather than failing.
func TestRenderSetupAcceptsNoFindings(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, RenderSetup(nil, &buf))
	assert.Contains(t, buf.String(), "<!DOCTYPE html>")
}
