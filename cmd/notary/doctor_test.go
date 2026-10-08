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
// written and no key is printed either. The package's working directory is
// inside the repository, so a relative path, an absolute path, and an absolute
// path given from elsewhere all resolve there.
//
// Every target lives in a fresh subdirectory of the package directory that the
// test removes afterwards. That keeps the checkout clean even when the guard
// is broken and the run writes a key -- as it did once, against the build that
// scoped the refusal to the working directory: the file existed, the test
// failed, and the cleanup removed the material.
func TestDoctorCmdGenerateKeyRefusesAPathInsideTheRepository(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)

	// inRepository returns a fresh, empty directory inside the repository and
	// removes it (and anything a buggy run wrote into it) after the test.
	inRepository := func(t *testing.T) string {
		t.Helper()
		dir, err := os.MkdirTemp(cwd, "doctor-key-refusal-")
		require.NoError(t, err)
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}

	t.Run("absolute", func(t *testing.T) {
		target := filepath.Join(inRepository(t), "doctor-key-must-not-exist.key")

		out, err := executeDoctorCmd(t, "--generate-key", "--key-out", target)
		require.Error(t, err, "a key path inside the repository must be refused")
		assert.Contains(t, err.Error(), "repository", "the refusal must say why")

		_, statErr := os.Stat(target)
		assert.ErrorIs(t, statErr, os.ErrNotExist, "the refused path must not be created")
		assertNoExportedKeyLine(t, out)
	})

	t.Run("relative", func(t *testing.T) {
		t.Chdir(inRepository(t)) // the relative path resolves inside the repository

		out, err := executeDoctorCmd(t, "--generate-key", "--key-out", "doctor-key-must-not-exist.key")
		require.Error(t, err, "a relative key path inside the repository must be refused")
		assert.Contains(t, err.Error(), "repository", "the refusal must say why")

		_, statErr := os.Stat("doctor-key-must-not-exist.key")
		assert.ErrorIs(t, statErr, os.ErrNotExist, "the refused path must not be created")
		assertNoExportedKeyLine(t, out)
	})

	// The checkout the TARGET lives in counts too. Run from a directory
	// outside any repository, an absolute path into this one must still be
	// refused: locating the repository only from the working directory would
	// let a key be written into the checkout from anywhere else.
	t.Run("from outside the repository", func(t *testing.T) {
		outside := t.TempDir()
		if root, ok := repositoryRoot(outside); ok {
			t.Skipf("the temp directory %s is inside a checkout (%s); cannot test the from-outside case", outside, root)
		}
		t.Chdir(outside) // the process no longer runs from the repository

		target := filepath.Join(inRepository(t), "doctor-key-must-not-exist.key")
		out, err := executeDoctorCmd(t, "--generate-key", "--key-out", target)
		require.Error(t, err, "a path inside a checkout must be refused even when doctor runs from elsewhere")
		assert.Contains(t, err.Error(), "repository", "the refusal must say why")

		_, statErr := os.Stat(target)
		assert.ErrorIs(t, statErr, os.ErrNotExist, "the refused path must not be created")
		assertNoExportedKeyLine(t, out)
	})
}

// TestDoctorCmdGenerateKeyRefusesToOverwriteAnExistingKey pins the O_EXCL
// guard: replacing an existing key file would destroy the only copy of a
// signing identity, so an existing path is a refusal -- non-zero, the original
// bytes untouched, and no key printed on the way out.
func TestDoctorCmdGenerateKeyRefusesToOverwriteAnExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "signing.key")
	original := []byte("an existing signing key that must survive\n")
	require.NoError(t, os.WriteFile(path, original, 0o600))

	out, err := executeDoctorCmd(t, "--generate-key", "--key-out", path)
	require.Error(t, err, "an existing key file must be refused, never overwritten")
	assert.ErrorIs(t, err, os.ErrExist, "the refusal must be the exclusive-create failure")
	assert.Contains(t, err.Error(), "signing key", "the refusal must say what it refused to write")
	assertNoExportedKeyLine(t, out)

	after, rerr := os.ReadFile(path)
	require.NoError(t, rerr)
	assert.Equal(t, original, after, "the existing key material must be byte-identical after the refusal")
}

