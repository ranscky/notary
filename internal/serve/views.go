package serve

import (
	"errors"
	"fmt"
	"net/url"
	"time"

	"notary/internal/export"
	"notary/internal/record"
)

// ErrMissingID reports that a view was asked for a record or memory with no id.
//
// It is a usage error, not a data error: an empty record id or an empty memory
// id names nothing, and reading on would be actively wrong for a memory -- the
// store's memory_id column defaults to the empty string, which it matches, so
// an empty filter would quietly list the whole ledger wearing one memory's
// clothes (design §5; the same guard explain owns). The HTTP layer maps this to
// a 400; a missing record id is store.ErrNotFound and maps to a 404 instead.
var ErrMissingID = errors.New("serve: a record or memory id is required")

// tierBadge is a record's visibility tier as a view shows it: the tier's name
// as text, and the CSS class that colours it. The name is on the badge as well
// as the colour so it survives colour-blindness, a greyscale printout and a
// screenshot (design §6).
type tierBadge struct {
	Name  string
	Class string
}

// badgeFor maps a record's visibility tier to its badge.
//
// Precondition: t is one of the three valid tiers. Every row's Tier comes from
// export.Render's Line.Tier, and Render rejects a record whose tier is invalid
// before any line exists, so an invalid tier can never reach badgeFor. That is
// why there is deliberately no fourth case: a case for "invalid" would be dead
// code implying badgeFor, rather than Render, were the guard. The trailing
// return is unreachable and returns the zero badge rather than panicking,
// because this is library code (.clinerules §4).
func badgeFor(t record.VisibilityTier) tierBadge {
	switch t {
	case record.Observed:
		return tierBadge{Name: "Observed", Class: "tier-observed"}
	case record.Reconstructed:
		return tierBadge{Name: "Reconstructed", Class: "tier-reconstructed"}
	case record.Internal:
		return tierBadge{Name: "Internal", Class: "tier-internal"}
	}
	return tierBadge{}
}

// recordRow is one record as every list view shows it: the export.Line that
// carries the claim and its tier badge.
//
// Line is the single source of the claim's tier, redaction and sentence --
// produced by export.Render and never recomputed here -- so a row cannot drift
// from the CLI. It also carries the two ids a view links by: Line.ID names the
// record's own page and Line.MemoryID names its memory's page (empty for a
// record whose subject names no memory -- the store's memory_id DEFAULT --
// which a template renders as no link at all).
//
// A view builds those links with the recordHref and memoryHref template
// functions -- href="{{recordHref .Line.ID}}" -- never by interpolating the raw
// id, and never from a pre-joined string built anywhere else. Those helpers
// url.QueryEscape the id exactly ONCE, and a link hands the SAME string to every
// URL attribute it carries (both the href and any hx-get), so the two cannot
// diverge. This cannot be delegated to html/template's contextual escaper: it
// URL-encodes only its fixed set of URL attributes (href, action, src, ...) and
// merely HTML-escapes a custom attribute such as htmx's hx-get, so an id left
// raw for the escaper starts a URL fragment in hx-get, the server never sees the
// rest of the id, and the request fetches the wrong record (or 404s). That is
// why recordRow deliberately holds no href field: the escaped shape lives in the
// helpers, not in a pre-joined string a view could hand around.
type recordRow struct {
	Line export.Line
	Tier tierBadge
}

// filter is a records-view query: the date range, the scope, and whether
// sensitive content is revealed. It is parsed once from the request query into
// these plain values, so the loading functions depend on a validated filter
// rather than on the raw query.
type filter struct {
	From, To time.Time
	Scope    record.Scope
	Reveal   bool
}

