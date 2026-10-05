package serve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

// shutdownTimeout bounds how long Serve waits for in-flight requests to finish
// after its context is cancelled. It is short on purpose: this is a single-user
// loopback console whose requests are reads, so a slow shutdown would only mean
// a Ctrl-C that appears not to work.
const shutdownTimeout = 5 * time.Second

// Handler returns the console's HTTP surface: one ServeMux wrapped by two
// guards, in this order.
//
// The method guard is OUTERMOST, so a non-GET/HEAD request is refused before
// the Host guard runs and never reaches a handler; the Host guard sits between
// it and the mux, so a GET from a non-loopback Host is refused with 403. Both
// refusal paths write a status and no body, so neither echoes anything about
// the ledger.
//
// The routes are exactly the ones this task ships -- the records view and the
// assets. The per-record, per-memory, gaps and verify routes arrive with their
// pages in a later task, so the route table and the set of pages that exist stay
// in step.
//
// The per-record and per-memory views that later task adds must build their
// links from the raw ids IN THE QUERY POSITION -- href="/record?id={{.Line.ID}}"
// -- and never from a pre-joined whole URL. html/template does not
// percent-encode a whole URL interpolated into href (it leaves "/", "#" and ":"
// alone), so an id containing "#" becomes a URL fragment, truncates the id
// server-side, and cannot round-trip. See recordRow's doc comment.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/assets/", s.handleAsset)
	return guardMethods(guardHost(mux))
}

// guardMethods refuses every method but GET and HEAD with 405 and no body, so
// there is no request shape that could carry a mutation even if a handler were
// ever added carelessly.
func guardMethods(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guardHost refuses a request whose Host is not loopback, with 403 and no body.
// It blunts DNS rebinding -- the one browser-reachable attack a read-only local
// server still has -- by making the console reachable only by the names a
// loopback client uses.
func guardHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost reports whether a Host header names loopback: 127.0.0.1,
// localhost, or the IPv6 loopback in either its bracketed or bare form. The
// port, when present, is stripped first, so "127.0.0.1:4317" and "127.0.0.1"
// are both accepted; anything else -- a public name, another interface, or an
// empty Host -- is refused.
func isLoopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1", "[::1]":
		return true
	default:
		return false
	}
}

// handleIndex renders the records view, the console's front page.
//
// Only the exact root path is the records view; any other path the mux sends
// here (a request the later routes do not yet claim) is 404 rather than a
// records view wearing the wrong URL.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	// The chain banner is on every page, so the chain verdict is taken here,
	// before the filter is even parsed -- a bad filter still renders it.
	chain, err := s.loadChain()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := indexData{
		pageData: pageData{Title: "Records", Now: s.now(), Chain: chain},
		Filter:   s.defaultWindow(),
	}

	f, ferr := s.parseFilter(r.URL.Query())
	if ferr != nil {
		// A usage error is a 400 that still renders the page: the banner names
		// the problem and the form falls back to the default window.
		data.Err = ferr.Error()
		s.respond(w, http.StatusBadRequest, "index", data)
		return
	}

	rows, err := s.loadRecords(f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Filter = f
	data.Rows = rows
	s.respond(w, http.StatusOK, "index", data)
}

// handleAsset serves one embedded asset by exact name and never reads the OS
// filesystem. A name the binary did not embed is a 404, so the handler cannot
// be talked into serving a directory listing or a file off disk.
//
// Every asset is served with Cache-Control: no-store, so a browser always
// revalidates and a stale page never pairs with a stale stylesheet.
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	data, ok := s.assets[name]
	if !ok || name == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", assetContentType(name))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// assetContentType names the media type of an embedded asset from its file name.
func assetContentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// respond renders page into a buffer and only then writes it, so the HTTP
// status can be set on a page that rendered successfully and a template fault
// yields a clean 500 rather than a half-written body.
func (s *Server) respond(w http.ResponseWriter, status int, page string, data any) {
	var buf bytes.Buffer
	if err := s.render(&buf, page, data); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// defaultWindow is the range the records form falls back to: the last thirty
// days, the same default parseFilter applies when a bound is absent. It exists
// so a bad filter can render a form with sensible values instead of the zero
// time.
func (s *Server) defaultWindow() filter {
	now := s.now()
	return filter{From: now.Add(-30 * 24 * time.Hour), To: now}
}

// Listen binds a TCP listener on the literal loopback address 127.0.0.1 at the
// given port, asking the OS for a free port when port is 0.
//
// The host is a literal "127.0.0.1", never ":<port>": there is no host or addr
// flag and there must be none, so there is no interface to misconfigure and the
// bind can be asserted. A failure names the port and the likely cause, since a
// bind failure on a fixed port is almost always that the port is already in use.
func (s *Server) Listen(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("serve: listening on 127.0.0.1:%d (is the port already in use?): %w", port, err)
	}
	return ln, nil
}

// Serve runs the console's HTTP surface on ln until ctx is cancelled, then
// shuts down gracefully and returns nil.
//
// It returns nil for a clean shutdown and a wrapped error only when the server
// could not be run or could not be shut down. A cancelled context is a clean
// stop, not an error: that is what Ctrl-C does.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{Handler: s.Handler()}

	serveErr := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("serve: shutdown: %w", err)
		}
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}
}
