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

	"notary/internal/record"
	"notary/internal/store"
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
// The route table is fixed and small: the records view, the per-record,
// per-memory, gaps and verify views, and the assets. Every link that carries an
// id is built by the recordHref and memoryHref template functions, which
// url.QueryEscape the id ONCE in Go and hand the SAME string to every URL
// attribute of the link -- both the href and any hx-get. html/template's
// contextual escaper only URL-encodes its fixed set of URL attributes (href,
// action, src, ...), so it reaches href but NOT a custom attribute such as
// htmx's hx-get; leaving an id raw for the escaper would, in hx-get, start a URL
// fragment and fetch a truncated id. See recordRow's and recordHref's doc
// comments. The mux prefers the longest matching pattern, so the exact
// "/record" (and its siblings) wins over the "/" prefix that catches
// everything else; handleIndex's own r.URL.Path != "/" guard 404s any path none
// of them claims.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/record", s.handleRecord)
	mux.HandleFunc("/memory", s.handleMemory)
	mux.HandleFunc("/gaps", s.handleGaps)
	mux.HandleFunc("/verify", s.handleVerify)
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
	// The index view reveals too -- its filter form carries reveal=1 -- so a
	// revealing records request writes the same one-line audit note, naming the
	// records view. It is logged with the other views' reveals, so no reveal
	// path is silent.
	if f.Reveal {
		s.logReveal("records")
	}
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

// revealControl is a view's own reveal toggle as the shared "revealToggle"
// partial renders it: the two URLs the toggle uses and the element id htmx
// swaps in place.
//
// Href and RevealHref are built by recordHref/memoryHref, so the id is escaped
// exactly once in Go and the SAME string is used for both the href and the
// hx-get of the anchor -- required because html/template URL-encodes href but
// only HTML-escapes hx-get (see recordHref's doc comment).
type revealControl struct {
	// Href is the view's own URL without reveal=1 -- the "hide" link, and the
	// base the reveal link is built from.
	Href string
	// RevealHref is Href with reveal=1 appended -- the reveal link.
	RevealHref string
	// On reports whether the view is currently revealed.
	On bool
	// Target is the id of the element htmx swaps, e.g. "record".
	Target string
}

// recordData is the per-record view's payload: the page shell, the record's row,
// its reveal control, and -- when the id was missing or named no record -- the
// error to show in place of the row.
type recordData struct {
	pageData
	RevealControl revealControl
	Row           recordRow
	Err           string
}

// memoryData is the per-memory view's payload: the page shell, the memory id,
// the rows in Seq order, the reveal control, and a usage error for an empty id.
type memoryData struct {
	pageData
	MemoryID      string
	RevealControl revealControl
	Rows          []recordRow
	Err           string
}

// gapsData is the gaps view's payload: the page shell and the shared gap
// report. It carries no reveal control: the gaps view renders no memory content,
// so there is nothing to reveal and a reveal note would be a false claim.
type gapsData struct {
	pageData
	View gapsView
}

// verifyData is the verify view's payload: the page shell, whose embedded Chain
// is the full chain state this page renders, plus the record count the clean
// state reports. Like gapsData it carries no reveal control -- the verify view
// renders no memory content.
type verifyData struct {
	pageData
	Records int
}

