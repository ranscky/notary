package reconcile

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/mem0"
)

// repoRoot walks up from the test's working directory until it finds the
// directory that holds go.mod, and returns it. It mirrors
// internal/record/negative_test.go: the throwaway module the fixture is
// compiled in points its replace directive at this checkout, so the test works
// from any checkout. It fails the test if no go.mod is found before the
// filesystem root.
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

// TestNegativeCompleteEnumerationDoesNotCompile proves the residual forgeable path into
// mem0.CompleteEnumeration is closed at compile time. CompleteEnumeration is the
// one token the reconciler trusts to mean "this listing was proven exhaustive";
// only GetAllComplete may set its single unexported field. Go permits an empty
// composite literal of a struct with unexported fields -- so mem0.
// CompleteEnumeration{} always compiles -- but it cannot make naming that field
// legal. The fixture therefore names the unexported field e in a keyed literal
// and this harness proves the compiler REFUSES it, so no caller outside
// internal/mem0 can build an exhaustive-looking listing that would let the
// absence rule conclude a memory was removed from a short or absent read.
//
// The fixture is copied into a throwaway module that resolves notary through a
// replace directive pointing at this checkout, then built. A fixture that
// compiles fails the test, and so does one that fails for the wrong reason (a
// module-resolution or path error rather than the unexported-field error),
// which would otherwise let the harness pass vacuously.
func TestNegativeCompleteEnumerationDoesNotCompile(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("negative fixtures: go tool not found on PATH: %v", err)
	}

	root := repoRoot(t)

	fixtures := []struct {
		file string
		// want are substrings the compiler output must contain. Requiring
		// "unexported field" distinguishes a genuine compile error from a
		// missing-module error, which would also mention "mem0".
		want []string
	}{
		{
			file: "construct_complete_enumeration.go.txt",
			want: []string{"unexported field", "CompleteEnumeration"},
		},
	}

	for _, f := range fixtures {
		f := f
		t.Run(f.file, func(t *testing.T) {
			// The fixture lives beside this test in the package's own
			// testdata directory (Go runs a package's tests with that package
			// directory as the working directory).
			src, err := os.ReadFile(filepath.Join("testdata", "negative", f.file))
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
			// importing the internal package notary/internal/mem0 is
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

// TestNegativeZeroCompleteEnumerationIsInert asserts the RUNTIME half of the same
// invariant, which the type cannot enforce. Go permits mem0.CompleteEnumeration{}
// -- an empty composite literal of a struct with unexported fields -- from ANY
// package, exactly as it permits record.VisibilityTier{} (the design spec
// acknowledges this same limitation). The zero value is therefore the one value
// a caller can always construct, and it must be HARMLESS: Valid reports false
// (only GetAllComplete sets the field a valid enumeration carries), and passing
// it to an absence rule yields an ERROR rather than a claim. The absence rule
// reads removal only from a PROVEN-exhaustive listing, so an invalid
// enumeration is a contract violation to report, never an absence to read.
func TestNegativeZeroCompleteEnumerationIsInert(t *testing.T) {
	var zero mem0.CompleteEnumeration
	require.False(t, zero.Valid(),
		"the zero CompleteEnumeration must be invalid: it was not produced by GetAllComplete")

	// An absence rule is handed the zero value and a known memory it could
	// otherwise claim removed. It must error and emit nothing: if it read the
	// zero value as an empty listing, every memory would look removed.
	rc := New(&fakeReader{}, nil)
	got, err := rc.resolveRemoved(context.Background(), zero, []knownMemory{
		removedKnown("mem-1"),
	})

	require.Error(t, err, "an invalid CompleteEnumeration is a contract violation, not a listing to read absence from")
	assert.Empty(t, got, "no claim may be derived from an invalid enumeration")
	assert.Contains(t, err.Error(), "invalid",
		"the error must name the contract violation rather than look like data")
}