// TestDoctorCmdRejectsOutWithGenerateKey pins that the flag pair cannot be
// silently half-honoured: --generate-key writes a key and no page, so
// combining it with --out is a refusal that names both flags, not a run that
// ignores one of them.
func TestDoctorCmdRejectsOutWithGenerateKey(t *testing.T) {
	dir := t.TempDir()

	out, err := executeDoctorCmd(t, "--generate-key", "--out", dir)
	require.Error(t, err, "--out with --generate-key must be refused")
	assert.Contains(t, err.Error(), "--out", "the refusal must name --out")
	assert.Contains(t, err.Error(), "--generate-key", "the refusal must name --generate-key")

	assertNoExportedKeyLine(t, out)
	_, statErr := os.Stat(filepath.Join(dir, setupPageFilename))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "a refused run must write no page")
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

// TestDoctorCmdTrustedKeysOutWritesThePairAndRequiresGenerateKey is the test
// that makes the two halves a PAIR rather than two strings: the file
// --trusted-keys-out writes must load through the real sign.LoadTrustedKeys
// into a keyring whose key id is the one sign.NewSigner reports for the
// private half this same run printed -- so the identity is checked by the
// loader and the signer, not by comparing base64 this test produced itself.
// Without --generate-key the flag is refused by name and writes nothing.
func TestDoctorCmdTrustedKeysOutWritesThePairAndRequiresGenerateKey(t *testing.T) {
	dir := t.TempDir()
	keysPath := filepath.Join(dir, "trusted.keys")

	out, err := executeDoctorCmd(t, "--generate-key", "--trusted-keys-out", keysPath)
	require.NoError(t, err)

	// The private half is still on stdout, exactly as without the flag; the
	// report of the public half's file follows it.
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 2, "the export line and the trusted-keys report, in that order, got %q", out)
	prefix := "export " + config.DefaultSigningKeyEnv + "="
	require.True(t, strings.HasPrefix(lines[0], prefix),
		"the private half must be printed first, got %q", lines[0])
	material := strings.TrimPrefix(lines[0], prefix)

	t.Setenv(config.DefaultSigningKeyEnv, material)
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: config.DefaultSigningKeyEnv})
	require.NoError(t, err, "the printed private half must load")

	keyring, err := sign.LoadTrustedKeys(keysPath)
	require.NoError(t, err, "the written file must be exactly what sign.LoadTrustedKeys reads")
	require.Len(t, keyring, 1)
	assert.Contains(t, keyring, sg.KeyID(),
		"the written public half must be the one belonging to the printed private half")

	// And the pair must work as a pair: sign with the private half, verify
	// under the public half loaded from the file.
	msg := []byte("the two halves must be one identity")
	sig, err := sg.Sign(msg)
	require.NoError(t, err)
	require.NoError(t, sign.NewVerifier(keyring).Verify(sg.KeyID(), msg, sig))

	info, err := os.Stat(keysPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm(),
		"the public half is 0644: it is not a credential, it is meant to be read")

	t.Run("with --key-out both halves become files", func(t *testing.T) {
		dir := t.TempDir()
		keyPath := filepath.Join(dir, "signing.key")
		keysPath := filepath.Join(dir, "trusted.keys")

		out, err := executeDoctorCmd(t, "--generate-key", "--key-out", keyPath, "--trusted-keys-out", keysPath)
		require.NoError(t, err)
		assertNoExportedKeyLine(t, out)

		privateInfo, err := os.Stat(keyPath)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), privateInfo.Mode().Perm())
		keysInfo, err := os.Stat(keysPath)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o644), keysInfo.Mode().Perm())

		sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceFile, Ref: keyPath})
		require.NoError(t, err)
		keyring, err := sign.LoadTrustedKeys(keysPath)
		require.NoError(t, err)
		assert.Contains(t, keyring, sg.KeyID(), "the two files must be one identity")
	})

	t.Run("without --generate-key the flag is refused", func(t *testing.T) {
		refused := filepath.Join(t.TempDir(), "refused.keys")

		_, err := executeDoctorCmd(t, "--trusted-keys-out", refused)
		require.Error(t, err, "the flag must require --generate-key")
		assert.Contains(t, err.Error(), "--trusted-keys-out", "the refusal must name the flag")
		assert.Contains(t, err.Error(), "--generate-key", "the refusal must name what it needs")
		_, statErr := os.Stat(refused)
		assert.ErrorIs(t, statErr, os.ErrNotExist, "a refused run must write nothing")
	})

	t.Run("an existing trusted keys file is refused", func(t *testing.T) {
		// O_EXCL, for the same reason --key-out has it: a public file is not a
		// credential, but it may be a keyring holding other keys, and
		// replacing it silently would drop every one of them.
		path := filepath.Join(t.TempDir(), "trusted.keys")
		original := []byte("# an existing keyring that must survive\n")
		require.NoError(t, os.WriteFile(path, original, 0o644))

		_, err := executeDoctorCmd(t, "--generate-key", "--trusted-keys-out", path)
		require.Error(t, err, "an existing trusted keys file must be refused, never overwritten")
		assert.ErrorIs(t, err, os.ErrExist, "the refusal must be the exclusive-create failure")

		after, rerr := os.ReadFile(path)
		require.NoError(t, rerr)
		assert.Equal(t, original, after, "the existing keyring must be byte-identical after the refusal")
	})
}

