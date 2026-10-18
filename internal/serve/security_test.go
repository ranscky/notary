package serve

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
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
	"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

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
		{"records view", http.MethodGet, "/", loopbackHost, http.StatusOK},
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
		"/?reveal=1",
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
