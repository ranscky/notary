package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlite "modernc.org/sqlite" // registers the pure-Go "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"

	"notary/internal/record"
)

// schemaStatements create the ledger's storage. Each is guarded with IF NOT
// EXISTS so Open is idempotent against an existing database.
//
// The idempotency index is deliberately partial (WHERE idempotency_key is not empty):
// only non-empty keys are unique. Phase 3 writes records before idempotency
// exists (spec section 13), and a plain UNIQUE column constraint would reject the
// second keyless record.
var schemaStatements = []string{
	`CREATE TABLE IF NOT EXISTS records (
  seq              INTEGER PRIMARY KEY,
  id               TEXT    NOT NULL UNIQUE,
  at               TEXT    NOT NULL,
  recorded_at      TEXT    NOT NULL,
  event            TEXT    NOT NULL,
  tier             TEXT    NOT NULL,
  reason_kind      TEXT    NOT NULL,
  reason_payload   BLOB    NOT NULL,
  memory_id        TEXT    NOT NULL DEFAULT '',
  user_id          TEXT    NOT NULL DEFAULT '',
  agent_id         TEXT    NOT NULL DEFAULT '',
  app_id           TEXT    NOT NULL DEFAULT '',
  run_id           TEXT    NOT NULL DEFAULT '',
  content_hash     BLOB    NOT NULL,
  content_text     TEXT,
  content_sensitive INTEGER NOT NULL DEFAULT 0,
  idempotency_key  TEXT    NOT NULL DEFAULT '',
  prev_hash        BLOB    NOT NULL,
  hash             BLOB    NOT NULL,
  signature        BLOB    NOT NULL,
  signer_key_id    TEXT    NOT NULL
)`,
	`CREATE INDEX IF NOT EXISTS idx_records_at ON records(at)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_records_idem ON records(idempotency_key)
  WHERE idempotency_key <> ''`,
}

// selectColumns lists every records column in a fixed order. scanRecord reads
// them in exactly this order, so the two must be kept in step.
const selectColumns = `seq, id, at, recorded_at, event, tier, reason_kind, reason_payload, ` +
	`memory_id, user_id, agent_id, app_id, run_id, content_hash, content_text, ` +
	`content_sensitive, idempotency_key, prev_hash, hash, signature, signer_key_id`

// SQLiteStore is the SQLite-backed implementation of Store.
type SQLiteStore struct {
	db *sql.DB
}

