package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"notary/config"
	"notary/internal/gap"
	"notary/internal/interceptor"
	"notary/internal/interceptor/proxy"
	"notary/internal/ledger"
	"notary/internal/sign"
	"notary/internal/store"
)

const (
	// defaultProxyAddr is the listen address the proxy binds when --addr is
	// unset. It is the loopback interface: the proxy holds no credential of
	// its own and forwards the caller's, so a deployment places it behind its
	// own edge rather than exposing it.
	defaultProxyAddr = "127.0.0.1:8080"
	// defaultQueueDepth is the write pipeline's queue capacity when
	// --queue-depth is unset, matching the design's default (spec section 6).
	defaultQueueDepth = 1024
	// defaultProxyMaxBody is the observation body cap when --max-body is
	// unset: 8 MiB, the same bound internal/mem0/client.go puts on a response.
	defaultProxyMaxBody = 8 << 20
	// proxyShutdownTimeout bounds a graceful stop, so a stuck in-flight
	// request cannot hang the shutdown indefinitely.
	proxyShutdownTimeout = 10 * time.Second

	// proxyReadHeaderTimeout bounds how long a client may take to send the
	// request line and headers once the connection is accepted. Headers are
	// small and arrive with the request, so they are held to a short bound:
	// this is the slowloris guard that keeps a client from parking connections
	// by dribbling headers.
	proxyReadHeaderTimeout = 10 * time.Second
	// proxyReadTimeout bounds a whole request, headers and body. The body cap
	// (--max-body) bounds bytes, not time, and the proxy buffers a recorded
	// request's body before forwarding it, so without this a client that sends
	// headers and then dribbles a body would hold a goroutine and a connection
	// with nothing reaching Mem0. Five minutes accepts a genuinely slow,
	// multi-megabyte upload while making "indefinitely" finite.
	proxyReadTimeout = 5 * time.Minute
	// proxyWriteTimeout bounds a whole response, from the request's headers
	// being read to the last response byte. A Mem0 response is a finite JSON
	// document, so five minutes is far beyond any legitimate one; the bound is
	// there so a stalled upstream cannot hold the caller's goroutine forever.
	proxyWriteTimeout = 5 * time.Minute
	// proxyIdleTimeout bounds an idle kept-alive connection between requests.
	// Two minutes is long enough to spare a chatty caller a new handshake and
	// short enough to release sockets a client has abandoned.
	proxyIdleTimeout = 2 * time.Minute
)

