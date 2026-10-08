package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
)

// newTestServeCmd builds a serve command whose stdout is captured in a plain
// buffer and whose stderr -- where the command prints its startup line from a
// serving goroutine -- is captured in a concurrency-safe buffer. The split is
// deliberate: the command's contract is that stdout stays empty (the dashboard
// speaks HTTP, not stdout), so a shared buffer could not pin that.
func newTestServeCmd(t *testing.T) (*cobra.Command, *bytes.Buffer, *syncBuffer) {
	t.Helper()
	cmd := newServeCmd()
	out := &bytes.Buffer{}
	errOut := &syncBuffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd, out, errOut
}

// serveFixture builds a real on-disk ledger holding one signed, observed
// record, and returns its path. The record's event time is "just now" rather
// than a fixed instant because the console's records view defaults to the last
// 30 days bounded on the event time: a record pinned to a fixed date would
// silently fall out of that window once the date is more than a month behind,
// and the view under test would render an empty table rather than the record.
// The record is signed through the common test signer, exactly as the other
// commands' fixtures are; serve needs no key of its own.
func serveFixture(t *testing.T) string {
	t.Helper()
	sg, _ := newVerifySigner(t)
	rec := validCmdRecord(t, "rec-0001")
	rec.At = time.Now().UTC()
	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	appendFixtureRecords(t, dbPath, sg, rec)
	return dbPath
}

// serveURLPattern matches the dashboard URL the command prints to stderr. It
// pins the literal loopback address: the command binds 127.0.0.1 and nothing
// else, so the URL a reader copies is the interface the server actually
// listens on.
var serveURLPattern = regexp.MustCompile(`http://127\.0\.0\.1:\d+`)

// waitForServeURL polls buf for the printed dashboard URL and returns it. It
// fails -- naming what stderr did show -- if the line never appears, so a
// command that never started is reported rather than hanging.
func waitForServeURL(t *testing.T, buf *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if u := serveURLPattern.FindString(buf.String()); u != "" {
			return u
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("serve never printed a dashboard URL; stderr:\n%s", buf.String())
	return ""
}

// TestServeFlagsAreExactlyPort pins the command's surface in the strictest
// direction: --port is the ONLY flag. A set comparison -- not a presence check
// -- is what makes a later --host, --addr or --include-sensitive a failing
// test rather than a silent addition: the bind address is deliberately not
// configurable in this version and the reveal default is not configurable at
// all, so any second flag is a design change that must be argued here.
func TestServeFlagsAreExactlyPort(t *testing.T) {
	cmd := newServeCmd()

	got := map[string]struct{}{}
	cmd.Flags().VisitAll(func(f *pflag.Flag) { got[f.Name] = struct{}{} })

	assert.Equal(t, map[string]struct{}{"port": {}}, got,
		"--port is the only flag serve may have; a --host, --addr or "+
			"--include-sensitive must not be addable without failing this test")
}

// TestServePortDefaultsTo4317 pins the default so a change to it is a visible
// test edit rather than a silent one.
func TestServePortDefaultsTo4317(t *testing.T) {
	cmd := newServeCmd()

	port, err := cmd.Flags().GetInt("port")
	require.NoError(t, err)
	assert.Equal(t, 4317, port, "serve's --port must default to 4317")
}

// TestServeRejectsAPortThatIsNotANumber pins that an Int flag refuses a
// non-numeric value at the command boundary, naming the flag, rather than
// coercing it to zero -- which would silently bind an arbitrary ephemeral port.
func TestServeRejectsAPortThatIsNotANumber(t *testing.T) {
	cmd := newServeCmd()

	err := cmd.ParseFlags([]string{"--port", "not-a-number"})
	require.Error(t, err, "a non-numeric --port must be refused")
	assert.Contains(t, err.Error(), "port", "the refusal must name the flag")
}

// TestServeCmdStartsTheDashboardOnALoopbackPort is the command's start-to-stop
// contract, pinned against real behaviour rather than a mock. It runs runServe
// on --port 0 (so the OS picks a free port), polls stderr for the printed URL
// -- which pins the startup line's format AND the loopback address -- fetches
// it over HTTP, asserts a 200 whose body carries the observed record, then
// cancels the context and asserts runServe returns nil. The started server is
// the real one, so a command that never actually served would fail the GET.
func TestServeCmdStartsTheDashboardOnALoopbackPort(t *testing.T) {
	dbPath := serveFixture(t)

	cmd, out, errOut := newTestServeCmd(t)
	require.NoError(t, cmd.Flags().Set("port", "0"))

	ctx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(ctx)

	ch := make(chan error, 1)
	go func() { ch <- runServe(cmd, &config.Config{DBPath: dbPath}) }()

	url := waitForServeURL(t, errOut)
	assert.Contains(t, errOut.String(), "notary serve: dashboard on ",
		"the startup line must keep its documented shape")
	assert.Contains(t, errOut.String(), "(read-only; Ctrl-C to stop)",
		"the startup line must say the console is read-only and how to stop it")

	resp, err := http.Get(url)
	require.NoError(t, err, "the printed URL must answer")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	_ = resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, string(body), "Observed",
		"the records view must render the observed record's tier")

	cancel()
	select {
	case err := <-ch:
		assert.NoError(t, err, "a cancelled context is a clean stop, not an error")
	case <-time.After(10 * time.Second):
		t.Fatal("runServe did not return after its context was cancelled")
	}

	assert.Empty(t, out.String(), "serve must write nothing to stdout")
}

// TestServeCmdRefusesAnOccupiedPort pins the bind failure's message: it names
// the port sought and the likely cause (the port is already in use), so an
// operator reading the refusal knows what to change.
func TestServeCmdRefusesAnOccupiedPort(t *testing.T) {
	// Occupy a loopback port for the duration of the test.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	dbPath := serveFixture(t)
	cmd, _, _ := newTestServeCmd(t)
	require.NoError(t, cmd.Flags().Set("port", strconv.Itoa(port)))

	err = runServe(cmd, &config.Config{DBPath: dbPath})
	require.Error(t, err, "an occupied port must refuse to start")
	assert.Contains(t, err.Error(), strconv.Itoa(port), "the refusal must name the port")
	assert.Contains(t, err.Error(), "in use", "the refusal must name the likely cause")
}

// TestServeCmdRefusesAnUnreadableKeyring pins one half of the keyring contract:
// a trusted-keys path that is SET but cannot be read is a plain-language
// start-up refusal naming the path, never a silent run with no chain check.
func TestServeCmdRefusesAnUnreadableKeyring(t *testing.T) {
	dbPath := serveFixture(t)
	missing := filepath.Join(t.TempDir(), "missing.keys")

	cmd, _, _ := newTestServeCmd(t)
	require.NoError(t, cmd.Flags().Set("port", "0"))

	err := runServe(cmd, &config.Config{DBPath: dbPath, TrustedKeysPath: missing})
	require.Error(t, err, "a configured keys file that cannot be read must refuse to start")
	assert.Contains(t, err.Error(), missing, "the refusal must name the unreadable file")
}
