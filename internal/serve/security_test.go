package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wantCSP is the exact policy the console must send. It is written out here as
// a literal rather than read from the implementation, so the security contract
// and the code have to be changed together: a test that imported the value
// would agree with whatever the code happened to say, which is precisely the
// property a contract test must not have.
const wantCSP = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// assertSecurityHeaders asserts the four headers on one response.
//
// It is a helper rather than four lines inlined at each call site because the
// same four must hold everywhere, and naming that once is what makes "the
// console sends these" a single claim instead of a repeated one.
func assertSecurityHeaders(t *testing.T, rr interface {
	Header() http.Header
}) {
	t.Helper()
	h := rr.Header()

	assert.Equal(t, "no-store", h.Get("Cache-Control"),
		"responses must never be cached: a revealed page would otherwise outlive the view "+
			"in the browser cache or the session history")
	assert.Equal(t, "nosniff", h.Get("X-Content-Type-Options"),
		"the browser must not second-guess a Content-Type")
	assert.Equal(t, wantCSP, h.Get("Content-Security-Policy"),
		"the policy must be exactly the recorded one, so the documented pages are the only "+
			"thing that can load")
	assert.Equal(t, "no-referrer", h.Get("Referrer-Policy"),
		"a record id or memory id in a URL must not travel in a Referer header")
}

// TestEveryRouteCarriesTheSecurityHeaders sweeps every route the console
// registers: the five page views, both assets, an unknown path, a rejected
// method and a rejected Host.
//
// It is a sweep rather than a per-handler assertion because the headers are set
// in ONE place. If that placement is dropped, or moved inside one of the guards,
// the refusal paths lose the headers while the page paths keep them -- and only
// a table that includes the refusals notices. That is the failure this test
// exists to catch, so the 404, the 405 and the 403 are the load-bearing rows.
func TestEveryRouteCarriesTheSecurityHeaders(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	cases := []struct {
		name   string
		method string
		target string
		host   string
		status int
	}{
		{"overview", http.MethodGet, "/", loopbackHost, http.StatusOK},
		{"records view", http.MethodGet, "/records", loopbackHost, http.StatusOK},
		{"as-of view", http.MethodGet, "/asof", loopbackHost, http.StatusOK},
		{"as-of results", http.MethodGet, "/asof?date=2026-09-28&time=12:00&zone=UTC", loopbackHost, http.StatusOK},
		{"record view", http.MethodGet, "/record?id=fixture-observed", loopbackHost, http.StatusOK},
		{"memory view", http.MethodGet, "/memory?id=mem-alpha", loopbackHost, http.StatusOK},
		{"gaps view", http.MethodGet, "/gaps", loopbackHost, http.StatusOK},
		{"verify view", http.MethodGet, "/verify", loopbackHost, http.StatusOK},
		{"stylesheet", http.MethodGet, "/assets/app.css", loopbackHost, http.StatusOK},
		{"htmx", http.MethodGet, "/assets/htmx.min.js", loopbackHost, http.StatusOK},
		{"unknown path", http.MethodGet, "/no-such-route", loopbackHost, http.StatusNotFound},
		{"asset the binary did not embed", http.MethodGet, "/assets/nope.css", loopbackHost, http.StatusNotFound},
		{"non-GET method", http.MethodPost, "/", loopbackHost, http.StatusMethodNotAllowed},
		{"non-loopback Host", http.MethodGet, "/", "evil.example", http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := request(t, h, tc.method, tc.target, tc.host)
			require.Equal(t, tc.status, rr.Code, "%s %s must answer %d", tc.method, tc.target, tc.status)
			assertSecurityHeaders(t, rr)
		})
	}
}

// parseCSP splits a policy into its directives, so a test can ask what the
// policy says by name rather than matching the whole string. The values are the
// source lists; an empty value is a directive that takes none.
func parseCSP(policy string) map[string]string {
	directives := map[string]string{}
	for _, part := range strings.Split(policy, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, sources, _ := strings.Cut(part, " ")
		directives[name] = strings.TrimSpace(sources)
	}
	return directives
}