// newProxyCmd builds the `notary proxy` subcommand: a reverse proxy in front of
// Mem0 that forwards every request to the real Mem0 and returns the response
// untouched, recording what library mode records for the add and search
// endpoints and nothing else (design sections 1, 5, 7).
//
// Its view of the world is reversed from every other command: the others read
// the ledger and print, this one serves traffic and writes. So its stdout is
// unused -- the operator's channel is stderr, where the listen banner and every
// drop marker go -- and it writes to no file but the ledger and the gap log.
//
// It needs a signing key, because ledger appends are signed (spec section 8),
// and it deliberately does NOT need a Mem0 API key: it forwards the caller's
// own Authorization header, so it is credential-neutral and holds no credential
// of its own. Refusing without a Mem0 key would defeat that property.
//
// It takes exactly three flags. --addr is the listen address, --queue-depth the
// write pipeline's queue capacity, and --max-body the observation body cap.
// The sensitivity rules it applies come from NOTARY_SENSITIVITY_RULES, through
// the same loader an application uses, and there is deliberately no flag for
// that path (spec section 8): a second way to say the same thing is how
// configuration drifts.
//
// Its listener carries deliberate time bounds, because --addr can expose it
// beyond the loopback default and a body cap bounds bytes, not time: a client
// that sends headers and then dribbles a body would otherwise hold a goroutine
// and a connection indefinitely, and this proxy buffers a request body before
// forwarding, so nothing reaches Mem0 until that body arrives. The bounds are
// proxyReadHeaderTimeout (10s: headers arrive with the request), proxyReadTimeout
// and proxyWriteTimeout (5m each: long enough for a slow multi-megabyte upload
// or a slow upstream, finite enough that a stuck peer cannot hold a connection
// forever), and proxyIdleTimeout (2m: a kept-alive connection between requests).
func newProxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Serve Mem0 traffic and record the add and search requests that pass through",
		Long: "Proxy runs a reverse proxy in front of Mem0: the application re-points its\n" +
			"Mem0 base URL at Notary, Notary forwards every request to the real Mem0 and\n" +
			"returns the response untouched, and on the add and search endpoints it\n" +
			"records what library mode would have recorded -- no code change in the\n" +
			"application, in any language.\n" +
			"\n" +
			"It forwards the caller's own Authorization header and holds no credential of\n" +
			"its own, so it needs no Mem0 API key; it DOES need a signing key, because\n" +
			"ledger appends are signed. Its stdout is unused: the listen banner and every\n" +
			"drop marker go to stderr.\n" +
			"\n" +
			"A response is returned before its record is written: records go through a\n" +
			"bounded queue drained by one writer, so a slow audit write can never delay a\n" +
			"caller. A graceful stop (SIGINT or SIGTERM) drains the queue before exiting,\n" +
			"so an accepted request is recorded; a crash loses whatever is still queued.\n" +
			"\n" +
			"Sensitivity rules come from " + config.EnvSensitivityRules + ", through the same\n" +
			"loader an application uses; there is deliberately no flag for that path.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return fmt.Errorf("loading configuration: %w", err)
			}
			return runProxy(cmd, cfg)
		},
	}

	// Every flag is declared here and read back through cmd.Flags() in
	// runProxy, so there is no package- or closure-level mutable state.
	cmd.Flags().String("addr", defaultProxyAddr,
		"the address to listen on for Mem0 traffic")
	cmd.Flags().Int("queue-depth", defaultQueueDepth,
		"the write pipeline's queue capacity; a full queue drops and marks records rather than blocking a caller")
	cmd.Flags().Var(newByteSize(defaultProxyMaxBody), "max-body",
		"the maximum request or response body buffered for observation (for example 8MiB); a larger body is forwarded untouched and not recorded")
	return cmd
}

// byteSize is the pflag.Value behind --max-body: a byte count that reads and
// writes as a human size ("8MiB") rather than a raw integer. pflag ships no
// such type and this project allows no new dependency, so the flag carries this
// one instead of a plain int64 -- which keeps the default legible in --help.
type byteSize int64

// newByteSize returns a byteSize initialised to n, for registering --max-body.
func newByteSize(n int64) *byteSize {
	b := byteSize(n)
	return &b
}

// String renders the size in the largest binary unit that divides it exactly,
// so the flag's default reads as "8MiB" rather than "8388608".
func (b *byteSize) String() string {
	n := int64(*b)
	switch {
	case n > 0 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n>>30)
	case n > 0 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	case n > 0 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// Set parses a byte size: a non-negative integer optionally followed by a
// binary unit suffix (B, KiB/MiB/GiB, or the aliases KB/MB/GB). A negative or
// malformed value is refused rather than silently coerced.
func (b *byteSize) Set(s string) error {
	n, err := parseByteSize(s)
	if err != nil {
		return err
	}
	*b = byteSize(n)
	return nil
}

// Type names the flag's value type in help output.
func (b *byteSize) Type() string { return "bytes" }

// parseByteSize parses a non-negative byte count with an optional binary unit
// suffix. Suffixes are powers of 1024 (K/KB/KiB alike), so "8MiB" is 8*1024*1024.
func parseByteSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("byte size %q: empty value", s)
	}

	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("byte size %q: missing a leading number", s)
	}
	n, err := strconv.ParseInt(t[:i], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("byte size %q: %w", s, err)
	}

	suffix := strings.TrimSpace(t[i:])
	var mult int64
	switch strings.ToLower(suffix) {
	case "", "b":
		mult = 1
	case "k", "kb", "kib":
		mult = 1 << 10
	case "m", "mb", "mib":
		mult = 1 << 20
	case "g", "gb", "gib":
		mult = 1 << 30
	default:
		return 0, fmt.Errorf("byte size %q: unknown unit %q (use B, KiB, MiB or GiB)", s, suffix)
	}
	// The integer part is bounded by ParseInt, but the multiplication is not:
	// a large-but-parseable count times a unit multiplier can wrap to a wrong
	// or negative value, so it is refused rather than silently coerced. The
	// message says so precisely: the count the operator typed does not
	// overflow on its own -- the unit scales it past int64.
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("byte size %q: %d scaled by the unit overflows int64", s, n)
	}
	return n * mult, nil
}

