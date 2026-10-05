package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
	"notary/internal/doctor"
	"notary/internal/sign"
)

// doctorCmdKeyEnv names the variable the command tests point cfg.SigningKeyEnv
// at. Diagnose asks sign.NewSigner for the key and sign.NewSigner's
// KeySourceEnv reads the *process* environment, so pointing the check at a
// variable the tests own and clearing it keeps every case deterministic
// whatever the ambient environment holds.
const doctorCmdKeyEnv = "NOTARY_DOCTOR_CMD_TEST_KEY"

// doctorCmdSeed is a fixed 32-byte ed25519 seed, mirroring the verify tests'
// pattern: the fixture key is loaded, never generated.
var doctorCmdSeed = []byte{
	0x41, 0x42, 0x43, 0x44, 0x45, 0x46, 0x47, 0x48,
	0x49, 0x4a, 0x4b, 0x4c, 0x4d, 0x4e, 0x4f, 0x50,
	0x51, 0x52, 0x53, 0x54, 0x55, 0x56, 0x57, 0x58,
	0x59, 0x5a, 0x5b, 0x5c, 0x5d, 0x5e, 0x5f, 0x60,
}

// newTestDoctorCmd builds a doctor command whose output is captured in a
// buffer, so a test can assert on the printed findings, not only on the
// returned error -- the way newTestVerifyCmd does.
func newTestDoctorCmd(t *testing.T) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	cmd := newDoctorCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

// executeDoctorCmd runs doctor through its real RunE -- the path that builds
// the environment map from os.Environ and resolves the configuration from it
// -- with args, and returns what it printed and what it returned.
func executeDoctorCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newDoctorCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// newDoctorEnv returns a healthy temp-dir environment map: the ledger and gap
// log live in a fresh directory, nothing else is configured.
func newDoctorEnv(t *testing.T) map[string]string {
	t.Helper()
	dir := t.TempDir()
	return map[string]string{
		config.EnvDBPath:     filepath.Join(dir, "ledger.db"),
		config.EnvGapLogPath: filepath.Join(dir, "notary-gaps.log"),
	}
}

// loadDoctorCfg resolves env the way the command does and points the
// signing-key check at the test's own (empty) variable, so no ambient key can
// leak into a case.
func loadDoctorCfg(t *testing.T, env map[string]string) *config.Config {
	t.Helper()
	cfg, err := config.LoadFrom(env)
	require.NoError(t, err)
	cfg.SigningKeyEnv = doctorCmdKeyEnv
	t.Setenv(doctorCmdKeyEnv, "")
	return cfg
}

// TestDoctorCmdExitsZeroOnWarningsOnly pins the exit contract's other half: a
// deployment that is merely unconfigured -- no trusted keys, no signing key,
// no Mem0 key, so the chain cannot be checked -- is warnings only, and
// warnings alone must not fail the command.
func TestDoctorCmdExitsZeroOnWarningsOnly(t *testing.T) {
	env := newDoctorEnv(t)
	cfg := loadDoctorCfg(t, env)

	cmd, buf := newTestDoctorCmd(t)
	require.NoError(t, runDoctor(cmd, cfg, env), "warnings alone must exit zero")

	out := buf.String()
	assert.Contains(t, out, "warn", "the report must show the warnings")
	assert.Contains(t, out, "signing key")
	assert.Contains(t, out, "0 error(s)", "the summary must count no errors")
	assert.Contains(t, out, doctor.CheckMem0BaseURL,
		"the Mem0 base URL check must be reported, even when it held")
}

// TestDoctorCmdExitsNonZeroOnAnError pins the exit contract: any error finding
// makes the command return an error, so it can gate a pipeline. It uses a
// malformed trusted keys file -- a configured thing that is broken -- and also
// pins that the chain is then reported as NOT CHECKED rather than as broken.
func TestDoctorCmdExitsNonZeroOnAnError(t *testing.T) {
	env := newDoctorEnv(t)
	env[config.EnvTrustedKeysPath] = filepath.Join(t.TempDir(), "trusted.keys")
	require.NoError(t, os.WriteFile(env[config.EnvTrustedKeysPath], []byte("not base64 !!!\n"), 0o644))
	cfg := loadDoctorCfg(t, env)

	cmd, buf := newTestDoctorCmd(t)
	err := runDoctor(cmd, cfg, env)
	require.Error(t, err, "an error finding must exit non-zero")

	out := buf.String()
	assert.Contains(t, out, "trusted keys", "the report must name the broken check")
	assert.Contains(t, out, "1 error(s)", "the summary must count the error")
	assert.Contains(t, out, "the chain was not checked",
		"a chain that could not be checked must be reported as not checked")
	assert.NotContains(t, out, "integrity break",
		"the keyless run must not fabricate breaks for a healthy ledger")
	assert.Contains(t, err.Error(), "doctor", "the error must say which command failed")
}

