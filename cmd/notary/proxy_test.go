package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
)

// syncBuffer is a bytes.Buffer safe for concurrent use. The proxy writes its
// listen banner and drop markers to stderr from a serving goroutine while the
// test reads them from its own, so an unsynchronized bytes.Buffer would be a
// data race. It mirrors the plain buffer newTestReconcileCmd set, with a lock.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// newTestProxyCmd builds a proxy command whose output is captured in a
// concurrency-safe buffer, so a test can read the listen banner it prints to
// stderr. It mirrors newTestReconcileCmd.
func newTestProxyCmd(t *testing.T) (*cobra.Command, *syncBuffer) {
	t.Helper()
	cmd := newProxyCmd()
	buf := &syncBuffer{}
	cmd.SetOut(buf)
	cmd.SetErr(buf)
	return cmd, buf
}

// waitForListenAddr polls buf for the proxy's listen banner and returns the
// address it is serving on. It fails the test if the banner never appears, so a
// command that never started is reported with its stderr rather than hanging.
func waitForListenAddr(t *testing.T, buf *syncBuffer) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := listenAddrFrom(buf.String()); addr != "" {
			return addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("proxy never reported a listen address; stderr:\n%s", buf.String())
	return ""
}

// listenAddrFrom extracts the address from the listen banner, so the test
// learns the port of a :0 listener without racing a second bind.
func listenAddrFrom(s string) string {
	const marker = "listening on "
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	j := strings.IndexByte(rest, '\n')
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}

// startTestProxy runs runProxy in a goroutine on a :0 listener and returns the
// address it is serving on, the command, and a channel that yields runProxy's
// result. The returned cancel is the shutdown signal: cancelling it makes
// runProxy drain and return, which is how a test drives a graceful stop.
func startTestProxy(t *testing.T, cfg *config.Config) (addr string, cancel context.CancelFunc, errCh <-chan error) {
	t.Helper()
	cmd, buf := newTestProxyCmd(t)
	require.NoError(t, cmd.Flags().Set("addr", "127.0.0.1:0"))

	ctx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(ctx)

	ch := make(chan error, 1)
	go func() { ch <- runProxy(cmd, cfg) }()

	return waitForListenAddr(t, buf), cancel, ch
}

// awaitProxyStop waits for runProxy to return after a shutdown and asserts it
// returned no error, so a graceful stop is not mistaken for a crash.
func awaitProxyStop(t *testing.T, errCh <-chan error) {
	t.Helper()
	select {
	case err := <-errCh:
		require.NoError(t, err, "a graceful shutdown must not fail")
	case <-time.After(10 * time.Second):
		t.Fatal("runProxy did not return after the shutdown signal")
	}
}

// addUpstream is an httptest server that answers the recorded add endpoint with
// a valid acknowledgement, matching the shape internal/mem0.AddResponse
// decodes, so the proxy observes a real add without a network.
func addUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v3/memories/add/" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"event_id":"evt-1","status":"PENDING"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// Spec §8: a signing key is required, a Mem0 API key is not.
// ---------------------------------------------------------------------------

// TestProxyCmdRefusesToStartWithoutASigningKey pins the config time the proxy
// shares with reconcile and export: it writes SIGNED records, so a missing
// signing key is a hard refusal to start, never a degraded unsigned mode. The
// error must NAME NOTARY_SIGNING_KEY, because the operator's fix is to set that
// variable.
func TestProxyCmdRefusesToStartWithoutASigningKey(t *testing.T) {
	base := &config.Config{
		DBPath:      filepath.Join(t.TempDir(), "ledger.db"),
		Mem0BaseURL: "http://127.0.0.1:0", // never contacted: the key check comes first
	}

	t.Run("no signing key variable configured", func(t *testing.T) {
		cmd, _ := newTestProxyCmd(t)
		cfg := *base
		cfg.SigningKeyEnv = ""
		err := runProxy(cmd, &cfg)
		require.Error(t, err, "no signing key must refuse to start")
		assert.Contains(t, err.Error(), config.DefaultSigningKeyEnv,
			"the error must name NOTARY_SIGNING_KEY")
	})

	t.Run("signing key variable unset", func(t *testing.T) {
		cmd, _ := newTestProxyCmd(t)
		cfg := *base
		cfg.SigningKeyEnv = "NOTARY_PROXY_TEST_UNSET_KEY"
		t.Setenv("NOTARY_PROXY_TEST_UNSET_KEY", "")
		err := runProxy(cmd, &cfg)
		require.Error(t, err, "an unset signing key must refuse to start")
		assert.Contains(t, err.Error(), "NOTARY_PROXY_TEST_UNSET_KEY",
			"the error must name the missing key variable")
	})
}

