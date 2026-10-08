package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/doctor"
	"notary/internal/interceptor/proxy"
)

// keyFilePerm is the only mode sign.KeySourceFile's loader accepts: it
// compares the key file's permissions with 0600 exactly and fails with
// sign.ErrLoosePerms otherwise (internal/sign/sign.go). --key-out must write
// what that loader reads, or doctor would produce a key its own commands
// refuse to load.
const keyFilePerm os.FileMode = 0o600

// trustedKeysFilePerm is the mode --trusted-keys-out writes with: 0644, not
// 0600. The public half is not a credential -- it is the material every
// verifier holds, meant to be readable by whatever checks a signature -- so it
// takes the mode a configuration file takes. That, and being allowed inside a
// checkout, is the deliberate asymmetry with --key-out's private half; the
// reasoning lives on writeTrustedKeysFile.
const trustedKeysFilePerm os.FileMode = 0o644

// setupPageFilename is the page --out writes: a fixed name, so the operator
// never has to guess what doctor produced in the directory it was handed.
const setupPageFilename = "setup.html"

// newDoctorCmd builds the `notary doctor` subcommand: the command that says
// what is wrong with this deployment's setup, and how to fix it.
//
// It computes its findings once (internal/doctor), prints them, and exits
// non-zero when any of them is an error, so it can gate a pipeline; warnings
// alone do not. --out DIR additionally writes the same findings to
// DIR/setup.html.
//
// --generate-key is the second half of the command and the only command in
// this repository that creates key material: no other command, and no other
// flag combination, ever generates a key (the demo script mints its own
// throwaway key, from /dev/urandom, and writes it to no file). It prints an
// `export NOTARY_SIGNING_KEY=...` line to stdout and writes no file;
// --key-out PATH writes the PRIVATE half to PATH instead, at mode 0600 and
// outside the repository; and --trusted-keys-out PATH writes the PUBLIC half --
// the one base64 line NOTARY_TRUSTED_KEYS_PATH names for verify, replay and
// report -- at mode 0644, allowed inside a checkout, because a public half is
// not a credential. It is explicit because silence is the safety property.
//
// It needs no signing key and no Mem0 key to run: it is a read-only check, and
// reporting an absent key is one of the things it does.
func newDoctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check the environment and generate a signing key pair",
		Long: "Doctor checks that this deployment is set up correctly. It opens the ledger\n" +
			"and the gap log (the latter for append, not merely to stat it), loads the\n" +
			"trusted keyring and the signing key, validates the Mem0 base URL and the\n" +
			"presence of a Mem0 key, parses the sensitivity rules when configured, and\n" +
			"checks the ledger's chain state through the same break collection `notary\n" +
			"verify` uses. Each finding says what was found and the exact command that\n" +
			"fixes it.\n" +
			"\n" +
			"It exits non-zero when any finding is an error, so it can gate a pipeline;\n" +
			"warnings alone do not. A signing key that is absent is a warning, not an\n" +
			"error: most read paths need none, but the commands that sign something\n" +
			"refuse to start without one -- reconcile and proxy sign the records they\n" +
			"write, and export is the strict case, signing the checkpoint\n" +
			"--checkpoint-out may write and refusing without a key even when that flag\n" +
			"was not given. The chain is reported as NOT\n" +
			"checked -- never as broken -- when no trusted keys are configured, because a\n" +
			"verifier with no trusted keys would report every record as a signature\n" +
			"break.\n" +
			"\n" +
			"--out DIR writes the same findings to DIR/setup.html, the setup page.\n" +
			"\n" +
			"--generate-key creates a fresh base64 ed25519 key pair and prints the\n" +
			"PRIVATE half as an export NOTARY_SIGNING_KEY=... line to stdout, writing no\n" +
			"file: it is the only command in this repository that creates key material\n" +
			"(the demo script mints its own throwaway key, and writes it to no file), and\n" +
			"it does so only when asked. --key-out PATH writes the private half to PATH\n" +
			"(mode 0600, refused inside the repository) instead of printing it.\n" +
			"\n" +
			"--trusted-keys-out PATH, with --generate-key, writes the pair's PUBLIC half\n" +
			"to PATH as the one base64 line NOTARY_TRUSTED_KEYS_PATH names for verify,\n" +
			"replay and report, at mode 0644. It requires --generate-key because a loaded\n" +
			"signer exposes no public key to recover, only its KeyID fingerprint and its\n" +
			"Sign method, so the public half can only come from a seed doctor has just\n" +
			"generated for it. Unlike --key-out it is allowed inside a checkout: a public\n" +
			"half is not a secret, and a deployment may want its configuration as code --\n" +
			"though it is normally deployment configuration and belongs outside the\n" +
			"checkout. The private half never appears in a finding or in the setup page.",
		Args: cobra.NoArgs,
		// The root command's PersistentPreRunE creates the ledger file before
		// every subcommand runs. Doctor opts out (root.go's
		// skipEnsureLedgerAnnotation): creating the ledger is the very thing
		// its ledger check reports, so doing it first would hide the failure it
		// exists to name and would make `doctor --generate-key` -- which
		// promises to write no file -- write one.
		Annotations: map[string]string{skipEnsureLedgerAnnotation: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The environment is read once, into a map, and both the
			// configuration and the checks are derived from it -- the way
			// config.LoadFrom is already written -- so the two can never
			// disagree about what the environment says.
			env := environMap()
			cfg, err := config.LoadFrom(env)
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			return runDoctor(cmd, cfg, env)
		},
	}

	// A bound variable would be a second source of truth for a flag's value;
	// every flag is declared here and read back through cmd.Flags() in
	// runDoctor, so there is no package- or closure-level mutable state.
	cmd.Flags().String("out", "",
		"write the setup page to setup.html in this directory")
	cmd.Flags().Bool("generate-key", false,
		"generate a fresh ed25519 key pair and print the private half as an export NOTARY_SIGNING_KEY=... line (see --key-out and --trusted-keys-out for the halves as files)")
	cmd.Flags().String("key-out", "",
		"with --generate-key, write the private half to this path (mode 0600, refused inside the repository) instead of printing it")
	cmd.Flags().String("trusted-keys-out", "",
		"with --generate-key, write the public half to this path (mode 0644, allowed inside the repository) for NOTARY_TRUSTED_KEYS_PATH to read")
	return cmd
}