// TestDoctorCmdReportsAnUnusableUpstreamURL pins the one check whose validator
// lives in this package: parseUpstream is the project's URL validator, so
// `notary doctor` calls it rather than writing a second one, and a URL that
// parses but has no host -- the typo parseUpstream exists to catch -- is an
// error whose fix names the variable.
func TestDoctorCmdReportsAnUnusableUpstreamURL(t *testing.T) {
	env := newDoctorEnv(t)
	env[config.EnvMem0BaseURL] = "mem0.internal:8081" // url.Parse accepts this: scheme, no host
	cfg := loadDoctorCfg(t, env)

	cmd, buf := newTestDoctorCmd(t)
	err := runDoctor(cmd, cfg, env)
	require.Error(t, err, "an unusable Mem0 base URL must exit non-zero")

	out := buf.String()
	assert.Contains(t, out, doctor.CheckMem0BaseURL)
	assert.Contains(t, out, config.EnvMem0BaseURL)
	assert.Contains(t, out, "export "+config.EnvMem0BaseURL)
}

// TestDoctorCmdGenerateKeyPrintsAnEnvLineAndWritesNothing pins the default
// half of --generate-key: the material goes to stdout as an export line, and
// no file is written anywhere. The printed material must be a KEY, not a
// string, so it is round-tripped through the loader every command uses.
func TestDoctorCmdGenerateKeyPrintsAnEnvLineAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.EnvDBPath, filepath.Join(dir, "ledger.db"))
	t.Setenv(config.EnvGapLogPath, filepath.Join(dir, "notary-gaps.log"))

	out, err := executeDoctorCmd(t, "--generate-key")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 1, "--generate-key must print exactly the export line, got %q", out)
	prefix := "export " + config.DefaultSigningKeyEnv + "="
	require.True(t, strings.HasPrefix(lines[0], prefix), "stdout must be the export line, got %q", lines[0])
	material := strings.TrimPrefix(lines[0], prefix)
	require.NotEmpty(t, material, "the export line must carry a key")

	t.Setenv(config.DefaultSigningKeyEnv, material)
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: config.DefaultSigningKeyEnv})
	require.NoError(t, err, "the printed material must load as a signing key, not merely look like one")
	assert.NotEmpty(t, sg.KeyID())

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "--generate-key must write no file at all")
}

// TestDoctorCmdGenerateKeyRefusesAPathInsideTheRepository is the guardrail,
// enforced rather than documented: the repository must hold no key material,
// ever, so a --key-out inside the checkout is refused before anything is
// written and no key is printed either. The package's own working directory is
// inside the repository, so both a relative and an absolute path resolve
// there.
func TestDoctorCmdGenerateKeyRefusesAPathInsideTheRepository(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)

	cases := []struct {
		name string
		path string
	}{
		{"absolute", filepath.Join(cwd, "doctor-key-must-not-exist.key")},
		{"relative", "doctor-key-must-not-exist.key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := executeDoctorCmd(t, "--generate-key", "--key-out", tc.path)
			require.Error(t, err, "a key path inside the repository must be refused")
			assert.Contains(t, err.Error(), "repository", "the refusal must say why")

			_, statErr := os.Stat(tc.path)
			assert.ErrorIs(t, statErr, os.ErrNotExist, "the refused path must not be created")
			assertNoExportedKeyLine(t, out)
		})
	}
}

// assertNoExportedKeyLine fails when out contains a line that is a printed
// export line for the signing key -- the only shape in which generated key
// material ever reaches stdout. The command's own help text mentions the
// export line, so the check is anchored at the start of a line rather than a
// substring search.
func assertNoExportedKeyLine(t *testing.T, out string) {
	t.Helper()
	prefix := "export " + config.DefaultSigningKeyEnv + "="
	for _, line := range strings.Split(out, "\n") {
		assert.False(t, strings.HasPrefix(line, prefix),
			"a refused run must print no key material, got %q", line)
	}
}

// TestDoctorCmdGenerateKeyWritesAKeyTheFileLoaderAccepts is the other half of
// the loader's rules, copied: sign.KeySourceFile accepts exactly mode 0600 and
// fails with sign.ErrLoosePerms otherwise, so the file --key-out writes must
// be 0600 and must load through that same loader, or doctor would write a key
// its own commands refuse to load.
func TestDoctorCmdGenerateKeyWritesAKeyTheFileLoaderAccepts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")

	out, err := executeDoctorCmd(t, "--generate-key", "--key-out", path)
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err, "--key-out must write the key file")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(),
		"sign.KeySourceFile accepts exactly 0600; the writer must not rely on the umask")

	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceFile, Ref: path})
	require.NoError(t, err, "doctor must write a key its own commands can load")
	assert.NotEmpty(t, sg.KeyID())

	material, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, out, strings.TrimSpace(string(material)),
		"the key material must never be echoed when it is written to a file")
}