// handleRecord renders the per-record view for ?id=<record-id>.
//
// The chain banner is on every page, so the chain verdict is taken first, before
// the id is validated -- a 400 or 404 still renders the banner. An empty id is
// ErrMissingID and a 400; an id that names no record keeps store.ErrNotFound in
// its chain and is a 404, both rendering the page with the error named rather
// than a contentless body.
func (s *Server) handleRecord(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	reveal := r.URL.Query().Get("reveal") == "1"

	chain, err := s.loadChain()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	revealHref := recordHref(record.RecordID(id))
	data := recordData{
		pageData: pageData{Title: "Record", Now: s.now(), Chain: chain},
		RevealControl: revealControl{
			Href:       revealHref,
			RevealHref: revealHref + "&reveal=1",
			On:         reveal,
			Target:     "record",
		},
	}
	if id != "" {
		data.Title = "Record " + id
	}

	row, err := s.loadRecord(record.RecordID(id), reveal)
	if err != nil {
		switch {
		case errors.Is(err, ErrMissingID):
			data.Err = err.Error()
			s.respond(w, http.StatusBadRequest, "record", data)
		case errors.Is(err, store.ErrNotFound):
			data.Err = err.Error()
			s.respond(w, http.StatusNotFound, "record", data)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	data.Row = row
	// One line per revealing request, written only once the view renders: a
	// request that revealed nothing (a 400 or 404) writes no note.
	if reveal {
		s.logReveal("record " + id)
	}
	s.respond(w, http.StatusOK, "record", data)
}

// handleMemory renders the per-memory view for ?id=<mem0-id>: every record whose
// subject memory is id, in Seq order, the same claims `notary explain --memory`
// prints.
//
// An empty id is ErrMissingID and a 400, rendering the page with the error and
// NO rows -- the store matches the empty memory_id column's DEFAULT, so an empty
// filter would otherwise list the whole ledger as one memory's life. A memory
// with no records is not an error: it renders its "no records for this memory"
// page rather than a blank success.
func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	reveal := r.URL.Query().Get("reveal") == "1"

	chain, err := s.loadChain()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	revealHref := memoryHref(id)
	data := memoryData{
		pageData: pageData{Title: "Memory", Now: s.now(), Chain: chain},
		MemoryID: id,
		RevealControl: revealControl{
			Href:       revealHref,
			RevealHref: revealHref + "&reveal=1",
			On:         reveal,
			Target:     "memory",
		},
	}
	if id != "" {
		data.Title = "Memory " + id
	}

	rows, err := s.loadMemory(id, reveal)
	if err != nil {
		if errors.Is(err, ErrMissingID) {
			data.Err = err.Error()
			s.respond(w, http.StatusBadRequest, "memory", data)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Rows = rows
	if reveal {
		s.logReveal("memory " + id)
	}
	s.respond(w, http.StatusOK, "memory", data)
}

// handleGaps renders the gaps view: every outstanding gap and any gap-log
// integrity break, from the same report `notary gaps` prints.
//
// It takes no reveal: the view renders no memory content, so there is nothing to
// reveal and it must not write a "revealed sensitive content" note that would be
// false, nor offer a control that reveals nothing.
func (s *Server) handleGaps(w http.ResponseWriter, _ *http.Request) {
	chain, err := s.loadChain()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	view, err := s.loadGaps()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := gapsData{
		pageData: pageData{Title: "Gaps", Now: s.now(), Chain: chain},
		View:     view,
	}
	s.respond(w, http.StatusOK, "gaps", data)
}

// handleVerify renders the chain state in full: the three-state verdict the
// banner only summarises, with every break, or the clean record count, or the
// keyring fix the "not verified" state names.
//
// Like handleGaps it takes no reveal: the view renders no memory content, so it
// must not offer a reveal control or write a false reveal note.
func (s *Server) handleVerify(w http.ResponseWriter, _ *http.Request) {
	chain, err := s.loadChain()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := verifyData{
		pageData: pageData{Title: "Verify", Now: s.now(), Chain: chain},
	}
	// The record count is the clean state's own claim, so it is read only for a
	// clean chain -- a broken or not-verified chain has no count to report.
	if chain.Clean() {
		n, err := s.recordCount()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.Records = n
	}
	s.respond(w, http.StatusOK, "verify", data)
}

// logReveal writes the one-line audit note a revealing request leaves on the
// operator's stderr, naming the view so the note records WHICH view was
// revealed. The write error is deliberately not surfaced: this is a note to a
// log writer (io.Discard when none is configured), and a failed note must not
// fail the page it annotates.
func (s *Server) logReveal(view string) {
	fmt.Fprintf(s.reveal, "serve: revealed sensitive content for %s\n", view)
}

// recordCount returns the number of records in the ledger, matching the count
// `notary verify` prints for a clean chain: the head's Seq plus one, or zero for
// an empty ledger.
func (s *Server) recordCount() (int, error) {
	head, ok, err := s.ledger.Head()
	if err != nil {
		return 0, fmt.Errorf("serve: read ledger head: %w", err)
	}
	if !ok {
		return 0, nil
	}
	return int(head.Seq) + 1, nil
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