// goTemplateCommentRe matches a Go template comment. Comments are stripped
// before markup is scanned because a comment is not markup: layout.html's own
// comments name hx-get and, once, a raw script tag, and a scanner that counted
// those would demand directives for things no browser ever fetches.
var goTemplateCommentRe = regexp.MustCompile(`(?s)\{\{/\*.*?\*/\}\}`)

// resourceDirective maps markup that pulls a resource to the CSP directive that
// governs it. The table is the point: the test reads what the console LOADS
// instead of a list of directive names someone remembered to keep in step.
var resourceDirective = []struct {
	directive string
	pattern   *regexp.Regexp
	what      string
}{
	{"script-src", regexp.MustCompile(`<script[^>]*\ssrc=`), "a script source"},
	{"style-src", regexp.MustCompile(`<link[^>]*\srel=["']?stylesheet`), "a linked stylesheet"},
	{"style-src", regexp.MustCompile(`<style[\s>]|\sstyle=["']`), "an inline style"},
	{"img-src", regexp.MustCompile(`<img[\s>]`), "an image"},
	{"connect-src", regexp.MustCompile(`\shx-(get|post|put|patch|delete)=`), "an htmx request"},
	{"font-src", regexp.MustCompile(`@font-face`), "a font face"},
}

// TestCSPCarriesADirectiveForEveryResourceTheConsoleUses ties the policy to what
// the console actually loads.
//
// default-src 'none' refuses anything the policy does not name, which is the
// right default but means a resource type is only as supported as its directive.
// htmx's requests are the case that proved it: every hx-get is a fetch, so with
// no connect-src the browser blocks it and the toggle silently degrades to a
// full page load. Nothing appears broken -- the swap just never happens.
//
// So this derives the requirement from the markup rather than asserting a list
// of names. Adding an image or a font face to a page fails this test until the
// policy names it, which is the only way the two stay in step.
func TestCSPCarriesADirectiveForEveryResourceTheConsoleUses(t *testing.T) {
	f := newFixture(t)
	policy := parseCSP(get(t, f.server.Handler(), "/").Header().Get("Content-Security-Policy"))
	require.NotEmpty(t, policy, "the policy must parse into directives")

	// What the markup requires, and the evidence for each.
	required := map[string]string{}
	files, err := fs.Glob(templateSources, "templates/*.html")
	require.NoError(t, err)
	require.NotEmpty(t, files, "the templates must be embedded for this test to mean anything")
	for _, name := range files {
		raw, err := fs.ReadFile(templateSources, name)
		require.NoError(t, err)
		markup := goTemplateCommentRe.ReplaceAll(raw, nil)
		for _, entry := range resourceDirective {
			if _, seen := required[entry.directive]; seen {
				continue
			}
			if entry.pattern.Match(markup) {
				required[entry.directive] = fmt.Sprintf("%s (%s)", entry.what, name)
			}
		}
	}

	// The stylesheet can pull a resource too, and it is embedded rather than
	// templated, so it is scanned as bytes.
	css, err := fs.ReadFile(assetSources, "assets/app.css")
	require.NoError(t, err)
	if _, seen := required["img-src"]; !seen && regexp.MustCompile(`url\(`).Match(css) {
		required["img-src"] = "a url(...) in assets/app.css"
	}

	require.NotEmpty(t, required, "the console must load something")
	for directive, why := range required {
		assert.Contains(t, policy, directive,
			"the console loads %s, so the policy must name %s: default-src 'none' refuses anything unnamed",
			why, directive)
	}

	// The directives the console is allowed to use, spelled out. Most are also
	// derived above; img-src is asserted even though no template uses an image
	// today, because the policy permits one and dropping the directive would
	// break that page at runtime instead of here.
	for _, directive := range []string{"script-src", "style-src", "img-src", "connect-src"} {
		assert.Contains(t, policy, directive, "the console relies on %s", directive)
	}
}

