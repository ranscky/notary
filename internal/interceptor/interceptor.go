// Package interceptor is Notary's fail-open-loud boundary. It sits between a
// caller's Mem0 operation and the audit ledger, attempts to record what
// happened, and -- when the ledger cannot take the record -- refuses to turn an
// audit problem into a caller problem. Instead it records a durable,
// chain-linked audit gap and marks every configured channel, then lets the
// operation proceed.
package interceptor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"notary/internal/gap"
	"notary/internal/ledger"
	"notary/internal/record"
)

// FailMode selects how an Interceptor behaves when the audit ledger cannot
// accept a record.
type FailMode uint8

const (
	// FailOpenLoud is the only mode implemented in v1: the operation completes
	// normally and the audit failure is reported loudly (channels) and durably
	// (the hash-chained gap log).
	FailOpenLoud FailMode = iota + 1
	// FailClosed is reserved and never implemented in v1. It is defined so the
	// vocabulary is closed and a future mode has a name; Validate rejects it,
	// so a configuration can never select a behaviour this build cannot honour.
	FailClosed
)

// ErrFailClosedUnimplemented reports that FailClosed was requested. FailClosed
// is defined but not implemented in v1 (spec section 1 non-goals), so this
// error is returned rather than the mode being silently honoured.
var ErrFailClosedUnimplemented = errors.New("interceptor: fail-closed mode is not implemented")

// Validate reports whether m is a mode this build can honour: nil for
// FailOpenLoud, ErrFailClosedUnimplemented for FailClosed, and a non-nil error
// for any other value. The zero value is deliberately invalid, so an unset or
// defaulted mode is never mistaken for a real one.
func (m FailMode) Validate() error {
	switch m {
	case FailOpenLoud:
		return nil
	case FailClosed:
		return ErrFailClosedUnimplemented
	default:
		return fmt.Errorf("interceptor: invalid fail mode %d", uint8(m))
	}
}

// Interceptor is a component that reports the fail mode it runs under and can
// be closed. The library Mem0 interceptor (Task 16) implements it too, so a
// caller can treat the audit writer and the Mem0 interceptor uniformly.
type Interceptor interface {
	// FailMode reports the mode this component runs under.
	FailMode() FailMode
	// Close releases the component's resources.
	Close() error
}

// AuditWriter is the fail-open-loud write path: it appends records to the
// ledger and, when that fails, records a durable, chain-valid gap entry and
// marks every configured channel. It borrows its ledger and gap log; the
// ledger's lifetime belongs to the caller (see Close).
type AuditWriter struct {
	// ledger is the primary write path. A nil ledger is treated as a ledger
	// that cannot accept the record -- not as a healthy one.
	ledger *ledger.Ledger
	// g is the hash-chained gap log: the authoritative, tamper-evident place a
	// gap is recorded, written only through g.Record.
	g *gap.Log
	// channels are additional human-readable, loud sinks for the marker. They
	// are loud sinks ONLY; they must never write to the gap log's own file.
	channels []io.Writer
}

// Compile-time proof that *AuditWriter is an Interceptor.
var _ Interceptor = (*AuditWriter)(nil)

// NewAuditWriter returns an AuditWriter that appends to l and, on failure,
// records a gap through g and marks every channel.
//
// channels are ADDITIONAL human-readable loud sinks -- typically os.Stderr --
// and are exactly that: loud sinks. They are NOT the gap log's own file, and a
// caller must NOT pass a writer that appends to the gap log path here. The
// marker is a plain-text line, not a chained JSON entry, so writing it into the
// gap log file would inject a non-JSON line and BREAK the gap log's hash chain
// -- defeating the very property (an unerasable outage record) the gap log
// exists to provide. The chain is written in exactly one place: g.Record.
//
// The AuditWriter borrows l and g. It does not open or close the ledger, and
// Close closes only the gap log.
func NewAuditWriter(l *ledger.Ledger, g *gap.Log, channels []io.Writer) *AuditWriter {
	return &AuditWriter{ledger: l, g: g, channels: channels}
}

// FailMode reports the mode this writer runs under: always FailOpenLoud.
func (w *AuditWriter) FailMode() FailMode { return FailOpenLoud }

// Close closes the gap log and returns its error. A nil gap log is safe, and
// calling Close more than once is safe. The ledger is deliberately NOT closed:
// the AuditWriter borrows it, so its lifetime belongs to the caller that
// created it.
func (w *AuditWriter) Close() error {
	if w.g == nil {
		return nil
	}
	return w.g.Close()
}

// Write attempts to append rec to the ledger. On success it returns nil. On a
// ledger failure it does not return an error for that failure: it records a
// durable audit_gap entry in the gap log, writes a one-line marker to every
// channel, and returns nil -- the fail-open contract, so an audit problem can
// never become a caller's problem.
//
// The one exception is a doubly-failed write. If the gap-log write (and the
// gap's own description) also fails, the channel markers are still written and
// then the error is returned: by then the caller is a background reconciliation
// path, and a write that failed on both the ledger and the gap log is a real
// error worth surfacing.
func (w *AuditWriter) Write(rec record.Record) error {
	cause := w.appendToLedger(rec)
	if cause == nil {
		return nil
	}
	return w.failOpen(rec, cause)
}

