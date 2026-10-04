package main

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
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

// holdWriteLock takes SQLite's write lock on the ledger at dbPath from a second
// connection (BEGIN IMMEDIATE) and returns a function that releases it. While
// the lock is held the proxy's single ledger writer blocks on its append, so a
// record the proxy accepted sits in the pipeline instead of being written
// immediately. It is how the shutdown test forces a record to be genuinely
// queued at Close time rather than relying on the writer happening to be slow
// on the day.
//
// The DSN mirrors the store's own (internal/store): _busy_timeout makes the
// proxy's writer wait for the lock rather than error out at once, and
// _txlock=immediate makes Begin take the write lock up front. The lock is also
// released by t.Cleanup, and release is idempotent, so a failure cannot leak it.
func holdWriteLock(t *testing.T, dbPath string) func() {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000&_txlock=immediate")
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	tx, err := db.Begin()
	require.NoError(t, err, "taking the ledger write lock")

	var once sync.Once
	release := func() {
		once.Do(func() {
			_ = tx.Rollback()
			_ = db.Close()
		})
	}
	t.Cleanup(release)
	return release
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

// TestProxyMaxBodyFlagParsesHumanSizes pins the byte-size flag's accepted
// spellings: a binary suffix ("4MiB") and a raw count ("4194304"). A typo'd,
// negative, or overflowing size must be refused rather than silently coerced.
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

	for _, bad := range []string{"", "8XiB", "-1MiB", "MiB", "9223372036854775807MiB"} {
		cmd := newProxyCmd()
		assert.Error(t, cmd.Flags().Set("max-body", bad), "--max-body %q must be refused", bad)
	}

	// An integer part that parses but overflows int64 once scaled must be
	// refused, not silently wrapped to a wrong or negative size, and the
	// message must say which value overflowed: the count the operator typed
	// does NOT overflow on its own, so naming it as the overflowing value
	// would be false.
	t.Run("overflows int64 when scaled", func(t *testing.T) {
		cmd := newProxyCmd()
		err := cmd.Flags().Set("max-body", "9223372036854775807MiB")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "overflow")
		assert.Contains(t, err.Error(), "9223372036854775807 scaled by the unit",
			"the message must name the count as scaled by the unit, which is what overflows")
		assert.NotContains(t, err.Error(), "9223372036854775807 bytes",
			"the unscaled count does not overflow int64, so the message must not claim it does")
	})

	// An unknown unit is rendered trimmed, so "8 xib" reports "xib", not " xib".
	t.Run("unknown unit is trimmed in the error", func(t *testing.T) {
		cmd := newProxyCmd()
		err := cmd.Flags().Set("max-body", "8 xib")
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"xib"`)
		assert.NotContains(t, err.Error(), `" xib"`)
	})
}

// ---------------------------------------------------------------------------
// The upstream, the banner that reports it, and the server's time bounds.
// ---------------------------------------------------------------------------

// TestProxyCmdRefusesAnUnusableUpstream pins finding B: url.Parse succeeds on
// "mem0.internal:8081" (scheme "mem0.internal", no host), so a plausible typo
// must be refused at startup rather than starting a proxy that 502s every
// request. Each case names NOTARY_MEM0_BASE_URL, because that is the operator's
// fix, and names what is wrong with the value too.
func TestProxyCmdRefusesAnUnusableUpstream(t *testing.T) {
	for _, tc := range []struct {
		name string
		url  string
		want string
	}{
		{"a bare host:port parses as a scheme", "mem0.internal:8081", `scheme "mem0.internal"`},
		{"a non-http scheme is refused", "ftp://mem0.internal:8081", `scheme "ftp"`},
		{"a scheme-relative reference has no scheme", "//mem0.internal:8081", `scheme ""`},
		{"an absolute URL with no host is refused", "https://", "empty host"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _ = newVerifySigner(t)
			cmd, _ := newTestProxyCmd(t)
			require.NoError(t, cmd.Flags().Set("addr", "127.0.0.1:0"))
			// The deadline bounds this test's own failure mode: if the
			// validation ever went missing, runProxy would bind a listener
			// and serve, and this makes it return (with a nil error, failing
			// the require below) instead of hanging the suite.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			cmd.SetContext(ctx)

			cfg := &config.Config{
				DBPath:        filepath.Join(t.TempDir(), "ledger.db"),
				GapLogPath:    filepath.Join(t.TempDir(), "gaps.log"),
				SigningKeyEnv: verifyKeyEnv,
				Mem0BaseURL:   tc.url,
			}
			err := runProxy(cmd, cfg)
			require.Error(t, err, "an unusable upstream must refuse to start")
			assert.Contains(t, err.Error(), config.EnvMem0BaseURL,
				"the refusal must name the variable the operator has to fix")
			assert.Contains(t, err.Error(), tc.want,
				"the refusal must name what is wrong with the value")
		})
	}
}

// TestProxyCmdRefusesAMalformedUpstreamWithoutEchoingIt pins finding I:
// url.Parse's own error repeats the raw value verbatim, so the refusal must
// render the value through the same redaction the rest of the project uses
// (scheme and host only -- here, "[url omitted]") rather than wrapping that
// error. The configured URL carries a userinfo password and a query-string
// sentinel, exactly the places a credential hides.
func TestProxyCmdRefusesAMalformedUpstreamWithoutEchoingIt(t *testing.T) {
	_, _ = newVerifySigner(t)
	cmd, _ := newTestProxyCmd(t)
	// See TestProxyCmdRefusesAnUnusableUpstream: the short context keeps a
	// validation-less build from serving forever if this refusal regressed.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	require.NoError(t, cmd.Flags().Set("addr", "127.0.0.1:0"))
	cfg := &config.Config{
		DBPath:        filepath.Join(t.TempDir(), "ledger.db"),
		GapLogPath:    filepath.Join(t.TempDir(), "gaps.log"),
		SigningKeyEnv: verifyKeyEnv,
		Mem0BaseURL:   "http://[::1]:namedport/?key=SUPER-SECRET-SENTINEL",
	}
	err := runProxy(cmd, cfg)
	require.Error(t, err, "a malformed upstream must refuse to start")
	assert.Contains(t, err.Error(), config.EnvMem0BaseURL)
	assert.NotContains(t, err.Error(), "SUPER-SECRET-SENTINEL",
		"the refusal must not echo a credential from the configured value")
	assert.NotContains(t, err.Error(), "[::1]",
		"the refusal must not repeat url.Parse's own message, which contains the raw value")
	assert.Contains(t, err.Error(), "invalid port", "the cause still names the fault")
}

// TestProxyCmdBannerShowsTheEffectiveUpstreamRedacted pins finding B's second
// half: the banner names the effective upstream, so an operator can see what
// they are forwarding to -- including that an unset NOTARY_MEM0_BASE_URL means
// the hosted api.mem0.ai -- and it renders it scheme-and-host only, so a
// credential hidden in the configured URL is never echoed.
//
// The banner is two lines precisely so this is possible: the listen line keeps
// its shape ("listening on <addr>", address only), and the upstream gets its
// own line. A single line would force every reader -- a person and the harness
// that learns the bound port -- to parse past the upstream.
func TestProxyCmdBannerShowsTheEffectiveUpstreamRedacted(t *testing.T) {
	sg, _ := newVerifySigner(t)
	require.NotNil(t, sg)

	upstream := addUpstream(t)
	credentialed := strings.Replace(upstream.URL, "http://", "http://operator:SUPER-SECRET@", 1) +
		"/?key=SUPER-SECRET"

	cfg := &config.Config{
		DBPath:        filepath.Join(t.TempDir(), "ledger.db"),
		GapLogPath:    filepath.Join(t.TempDir(), "gaps.log"),
		SigningKeyEnv: verifyKeyEnv,
		Mem0BaseURL:   credentialed,
	}

	cmd, buf := newTestProxyCmd(t)
	require.NoError(t, cmd.Flags().Set("addr", "127.0.0.1:0"))
	ctx, cancel := context.WithCancel(context.Background())
	cmd.SetContext(ctx)
	ch := make(chan error, 1)
	go func() { ch <- runProxy(cmd, cfg) }()

	require.Eventually(t, func() bool {
		return strings.Contains(buf.String(), "forwarding to ")
	}, 5*time.Second, 5*time.Millisecond, "the banner must name the upstream")

	banner := buf.String()
	assert.Contains(t, banner, "forwarding to "+upstream.URL,
		"the banner must name the effective upstream, scheme and host only")
	assert.NotContains(t, banner, "SUPER-SECRET", "the banner must not echo a credential from the configured URL")
	assert.NotContains(t, banner, "operator", "the banner must not echo the configured userinfo")

	cancel()
	awaitProxyStop(t, ch)
}

// TestProxyServerSetsDeliberateTimeBounds pins finding C: the server is
// network-facing (--addr can move it off loopback), and the body cap bounds
// bytes, not time, so every bound must be set. The values live in the
// proxy*Timeout constants documented on newProxyCmd; this asserts none was
// silently dropped, since the zero value of each means "no bound".
func TestProxyServerSetsDeliberateTimeBounds(t *testing.T) {
	srv := newProxyServer(http.NotFoundHandler())
	assert.NotNil(t, srv.Handler)
	assert.NotZero(t, srv.ReadHeaderTimeout, "a client must not hold a connection by dribbling headers")
	assert.NotZero(t, srv.ReadTimeout, "a client must not hold a connection by dribbling a body")
	assert.NotZero(t, srv.WriteTimeout, "a stalled upstream must not hold the caller's goroutine forever")
	assert.NotZero(t, srv.IdleTimeout, "an abandoned keep-alive connection must not be held open forever")
}

// ---------------------------------------------------------------------------
// Spec §9: the proxy honours NOTARY_SENSITIVITY_RULES.
// ---------------------------------------------------------------------------

// TestProxyCmdHonoursSensitivityRules is spec §9's config test for the rules
// path: with NOTARY_SENSITIVITY_RULES naming a file whose only clause is a
// metadata match, an add whose JSON body carries that key is recorded with
// Content.Sensitive true. It pins the whole classification path the command
// owns -- the environment read (loadProxyRules), the loader, and the proxy
// decoding the add's metadata from the request body -- and the control case
// pins that the rule actually decided: a classifier that marked everything
// would fail it.
func TestProxyCmdHonoursSensitivityRules(t *testing.T) {
	// A metadata-only rule: no scope clause, so it can only be matched by the
	// add's own metadata -- which is exactly the case library mode cannot
	// produce (library.Add's signature carries no metadata).
	rulesPath := filepath.Join(t.TempDir(), "rules.yaml")
	require.NoError(t, os.WriteFile(rulesPath, []byte(`rules:
  - name: health-data
    match:
      metadata:
        category: health
`), 0o600))
	t.Setenv(config.EnvSensitivityRules, rulesPath)

	for _, tc := range []struct {
		name       string
		metadata   string
		wantMarked bool
	}{
		{"a matching metadata value marks the record", `"metadata":{"category":"health"}`, true},
		{"a non-matching metadata value does not", `"metadata":{"category":"other"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

			body := `{"messages":[{"role":"user","content":"remember this"}],` +
				`"user_id":"u1",` + tc.metadata + `}`
			resp, err := http.Post("http://"+addr+"/v3/memories/add/", "application/json", strings.NewReader(body))
			require.NoError(t, err, "the listener must answer")
			require.Equal(t, http.StatusOK, resp.StatusCode)
			_ = resp.Body.Close()

			cancel()
			awaitProxyStop(t, errCh)

			head, ok, rows := ledgerSnapshot(t, dbPath)
			require.True(t, ok, "the proxied add must be recorded")
			require.Equal(t, 1, rows)
			require.NotNil(t, head.Content)
			assert.Equal(t, tc.wantMarked, head.Content.Sensitive,
				"the configured metadata rule must decide the classification")
		})
	}
}

