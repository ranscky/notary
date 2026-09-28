// Package ledger is the single write path into Notary's audit trail. It is the
// only component that appends records, and it does so inside one store
// transaction that assigns the record's chain position, links it to its
// predecessor, hashes it, and signs it together. Because the position is
// assigned under the same lock as the insert, a caller can never choose where a
// record lands, and a crash cannot leave a half-written link.
package ledger

import (
	"errors"
	"fmt"
	"time"

	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// ErrInvalidTier reports that a record's Reason -- and so its visibility tier
// -- is invalid, including the zero Reason. It is wrapped with context, so
// callers detect it with errors.Is.
var ErrInvalidTier = errors.New("ledger: record has an invalid visibility tier")

// ErrInvalidRecord reports that a record failed validation for a reason other
// than its Reason. It is wrapped with context, so callers detect it with
// errors.Is.
var ErrInvalidRecord = errors.New("ledger: record is invalid")

// ErrSeqAssigned reports that a record was supplied with a non-zero Seq. Chain
// position is the ledger's to assign and no caller may choose it. It is wrapped
// with context, so callers detect it with errors.Is.
var ErrSeqAssigned = errors.New("ledger: record already has a chain position")

// Ledger is the single write path into the audit trail: the only component that
// appends records. Append assigns each record's chain position, links, hashes,
// and signs it inside one store transaction; the remaining methods are thin
// reads of the chain.
type Ledger struct {
	store  store.Store
	signer *sign.Signer
	now    func() time.Time
}

// New returns a Ledger that writes through st and signs with sg. now supplies
// the clock used to stamp a record's RecordedAt; when it is nil, time.Now is
// used.
func New(st store.Store, sg *sign.Signer, now func() time.Time) *Ledger {
	if now == nil {
		now = time.Now
	}
	return &Ledger{store: st, signer: sg, now: now}
}

// Append validates rec and then appends it to the chain inside a single store
// transaction: it assigns the next chain position (Seq), links the record to
// its predecessor (PrevHash, or record.GenesisHash at position 0), stamps
// RecordedAt from the ledger's clock when it is zero, hashes the record over
// that final position, and signs the hash. It returns the record's ID.
//
// Validation runs before the store is touched, so a rejected append leaves the
// store provably unchanged. A record whose Reason is invalid -- including the
// zero Reason, whose tier is invalid -- is rejected with ErrInvalidTier; any
// other validation failure is ErrInvalidRecord.
//
// Chain position is assigned under the same lock as the insert, so no caller
// can influence where a record lands: a record supplied with a non-zero Seq is
// rejected with ErrSeqAssigned.
func (l *Ledger) Append(rec record.Record) (record.RecordID, error) {
	if err := rec.Validate(); err != nil {
		if rerr := rec.Reason.Validate(); rerr != nil {
			return "", fmt.Errorf("ledger: append record %s: %w: %v", rec.ID, ErrInvalidTier, err)
		}
		return "", fmt.Errorf("ledger: append record %s: %w: %v", rec.ID, ErrInvalidRecord, err)
	}
	if rec.Seq != 0 {
		return "", fmt.Errorf("ledger: append record %s: %w: seq is %d", rec.ID, ErrSeqAssigned, rec.Seq)
	}
	if l.signer == nil {
		return "", fmt.Errorf("ledger: append record %s: no signer configured", rec.ID)
	}

	err := l.store.AppendChained(func(prev record.Record, hasPrev bool) (record.Record, error) {
		// The head is read under the write lock, so prev.Seq + 1 is the one
		// authoritative next position. Derive it here too, so the hash is
		// computed over the record's final chain position; the store re-asserts
		// the same value before inserting.
		if hasPrev {
			rec.Seq = prev.Seq + 1
			rec.PrevHash = prev.Hash
		} else {
			rec.Seq = 0
			rec.PrevHash = record.GenesisHash
		}

		if rec.RecordedAt.IsZero() {
			rec.RecordedAt = l.now().UTC()
		}

		// Set the signer identity before hashing: record.CanonicalBytes covers
		// SignerKeyID, so the digest must commit to it, and the stored record
		// must recompute to its own Hash. Signing the hash afterwards binds the
		// signature to the key named here.
		rec.SignerKeyID = l.signer.KeyID()

		h, err := record.ComputeHash(rec)
		if err != nil {
			return record.Record{}, fmt.Errorf("ledger: append record %s: compute hash: %w", rec.ID, err)
		}
		rec.Hash = h

		sig, err := l.signer.Sign(rec.Hash[:])
		if err != nil {
			return record.Record{}, fmt.Errorf("ledger: append record %s: sign: %w", rec.ID, err)
		}
		rec.Signature = sig

		return rec, nil
	})
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// GetRecord returns the record with the given ID, delegating to the store. It
// returns a wrapped store.ErrNotFound when no such record exists.
func (l *Ledger) GetRecord(id record.RecordID) (record.Record, error) {
	return l.store.GetRecord(id)
}

// ListRecords returns the records whose At lies within [from, to], delegating
// to the store.
func (l *Ledger) ListRecords(from, to time.Time) ([]record.Record, error) {
	return l.store.ListRecords(from, to)
}

// Head returns the record with the greatest Seq, delegating to the store. The
// bool is false, with a zero record and a nil error, when the ledger is empty.
func (l *Ledger) Head() (record.Record, bool, error) {
	return l.store.Head()
}