// environMap converts os.Environ() into a map, keeping the last value for any
// duplicated key. It mirrors config's unexported environ, so the command can
// hand the checks the same environment it handed config.LoadFrom.
func environMap() map[string]string {
	pairs := os.Environ()
	env := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	return env
}

// runDoctor is the command's body, separated from the cobra wiring so tests
// can drive it with an explicit configuration, environment and clock-free
// dependencies, the way runVerify and runReconcile are tested. It returns a
// non-nil error -- and so a non-zero exit -- when any finding is an error, and
// also when --out cannot be written.
//
// It prints every finding (message and fix), the severity counts, and -- when
// --out was given -- a line naming the setup page it wrote; the page carries
// the same findings and no key material.
func runDoctor(cmd *cobra.Command, cfg *config.Config, env map[string]string) error {
	out := cmd.OutOrStdout()

	generateKey, err := cmd.Flags().GetBool("generate-key")
	if err != nil {
		return fmt.Errorf("reading --generate-key: %w", err)
	}
	keyOut, err := cmd.Flags().GetString("key-out")
	if err != nil {
		return fmt.Errorf("reading --key-out: %w", err)
	}
	outDir, err := cmd.Flags().GetString("out")
	if err != nil {
		return fmt.Errorf("reading --out: %w", err)
	}
	trustedKeysOut, err := cmd.Flags().GetString("trusted-keys-out")
	if err != nil {
		return fmt.Errorf("reading --trusted-keys-out: %w", err)
	}

	// --generate-key is an action, not a diagnosis: it creates key material
	// and prints or writes it, and it runs no check and opens nothing (spec
	// section 4 -- it writes no file but the one it is asked for).
	if generateKey {
		// --out promises a page of findings, and generating a key produces
		// none. Honouring one of the two flags silently is not something this
		// project does, so the combination is refused rather than half-run.
		if outDir != "" {
			return fmt.Errorf(
				"--out %s cannot be combined with --generate-key: generating a key runs no checks, so no setup page is written; "+
					"run `notary doctor --out %s` without --generate-key to write the page", outDir, outDir)
		}
		return generateSigningKey(out, keyOut, trustedKeysOut)
	}
	// The flag pair is deliberately not silently tolerated: --key-out alone
	// would look like it had written a key somewhere, and a diagnostic must
	// never imply an action it did not take.
	if keyOut != "" {
		return fmt.Errorf(
			"--key-out %s was given without --generate-key: no key is ever written silently; "+
				"add --generate-key to generate one, or drop --key-out", keyOut)
	}
	// --trusted-keys-out carries the same rule for its own reason: it writes
	// the public half of a key THIS run generated. A loaded signer exposes no
	// public key to recover -- sign.Signer has KeyID() and Sign, nothing else --
	// so doctor will not pretend it can derive one.
	if trustedKeysOut != "" {
		return fmt.Errorf(
			"--trusted-keys-out %s was given without --generate-key: the public half cannot be recovered from a loaded "+
				"signing key (sign.Signer exposes only its KeyID fingerprint and Sign), so doctor writes it only for a key pair "+
				"it has just generated; add --generate-key, or point %s at a keyring you already hold",
			trustedKeysOut, config.EnvTrustedKeysPath)
	}

	findings := doctor.Diagnose(cfg, env, time.Now())

	// The Mem0 base URL is checked HERE, not in internal/doctor, because its
	// validator is parseUpstream, which lives in this package: internal/doctor
	// cannot import package main, and the brief's "reuse its validation" is
	// literal -- writing a second URL validator is exactly what must not
	// happen. The finding is slotted in where the spec's check list puts it.
	findings = insertFindingAfter(findings, doctor.CheckSigningKey, checkMem0BaseURL(cfg))

	var oks, warns, errs int
	for _, f := range findings {
		printFinding(out, f)
		switch f.Severity {
		case doctor.Err:
			errs++
		case doctor.Warn:
			warns++
		default:
			oks++
		}
	}
	fmt.Fprintf(out, "doctor: %d ok, %d warning(s), %d error(s)\n", oks, warns, errs)

	// The page is written even when there are errors -- it is the artefact the
	// operator takes to the machine that needs fixing. A failure to write it
	// is its own error, and hides nothing.
	if outDir != "" {
		path := filepath.Join(outDir, setupPageFilename)
		if err := writeSetupPage(path, findings); err != nil {
			return err
		}
		fmt.Fprintf(out, "doctor: wrote the setup page to %s\n", path)
	}

	if errs > 0 {
		return fmt.Errorf("doctor: %d check(s) reported an error; fix the finding(s) above and re-run", errs)
	}
	return nil
}