// TestDoctorCmdTrustedKeysOutIsAllowedInsideTheRepository pins the asymmetry
// between the two -out flags, which is deliberate and belongs in a test as
// well as in the help text: the private half is a credential (0600, refused
// inside the repository), while the public half is the material a verifier
// reads (0644, allowed there, so a deployment can keep its configuration as
// code). The private refusal is asserted beside the public allowance, so a
// future "make them symmetric" change breaks this test instead of a
// deployment.
func TestDoctorCmdTrustedKeysOutIsAllowedInsideTheRepository(t *testing.T) {
	cwd, err := os.Getwd()
	require.NoError(t, err)
	dir, err := os.MkdirTemp(cwd, "doctor-trusted-keys-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	keysPath := filepath.Join(dir, "trusted.keys")
	_, err = executeDoctorCmd(t, "--generate-key", "--trusted-keys-out", keysPath)
	require.NoError(t, err, "a public half may live in a checkout: it is not a credential")

	keyring, err := sign.LoadTrustedKeys(keysPath)
	require.NoError(t, err)
	assert.Len(t, keyring, 1)

	keyPath := filepath.Join(dir, "signing.key")
	out, err := executeDoctorCmd(t, "--generate-key", "--key-out", keyPath)
	require.Error(t, err, "the private half is still refused inside the repository")
	assert.Contains(t, err.Error(), "repository")
	assertNoExportedKeyLine(t, out)

	_, statErr := os.Stat(keyPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "the refused private half must not be created")
}

// TestDoctorHelpAndSetupPageExplainTheKeyPair pins the discoverability half of
// the flag: a first-run operator reading the help -- or the setup page doctor
// writes -- must learn that the pair's second file is the one
// NOTARY_TRUSTED_KEYS_PATH names for verify, replay and report. Without that
// the flag exists but setup still ends with "now find another tool".
func TestDoctorHelpAndSetupPageExplainTheKeyPair(t *testing.T) {
	cmd := newDoctorCmd()

	flag := cmd.Flags().Lookup("trusted-keys-out")
	require.NotNil(t, flag, "the flag that completes the setup must exist")
	assert.Contains(t, flag.Usage, "public", "the flag must say it writes the public half")
	assert.Contains(t, flag.Usage, config.EnvTrustedKeysPath, "the flag must say where that half is read from")
	assert.Contains(t, cmd.Long, "--trusted-keys-out", "the long help must explain the flag pair")

	var page bytes.Buffer
	require.NoError(t, doctor.RenderSetup(nil, &page))
	assert.Contains(t, page.String(), "--trusted-keys-out",
		"the setup page must tell a first-run operator how to produce the key pair")
	assert.Contains(t, page.String(), config.EnvTrustedKeysPath,
		"the setup page must name the variable the public half goes to")

	// The help must carry the corrected signing-key fact as well, and this is
	// where it is held: the long help used to say a missing signing key "is not
	// needed for the read paths", which is false for `export` -- a read path
	// that refuses to start without one because it signs the checkpoint
	// --checkpoint-out may write. A help text naming only the writers
	// (reconcile, proxy) would leave an operator to meet that refusal instead,
	// so the help must name export, and must not carry the false claim.
	assert.Contains(t, cmd.Long, "export is the strict case",
		"the help must name export among the commands that refuse to start without a signing key")
	assert.NotContains(t, cmd.Long, "not needed for the read paths",
		"the help must not claim a signing key is unneeded for reads: export refuses without one")
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
		pos := strings.Index(out, check)
		require.NotEqual(t, -1, pos, "%s must be reported", check)
		assert.Less(t, pos, urlPos, "%s must come before the Mem0 base URL check", check)
	}
	for _, check := range []string{doctor.CheckMem0APIKey, doctor.CheckSensitivityRules, doctor.CheckChain} {
		pos := strings.Index(out, check)
		require.NotEqual(t, -1, pos, "%s must be reported", check)
		assert.Less(t, urlPos, pos, "the Mem0 base URL check must come before %s", check)
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

// ---------------------------------------------------------------------------
// Through the root command
// ---------------------------------------------------------------------------

// executeRootCmd runs `notary <args...>` through newRootCmd -- the real command
// tree, with the root's PersistentPreRunE that the direct runDoctor tests
// bypass -- and returns what it printed and what it returned.
func executeRootCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	buf := &bytes.Buffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

// TestRootDoctorGenerateKeyWritesNoLedger pins the promise `doctor
// --generate-key` makes -- "writes no file by default" -- where the operator
// actually meets it: through the root command, whose pre-run creates the
// ledger file before every subcommand unless that subcommand opts out. Without
// the exemption this fails with NOTARY_DB_PATH (and its parent directory)
// created by a run that generated a key and no ledger.
func TestRootDoctorGenerateKeyWritesNoLedger(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sub", "ledger.db")
	t.Setenv(config.EnvDBPath, dbPath)
	t.Setenv(config.EnvGapLogPath, filepath.Join(dir, "notary-gaps.log"))

	out, err := executeRootCmd(t, "doctor", "--generate-key")
	require.NoError(t, err)

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.Len(t, lines, 1, "the run must print exactly the export line, got %q", out)
	assert.True(t, strings.HasPrefix(lines[0], "export "+config.DefaultSigningKeyEnv+"="),
		"stdout must be the export line, got %q", lines[0])

	_, statErr := os.Stat(dbPath)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "--generate-key must not create the ledger")
	_, statErr = os.Stat(filepath.Dir(dbPath))
	assert.ErrorIs(t, statErr, os.ErrNotExist,
		"--generate-key must not create the ledger's parent directory either")
}

// TestRootDoctorReportsFindingsForAnUnusableLedgerPath pins the other half of
// the exemption: when NOTARY_DB_PATH cannot hold a ledger, the CLI must print
// doctor's findings -- naming what was found and the command that fixes it --
// rather than dying in the root pre-run before doctor ever runs.
func TestRootDoctorReportsFindingsForAnUnusableLedgerPath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o600))
	t.Setenv(config.EnvDBPath, filepath.Join(file, "ledger.db"))
	t.Setenv(config.EnvGapLogPath, filepath.Join(t.TempDir(), "notary-gaps.log"))

	out, err := executeRootCmd(t, "doctor")
	require.Error(t, err, "a broken deployment must exit non-zero")

	assert.Contains(t, out, doctor.CheckLedger,
		"doctor's findings must be printed, not replaced by a pre-run failure")
	assert.Contains(t, out, "cannot be opened", "the finding must say what was found")
	assert.Contains(t, out, "export "+config.EnvDBPath, "the finding must name the fix")
	assert.NotContains(t, out, "checking ledger file",
		"the root pre-run's own error must not replace doctor's diagnosis")
}

// TestRootStillEnsuresTheLedgerForOtherCommands pins that the doctor exemption
// is scoped to doctor: every other subcommand still gets the ledger file
// created ahead of it, which is the root pre-run's whole purpose.
func TestRootStillEnsuresTheLedgerForOtherCommands(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sub", "ledger.db")
	t.Setenv(config.EnvDBPath, dbPath)

	_, err := executeRootCmd(t, "version")
	require.NoError(t, err)

	_, statErr := os.Stat(dbPath)
	assert.NoError(t, statErr, "a non-doctor command must still have the ledger created before it runs")
}