// maxBodyBytes reads the --max-body flag back through cmd.Flags(). The flag
// carries a *byteSize, so it is fetched and type-asserted rather than read with
// a typed getter.
func maxBodyBytes(cmd *cobra.Command) (int64, error) {
	f := cmd.Flags().Lookup("max-body")
	if f == nil {
		return 0, fmt.Errorf("proxy: the --max-body flag is not registered")
	}
	bs, ok := f.Value.(*byteSize)
	if !ok {
		return 0, fmt.Errorf("proxy: the --max-body flag has unexpected type %q", f.Value.Type())
	}
	return int64(*bs), nil
}

// runProxy wires the proxy's components, binds its listener, and serves Mem0
// traffic until the process is signalled to stop.
//
// The composition order is deliberate, and it is what makes shutdown drain the
// queue. It builds the AuditWriter (which owns the gap log's lifetime), then
// the write pipeline (which borrows the gap log and drains drops into it), then
// the Observer (which writes records into the pipeline), then the handler
// (which borrows the pipeline). See proxy.New's documentation: the handler's
// Close closes that borrowed pipeline. So h.Close is the ENTIRE shutdown path
// -- it closes the pipeline, which drains the queued records and the drop
// tally, then closes the AuditWriter, which closes the gap log. A missing
// h.Close would lose the queue, because nothing else drains it.
//
// It requires a signing key and deliberately does NOT require a Mem0 API key:
// the proxy forwards the caller's credentials, so an empty cfg.Mem0APIKey is
// not a reason to refuse. It returns a non-nil error for a missing signing
// key, an unreadable rules file, an unusable upstream base URL, a bind or
// serve failure, or a failed drain, and it never panics.
func runProxy(cmd *cobra.Command, cfg *config.Config) error {
	errOut := cmd.ErrOrStderr()

	// A missing signing key is a hard failure at config time (spec section 8):
	// the proxy writes SIGNED records, so it refuses to start rather than
	// write unsigned ones. NewSigner never generates a key, so an unset or
	// empty variable fails here too, naming the variable it looked for.
	if cfg.SigningKeyEnv == "" {
		return fmt.Errorf(
			"no signing key is configured, so proxy cannot sign the records it writes: "+
				"set %s to base64-encoded ed25519 key material (proxy refuses to start "+
				"without it because it writes signed records)",
			config.DefaultSigningKeyEnv)
	}
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: cfg.SigningKeyEnv})
	if err != nil {
		return fmt.Errorf("loading signing key from %s: %w", cfg.SigningKeyEnv, err)
	}

	// There is deliberately NO Mem0 API key check here. The proxy forwards the
	// caller's own Authorization header, so it is credential-neutral and holds
	// no credential of its own; requiring cfg.Mem0APIKey would be a real bug.

	// Rules are loaded before any resource is opened, so a bad rules file fails
	// fast without a leaked store or gap-log handle. An unset variable means no
	// rules, which matches nothing.
	rs, err := loadProxyRules()
	if err != nil {
		return err
	}

	addr, err := cmd.Flags().GetString("addr")
	if err != nil {
		return fmt.Errorf("reading --addr: %w", err)
	}
	depth, err := cmd.Flags().GetInt("queue-depth")
	if err != nil {
		return fmt.Errorf("reading --queue-depth: %w", err)
	}
	maxBody, err := maxBodyBytes(cmd)
	if err != nil {
		return err
	}

	upstream, err := parseUpstream(cfg.Mem0BaseURL)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("opening ledger %s: %w", cfg.DBPath, err)
	}
	defer func() { _ = st.Close() }()

	l := ledger.New(st, sg, nil)

	g, err := gap.Open(cfg.GapLogPath)
	if err != nil {
		return fmt.Errorf("opening gap log %s: %w", cfg.GapLogPath, err)
	}

	// The composition order is load-bearing; see runProxy's doc comment.
	//
	// One synchronizing writer backs every operator-facing marker. The
	// AuditWriter's channels, the pipeline's drop markers and the handler's
	// out all receive writes from different goroutines -- the pipeline's
	// single writer, and every request goroutine -- and the handler serializes
	// only its own. Wrapping once here makes sharing one writer safe by
	// construction, rather than leaving it to each component's private
	// discipline (or its absence) on a writer that may be a plain buffer.
	out := &syncWriter{w: errOut}
	channels := []io.Writer{out}
	aw := interceptor.NewAuditWriter(l, g, channels)
	pipe := proxy.NewPipeline(aw, g, channels, depth)
	obs := interceptor.NewObserver(pipe, interceptor.WithRules(rs))
	h := proxy.New(upstream, obs, pipe, out, maxBody)

	// h.Close closes the pipeline (draining it) and, through it, the
	// AuditWriter and the gap log. Deferring it guarantees the drain even on an
	// early return below; the explicit call after serving surfaces its error,
	// and Close is idempotent so the second call is a no-op.
	defer func() { _ = h.Close() }()

	// Bind explicitly so the banner can name the address actually in use -- a
	// :0 listener's port is only known after binding -- and so a bind failure
	// is reported before serving.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}

	srv := newProxyServer(h)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	// The operator's channel is stderr, not stdout (design section 8). The
	// banner is two lines: the listen line keeps the shape every operator and
	// test already reads (`listening on <addr>`, one line, address only), and
	// the second names the EFFECTIVE upstream, so an operator can see what
	// they are forwarding to -- in particular, that an unset
	// NOTARY_MEM0_BASE_URL means the hosted default. The upstream is rendered
	// through proxy.RedactedURL (scheme and host only), so a credential in
	// the configured URL is never echoed.
	fmt.Fprintf(out, "notary proxy: listening on %s\n", ln.Addr())
	fmt.Fprintf(out, "notary proxy: forwarding to %s\n", proxy.RedactedURL(cfg.Mem0BaseURL))

	// The signal-aware context is the whole graceful-stop mechanism: SIGINT or
	// SIGTERM cancels it, and the shutdown below drains the queue. cmd.Context
	// is nil for a command not run through Execute, so fall back to a
	// background context before wrapping it.
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	var runErr error
	select {
	case <-ctx.Done():
		// Signalled (or a test cancelled the context): stop accepting and
		// shut the server down.
	case err := <-serveErr:
		// Serve returned before any signal: a bind/serve failure ends the run.
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serving on %s: %w", ln.Addr(), err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), proxyShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("shutting down proxy on %s: %w", ln.Addr(), err))
	}

	// The drain: h.Close stops the pipeline, waits for the writer goroutine to
	// finish the queue, records the drop tally, and closes the gap log. This is
	// the only thing that ever drains the queue.
	if err := h.Close(); err != nil {
		runErr = errors.Join(runErr, fmt.Errorf("draining the proxy write pipeline: %w", err))
	}
	return runErr
}

