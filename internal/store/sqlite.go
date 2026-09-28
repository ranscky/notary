package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the pure-Go "sqlite" driver

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
func dsn(path string) string {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "_foreign_keys=1&_txlock=immediate"
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
		return fmt.Errorf("store: append chained: insert: %w", err)
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

// Head returns the record with the greatest Seq. It reports false with a zero
// record and a nil error when the table is empty.
func (s *SQLiteStore) Head() (record.Record, bool, error) {
	r, ok, err := headFrom(s.db)
	if err != nil {
		return record.Record{}, false, fmt.Errorf("store: head: %w", err)
	}
	return r, ok, nil
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

// scanRecord reads one row and rebuilds a Record from it. It rebuilds the
// Reason with record.ParseReason and propagates any decode error, so a row
// edited directly in SQLite fails here rather than becoming a different claim.
// It also rejects a readability column (tier, reason_kind) that disagrees with
// the decoded payload, which is what makes a hand-edited tier column
// detectable.
func scanRecord(row scanner) (record.Record, error) {
	var (
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
	)

	if err := row.Scan(
		&seq, &id, &atStr, &recordedAtStr, &event, &tierStr, &reasonKind,
		&reasonPayload, &memoryID, &userID, &agentID, &appID, &runID, &contentHash,
		&contentText, &contentSensitive, &idemKey, &prevHash, &hash, &signature,
		&signerKeyID,
	); err != nil {
		return record.Record{}, err
	}

	at, err := time.Parse(instantLayout, atStr)
	if err != nil {
		return record.Record{}, fmt.Errorf("parse at %q: %w", atStr, err)
	}
	recordedAt, err := time.Parse(instantLayout, recordedAtStr)
	if err != nil {
		return record.Record{}, fmt.Errorf("parse recorded_at %q: %w", recordedAtStr, err)
	}

	reason, err := record.ParseReason(reasonPayload)
	if err != nil {
		return record.Record{}, fmt.Errorf("decode reason: %w", err)
	}
	if want := reason.Tier().String(); want != tierStr {
		return record.Record{}, fmt.Errorf(
			"stored tier %q disagrees with payload tier %q", tierStr, want)
	}
	if want := string(reason.Kind()); want != reasonKind {
		return record.Record{}, fmt.Errorf(
			"stored reason_kind %q disagrees with payload kind %q", reasonKind, want)
	}

	ch, err := hashFromBytes("content_hash", contentHash)
	if err != nil {
		return record.Record{}, err
	}
	ph, err := hashFromBytes("prev_hash", prevHash)
	if err != nil {
		return record.Record{}, err
	}
	hh, err := hashFromBytes("hash", hash)
	if err != nil {
		return record.Record{}, err
	}

	rec := record.Record{
		ID:         record.RecordID(id),
		Seq:        uint64(seq),
		At:         at,
		RecordedAt: recordedAt,
		Event:      record.EventType(event),
		Reason:     reason,
		Subject: record.Subject{
			MemoryID: memoryID,
			Scope: record.Scope{
				UserID:  userID,
				AgentID: agentID,
				AppID:   appID,
				RunID:   runID,
			},
			ContentHash: ch,
		},
		IdempotencyKey: record.IdemKey(idemKey),
		PrevHash:       ph,
		Hash:           hh,
		Signature:      signature,
		SignerKeyID:    signerKeyID,
	}
	if contentText.Valid {
		rec.Content = &record.Content{
			Text:      contentText.String,
			Sensitive: contentSensitive != 0,
		}
	}
	return rec, nil
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

// isDuplicateIdemKey reports whether err is a uniqueness violation on the
// idempotency_key column. The id column is also UNIQUE, so it must not be
// mistaken for this.
func isDuplicateIdemKey(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed: records.idempotency_key")
}
