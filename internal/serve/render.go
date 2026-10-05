package serve

import (
	"bytes"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"strings"
	"time"

	"notary/internal/export"
)

// pageData is the shell every page renders through the shared layout: the
// page's title, the instant it was built, and the chain banner's verdict.
//
// Every page's own data type EMBEDS pageData rather than restating these three
// fields, so the chain banner has exactly one source -- Chain -- and a page
// cannot accidentally render a different chain view from the one the banner
// shows. `html/template` promotes the embedded fields, so a template written
// against indexData reads .Title, .Now and .Chain directly.
type pageData struct {
	// Title names the page in the document title.
	Title string
	// Now is the instant the page was built, from the Server's clock.
	Now time.Time
	// Chain is the whole ledger's chain verdict, taken for this render.
	Chain chainView
}

// indexData is the records view's payload: the page shell, the filter it ran,
// the rows it read, and -- when the filter was unusable -- the usage error.
//
// Err is carried rather than returned as a bare HTTP error so a bad filter
// renders the page with a banner naming the problem, at 400, instead of a
// contentless error body: a reviewer sees what they typed, what was wrong, and
// the chain banner, in one response.
type indexData struct {
	pageData
	// Filter is the range and scope the view ran (or, on a usage error, the
	// default window the form falls back to).
	Filter filter
	// Rows is one row per record in range, in ledger order.
	Rows []recordRow
	// Err is the filter's usage error, empty when the filter was usable.
	Err string
}

// Clean, Broken and NotVerified project chainView's three-way verdict into the
// booleans a template branches on. They exist because chainState is unexported
// and its values have no names a template could compare against; naming the
// three states here is what lets the banner render each one distinguishably
// rather than collapsing "not verified" into "clean".
func (v chainView) Clean() bool       { return v.State == chainClean }
func (v chainView) Broken() bool      { return v.State == chainBroken }
func (v chainView) NotVerified() bool { return v.State == chainNotVerified }

// templateFuncs is the function map every page set parses with. It holds only
// presentation helpers that add no claim of their own.
func templateFuncs() template.FuncMap {
	return template.FuncMap{"scopeString": scopeString}
}

// scopeString renders a record's scope as one readable line, showing only the
// dimensions the record actually carried (an empty dimension is omitted rather
// than printed as "user=" with nothing after it).
func scopeString(s export.LineScope) string {
	parts := make([]string, 0, 4)
	if s.UserID != "" {
		parts = append(parts, "user="+s.UserID)
	}
	if s.AgentID != "" {
		parts = append(parts, "agent="+s.AgentID)
	}
	if s.AppID != "" {
		parts = append(parts, "app="+s.AppID)
	}
	if s.RunID != "" {
		parts = append(parts, "run="+s.RunID)
	}
	return strings.Join(parts, " ")
}

// buildTemplateSets builds one template set per page file.
//
// The sets are DERIVED from the files present, not from a hard-coded list:
// layout.html and partials.html are shared, and every other templates/<name>.html
// yields a set of layout.html + partials.html + <name>.html, keyed by <name>.
// This is what lets a later task add a page by adding a file plus a handler and
// never touch this function.
//
// The pages get one set EACH, never one set together, because every page file
// defines the same {{define "page"}}; parsing them into a single set would make
// each page's body overwrite the last.
func buildTemplateSets() (map[string]*template.Template, error) {
	shared := []string{"templates/layout.html", "templates/partials.html"}
	entries, err := fs.Glob(templateSources, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("serve: glob templates: %w", err)
	}

	sets := make(map[string]*template.Template)
	for _, entry := range entries {
		name := strings.TrimSuffix(strings.TrimPrefix(entry, "templates/"), ".html")
		if name == "layout" || name == "partials" {
			continue
		}
		files := append(append([]string{}, shared...), entry)
		tmpl, err := template.New(name).Funcs(templateFuncs()).ParseFS(templateSources, files...)
		if err != nil {
			return nil, fmt.Errorf("serve: parse template set %q: %w", name, err)
		}
		sets[name] = tmpl
	}
	return sets, nil
}

// render renders the named page's template set with data and writes the result
// to w.
//
// It renders into a bytes.Buffer and only writes to w once the whole page has
// been produced, so a template that fails partway cannot leave half a page on
// the wire. An unknown page name is an error rather than a silently blank page:
// a handler that names a page it did not ship is a programming fault this
// package refuses to hide.
func (s *Server) render(w io.Writer, page string, data any) error {
	tmpl, ok := s.templates[page]
	if !ok {
		return fmt.Errorf("serve: no template set named %q", page)
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		return fmt.Errorf("serve: render page %q: %w", page, err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("serve: write page %q: %w", page, err)
	}
	return nil
}
