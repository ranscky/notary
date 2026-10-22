package serve

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/store"
)

// readFailingStore wraps the real store and fails the three reads the data routes
// use, leaving SeqEntries alone.
//
// That split is deliberate: with SeqEntries delegating, the chain verdict still
// renders, so each route reaches ITS OWN loader and fails there. A double that
// failed everything would 500 every route at the banner instead, and would prove
// nothing about the loaders.
type readFailingStore struct {
	*store.SQLiteStore
	err error
}

func (s *readFailingStore) ListRecords(_, _ time.Time) ([]record.Record, error) {
	return nil, s.err
}

func (s *readFailingStore) GetRecord(record.RecordID) (record.Record, error) {
	return record.Record{}, s.err
}

func (s *readFailingStore) ListRecordsByMemory(string) ([]record.Record, error) {
	return nil, s.err
}

// newServerWithFailingReads builds a console over a ledger whose data reads fail.
func newServerWithFailingReads(t *testing.T, f fixture, readErr error, logw *strings.Builder) *Server {
	t.Helper()
	failing := &readFailingStore{SQLiteStore: f.store, err: readErr}
	srv, err := New(Options{
		Ledger:     ledger.New(failing, nil, fixedClock),
		Store:      failing,
		GapLogPath: f.gapPath,
		Log:        logw,
		Now:        fixedClock,
	})
	require.NoError(t, err)
	return srv
}

// TestInternalErrorsAreGenericAndLogged pins that a failed read never tells the
// reader what failed, and always tells the operator.
//
// An internal error can name a file path, a SQL statement or a stored value --
// and this console's reader is a compliance reviewer, not someone sitting at a
// terminal. So every route answers with one fixed sentence, and the real error
// goes to the log together with the route that produced it, which is the only
// place it can be acted on.
//
// The three routes are tested together because they fail in three different
// loaders (loadRecords, loadRecord, loadMemory). One route passing would not mean
// the others do: the sites were separate calls before they shared a helper, and
// the next one added must not be a fourth ungenericed path.
func TestInternalErrorsAreGenericAndLogged(t *testing.T) {
	const marker = "underlying-store-failure-marker"
	underlying := errors.New(marker)

	targets := []struct {
		name   string
		route  string
		target string
	}{
		{"records", "/records", "/records"},
		{"record", "/record", "/record?id=fixture-observed"},
		{"memory", "/memory", "/memory?id=mem-alpha"},
	}

	for _, tc := range targets {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			var logged strings.Builder
			srv := newServerWithFailingReads(t, f, underlying, &logged)

			rr := get(t, srv.Handler(), tc.target)
			require.Equal(t, http.StatusInternalServerError, rr.Code,
				"%s must report a failed read as 500", tc.target)

			// Exactly the generic sentence -- http.Error appends one newline.
			assert.Equal(t, "internal server error\n", rr.Body.String(),
				"the body must be the fixed sentence, with nothing appended")

			assert.NotContains(t, rr.Body.String(), marker,
				"the underlying error must never reach the reader")
			assert.NotContains(t, rr.Body.String(), "serve:",
				"no internal wrapping prefix may reach the reader")

			// The operator gets the detail, and gets it once, with the route.
			// `route` is the PATH the log names -- a query string is the reader's
			// input, not the route, and repeating it in a log line invites the very
			// leakage this helper prevents.
			assert.Contains(t, logged.String(), "serve: "+tc.route+":",
				"the log must name the route that failed")
			assert.Contains(t, logged.String(), marker,
				"the log must carry the underlying error")
			assert.Equal(t, 1, strings.Count(logged.String(), marker),
				"the error must be logged once, not once per wrapping layer")
		})
	}
}