// TestProxyCmdRefusesAnUnreadableRulesFile is the other half of spec §9's
// config test: an operator who configured rules must not silently run without
// them, so a set-but-unreadable path refuses to start and names the file. It is
// checked before the ledger, the gap log or the listener is opened, so nothing
// is left behind by the refusal.
func TestProxyCmdRefusesAnUnreadableRulesFile(t *testing.T) {
	_, _ = newVerifySigner(t)

	missing := filepath.Join(t.TempDir(), "missing-rules.yaml")
	t.Setenv(config.EnvSensitivityRules, missing)

	cmd, _ := newTestProxyCmd(t)
	require.NoError(t, cmd.Flags().Set("addr", "127.0.0.1:0"))
	// The short context makes this test fail fast, not hang, if the refusal
	// ever regressed into serving with no rules (see
	// TestProxyCmdRefusesAnUnusableUpstream).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd.SetContext(ctx)

	cfg := &config.Config{
		DBPath:        filepath.Join(t.TempDir(), "ledger.db"),
		GapLogPath:    filepath.Join(t.TempDir(), "gaps.log"),
		SigningKeyEnv: verifyKeyEnv,
		Mem0BaseURL:   "http://127.0.0.1:0", // never contacted: the rules are loaded first
	}
	err := runProxy(cmd, cfg)
	require.Error(t, err, "a configured rules file that cannot be read must refuse to start")
	assert.Contains(t, err.Error(), missing, "the refusal must name the unreadable file")
}

