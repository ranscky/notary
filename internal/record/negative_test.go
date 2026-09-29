package record_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot walks up from the test's working directory until it finds the
// directory that holds go.mod, and returns it. Walking up rather than
// hardcoding an absolute path keeps the test working from any checkout. It
// fails the test if no go.mod is found before the filesystem root.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("negative fixtures: getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("negative fixtures: no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// TestNegativeFixturesDoNotCompile proves the residual forgeable paths into
// record's value types are closed. Go permits an empty composite literal of a
// struct with unexported fields and cannot make the zero value a compile error,
// so the remaining paths -- naming an unexported field in a keyed literal, and
// building an ObservedEvidence without its constructor -- are proven closed by
// compiling snippets that MUST fail. The genesis anchor is pinned the same way:
// a snippet that assigns to record.GenesisHash must not compile, so no caller
// can redefine where a chain starts.
//
// Each fixture is copied into a throwaway module that resolves notary through a
// replace directive pointing at this checkout, then built. A fixture that
// compiles fails the test. So does one that fails for the wrong reason (a
// module-resolution or path error rather than a compile error about the
// unexported field), which would otherwise let the harness pass vacuously.
func TestNegativeFixturesDoNotCompile(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("negative fixtures: go tool not found on PATH: %v", err)
	}

	root := repoRoot(t)

	fixtures := []struct {
		file string
		// want are substrings the compiler output must contain. Requiring
		// "unexported field" distinguishes a genuine compile error from a
		// missing-module error, which would also mention "notary/internal/record".
		want []string
	}{
		{file: "forge_tier.go.txt", want: []string{"unexported field", "VisibilityTier"}},
		{file: "bare_reason.go.txt", want: []string{"unexported field", "Reason"}},
		{file: "observed_from_nothing.go.txt", want: []string{"unexported field", "ObservedEvidence"}},
		// GenesisHash is a function, not a variable, so no caller can reassign
		// the genesis anchor. The expected message is the assignment error, not
		// a missing-module or unexported-field one, so this fixture fails for
		// the right reason.
		{file: "redefine_genesis.go.txt", want: []string{"cannot assign to record.GenesisHash"}},
	}

	for _, f := range fixtures {
		f := f
		t.Run(f.file, func(t *testing.T) {
			src, err := os.ReadFile(filepath.Join(root, "testdata", "negative", f.file))
			if err != nil {
				t.Fatalf("negative fixtures: read %s: %v", f.file, err)
			}

			dir := t.TempDir()
			pkgDir := filepath.Join(dir, "pkg")
			if err := os.MkdirAll(pkgDir, 0o755); err != nil {
				t.Fatalf("negative fixtures: mkdir pkg: %v", err)
			}
			if err := os.WriteFile(filepath.Join(pkgDir, "fixture.go"), src, 0o644); err != nil {
				t.Fatalf("negative fixtures: write fixture.go: %v", err)
			}

			// The throwaway module must be named under notary/ so that
			// importing the internal package notary/internal/record is
			// permitted by the internal-package rule.
			goMod := "module notary/fixtureprobe\n\n" +
				"go 1.25.0\n\n" +
				"require notary v0.0.0\n\n" +
				"replace notary => " + filepath.ToSlash(root) + "\n"
			if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goMod), 0o644); err != nil {
				t.Fatalf("negative fixtures: write go.mod: %v", err)
			}

			cmd := exec.Command("go", "build", "./...")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "GOWORK=off")
			out, err := cmd.CombinedOutput()

			if err == nil {
				t.Fatalf("negative fixtures: %s compiled, but it must not\ncompiler output:\n%s", f.file, out)
			}

			got := string(out)
			for _, want := range f.want {
				if !strings.Contains(got, want) {
					t.Fatalf("negative fixtures: %s failed for the wrong reason: output does not contain %q\ncompiler output:\n%s", f.file, want, got)
				}
			}
		})
	}
}
