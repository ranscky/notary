// Package store is the durable, ordered home of Notary's audit trail. It
// persists records as a sequence and reads them back, rebuilding each record's
// Reason from its stored payload. That rebuild is deliberate: because a
// tampered row fails to decode, a hand-edited row surfaces as an error rather
// than as a silently different claim, which is what makes the audit trail
// tamper-evident at rest.
package store

import (
	"errors"
	"time"

	"notary/internal/record"
)

// ErrNotFound is returned by GetRecord when no record with the requested ID
// exists. It is wrapped with context, so callers detect it with errors.Is.
var ErrNotFound = errors.New("store: record not found")

// ErrDuplicateIdemKey is returned by PutRecord when a record carries a
// non-empty IdempotencyKey that another record already uses. An empty key is
// never considered a duplicate.
var ErrDuplicateIdemKey = errors.New("store: duplicate idempotency key")

// ErrDecode is wrapped into SeqEntry.DecodeErr when a stored row cannot be
// rebuilt into a Record. It lets a caller detect a decode failure with
// errors.Is rather than by matching an error message, so a wording change in a
// decode error can never silently stop the failure from being recognised.
var ErrDecode = errors.New("store: record failed to decode")

// SeqEntry is one stored row in chain order. Rec is the decoded record; when
// the row cannot be decoded, Rec is the zero Record and DecodeErr is set
// instead. ID and Seq are read from their own columns, independently of the
// record payload, so a row whose reason payload has been corrupted can still be
// identified by its exact identity and position.
type SeqEntry struct {
	// Seq is the row's chain position, read from the seq column.
	Seq uint64
	// ID is the row's identifier, read from the id column.
	ID record.RecordID
	// Rec is the decoded record. It is the zero Record when DecodeErr is set.
	Rec record.Record
	// DecodeErr is non-nil when the row could not be rebuilt into a Record, and
	// is nil otherwise. It wraps ErrDecode, so callers detect it with errors.Is.
	DecodeErr error
}

// Store is the durable, ordered home of the audit trail. Implementations
// persist records and read them back faithfully; a read must reproduce the
// record that was written, or fail rather than return a different claim.
type Store interface {
	// PutRecord persists a record. It returns ErrDuplicateIdemKey when the
	// record carries a non-empty IdempotencyKey already in use.
	//
	// PutRecord inserts a record exactly as given, including its Seq. It is the
	// raw write used to rebuild a store; ordinary writes go through
	// AppendChained so the chain position is assigned atomically.
	PutRecord(record.Record) error
	// AppendChained appends a record built from the current chain head in a
	// single write transaction. build is called with the head record (prev,
	// hasPrev) while the write lock is held; it returns the fully-formed record
	// to insert. The store alone assigns the record's chain position (Seq): 0
	// when the table is empty, otherwise prev.Seq + 1. The head read and the
	// insert share one transaction, so two appends can never be assigned the
	// same position, and a crash cannot leave a partial link. Any error from
	// build, or from the insert, rolls the whole transaction back.
	AppendChained(build func(prev record.Record, hasPrev bool) (record.Record, error)) error
	// GetRecord returns the record with the given ID, or ErrNotFound when
	// none exists.
	GetRecord(record.RecordID) (record.Record, error)
	// ListRecords returns the records whose At falls within [from, to],
	// inclusive at both bounds, ordered by At ascending and then by Seq.
	ListRecords(from, to time.Time) ([]record.Record, error)
	// ListRecordsAsOf returns the records whose RecordedAt is at or before t,
	// inclusive, ordered by Seq ascending. It is the knowledge-time view -- the
	// ledger as it stood at instant t -- as opposed to ListRecords, which
	// bounds on the event time At.
	ListRecordsAsOf(t time.Time) ([]record.Record, error)
	// ListRecordsByMemory returns the records whose Subject.MemoryID is
	// memoryID, ordered by Seq ascending. It is one memory's lifecycle -- the
	// records that memory accumulated, in chain order -- as opposed to the
	// time-bounded ListRecords and ListRecordsAsOf. A row that cannot be
	// decoded is an error, matching those reads rather than SeqEntries'
	// tolerance.
	ListRecordsByMemory(memoryID string) ([]record.Record, error)
	// SeqEntries returns every stored row in chain order (seq ascending). It
	// differs from ListRecords in two ways that matter for verification: it
	// selects on seq rather than on at, so no record can fall outside a time
	// window, and it does not abort on a row that cannot be decoded. Such a
	// row is returned with its own Seq and ID set and DecodeErr non-nil
	// (wrapping ErrDecode); a row that decodes carries the rebuilt record in
	// Rec.
	SeqEntries() ([]SeqEntry, error)
	// Head returns the record with the greatest Seq. The bool is false, with
	// a zero record and a nil error, when the table is empty.
	Head() (record.Record, bool, error)
	// ByIdemKey returns the record with the given idempotency key. The bool
	// is false when no such record exists, including for the empty key.
	ByIdemKey(record.IdemKey) (record.Record, bool, error)
	// Close releases the store's resources.
	Close() error
}