// appendToLedger appends rec to the ledger and returns the failure cause, or
// nil on success. A nil ledger is reported as a failure, never as success -- a
// nil ledger is not a healthy ledger.
func (w *AuditWriter) appendToLedger(rec record.Record) error {
	if w.ledger == nil {
		return errors.New("interceptor: no ledger configured")
	}
	_, err := w.ledger.Append(rec)
	return err
}

// failOpen records the gap, marks every channel, and returns nil unless the
// gap-log write itself failed.
func (w *AuditWriter) failOpen(rec record.Record, cause error) error {
	entry, err := buildGapEntry(rec, cause)
	if err != nil {
		// The gap could not even be described. Still be loud, then surface the
		// error: this is a doubly-failed write.
		w.markChannels(rec, cause)
		return fmt.Errorf("interceptor: describe audit gap for record %s: %w", rec.ID, err)
	}

	var gapErr error
	if w.g == nil {
		gapErr = errors.New("interceptor: no gap log configured")
	} else {
		gapErr = w.g.Record(entry)
	}

	w.markChannels(rec, cause)

	if gapErr != nil {
		return fmt.Errorf("interceptor: record audit gap for record %s: %w", rec.ID, gapErr)
	}
	return nil
}

// markChannels writes the one-line human marker to every channel. It is
// best-effort: a failing loud sink must not itself become an error. Every
// channel is attempted (never just the first), and a nil element is skipped.
func (w *AuditWriter) markChannels(rec record.Record, cause error) {
	marker := gapMarker(rec, cause)
	for _, ch := range w.channels {
		if ch == nil {
			continue
		}
		_, _ = io.WriteString(ch, marker)
	}
}

// buildGapEntry builds the reconcilable gap-log entry for a record the ledger
// could not accept.
//
// The entry's Kind, Scope, and CorrelationID are the missed record's Event,
// Scope, and ID. That is deliberate: ledger.GapBreaks accounts for a gap entry
// by matching (Kind, Scope, CorrelationID) against a stored record's (Event,
// Scope, ID), so only the real event lets the entry be reconciled once the
// record is finally written -- the whole point of a gap log.
func buildGapEntry(rec record.Record, cause error) (gap.Entry, error) {
	// The audit_gap record is the in-vocabulary description of the failed
	// write: Event is record.EventAuditGap and Reason is an observed
	// audit_unavailable. It is never appended to the ledger -- that is what
	// failed -- so it carries no Seq, Hash, PrevHash, or Signature (the ledger
	// would reject a caller-supplied non-zero Seq anyway). It is built so the
	// gap's identity (ID, scope) and meaning (reason) come from one place.
	reason, err := auditUnavailableReason(rec, cause)
	if err != nil {
		return gap.Entry{}, err
	}
	gapRec := record.Record{
		ID:      rec.ID,
		At:      rec.At,
		Event:   record.EventAuditGap,
		Reason:  reason,
		Subject: rec.Subject,
	}

	return gap.Entry{
		At:            time.Now().UTC(),
		Kind:          rec.Event,
		Scope:         gapRec.Subject.Scope,
		CorrelationID: string(gapRec.ID),
		Detail: fmt.Sprintf("audit unavailable (%s): %v",
			gapRec.Reason.Kind(), cause),
	}, nil
}

// auditUnavailableReason builds the Observed reason carried by the audit_gap
// record: an audit_unavailable observation sourced from Notary's own
// instrumentation. The evidence payload is canonical JSON of non-sensitive
// metadata only -- the event, the correlation ID, and the failure -- never
// memory text.
func auditUnavailableReason(rec record.Record, cause error) (record.Reason, error) {
	payload, err := json.Marshal(map[string]string{
		"event":       string(rec.Event),
		"correlation": string(rec.ID),
		"reason":      cause.Error(),
	})
	if err != nil {
		return record.Reason{}, fmt.Errorf("marshal audit gap payload: %w", err)
	}
	ev, err := record.NewObservedEvidence(record.SourceNotaryInstrumentation, payload)
	if err != nil {
		return record.Reason{}, fmt.Errorf("build audit gap evidence: %w", err)
	}
	reason, err := record.NewObservedReason(record.ReasonAuditUnavailable, ev)
	if err != nil {
		return record.Reason{}, fmt.Errorf("build audit gap reason: %w", err)
	}
	return reason, nil
}

// gapMarker renders the single-line, human-readable marker written to every
// channel. It names only non-sensitive metadata -- the event kind, the scope,
// the correlation ID, and the failure reason -- and NEVER the record's content,
// so no raw memory text can reach a log through the loud path.
func gapMarker(rec record.Record, cause error) string {
	scope := rec.Subject.Scope
	return fmt.Sprintf(
		"notary: audit gap: event=%s correlation=%s scope{user=%q agent=%q app=%q run=%q} reason=%v\n",
		rec.Event, rec.ID, scope.UserID, scope.AgentID, scope.AppID, scope.RunID, cause,
	)
}