// Open opens (creating it if necessary) a SQLite ledger at path and returns a
// ready-to-use store. It creates the path's parent directory when missing,
// enables WAL journalling and foreign keys, applies the schema (idempotently),
// and pings the database so a bad path fails immediately with a clear error.
func Open(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, fmt.Errorf("store: open: empty database path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: create database directory %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}

	s := &SQLiteStore{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds the driver connection string for path. foreign_keys is a
// per-connection PRAGMA, so under database/sql's connection pool it must be
// applied to every pooled connection rather than once with Exec; the driver's
// DSN applies it as each connection is established.
//
// _txlock=immediate makes every Begin issue "BEGIN IMMEDIATE", taking the
// write lock up front rather than on first write. AppendChained relies on this:
// it reads the chain head and inserts within one transaction, and the write
// lock must be held across both so no second writer can interleave between the
// read and the insert. Autocommit statements are unaffected.
//
// _busy_timeout is what makes concurrent writers serialize rather than fail.
// The driver defaults busy_timeout to 0, so with BEGIN IMMEDIATE a second
// writer that finds the write lock held returns SQLITE_BUSY immediately instead
// of waiting -- a dropped append, i.e. an audit gap. Because a ledger file is
// written one writer at a time and serialize by SQLite (spec section 7), the
// losing writer must block until the lock is free, so a busy timeout is set.
// This is the SQLite lock-wait knob, not an application retry loop (we never
// re-execute a failed append) and not connection-pool tuning (MaxOpenConns et
// al. are untouched). It only bounds how long a writer waits before a genuinely
// stuck lock surfaces as SQLITE_BUSY.
func dsn(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "_foreign_keys=1&_txlock=immediate&_busy_timeout=5000"
}

// migrate enables WAL journalling and applies the schema. It is safe to run
// against an already-initialised database.
//
// journal_mode=WAL is persistent (stored in the database file), so setting it
// once on one connection is enough. foreign_keys is per-connection, so it is
// applied to every pooled connection through the DSN instead (see dsn).
func (s *SQLiteStore) migrate() error {
	if _, err := s.db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		return fmt.Errorf("store: enable WAL: %w", err)
	}
	for _, stmt := range schemaStatements {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("store: create schema: %w", err)
		}
	}
	return nil
}

// PutRecord persists r exactly as given. It returns an error wrapping
// ErrDuplicateIdemKey when r carries a non-empty IdempotencyKey already in use,
// and otherwise a wrapped error describing the failure.
func (s *SQLiteStore) PutRecord(r record.Record) error {
	if err := insertRecord(s.db, r); err != nil {
		return fmt.Errorf("store: put record %s: %w", r.ID, err)
	}
	return nil
}

// insertSQL is the single INSERT used by every write path. PutRecord and
// AppendChained must persist a record identically, so they share it.
const insertSQL = `INSERT INTO records (
  seq, id, at, recorded_at, event, tier, reason_kind, reason_payload,
  memory_id, user_id, agent_id, app_id, run_id, content_hash, content_text,
  content_sensitive, idempotency_key, prev_hash, hash, signature, signer_key_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// insertRecord writes r through q, which may be the pool or a transaction. It
// returns the bare ErrDuplicateIdemKey on a uniqueness violation so each caller
// can wrap it with its own context.
func insertRecord(q execer, r record.Record) error {
	payload, err := r.Reason.Encode()
	if err != nil {
		return fmt.Errorf("encode reason: %w", err)
	}

	// content_text is NULL exactly when there is no content, so a keyless
	// record and one with empty content remain distinguishable.
	var contentText any
	sensitive := 0
	if r.Content != nil {
		contentText = r.Content.Text
		if r.Content.Sensitive {
			sensitive = 1
		}
	}

	// signature is NOT NULL; store a nil signature as an empty blob.
	signature := r.Signature
	if signature == nil {
		signature = []byte{}
	}

	_, err = q.Exec(insertSQL,
		r.Seq,
		string(r.ID),
		formatTime(r.At),
		formatTime(r.RecordedAt),
		string(r.Event),
		r.Reason.Tier().String(),
		string(r.Reason.Kind()),
		payload,
		r.Subject.MemoryID,
		r.Subject.Scope.UserID,
		r.Subject.Scope.AgentID,
		r.Subject.Scope.AppID,
		r.Subject.Scope.RunID,
		r.Subject.ContentHash[:],
		contentText,
		sensitive,
		string(r.IdempotencyKey),
		r.PrevHash[:],
		r.Hash[:],
		signature,
		r.SignerKeyID,
	)
	if err != nil {
		if isDuplicateIdemKey(err) {
			return ErrDuplicateIdemKey
		}
		return err
	}
	return nil
}

// AppendChained appends a record built from the current chain head in a single
// write transaction. The head read and the insert share one immediate (write-
// locked) transaction, so the chain position cannot race and no error can leave
// a partial link. On any failure the transaction is rolled back.
func (s *SQLiteStore) AppendChained(build func(prev record.Record, hasPrev bool) (record.Record, error)) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: append chained: begin transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	prev, hasPrev, err := headFrom(tx)
	if err != nil {
		return fmt.Errorf("store: append chained: read head: %w", err)
	}

	rec, err := build(prev, hasPrev)
	if err != nil {
		return err
	}

	// The store owns the chain position: it is derived from the head read under
	// the write lock, so no value supplied by build can influence it. The
	// callback is expected to have set the same Seq so it could hash over the
	// final position; the store re-asserts it here.
	if hasPrev {
		rec.Seq = prev.Seq + 1
	} else {
		rec.Seq = 0
	}

	if err := insertRecord(tx, rec); err != nil {
		return fmt.Errorf("store: append chained: insert record %s: %w", rec.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: append chained: commit: %w", err)
	}
	committed = true
	return nil
}

// GetRecord returns the record with the given ID. It returns an error wrapping
// ErrNotFound when no such record exists, and a decode error when a stored row
// has been tampered with.
func (s *SQLiteStore) GetRecord(id record.RecordID) (record.Record, error) {
	row := s.db.QueryRow(`SELECT `+selectColumns+` FROM records WHERE id = ?`, string(id))
	r, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return record.Record{}, fmt.Errorf("store: get record %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return record.Record{}, fmt.Errorf("store: get record %s: %w", id, err)
	}
	return r, nil
}

// ListRecords returns the records whose At lies in [from, to], inclusive at
// both bounds, ordered by At ascending and then by Seq ascending so the order
// is total and stable.
func (s *SQLiteStore) ListRecords(from, to time.Time) ([]record.Record, error) {
	rows, err := s.db.Query(
		`SELECT `+selectColumns+` FROM records WHERE at >= ? AND at <= ? ORDER BY at ASC, seq ASC`,
		formatTime(from), formatTime(to))
	if err != nil {
		return nil, fmt.Errorf("store: list records: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []record.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list records: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list records: %w", err)
	}
	return out, nil
}

// ListRecordsAsOf returns the records recorded at or before t -- the ledger as
// it stood at instant t -- ordered by Seq ascending. It is the knowledge-time
// view: it selects on recorded_at rather than on At, so it answers what Notary
// knew by t, not what the world looked like at t (compare ListRecords, which
// bounds on the event time At and orders by it).
//
// recorded_at is compared as TEXT, and that is correct, not an accident.
// instantLayout writes exactly nine fractional digits for every value, so all
// stored instants have the same width and SQLite's BINARY collation orders the
// text chronologically; a lexical "<=" therefore matches a chronological "<="
// and the column stays index-able. time.RFC3339Nano must NOT be used for this
// comparison: it trims trailing zeros, so a whole-second value
// ("...T12:00:00Z") and a sub-second one ("...T12:00:00.5Z") differ in width,
// and under BINARY collation 'Z' sorts after '.', which would silently drop
// in-range rows and invert order within a second. Do not "fix" this into a
// parse-and-compare; the fixed width is what makes the string comparison a
// faithful instant comparison.
//
// The bound is passed through formatTime, which applies the .UTC()
// normalisation the layout depends on. A row that cannot be decoded is an
// error here, not a tolerated entry: replay cannot render what it cannot
// decode, and silently skipping it would hide a record. This is deliberately
// stricter than SeqEntries, whose tolerance exists so verification can still
// name a tampered row.
func (s *SQLiteStore) ListRecordsAsOf(t time.Time) ([]record.Record, error) {
	rows, err := s.db.Query(
		`SELECT `+selectColumns+` FROM records WHERE recorded_at <= ? ORDER BY seq ASC`,
		formatTime(t))
	if err != nil {
		return nil, fmt.Errorf("store: list records as of: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []record.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list records as of: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list records as of: %w", err)
	}
	return out, nil
}

// ListRecordsByMemory returns every record whose subject memory is memoryID,
// ordered by Seq ascending -- the lifecycle of one memory as the ledger holds
// it, including a Reconstructed claim written long after the event it
// describes.
//
// The read filters on the memory_id column, which PutRecord writes from
// r.Subject.MemoryID, so the whole filter is one SQL predicate rather than a
// Go-side scan.
//
// An empty memoryID is NOT rejected here, and that is deliberate. A record
// with no memory writes the column's empty-string default, so an empty filter
// would match every such record -- a silent whole-ledger read masquerading as
// one memory's life. Refusing "" belongs with the caller (internal/explain
// the empty --memory check, spec §4); the store is deliberately not the only
// guard, because the store cannot know whether the empty value was meant as a
// memory id or as "no filter".
//
// A row that cannot be decoded is an error here, matching ListRecords and
// ListRecordsAsOf and NOT SeqEntries' deliberate tolerance. explain cannot
// explain what it cannot read, and silently skipping the row would hide a
// record, quietly shortening the lifecycle. The two policies sit in one file
// on purpose: SeqEntries must still name a tampered row (Seq and ID read from
// their own columns, DecodeErr set) so verification can report it rather than
// lose it, whereas this read path tolerates no undecodable row at all. Do not
// "align" them -- the difference is the point.
func (s *SQLiteStore) ListRecordsByMemory(memoryID string) ([]record.Record, error) {
	rows, err := s.db.Query(
		`SELECT `+selectColumns+` FROM records WHERE memory_id = ? ORDER BY seq ASC`,
		memoryID)
	if err != nil {
		return nil, fmt.Errorf("store: list records by memory: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []record.Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list records by memory: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list records by memory: %w", err)
	}
	return out, nil
}

// Head returns the record with the greatest Seq. It reports false with a zero
// record and a nil error when the table is empty.
func (s *SQLiteStore) Head() (record.Record, bool, error) {
	r, ok, err := headFrom(s.db)
	if err != nil {
		return record.Record{}, false, fmt.Errorf("store: head: %w", err)
	}
	return r, ok, nil
}

// SeqEntries returns every stored row in chain order (seq ascending).
//
// It differs from ListRecords in two ways that matter for verification: it
// orders by and selects on seq rather than at, so no record can fall outside a
// time window, and it does not abort on a row that cannot be decoded. Such a
// row is returned with its own Seq and ID -- read from their own columns,
// independently of the payload -- and DecodeErr set (wrapping ErrDecode), while
// a row that decodes carries the rebuilt record in Rec.
func (s *SQLiteStore) SeqEntries() ([]SeqEntry, error) {
	rows, err := s.db.Query(`SELECT ` + selectColumns + ` FROM records ORDER BY seq ASC`)
	if err != nil {
		return nil, fmt.Errorf("store: seq entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SeqEntry
	for rows.Next() {
		raw, err := scanRawRow(rows)
		if err != nil {
			return nil, fmt.Errorf("store: seq entries: %w", err)
		}
		entry := SeqEntry{Seq: uint64(raw.seq), ID: record.RecordID(raw.id)}
		if rec, derr := raw.buildRecord(); derr != nil {
			entry.DecodeErr = fmt.Errorf("%w: %w", ErrDecode, derr)
		} else {
			entry.Rec = rec
		}
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: seq entries: %w", err)
	}
	return out, nil
}

// headFrom returns the record with the greatest Seq read through q, reporting
// false with a zero record and a nil error when the table is empty. It is
// shared by Head and AppendChained, so the head is read identically wherever it
// is needed.
func headFrom(q queryRower) (record.Record, bool, error) {
	row := q.QueryRow(`SELECT ` + selectColumns + ` FROM records ORDER BY seq DESC LIMIT 1`)
	r, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return record.Record{}, false, nil
	}
	if err != nil {
		return record.Record{}, false, err
	}
	return r, true, nil
}

// ByIdemKey returns the record with the given idempotency key. It reports false
// for the empty key, for an unknown key, and when the table is empty, and never
// treats the empty key as a match.
func (s *SQLiteStore) ByIdemKey(key record.IdemKey) (record.Record, bool, error) {
	if key == "" {
		return record.Record{}, false, nil
	}
	row := s.db.QueryRow(
		`SELECT `+selectColumns+` FROM records WHERE idempotency_key = ?`, string(key))
	r, err := scanRecord(row)
	if errors.Is(err, sql.ErrNoRows) {
		return record.Record{}, false, nil
	}
	if err != nil {
		return record.Record{}, false, fmt.Errorf("store: by idempotency key %q: %w", key, err)
	}
	return r, true, nil
}

// Close releases the underlying database handle.
func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// execer is satisfied by both *sql.DB and *sql.Tx, so insertRecord can run
// either on the pool (PutRecord) or inside a transaction (AppendChained).
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// queryRower is satisfied by both *sql.DB and *sql.Tx, so headFrom can read the
// head either from the pool (Head) or inside a transaction (AppendChained).
type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// rawRow holds one row's columns exactly as scanned from the database, before
// any decoding. Splitting the scan from the rebuild (buildRecord) is what lets
// SeqEntries report a row that fails to decode by the seq and id it was read
// with, rather than losing the whole read.
type rawRow struct {
	seq              int64
	id               string
	atStr            string
	recordedAtStr    string
	event            string
	tierStr          string
	reasonKind       string
	reasonPayload    []byte
	memoryID         string
	userID           string
	agentID          string
	appID            string
	runID            string
	contentHash      []byte
	contentText      sql.NullString
	contentSensitive int64
	idemKey          string
	prevHash         []byte
	hash             []byte
	signature        []byte
	signerKeyID      string
}

// scanRawRow scans one row's columns into a rawRow in the fixed selectColumns
// order. It performs no decoding and returns no decode error: a failure here is
// a genuine read error.
func scanRawRow(row scanner) (rawRow, error) {
	var r rawRow
	err := row.Scan(
		&r.seq, &r.id, &r.atStr, &r.recordedAtStr, &r.event, &r.tierStr, &r.reasonKind,
		&r.reasonPayload, &r.memoryID, &r.userID, &r.agentID, &r.appID, &r.runID, &r.contentHash,
		&r.contentText, &r.contentSensitive, &r.idemKey, &r.prevHash, &r.hash, &r.signature,
		&r.signerKeyID,
	)
	return r, err
}

// buildRecord rebuilds a Record from a scanned row: it decodes the Reason with
// record.ParseReason and re-checks the readability columns (tier, reason_kind)
// against it. Any failure here means the stored row no longer describes a valid
// record -- a hand-edited row -- and surfaces as an error rather than a
// different claim.
func (r rawRow) buildRecord() (record.Record, error) {
	at, err := time.Parse(instantLayout, r.atStr)
	if err != nil {
		return record.Record{}, fmt.Errorf("parse at %q: %w", r.atStr, err)
	}
	recordedAt, err := time.Parse(instantLayout, r.recordedAtStr)
	if err != nil {
		return record.Record{}, fmt.Errorf("parse recorded_at %q: %w", r.recordedAtStr, err)
	}

	reason, err := record.ParseReason(r.reasonPayload)
	if err != nil {
		return record.Record{}, fmt.Errorf("decode reason: %w", err)
	}
	if want := reason.Tier().String(); want != r.tierStr {
		return record.Record{}, fmt.Errorf(
			"stored tier %q disagrees with payload tier %q", r.tierStr, want)
	}
	if want := string(reason.Kind()); want != r.reasonKind {
		return record.Record{}, fmt.Errorf(
			"stored reason_kind %q disagrees with payload kind %q", r.reasonKind, want)
	}

	ch, err := hashFromBytes("content_hash", r.contentHash)
	if err != nil {
		return record.Record{}, err
	}
	ph, err := hashFromBytes("prev_hash", r.prevHash)
	if err != nil {
		return record.Record{}, err
	}
	hh, err := hashFromBytes("hash", r.hash)
	if err != nil {
		return record.Record{}, err
	}

	rec := record.Record{
		ID:         record.RecordID(r.id),
		Seq:        uint64(r.seq),
		At:         at,
		RecordedAt: recordedAt,
		Event:      record.EventType(r.event),
		Reason:     reason,
		Subject: record.Subject{
			MemoryID: r.memoryID,
			Scope: record.Scope{
				UserID:  r.userID,
				AgentID: r.agentID,
				AppID:   r.appID,
				RunID:   r.runID,
			},
			ContentHash: ch,
		},
		IdempotencyKey: record.IdemKey(r.idemKey),
		PrevHash:       ph,
		Hash:           hh,
		Signature:      r.signature,
		SignerKeyID:    r.signerKeyID,
	}
	if r.contentText.Valid {
		rec.Content = &record.Content{
			Text:      r.contentText.String,
			Sensitive: r.contentSensitive != 0,
		}
	}
	return rec, nil
}

// scanRecord reads one row and rebuilds a Record from it. It rebuilds the
// Reason with record.ParseReason and propagates any decode error, so a row
// edited directly in SQLite fails here rather than becoming a different claim.
// It also rejects a readability column (tier, reason_kind) that disagrees with
// the decoded payload, which is what makes a hand-edited tier column
// detectable.
func scanRecord(row scanner) (record.Record, error) {
	raw, err := scanRawRow(row)
	if err != nil {
		return record.Record{}, err
	}
	return raw.buildRecord()
}

// hashFromBytes decodes a fixed-width hash column, rejecting any width other
// than the record.Hash width so a truncated hash cannot pass as a real one.
func hashFromBytes(field string, b []byte) (record.Hash, error) {
	var h record.Hash
	if len(b) != len(h) {
		return record.Hash{}, fmt.Errorf("%s is %d bytes, want %d", field, len(b), len(h))
	}
	copy(h[:], b)
	return h, nil
}

// instantLayout is the fixed-width UTC instant layout used for the at and
// recorded_at columns. Exactly nine fractional digits are always written, so
// every stored value has the same width and SQLite's BINARY collation orders
// the text chronologically.
//
// time.RFC3339Nano cannot be used here: it trims trailing zeros, so a
// whole-second value ("...T12:00:00Z") and a sub-second value
// ("...T12:00:00.5Z") have different widths and lexical order disagrees with
// chronological order ('Z' sorts after '.'), silently dropping in-range rows
// from range queries and inverting order within a second.
//
// The layout deliberately uses the Z07:00 offset form rather than a literal
// "Z": a literal Z is emitted verbatim regardless of the time's zone, so a
// forgotten .UTC() would silently render a wrong instant, whereas Z07:00
// renders the real offset (still "Z" for UTC) and would show a visible
// "+HH:MM" if .UTC() were dropped. Keep the explicit .UTC() call.
const instantLayout = "2006-01-02T15:04:05.000000000Z07:00"

// formatTime renders t as a fixed-width UTC instant so lexicographic order over
// the stored text equals chronological order and the time zone is not lost.
func formatTime(t time.Time) string {
	return t.UTC().Format(instantLayout)
}

// isDuplicateIdemKey reports whether err is a UNIQUE-constraint violation on the
// records.idempotency_key column (the partial unique index idx_records_idem).
//
// It matches the DRIVER'S STRUCTURED ERROR, not a substring of the driver's
// message. modernc.org/sqlite returns a *sqlite.Error whose Code() is the
// extended SQLite result code -- the driver enables extended result codes on
// every connection -- and a UNIQUE violation carries SQLITE_CONSTRAINT_UNIQUE
// (2067). Matching that code is what makes this robust to a driver upgrade, a
// locale, or a reworded message, any of which would otherwise silently turn a
// genuine duplicate key into a generic error and let a duplicate record through
// (breaking Ledger.Append's idempotent no-op).
//
// The code alone cannot name the offending column: a violation of the id UNIQUE
// column carries the SAME 2067 code, and the driver exposes no constraint-name
// accessor (only Error() and Code()), so the column can only come from the
// message. So, only once a unique constraint is known to have fired, the
// message is checked for the COLUMN REFERENCE alone -- idemKeyConstraintRef,
// not the whole sentence -- since "records.idempotency_key" is generated by
// SQLite from the schema and cannot appear in a message for any other failure
// once the code has already established a unique violation. Matching the bare
// column reference rather than SQLite's full sentence means a change to that
// sentence's wording cannot break duplicate detection, which is the coupling
// this function exists to avoid.
const idemKeyConstraintRef = "records.idempotency_key"

func isDuplicateIdemKey(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	if se.Code() != sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return false
	}
	return strings.Contains(se.Error(), idemKeyConstraintRef)
}