// TestRevealedPageIsNotStored pins the reason Cache-Control is no-store at all.
//
// A page rendered with reveal=1 can carry the sensitive content the ledger
// otherwise withholds, so a copy kept in the browser cache or reachable from
// session history would outlive the view the reader was in. The header is
// asserted on the revealing URLs specifically: removing the middleware because
// "every page has it anyway" must fail here, not pass quietly because a
// non-revealing page happened to be the one sampled.
func TestRevealedPageIsNotStored(t *testing.T) {
	f := newFixture(t)
	h := f.server.Handler()

	for _, target := range []string{
		"/records?reveal=1",
		"/record?id=fixture-observed&reveal=1",
		"/memory?id=mem-alpha&reveal=1",
	} {
		t.Run(target, func(t *testing.T) {
			rr := get(t, h, target)
			require.Equal(t, http.StatusOK, rr.Code, "%s must render", target)
			assert.Equal(t, "no-store", rr.Header().Get("Cache-Control"),
				"a page that can show revealed content must never be stored")
		})
	}
}

// htmxConfigRe pulls the htmx config JSON out of a rendered page. The content
// attribute is single-quoted so the JSON inside it can use double quotes, which
// is what htmx's own documentation uses and what its parser expects.
var htmxConfigRe = regexp.MustCompile(`<meta name="htmx-config" content='([^']*)'>`)

// TestHTMXIsConfiguredWithoutEvalOrInjection pins the configuration the CSP
// depends on.
//
// The policy in security.go carries no 'unsafe-eval' and no 'unsafe-inline', so
// any htmx feature that needed eval -- hx-on attributes, js: expressions -- or
// that injected a <style> element into the document would be refused by the
// browser. The choice recorded here is to tell the library not to use those
// features rather than to widen the policy to suit it: the console's templates
// use neither, so permitting them would be surface bought for nothing.
//
// It asserts the config actually reached the page, because a meta tag lost in a
// refactor would leave the console quietly relying on a capability the browser
// then refuses -- and the failure would be a dead filter, not an error.
func TestHTMXIsConfiguredWithoutEvalOrInjection(t *testing.T) {
	f := newFixture(t)
	body := get(t, f.server.Handler(), "/").Body.String()

	m := htmxConfigRe.FindStringSubmatch(body)
	require.NotNil(t, m, "every page must carry the htmx-config meta tag")

	var cfg map[string]bool
	require.NoError(t, json.Unmarshal([]byte(m[1]), &cfg), "the config must be valid JSON: %q", m[1])

	assert.False(t, cfg["allowEval"],
		"htmx eval must be off: the policy has no 'unsafe-eval', so anything that needed it would fail")
	assert.False(t, cfg["allowScriptTags"],
		"htmx must not run <script> found in swapped content")
	assert.False(t, cfg["includeIndicatorStyles"],
		"htmx must not inject a <style> element: the policy has no 'unsafe-inline'")
}

// TestServedHtmxMatchesTheRecordedHash pins the vendored htmx to the digest and
// length the package records for it.
//
// htmx is the only script the console ships, and it is embedded BY NAME
// precisely so that what a reviewer approved is what runs. A silent edit to the
// minified file would otherwise change the console's behaviour with nothing
// failing; this is that nothing. The hash is taken over the bytes the handler
// actually serves -- not the file on disk -- because the served bytes are what
// a browser executes.
func TestServedHtmxMatchesTheRecordedHash(t *testing.T) {
	f := newFixture(t)
	body := get(t, f.server.Handler(), "/assets/htmx.min.js").Body.Bytes()

	require.Len(t, body, htmxSize, "the served htmx must be the length the package records")
	sum := sha256.Sum256(body)
	assert.Equal(t, htmxSHA256, hex.EncodeToString(sum[:]),
		"the served htmx must hash to the value the package records for it")
}