// parseFilter turns a request's query into a validated filter.
//
// It accepts from and to as either RFC3339 or a bare calendar date
// (2006-01-02). A date-only from is the start of that day (00:00:00Z); a
// date-only to is the END of that day (...T23:59:59.999999999Z), because the
// store's ListRecords is inclusive at BOTH bounds, so a date-only to that
// stopped at midnight would silently drop the named day's records.
//
// The defaults, when a bound is absent, are the last thirty days: from =
// now - 30*24h, to = now (design §3 record 7 -- the console gives the browser a
// sensible window rather than export's demanding error). A reversed range is an
// error naming both bounds, and a span wider than export.DefaultMaxSpan is an
// error naming that same cap -- the constant export itself applies, so the
// console's limit and the CLI's cannot drift. Reveal is exactly q.Get("reveal")
// == "1".
func (s *Server) parseFilter(q url.Values) (filter, error) {
	now := s.now()
	f := filter{
		From: now.Add(-30 * 24 * time.Hour),
		To:   now,
		Scope: record.Scope{
			UserID:  q.Get("user_id"),
			AgentID: q.Get("agent_id"),
			AppID:   q.Get("app_id"),
			RunID:   q.Get("run_id"),
		},
		Reveal: q.Get("reveal") == "1",
	}

	if v := q.Get("from"); v != "" {
		from, err := parseBound(v, false)
		if err != nil {
			return filter{}, fmt.Errorf("serve: from: %w", err)
		}
		f.From = from
	}
	if v := q.Get("to"); v != "" {
		to, err := parseBound(v, true)
		if err != nil {
			return filter{}, fmt.Errorf("serve: to: %w", err)
		}
		f.To = to
	}

	if f.To.Before(f.From) {
		return filter{}, fmt.Errorf("serve: to %s is before from %s",
			f.To.Format(time.RFC3339Nano), f.From.Format(time.RFC3339Nano))
	}
	if span := f.To.Sub(f.From); span > export.DefaultMaxSpan {
		return filter{}, fmt.Errorf("serve: range %s exceeds max span %s", span, export.DefaultMaxSpan)
	}
	return f, nil
}

// parseBound parses one range bound: RFC3339, or a bare calendar date. A
// date-only value parses to that day's start (00:00:00Z); when endOfDay is
// true it is advanced to the END of that day, so the store's inclusive upper
// bound covers the whole named day rather than only its first instant.
func parseBound(v string, endOfDay bool) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	d, err := time.Parse("2006-01-02", v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is neither an RFC3339 instant nor a YYYY-MM-DD date", v)
	}
	if endOfDay {
		return d.AddDate(0, 0, 1).Add(-time.Nanosecond), nil
	}
	return d, nil
}

