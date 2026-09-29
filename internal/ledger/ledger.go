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

// errIdemKeyPresent is an internal sentinel that aborts AppendChained's write
// transaction when the record's idempotency key is already stored. It never
// reaches a caller: Append converts it into the existing record's ID. It is an
// error -- rather than a nil "skip" -- because AppendChained has no skip path;
// returning the sentinel makes it roll back the (still empty) transaction so no
// second row is written.
var errIdemKeyPresent = errors.New("ledger: idempotency key already present")

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
//
// Append is idempotent on rec.IdempotencyKey (spec §7). When the key is
// non-empty and a record already carries it, the append is a no-op: Append
// returns the EXISTING record's ID -- not the caller's -- and writes nothing.
// The first observation is the record; content is never compared and nothing is
// updated. A different event, tier, scope, or identifier derives a different
// key (record.DeriveIdemKey), so later knowledge appends as a new record rather
// than mutating the first.
//
// The key lookup runs inside the callback AppendChained invokes, which executes
// while that transaction holds the write lock (the store opens every
// transaction with BEGIN IMMEDIATE). Holding the write lock across both the
// lookup and the insert is what makes two concurrent appends with the same key
// yield exactly one record: the second append cannot even begin its transaction
// until the first commits, so it sees the committed record and no-ops. A lookup
// issued before AppendChained, outside the lock, would race and is deliberately
// not done here. As belt and braces -- should a duplicate ever reach the insert
// anyway -- a store.ErrDuplicateIdemKey from the insert is converted to the same
// no-op by looking the existing record up and returning its ID.
//
// An empty key never deduplicates: a keyless record always appends, is never
// treated as a duplicate of another keyless record, and any number of keyless
// records remain legal. Dedupability comes from the interceptor always
// supplying a key, not from the ledger requiring one.
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

	// existing is set inside the callback (below), on this goroutine, when the
	// key is already stored. AppendChained invokes build synchronously, so
	// reading it afterwards is not a data race.
	var existing record.RecordID
	var present bool

	err := l.store.AppendChained(func(prev record.Record, hasPrev bool) (record.Record, error) {
		// Idempotency: a non-empty key already stored makes this append a
		// no-op. The lookup runs here, under the write lock AppendChained
		// already holds, so no concurrent writer can insert a same-key record
		// between the lookup and this append's insert. The empty key is never
		// a match (store.ByIdemKey reports false for it), so keyless records
		// always proceed to insert.
		if rec.IdempotencyKey != "" {
			prior, ok, lerr := l.store.ByIdemKey(rec.IdempotencyKey)
			if lerr != nil {
				return record.Record{}, fmt.Errorf(
					"ledger: append record %s: look up idempotency key: %w", rec.ID, lerr)
			}
			if ok {
				existing, present = prior.ID, true
				return record.Record{}, errIdemKeyPresent
			}
		}

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

	if present || errors.Is(err, errIdemKeyPresent) {
		// The sentinel is set together with present, so this covers the no-op
		// path; the errors.Is form is defensive in case the callback ever set
		// the sentinel without present.
		return existing, nil
	}
	if errors.Is(err, store.ErrDuplicateIdemKey) {
		// Belt and braces: a duplicate that somehow reached the insert (rather
		// than being caught by the in-transaction lookup) is the same no-op.
		// The key is non-empty because an empty key can never be a duplicate.
		prior, ok, lerr := l.store.ByIdemKey(rec.IdempotencyKey)
		if lerr != nil {
			return "", fmt.Errorf("ledger: append record %s: resolve duplicate idempotency key: %w", rec.ID, lerr)
		}
		if ok {
			return prior.ID, nil
		}
		return "", err
	}
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