// loadProxyRules loads the sensitivity rules the proxy applies to the records
// it writes, from the path in NOTARY_SENSITIVITY_RULES, through the same
// write-path loader an application constructing an interceptor uses (spec
// section 8). Rules mark content sensitive at write time, so they concern the
// paths whose records CARRY content, and `notary proxy` is the first CLI
// command whose records do. An unset variable means no rules: the returned
// *interceptor.RuleSet is nil, which matches nothing. A set-but-unreadable
// file is a refusal to start, naming the file, because an operator who
// configured rules must not silently run without them.
func loadProxyRules() (*interceptor.RuleSet, error) {
	path := os.Getenv(config.EnvSensitivityRules)
	if path == "" {
		return nil, nil
	}
	rules, err := config.LoadSensitivityRules(path)
	if err != nil {
		return nil, fmt.Errorf("loading sensitivity rules: %w", err)
	}
	return interceptor.NewRuleSet(rules), nil
}

// parseUpstream parses and validates the upstream base URL the proxy forwards
// to, from cfg.Mem0BaseURL, and returns it as the borrowed *url.URL proxy.New
// takes.
//
// Parsing alone is not validation: url.Parse("mem0.internal:8081") SUCCEEDS,
// with scheme "mem0.internal", an empty host and "8081" in url.Opaque, so a
// plausible typo would otherwise start a proxy that 502s every request. The
// value must therefore be an absolute http(s) URL with a host. Anything else is
// refused at startup, naming NOTARY_MEM0_BASE_URL, so the operator's fix is
// named with the failure.
//
// Every rendering of the value here goes through proxy.RedactedURL -- scheme
// and host only, never path, query or userinfo -- and a parse error contributes
// only its cause, never url.Parse's own message, which repeats the raw value
// verbatim. An operator may put a credential in the base URL (the ?key= form),
// so the refusal must not be the thing that logs it.
func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf(
			"upstream Mem0 base URL %s is not a valid URL: %w (set %s to an absolute URL such as http://mem0.internal:8081)",
			proxy.RedactedURL(raw), urlParseCause(err), config.EnvMem0BaseURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf(
			"upstream Mem0 base URL %s has scheme %q, which is not http or https (set %s to an absolute URL such as http://mem0.internal:8081)",
			proxy.RedactedURL(raw), u.Scheme, config.EnvMem0BaseURL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf(
			"upstream Mem0 base URL %s has an empty host (set %s to an absolute URL such as http://mem0.internal:8081)",
			proxy.RedactedURL(raw), config.EnvMem0BaseURL)
	}
	return u, nil
}