// printFinding writes one finding as the operator reads it: a line with the
// severity, the check and the message, and under it the fix, indented, when
// there is one. Neither field can contain key material (doctor.Finding's
// contract), so nothing printed here needs redacting.
func printFinding(out io.Writer, f doctor.Finding) {
	fmt.Fprintf(out, "%-5s %-18s %s\n", f.Severity, f.Check, f.Message)
	if f.Fix != "" {
		fmt.Fprintf(out, "%-5s %-18s fix: %s\n", "", "", f.Fix)
	}
}

// checkMem0BaseURL validates cfg.Mem0BaseURL with parseUpstream, the
// project's one URL validator, and reports its answer as a finding. The
// successful message names the value through proxy.RedactedURL (scheme and
// host only), and a failure carries parseUpstream's own error text -- which
// already names NOTARY_MEM0_BASE_URL and never repeats a value that may hold
// a credential.
func checkMem0BaseURL(cfg *config.Config) doctor.Finding {
	if _, err := parseUpstream(cfg.Mem0BaseURL); err != nil {
		return doctor.Finding{
			Check:    doctor.CheckMem0BaseURL,
			Severity: doctor.Err,
			Message:  err.Error(),
			Fix:      "export " + config.EnvMem0BaseURL + "=http://mem0.internal:8081",
		}
	}
	return doctor.Finding{
		Check:    doctor.CheckMem0BaseURL,
		Severity: doctor.OK,
		Message: "the Mem0 base URL " + proxy.RedactedURL(cfg.Mem0BaseURL) +
			" is an absolute http(s) URL with a host",
	}
}

// insertFindingAfter returns findings with f inserted directly after the
// finding for check, so the report keeps the design's check order even though
// this one finding is computed here. It appends when no finding for check
// exists, because losing a finding is worse than an odd-looking order.
func insertFindingAfter(findings []doctor.Finding, check string, f doctor.Finding) []doctor.Finding {
	for i, existing := range findings {
		if existing.Check != check {
			continue
		}
		findings = append(findings, doctor.Finding{})
		copy(findings[i+2:], findings[i+1:])
		findings[i+1] = f
		return findings
	}
	return append(findings, f)
}

