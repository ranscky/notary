package serve

import "net/http"

// The response headers every route sends.
//
// They are set by the one middleware below, wrapped around the whole handler
// chain, rather than by each handler. Two things follow, and both are the
// reason it is shaped this way: a route added later cannot forget them, and the
// paths that never reach a handler -- the 405 from the method guard, the 403
// from the Host guard, every 404 from http.NotFound -- carry them too. Setting
// them inside the handlers would leave exactly those responses bare.
const (
	// contentSecurityPolicy allows only what the console actually loads: its own
	// stylesheet, its own script and its own images, all same-origin. There is no
	// 'unsafe-inline' and no 'unsafe-eval', so an injected script or an inline
	// style has no way in; default-src 'none' means anything the policy does not
	// name is refused rather than allowed, so a new resource type is a deliberate
	// edit here. form-action and base-uri are pinned to 'self' so a form cannot be
	// redirected off-origin and a <base> cannot be injected to re-point relative
	// URLs, and frame-ancestors 'none' refuses to be framed at all -- the
	// clickjacking guard, and the modern replacement for X-Frame-Options.
	//
	// htmx is configured with allowEval false rather than this policy being
	// widened to permit eval: the console's templates use no hx-on attributes and
	// no js: expressions, so eval would buy nothing and permitting it would be
	// surface with no purpose. See the htmx-config meta tag in layout.html.
	contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
		"form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

	// cacheControlNoStore is why a revealed page does not outlive the view. A
	// page rendered with reveal=1 can carry the sensitive content the ledger
	// otherwise withholds, and any stored copy -- in the browser cache, or
	// reachable from the session history -- would still hold it after the reader
	// navigated away. It applies to every response rather than to revealing ones
	// alone, because a header that depends on a query parameter is a header that
	// can be got wrong silently.
	cacheControlNoStore = "no-store"

	// referrerPolicyNoReferrer keeps a record id or a memory id out of the
	// Referer header of any request the page makes. The ids are the subjects of
	// audit records, so they are worth not leaking to wherever a link points.
	referrerPolicyNoReferrer = "no-referrer"

	// contentTypeOptionsNoSniff stops the browser second-guessing the
	// Content-Type. The console serves exactly two content types it chose
	// itself, and an asset whose bytes were ever reinterpreted as HTML would run
	// with the page's origin.
	contentTypeOptionsNoSniff = "nosniff"
)

// securityHeaders sets the console's response headers on every response.
//
// It must stay OUTERMOST in Handler, outside guardMethods and guardHost: those
// two answer 405 and 403 without calling onward, so a middleware placed inside
// them would leave every refusal bare. TestEveryRouteCarriesTheSecurityHeaders
// asserts the refusals specifically, which is what holds this ordering.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", cacheControlNoStore)
		h.Set("X-Content-Type-Options", contentTypeOptionsNoSniff)
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("Referrer-Policy", referrerPolicyNoReferrer)
		next.ServeHTTP(w, r)
	})
}