// TestProxyCmdStartsWithoutAMem0APIKey is the credential-neutrality property:
// the proxy forwards the caller's own Authorization header and holds no
// credential of its own, so an empty cfg.Mem0APIKey must NOT refuse to start.
// Refusing here would be a real bug. The successful forwarded response proves
// the listener is up and serving, not merely that a flag was set.
func TestProxyCmdStartsWithoutAMem0APIKey(t *testing.T) {
	sg, _ := newVerifySigner(t) // sets the signing-key environment variable
	require.NotNil(t, sg)

	upstream := addUpstream(t)

	cfg := &config.Config{
		DBPath:        filepath.Join(t.TempDir(), "ledger.db"),
		GapLogPath:    filepath.Join(t.TempDir(), "gaps.log"),
		SigningKeyEnv: verifyKeyEnv,
		Mem0BaseURL:   upstream.URL,
		Mem0APIKey:    "", // deliberately empty: the proxy is credential-neutral
	}

	addr, cancel, errCh := startTestProxy(t, cfg)

	// An unobserved path forwards to the upstream, so a 200 proves the real
	// listener answered rather than that start-up merely returned.
	resp, err := http.Get("http://" + addr + "/health")
	require.NoError(t, err, "the listener must answer")
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	cancel()
	awaitProxyStop(t, errCh)
}

// ---------------------------------------------------------------------------
// The command surface: exactly the three flags the design asks for.
// ---------------------------------------------------------------------------

// TestProxyCmdRegistersTheThreeFlagsAndNoOthers pins the command's surface: it
// serves traffic and writes, so it takes --addr, --queue-depth and --max-body
// and nothing from the read commands (--phrase, --checkpoint-out, --from, --to)
// or the scoped passes (the four scope flags). The defaults are pinned too, so
// a change to them is a visible test edit rather than a silent one.
func TestProxyCmdRegistersTheThreeFlagsAndNoOthers(t *testing.T) {
	cmd := newProxyCmd()

	for _, name := range []string{"addr", "queue-depth", "max-body"} {
		require.NotNil(t, cmd.Flags().Lookup(name), "flag --%s must be registered", name)
	}
	for _, name := range []string{
		"phrase", "checkpoint-out", "from", "to",
		"user-id", "agent-id", "app-id", "run-id",
	} {
		assert.Nil(t, cmd.Flags().Lookup(name), "flag --%s must not be registered", name)
	}

	assert.Equal(t, "127.0.0.1:8080", cmd.Flags().Lookup("addr").DefValue)
	assert.Equal(t, "1024", cmd.Flags().Lookup("queue-depth").DefValue)
	assert.Equal(t, "8MiB", cmd.Flags().Lookup("max-body").DefValue,
		"--max-body must read as a human size, not a raw byte count")
}

// TestProxyMaxBodyFlagParsesHumanSizes pins the byte-size flag's two accepted
// spellings: a binary suffix ("4MiB") and a raw count ("4194304"). A typo'd or
// negative size must be refused rather than silently coerced.
func TestProxyMaxBodyFlagParsesHumanSizes(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"8MiB", 8 << 20},
		{"4MiB", 4 << 20},
		{"512KiB", 512 << 10},
		{"1048576", 1 << 20},
		{"2GB", 2 << 30},
	} {
		cmd := newProxyCmd()
		require.NoError(t, cmd.Flags().Set("max-body", tc.in), "setting --max-body %s", tc.in)
		got, err := maxBodyBytes(cmd)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got, "--max-body %s", tc.in)
	}

	for _, bad := range []string{"", "8XiB", "-1MiB", "MiB"} {
		cmd := newProxyCmd()
		assert.Error(t, cmd.Flags().Set("max-body", bad), "--max-body %q must be refused", bad)
	}
}

// ---------------------------------------------------------------------------
// Review Focus: Close racing with in-flight enqueues; the shutdown drains.
// ---------------------------------------------------------------------------

// TestProxyCmdShutdownDrainsThePipeline is the command-level proof of the
// shutdown contract. A record accepted before the shutdown signal must be in
// the ledger after runProxy returns, not merely counted or flagged. The add is
// enqueued into the pipeline before its response reaches the caller, so a
// record exists in the queue when we stop; h.Close -- the command's whole
// shutdown path -- must drain it, or the queue is lost. Asserting the ledger's
// row count is what makes this non-vacuous: a flag would prove nothing.
func TestProxyCmdShutdownDrainsThePipeline(t *testing.T) {
	sg, _ := newVerifySigner(t)
	require.NotNil(t, sg)

	dbPath := filepath.Join(t.TempDir(), "ledger.db")
	upstream := addUpstream(t)

	cfg := &config.Config{
		DBPath:        dbPath,
		GapLogPath:    filepath.Join(t.TempDir(), "gaps.log"),
		SigningKeyEnv: verifyKeyEnv,
		Mem0BaseURL:   upstream.URL,
	}

	addr, cancel, errCh := startTestProxy(t, cfg)

	body := `{"messages":[{"role":"user","content":"hello"}],"user_id":"u1"}`
	resp, err := http.Post("http://"+addr+"/v3/memories/add/", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	// The add's record is enqueued from inside the proxy's ModifyResponse,
	// before the response is written to this caller, so by the time Post
	// returns the queue holds it. Shutting down now must drain it.
	cancel()
	awaitProxyStop(t, errCh)

	_, ok, rows := ledgerSnapshot(t, dbPath)
	require.True(t, ok, "the ledger must hold the record accepted before shutdown")
	assert.Equal(t, 1, rows,
		"the add accepted before shutdown must be drained into the ledger, not lost with the queue")
}