// writeSetupPage renders the same findings the command printed into path.
// The caller passes the full path (--out's directory joined with the page's
// fixed name), so the written file is always named on the operator's command
// line's terms. The page never contains key material (doctor.RenderSetup's
// contract).
func writeSetupPage(path string, findings []doctor.Finding) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating the setup page directory %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("writing the setup page %s: %w", path, err)
	}
	if rerr := doctor.RenderSetup(findings, f); rerr != nil {
		_ = f.Close()
		return fmt.Errorf("rendering the setup page %s: %w", path, rerr)
	}
	if cerr := f.Close(); cerr != nil {
		return fmt.Errorf("closing the setup page %s: %w", path, cerr)
	}
	return nil
}

// generateSigningKey creates a fresh ed25519 key pair and emits it: the seed
// (32 random bytes, base64-encoded -- the form sign.NewSigner accepts and the
// `export` line a shell can evaluate) as the PRIVATE half, and the public key
// derived from that same seed as the trusted-keys line when trustedKeysOut
// asks for it.
//
// This is the repository's only generator of key material, and it runs only
// when an operator asks for it by name: silence is the safety property (design
// section 4), so nothing here is a side effect of any other flag or command.
//
// The private half is emitted first, so a failure to write the public file is
// reported only after the operator already holds the private half; re-running
// makes a new pair, and nothing here can leave a public file whose private
// half was never emitted.
func generateSigningKey(out io.Writer, keyOut, trustedKeysOut string) error {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return fmt.Errorf("generating a signing key: %w", err)
	}
	material := base64.StdEncoding.EncodeToString(seed)

	if keyOut != "" {
		if err := writeSigningKeyFile(out, keyOut, material); err != nil {
			return err
		}
	} else {
		// The default: the private half goes to stdout, and nothing is written
		// for it. A caller can capture the line however it likes -- an env
		// file, a secret manager, a subshell -- and the command keeps no copy.
		fmt.Fprintf(out, "export %s=%s\n", config.DefaultSigningKeyEnv, material)
	}

	if trustedKeysOut == "" {
		return nil
	}

	// The public half is derived from the seed THIS run generated, by the
	// ed25519 construction every Go program shares, and never guessed from a
	// loaded signer -- sign.Signer exposes only KeyID() (a one-way SHA-256
	// fingerprint) and Sign. Checked, not asserted: doctor never panics.
	public, ok := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("generating a signing key: the seed did not yield an ed25519 public key")
	}
	return writeTrustedKeysFile(out, trustedKeysOut, base64.StdEncoding.EncodeToString(public))
}

// writeSigningKeyFile writes material to path satisfying the loader's own
// rules, copied rather than restated: sign.KeySourceFile reads a key file that
// "must be mode 0600 and live outside the repository" (internal/sign/sign.go),
// and its loader fails with sign.ErrLoosePerms when the mode is anything else.
// So this writes 0600 and refuses a path inside the repository -- otherwise
// doctor would write a key its own commands refuse to load, which is worse
// than refusing to write one at all.
//
// The file is created O_EXCL: silently replacing existing key material would
// destroy the only copy of a signing identity, so an existing path is an
// error, not a clobber. The mode is chmod'ed after creation because the umask
// applies to the open and a restrictive one could clear bits from the 0600 the
// loader demands exactly.
func writeSigningKeyFile(out io.Writer, path, material string) error {
	if err := refuseKeyInsideRepository(path); err != nil {
		return err
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, keyFilePerm)
	if err != nil {
		return fmt.Errorf("writing the signing key to %s: %w", path, err)
	}
	werr := writeKeyContents(f, material, keyFilePerm)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		// Never leave a partial key behind. The file was created by this call
		// (O_EXCL), so removing it removes only what this call made.
		_ = os.Remove(path)
		return fmt.Errorf("writing the signing key to %s: %w", path, werr)
	}

	fmt.Fprintf(out, "wrote a fresh ed25519 signing key to %s (mode %04o)\n", path, keyFilePerm)
	return nil
}

