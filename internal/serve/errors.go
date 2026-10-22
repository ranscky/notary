package serve

import (
	"fmt"
	"net/http"
)

// internalErrorMessage is the only body an internal failure ever sends.
//
// It is a constant rather than a literal at each call site so the console has one
// answer to "something broke", and so no route can quietly grow its own.
const internalErrorMessage = "internal server error"

// internalError reports a failed read without telling the reader what failed.
//
// It logs the real error once -- with the route that produced it -- and sends the
// fixed sentence above. The split is the point: an internal error can carry a file
// path, a SQL fragment or a stored value, and this console's reader is a compliance
// reviewer, not an operator sitting at a terminal. The route in the log is what
// makes the detail useful to the person who can act on it; the fixed body is what
// stops the response shape from disclosing which read broke, or how.
//
// It is deliberately NOT used for input validation. A bad filter or a missing id is
// the reader's own business, and those paths render a usage message on the page
// through data.Err -- they never reach here.
func (s *Server) internalError(w http.ResponseWriter, where string, err error) {
	fmt.Fprintf(s.log, "serve: %s: %v\n", where, err)
	http.Error(w, internalErrorMessage, http.StatusInternalServerError)
}