// loadRecords returns the records view's rows: the records in f's range, in
// the ledger's read order, narrowed by scope and rendered as export.Lines.
//
// The scope narrowing runs AFTER the range read, with export.ScopeMatches,
// exactly as Exporter.Export narrows -- the console and the CLI must select the
// same records from the same range, and the differential test
// (TestLoadRecordsMatchesTheExportPath) is what holds the two together.
func (s *Server) loadRecords(f filter) ([]recordRow, error) {
	records, err := s.ledger.ListRecords(f.From, f.To)
	if err != nil {
		return nil, fmt.Errorf("serve: list records: %w", err)
	}

	rows := make([]recordRow, 0, len(records))
	for _, rec := range records {
		if !export.ScopeMatches(f.Scope, rec.Subject.Scope) {
			continue
		}
		row, err := s.rowFor(rec, f.Reveal)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// loadRecord returns the single-record view's row for id.
//
// An empty id is ErrMissingID before the store is touched -- there is no record
// to read and the store would do nothing sensible with "". An id that names no
// record keeps store.ErrNotFound in its chain (wrapped with %w), so the HTTP
// layer can tell "you asked wrong" (400) from "there is no such record" (404).
func (s *Server) loadRecord(id record.RecordID, reveal bool) (recordRow, error) {
	if id == "" {
		return recordRow{}, ErrMissingID
	}
	rec, err := s.ledger.GetRecord(id)
	if err != nil {
		return recordRow{}, fmt.Errorf("serve: get record %s: %w", id, err)
	}
	return s.rowFor(rec, reveal)
}

// loadMemory returns the per-memory view's rows: every record whose subject
// memory is id, in Seq order, rendered as export.Lines.
//
// An empty id is ErrMissingID before the store is touched. The store does NOT
// guard this (ListRecordsByMemory matches the empty memory_id column's
// DEFAULT), which is exactly why the guard lives here: an empty filter would
// quietly list the whole ledger as if it were one memory's life.
func (s *Server) loadMemory(id string, reveal bool) ([]recordRow, error) {
	if id == "" {
		return nil, ErrMissingID
	}
	records, err := s.ledger.ListRecordsByMemory(id)
	if err != nil {
		return nil, fmt.Errorf("serve: list records for memory %s: %w", id, err)
	}

	rows := make([]recordRow, 0, len(records))
	for _, rec := range records {
		row, err := s.rowFor(rec, reveal)
		if err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// rowFor projects one record into a recordRow. It is the single place a view
// turns a record into a row, so every row's Line is export.Render(rec, reveal)
// and its badge is badgeFor of that Line's own tier -- the console owns no
// tier, redaction or phrasing of its own.
func (s *Server) rowFor(rec record.Record, reveal bool) (recordRow, error) {
	line, err := export.Render(rec, reveal)
	if err != nil {
		return recordRow{}, fmt.Errorf("serve: render record %s: %w", rec.ID, err)
	}
	return recordRow{
		Line: line,
		Tier: badgeFor(line.Tier),
	}, nil
}

// widerRange is the range the records view offers when the requested window came
// back empty but the ledger is not: the href to ask for it, the ledger's own
// extent that the page names in words, and the bounds the href actually covers.
//
// From/To are the OFFERED bounds and may be narrower than Earliest/Latest: a
// ledger wider than the view's span limit is offered its most recent stretch,
// and Clamped records that so the page can say so rather than implying the whole
// ledger is about to appear.
type widerRange struct {
	// Href is the query string that requests the offered range. It is built
	// with url.Values.Encode over date values only, so it carries no raw id,
	// and it is handed to the template whole.
	Href string
	// Earliest and Latest are the ledger's true extent, for the sentence.
	Earliest time.Time
	Latest   time.Time
	// From and To are the bounds Href requests.
	From time.Time
	To   time.Time
	// Count is how many records the ledger holds.
	Count int
	// Clamped reports that From/To are narrower than Earliest/Latest because
	// the ledger's span exceeds the view's limit.
	Clamped bool
}

// ledgerExtent is the range of event times a ledger holds, and how many records
// it holds there.
type ledgerExtent struct {
	Earliest time.Time
	Latest   time.Time
	Count    int
}

// extent reads the ledger's own extent: the earliest and latest event time over
// its decodable records, and how many it holds.
//
// It walks every stored row, so it is called ONLY on the records view's empty
// path -- a window that returned rows never needs it, and the common case must
// not pay for the rare one. It reads through the store's seq-ordered accessor,
// the same one `notary verify` and the gap report use, so a corrupt row cannot
// hide the records around it: such a row has no identity and no event time to
// bound, so it is skipped, and a ledger whose rows are ALL undecodable reports
// ok false rather than a zero extent that would read as "records from year 1".
func (s *Server) extent() (ledgerExtent, bool, error) {
	entries, err := s.store.SeqEntries()
	if err != nil {
		return ledgerExtent{}, false, fmt.Errorf("serve: read ledger extent: %w", err)
	}
	var ext ledgerExtent
	for _, e := range entries {
		if e.DecodeErr != nil {
			continue
		}
		if ext.Count == 0 || e.Rec.At.Before(ext.Earliest) {
			ext.Earliest = e.Rec.At
		}
		if ext.Count == 0 || e.Rec.At.After(ext.Latest) {
			ext.Latest = e.Rec.At
		}
		ext.Count++
	}
	return ext, ext.Count > 0, nil
}

// widerWindow returns the range to offer when the requested window held no
// records: the ledger's own extent, which is the range that shows the reviewer
// what is actually there. A nil result means there is nothing to offer -- the
// ledger is empty, or none of its rows decode -- and the page then says only
// that the window was empty.
//
// The offered range is clamped to export.DefaultMaxSpan, the SAME cap the
// filter enforces, because offering a range the page would immediately refuse
// as over-cap would be a dead end: the reviewer follows the link and gets an
// error. A ledger wider than the cap is therefore offered its most recent
// stretch, and Clamped tells the page to say so. This is the one place the
// console reads every stored row on a render, which is why it lives on the
// empty path only.
func (s *Server) widerWindow() (*widerRange, error) {
	ext, ok, err := s.extent()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}

	from, to := ext.Earliest, ext.Latest
	clamped := false
	if span := to.Sub(from); span > export.DefaultMaxSpan {
		from = to.Add(-export.DefaultMaxSpan)
		clamped = true
	}

	q := url.Values{}
	q.Set("from", from.Format(time.RFC3339))
	q.Set("to", to.Format(time.RFC3339))
	return &widerRange{
		Href:     "/?" + q.Encode(),
		Earliest: ext.Earliest,
		Latest:   ext.Latest,
		From:     from,
		To:       to,
		Count:    ext.Count,
		Clamped:  clamped,
	}, nil
}
