package store

import (
	"database/sql"
	"fmt"
	"time"

	"notary/internal/record"
)

// Stats is the ledger's shape at a glance: how many records it holds, when it
// starts and ends, and how those records split across the three visibility tiers.
//
// It is what the console's Overview page shows, and it is deliberately the only
// place in the tree those numbers are computed, so no two surfaces can disagree
// about them.
type Stats struct {
	// Count is the number of stored rows.
	Count int
	// Earliest and Latest are the smallest and largest event time (At) across
	// those rows -- not the first and last chain positions. A record written late
	// about an old event belongs to the old event's window, and a reader asking
	// what period this ledger covers means the events, not the writes. Both are
	// zero when Count is 0.
	Earliest time.Time
	Latest   time.Time
	// ByTier counts rows per visibility tier. A tier the ledger has no rows for is
	// absent from the map rather than present with a zero, so "none of these" and
	// "some of these" cannot be confused.
	ByTier map[record.VisibilityTier]int
}

// LedgerStats reads the ledger's aggregate shape in two small queries rather than
// by walking its rows.
//
// A walk is what the console's extent() used to do, and it is the wrong shape for
// a page that shows these numbers on every load: its cost grows with the ledger
// while its answer does not. SQLite answers both questions without leaving the
// index.
func (s *SQLiteStore) LedgerStats() (Stats, error) {
	stats := Stats{ByTier: map[record.VisibilityTier]int{}}

	var (
		count    int
		earliest sql.NullString
		latest   sql.NullString
	)
	if err := s.db.QueryRow(
		`SELECT COUNT(*), MIN(at), MAX(at) FROM records`,
	).Scan(&count, &earliest, &latest); err != nil {
		return Stats{}, fmt.Errorf("store: ledger stats: %w", err)
	}
	stats.Count = count

	// MIN and MAX over an empty table are NULL rather than an error, and an empty
	// ledger is a readable ledger: a zero time is the honest bound for one.
	if earliest.Valid {
		at, err := time.Parse(instantLayout, earliest.String)
		if err != nil {
			return Stats{}, fmt.Errorf("store: ledger stats: parse earliest at %q: %w", earliest.String, err)
		}
		stats.Earliest = at
	}
	if latest.Valid {
		at, err := time.Parse(instantLayout, latest.String)
		if err != nil {
			return Stats{}, fmt.Errorf("store: ledger stats: parse latest at %q: %w", latest.String, err)
		}
		stats.Latest = at
	}

	rows, err := s.db.Query(`SELECT tier, COUNT(*) FROM records GROUP BY tier`)
	if err != nil {
		return Stats{}, fmt.Errorf("store: ledger stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var (
			tierStr string
			n       int
		)
		if err := rows.Scan(&tierStr, &n); err != nil {
			return Stats{}, fmt.Errorf("store: ledger stats: %w", err)
		}
		tier, ok := tierFromWire(tierStr)
		if !ok {
			// A stored tier that is not one of the three cannot be bucketed, and
			// it only happens to a row edited behind the ledger's back. The honest
			// answer is that the split cannot be stated -- not a split that
			// silently omits a record the total counts. notary verify names such a
			// row as a break, so the reader is sent to the page that explains it.
			return Stats{}, fmt.Errorf("store: ledger stats: unrecognised stored tier %q", tierStr)
		}
		stats.ByTier[tier] = n
	}
	if err := rows.Err(); err != nil {
		return Stats{}, fmt.Errorf("store: ledger stats: %w", err)
	}
	return stats, nil
}

// tierFromWire maps a stored tier string back to its VisibilityTier -- the
// inverse of VisibilityTier.String(). It lives here rather than in record
// because the store is the only package that reads the raw column, and record
// deliberately offers no way to construct a tier from a bare string.
func tierFromWire(s string) (record.VisibilityTier, bool) {
	switch s {
	case record.Observed.String():
		return record.Observed, true
	case record.Reconstructed.String():
		return record.Reconstructed, true
	case record.Internal.String():
		return record.Internal, true
	default:
		return record.VisibilityTier{}, false
	}
}
