package ledger_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/ledger"
	"notary/internal/record"
)

// chainWrapsCause reports whether target is reachable as a WRAPPED node in err's
// chain -- err itself, or any node it unwraps to. It follows both the
// single-Unwrap and the multi-Unwrap (Unwrap() []error) forms, which
// fmt.Errorf produces for one and several %w verbs respectively.
//
// It is how a test proves a cause was WRAPPED with %w rather than interpolated
// with %v: an interpolated cause is text inside a message and is never a node
// in the chain, so no walk can find it. Nodes are compared by their message
// (the wrapped cause is a fresh value each call, so identity cannot be used).
func chainWrapsCause(err, target error) bool {
	if err == nil || target == nil {
		return false
	}
	if err.Error() == target.Error() {
		return true
	}
	switch u := err.(type) {
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if chainWrapsCause(e, target) {
				return true
			}
		}
	case interface{ Unwrap() error }:
		return chainWrapsCause(u.Unwrap(), target)
	}
	return false
}

// TestAppendInvalidTierWrapsSentinelAndCause pins the "%w: %w" form on the
// invalid-tier rejection. The .clinerules require wrapping errors with %w;
// interpolating the inner validation cause with %v would leave errors.Is able to
// reach only the ErrInvalidTier sentinel and strand the real cause, so no caller
// could reach it with errors.Is/errors.As. fmt.Errorf accepts several %w verbs
// (Go 1.20+), so both the sentinel and the cause are wrapped.
func TestAppendInvalidTierWrapsSentinelAndCause(t *testing.T) {
	l, _, _, _ := newLedger(t)

	rec := validRecord(t, "rec-bad-tier")
	rec.Reason = record.Reason{} // zero value: invalid tier

	cause := rec.Validate()
	require.Error(t, cause, "fixture: the zero Reason must fail validation")

	_, err := l.Append(rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ledger.ErrInvalidTier, "the sentinel must remain reachable")
	assert.NotErrorIs(t, err, ledger.ErrInvalidRecord)
	assert.True(t, chainWrapsCause(err, cause),
		"the inner validation cause must be reachable through the error chain "+
			"(wrap it with %%w, not interpolate it with %%v): got %v", err)
}

// TestAppendInvalidRecordWrapsSentinelAndCause is the same contract for the
// non-reason validation failure reported as ErrInvalidRecord.
func TestAppendInvalidRecordWrapsSentinelAndCause(t *testing.T) {
	l, _, _, _ := newLedger(t)

	rec := validRecord(t, "rec-bad-event")
	rec.Event = record.EventType("not_a_real_event")

	cause := rec.Validate()
	require.Error(t, cause, "fixture: an unknown event type must fail validation")

	_, err := l.Append(rec)
	require.Error(t, err)
	assert.ErrorIs(t, err, ledger.ErrInvalidRecord, "the sentinel must remain reachable")
	assert.NotErrorIs(t, err, ledger.ErrInvalidTier)
	assert.True(t, chainWrapsCause(err, cause),
		"the inner validation cause must be reachable through the error chain "+
			"(wrap it with %%w, not interpolate it with %%v): got %v", err)
}
