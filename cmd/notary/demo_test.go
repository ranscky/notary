package main

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// demoTimeout bounds the demo's build-and-run. The script builds the CLI,
// builds and runs the fixture seeder four times, and runs the CLI's read
// commands over a nine-record ledger, so a cold build dominates it; four
// minutes is well above the observed cost (about 10s with a warm build cache,
// where the CLI build is the only substantial step), while still failing a hung
// script inside a bounded time instead of hanging the suite. On expiry
// exec.CommandContext kills the child.
const demoTimeout = 4 * time.Minute

// TestDemoScriptRunsTheWholeSurfaceOffline runs scripts/demo.sh end to end and
// asserts on what it printed. Without this test the demo is a script that used
// to work: it builds the binary, generates its own throwaway key, seeds a
// ledger through the real write paths and exercises verify, gaps, export,
// replay and explain -- and every one of those moves only if the product still
// does what the script says it does.
//
// It asserts the two properties the demo exists to show, on the exact strings
// the failing commands print:
//
//   - the tamper step: `verify` must name the edited record and the field
//     ("hash") that no longer recomputes;
//   - the gap step: `gaps` must report one unreconciled entry, and `verify`
//     must report the same entry as a "gap" break.
//
// It also pins the clean pass that precedes both (`ok: 9 records verified`) and
// the Phase 10 timeline instant (`recorded at 2026-10-01T09:20:00Z`), so a
// demo that printed failures without a working ledger underneath would not
// pass by accident.
//
// The script is repository infrastructure rather than part of the CLI, so a
// machine without bash or a Go toolchain skips this test rather than failing
// it; the demo cannot run there at all. The repository is located from the
// test's own working directory (its package directory), the way
// internal/ledger/crash_test.go reaches ./testdata/crashwriter relative to
// its own: no assumption about where the module is checked out.
func TestDemoScriptRunsTheWholeSurfaceOffline(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("scripts/demo.sh is a bash script; the demo does not run on Windows")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash is not on PATH, so the demo cannot run: %v", err)
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("the go toolchain is not on PATH, so the demo cannot run: %v", err)
	}

	// The test's working directory is this package's directory
	// (<root>/cmd/notary), so the repository root is two levels up -- the same
	// relative hop internal/ledger/crash_test.go makes when it builds
	// ./testdata/crashwriter.
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	script := filepath.Join(root, "scripts", "demo.sh")

	ctx, cancel := context.WithTimeout(context.Background(), demoTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bash, script)
	cmd.Dir = root

	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("scripts/demo.sh timed out after %s without finishing; output so far:\n%s",
			demoTimeout, out)
	}
	require.NoErrorf(t, err, "scripts/demo.sh must exit 0; output:\n%s", out)

	got := string(out)

	// The clean pass, before anything is tampered with: the demo must show a
	// working ledger first, or the failures it goes on to show prove nothing.
	assert.Contains(t, got, "ok: 9 records verified",
		"the demo must verify the freshly seeded ledger before it breaks anything")

	// The read-path surface answered: each command's own success line.
	for _, want := range []string{
		"no outstanding gaps",
		"export: wrote 9 record(s) to stdout",
		"replay: wrote 9 record(s) to stdout",
		"the memory was returned by a search",
		"Memory mem-demo-1",
	} {
		assert.Contains(t, got, want, "the demo must run %q's step", want)
	}

	// The Phase 10 timeline: one line per record carrying both instants, and
	// the Reconstructed claim's write instant a month after its event.
	assert.Contains(t, got, "recorded at 2026-10-01T09:20:00Z",
		"the memory view must show the late claim's write instant")

	// The tamper: verify names the edited record and the exact field.
	assert.Contains(t, got, "record demo-search-1#1 (seq 3): hash — ",
		"the tamper step must make verify name the edited record and its hash field")
	assert.Contains(t, got, "the beta customer prefers SMS at 2am",
		"the demo must show the edited content the tamper actually wrote")

	// The gap: gaps reports it, and verify reports the same entry as a break.
	assert.Contains(t, got, "1 unreconciled gap(s)",
		"the gap step must leave one unreconciled gap for gaps to report")
	assert.Contains(t, got, "(seq 0): gap — gap entry 0 for demo-drop-2#lost-search",
		"verify must cross-check the gap log and name the unaudited span")

	// A script that leaked a failed command substitution or a missing tool
	// still exits 0, so its own output is the only place that shows it.
	assert.NotContains(t, got, "command not found",
		"the demo must not print a shell failure while reporting success")
	assert.NotContains(t, strings.ToLower(got), "no such file or directory",
		"the demo must not print a missing-file error while reporting success")
}