// urlParseCause returns the cause of a url.Parse failure without its URL.
// url.Parse wraps every failure in a *url.Error whose message is
// `parse "<the raw value>": <cause>`, so wrapping that error as-is would echo
// the value the redaction exists to hide. The cause alone names the fault
// (an invalid port, an invalid escape) without the URL.
func urlParseCause(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		if uerr.Err != nil {
			return uerr.Err
		}
		return errors.New("invalid URL")
	}
	return err
}

// newProxyServer returns the HTTP server the command serves on, with the time
// bounds documented on newProxyCmd: a listener the operator can expose with
// --addr must not let one client hold a connection and a goroutine forever by
// dribbling headers or a body.
func newProxyServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: proxyReadHeaderTimeout,
		ReadTimeout:       proxyReadTimeout,
		WriteTimeout:      proxyWriteTimeout,
		IdleTimeout:       proxyIdleTimeout,
	}
}

// syncWriter serializes writes to one underlying writer.
//
// The proxy's operator-facing markers have two independent producers -- request
// handlers (proxy.ProxyInterceptor.mark, which serializes only its own writes)
// and the pipeline's single writer goroutine (drop and error markers, and the
// AuditWriter's gap failures) -- and runProxy hands them all the same channel.
// Handing them one synchronizing writer makes that sharing safe by
// construction: a writer that is not itself concurrency-safe (a bytes.Buffer,
// for instance) can no longer be interleaved just because two components were
// wired to it. The alternative -- documenting the requirement on both
// constructors -- leaves a hazard the compiler cannot check, and the command
// would still have to satisfy its own rule.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

// Write writes p to the underlying writer under the lock.
func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