// writeTrustedKeysFile writes the base64 public half to path as one line --
// the exact format sign.LoadTrustedKeys reads, one base64 ed25519 public key
// per line (internal/sign/keys.go) -- at mode 0644.
//
// The mode, and the absence of the repository refusal writeSigningKeyFile
// applies, are the deliberate asymmetry between the two -out flags, and this
// is where the reason lives: the public half is NOT a credential. It is the
// material a verifier holds; it is meant to be readable by whatever checks a
// signature, and the guardrail covers secrets and signing keys, which this is
// neither of. So a deployment may keep it in a checkout as configuration as
// code -- though the help text says it is normally deployment configuration and
// belongs outside one.
//
// O_EXCL still applies: an existing keyring is never replaced silently, since
// that would drop every other key in it. The mode is chmod'ed after creation
// because the umask applies to the open, and a restrictive one could leave the
// file narrower than the 0644 the spec fixes -- which would defeat the point
// of a public file.
func writeTrustedKeysFile(out io.Writer, path, publicHalf string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, trustedKeysFilePerm)
	if err != nil {
		return fmt.Errorf("writing the trusted keys to %s: %w", path, err)
	}
	werr := writeKeyContents(f, publicHalf, trustedKeysFilePerm)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		// Never leave a partial line behind. The file was created by this call
		// (O_EXCL), so removing it removes only what this call made.
		_ = os.Remove(path)
		return fmt.Errorf("writing the trusted keys to %s: %w", path, werr)
	}

	fmt.Fprintf(out,
		"wrote the trusted keys (the public half) to %s (mode %04o); point %s at it for verify, replay and report\n",
		path, trustedKeysFilePerm, config.EnvTrustedKeysPath)
	return nil
}

// writeKeyContents writes the base64 material as one line and pins the file's
// mode to perm with fchmod, because the umask applies to the open and a
// restrictive one could clear bits from the mode the caller needs: 0600
// exactly for the private half, which sign.KeySourceFile accepts at no other
// mode, and the 0644 the spec fixes for the public half. The trailing newline
// is cosmetic -- both loaders trim whitespace -- and keeps the file a
// well-formed text file.
func writeKeyContents(f *os.File, material string, perm os.FileMode) error {
	if _, err := f.WriteString(material + "\n"); err != nil {
		return err
	}
	if err := f.Chmod(perm); err != nil {
		return err
	}
	return nil
}

// refuseKeyInsideRepository refuses a --key-out path inside a repository this
// process can see. Two repositories count, and the target is refused when it
// lies in EITHER:
//
//   - the checkout the process runs from -- the working directory's nearest
//     ancestor holding a .git entry -- which catches a relative path and an
//     absolute path given from inside the checkout; and
//   - the checkout the TARGET lives in -- the target directory's nearest
//     ancestor holding a .git entry -- which catches an absolute path into a
//     checkout from somewhere else. Running doctor from /tmp must not make
//     this repository writable key storage.
//
// When neither applies there is no repository to write into, so the path is
// allowed. Paths are compared after filepath.Abs, as given: a path that
// reaches a checkout through a symlink is not chased, because the guard is
// against the ordinary mistake -- writing a key next to the code -- and a
// symlink into the repository is a deliberate act, not a slip.
func refuseKeyInsideRepository(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("refusing to write key material to %s: cannot resolve the path: %w", path, err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		// Without a working directory there is no way to tell whether the
		// target is inside the checkout this process runs from; refuse rather
		// than risk writing into one.
		return fmt.Errorf("refusing to write key material to %s: cannot determine the working directory: %w", abs, err)
	}
	if root, ok := repositoryRoot(cwd); ok && pathWithin(root, abs) {
		return keyInRepositoryError(abs, root)
	}
	if root, ok := repositoryRoot(filepath.Dir(abs)); ok && pathWithin(root, abs) {
		return keyInRepositoryError(abs, root)
	}
	return nil
}

// keyInRepositoryError is the refusal both repository checks return: it names
// the path, the checkout it is inside, and the guardrail it would break.
func keyInRepositoryError(abs, root string) error {
	return fmt.Errorf(
		"refusing to write key material to %s: it is inside the repository at %s, and no key material "+
			"lives in the repository, ever -- write it outside the checkout, for example --key-out ~/.notary/signing.key",
		abs, root)
}

// repositoryRoot returns the nearest ancestor of dir (including dir) that
// holds a .git entry, and whether one was found. A .git entry is a directory
// in an ordinary checkout and a file in a worktree or submodule, so both
// count.
func repositoryRoot(dir string) (string, bool) {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// pathWithin reports whether path is root itself or lies under it. It uses
// filepath.Rel rather than a string prefix, so a sibling named like a prefix
// of root ("/repo-other" against "/repo") is not mistaken for a child.
func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
