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
	// Head returns the record with the greatest Seq. The bool is false, with
	// a zero record and a nil error, when the table is empty.
	Head() (record.Record, bool, error)
	// ByIdemKey returns the record with the given idempotency key. The bool
	// is false when no such record exists, including for the empty key.
	ByIdemKey(record.IdemKey) (record.Record, bool, error)
	// Close releases the store's resources.
	Close() error
}