// TestDoctorCmdReportsTheMem0URLCheckInTheCheckListOrder pins that the one
// finding the command computes itself (the Mem0 base URL, whose validator is
// parseUpstream) is spliced into the design's check order -- after the signing
// key, before the Mem0 API key -- rather than appended at the end.
func TestDoctorCmdReportsTheMem0URLCheckInTheCheckListOrder(t *testing.T) {
	env := newDoctorEnv(t)
	cfg := loadDoctorCfg(t, env)

	cmd, buf := newTestDoctorCmd(t)
	require.NoError(t, runDoctor(cmd, cfg, env))

	out := buf.String()
	urlPos := strings.Index(out, doctor.CheckMem0BaseURL)
	require.NotEqual(t, -1, urlPos, "the Mem0 base URL check must be reported")
	for _, check := range []string{doctor.CheckLedger, doctor.CheckSigningKey} {
		assert.Less(t, strings.Index(out, check), urlPos, "%s must come before the Mem0 base URL check", check)
	}
	for _, check := range []string{doctor.CheckMem0APIKey, doctor.CheckSensitivityRules, doctor.CheckChain} {
		assert.Less(t, urlPos, strings.Index(out, check), "the Mem0 base URL check must come before %s", check)
	}
}

// TestInsertFindingAfter pins the splice itself, including the overlapping
// copy that makes room for the inserted finding and the append fallback.
func TestInsertFindingAfter(t *testing.T) {
	f := func(check string) doctor.Finding { return doctor.Finding{Check: check} }
	checks := func(findings []doctor.Finding) []string {
		names := make([]string, 0, len(findings))
		for _, finding := range findings {
			names = append(names, finding.Check)
		}
		return names
	}

	findings := []doctor.Finding{f("a"), f("b"), f("c")}
	got := insertFindingAfter(findings, "a", f("spliced"))
	assert.Equal(t, []string{"a", "spliced", "b", "c"}, checks(got))

	// A check that produced no finding must not lose the inserted one.
	appended := insertFindingAfter([]doctor.Finding{f("a")}, "missing", f("spliced"))
	assert.Equal(t, []string{"a", "spliced"}, checks(appended))
}

// TestDoctorCmdRejectsKeyOutWithoutGenerateKey pins that the write half of the
// flag pair cannot run alone: --key-out without --generate-key is a usage
// error naming the missing flag, and nothing is written.
func TestDoctorCmdRejectsKeyOutWithoutGenerateKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")

	_, err := executeDoctorCmd(t, "--key-out", path)
	require.Error(t, err, "--key-out alone must be refused")
	assert.Contains(t, err.Error(), "--generate-key", "the refusal must name the missing flag")

	_, statErr := os.Stat(path)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "a refused run must write nothing")
}

// TestDoctorCmdSetupPageContainsNoKeyMaterial is the canary for the page:
// setup.html is rendered from the same findings the command prints, and no
// finding ever carries key material, so neither the page nor the text output
// may contain the configured key -- while the run still reports the key it
// loaded, by its public fingerprint, so the canary cannot pass by the check
// silently not running.
func TestDoctorCmdSetupPageContainsNoKeyMaterial(t *testing.T) {
	dir := t.TempDir()
	material := base64.StdEncoding.EncodeToString(doctorCmdSeed)
	t.Setenv(config.DefaultSigningKeyEnv, material)
	t.Setenv(config.EnvDBPath, filepath.Join(dir, "ledger.db"))
	t.Setenv(config.EnvGapLogPath, filepath.Join(dir, "notary-gaps.log"))
	t.Setenv(config.EnvTrustedKeysPath, "")

	// The fingerprint the run must report: prove the check saw the key.
	pg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: config.DefaultSigningKeyEnv})
	require.NoError(t, err)

	out, err := executeDoctorCmd(t, "--out", dir)
	require.NoError(t, err, "a healthy environment with a signing key must exit zero")
	assert.Contains(t, out, pg.KeyID(),
		"the run must report the loaded key by its public fingerprint, or this canary proves nothing")
	assert.NotContains(t, out, material, "the text output must never contain key material")

	page, err := os.ReadFile(filepath.Join(dir, "setup.html"))
	require.NoError(t, err, "--out must write setup.html")
	assert.Contains(t, string(page), "Notary setup")
	assert.NotContains(t, string(page), material, "setup.html must never contain key material")
}