// ---------------------------------------------------------------------------
// Review Focus: Close racing with in-flight enqueues; the shutdown drains.
// ---------------------------------------------------------------------------

// TestProxyCmdShutdownDrainsThePipeline is the command-level proof of the
// shutdown contract, made non-vacuous with respect to the drain. A record
// accepted before the shutdown signal must be in the ledger after runProxy
// returns.
//
// The naive version of this test is weak: the pipeline's writer goroutine
// drains the queue continuously, so a single add is usually appended by the
// time the test cancels, and deleting h.Close would not change the outcome. To
// pin the drain, the test takes SQLite's write lock BEFORE sending the add, so
// the writer BLOCKS on its append and the accepted record provably cannot be
// written until the lock is released. Shutdown is then signalled while the
// record is still in the pipeline; the lock is released on a delay, and only a
// real drain (h.Close, the command's whole shutdown path) makes runProxy return
// with the record written. A shutdown that returned without draining would
// return before that delay, with the record still blocked, and the ledger
// assertion would fail.
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

	// Block the ledger writer before any add can be written, so the accepted
	// record is deterministically still in the pipeline when we shut down.
	release := holdWriteLock(t, dbPath)

	body := `{"messages":[{"role":"user","content":"hello"}],"user_id":"u1"}`
	resp, err := http.Post("http://"+addr+"/v3/memories/add/", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()

	// The record was enqueued before this response reached us and the writer is
	// blocked on the lock we hold, so nothing has been appended yet. Signal
	// shutdown; the drain must wait for the writer, which can only proceed once
	// the lock is released -- so release it shortly after. A shutdown that
	// returned without draining (no h.Close) returns immediately, before this
	// release, with the record still queued, which the assertion below catches.
	cancel()
	time.AfterFunc(300*time.Millisecond, release)

	awaitProxyStop(t, errCh)

	_, ok, rows := ledgerSnapshot(t, dbPath)
	require.True(t, ok, "the ledger must hold the record accepted before shutdown")
	assert.Equal(t, 1, rows,
		"the add accepted before shutdown must be drained into the ledger, not lost with the queue")
}
