package reconcile_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/reconcile"
)

// TestReconcileModeZeroValueIsInvalid pins the iota+1 discipline: the zero
// value of ReconcileMode is deliberately not a real mode, so a mode that was
// never set (or was defaulted by mistake) can never pass as one.
func TestReconcileModeZeroValueIsInvalid(t *testing.T) {
	var zero reconcile.ReconcileMode
	assert.False(t, zero.Valid(), "the zero value must never be a valid mode")
	assert.Equal(t, "invalid", zero.String(), "the zero value renders as invalid")

	// An out-of-range value is likewise invalid, not silently accepted.
	var outOfRange reconcile.ReconcileMode = 99
	assert.False(t, outOfRange.Valid())
	assert.Equal(t, "invalid", outOfRange.String())
}

// TestReconcileInProcessIsReservedNotImplemented fixes the intent for a future
// implementer: ReconcileCommand is the one mode v1 implements, and
// ReconcileInProcess is a DEFINED BUT RESERVED member of the vocabulary -- named
// and Valid(), exactly as interceptor.FailClosed is named, but with no code
// path that honours it. The String() forms are asserted so the names cannot
// drift, and the zero value is asserted invalid in the same breath so
// "reserved" (a valid name) can never be confused with "unset" (not a name).
func TestReconcileInProcessIsReservedNotImplemented(t *testing.T) {
	// The names, so the vocabulary is legible.
	assert.Equal(t, "command", reconcile.ReconcileCommand.String())
	assert.Equal(t, "in-process", reconcile.ReconcileInProcess.String())

	// Both named values are valid vocabulary...
	assert.True(t, reconcile.ReconcileCommand.Valid())
	assert.True(t, reconcile.ReconcileInProcess.Valid(),
		"ReconcileInProcess is a named (valid) mode that v1 deliberately leaves reserved: Valid reports vocabulary membership, not implementation")

	// ...whereas the zero value is not valid, so "reserved" and "unset" cannot
	// be confused.
	var zero reconcile.ReconcileMode
	assert.False(t, zero.Valid())
}

// TestReconcileModeValidate separates IMPLEMENTATION from VOCABULARY: Valid is
// the closed-vocabulary check, Validate is the this-build-can-run-it check.
// Validate is nil only for ReconcileCommand, the one mode v1 implements; the
// reserved ReconcileInProcess is valid vocabulary (Valid == true) yet Validate
// rejects it with a matchable sentinel; and an out-of-range or zero value is
// rejected too, so an unset mode can never be run.
func TestReconcileModeValidate(t *testing.T) {
	require.NoError(t, reconcile.ReconcileCommand.Validate(),
		"ReconcileCommand is the one implemented mode, so Validate must accept it")

	// InProcess is valid vocabulary but NOT implemented: the two checks must
	// disagree, and that disagreement is the whole point.
	assert.True(t, reconcile.ReconcileInProcess.Valid(),
		"Valid stays vocabulary-only: InProcess is a named mode")
	err := reconcile.ReconcileInProcess.Validate()
	require.Error(t, err, "the reserved in-process mode must be rejected")
	assert.ErrorIs(t, err, reconcile.ErrReconcileInProcessUnimplemented,
		"the rejection must be the matchable sentinel so a caller can branch on it")

	// An out-of-range value is rejected, and rejected as "invalid", not as the
	// reserved-mode sentinel.
	var outOfRange reconcile.ReconcileMode = 99
	oerr := outOfRange.Validate()
	require.Error(t, oerr, "an out-of-range mode must be rejected")
	assert.NotErrorIs(t, oerr, reconcile.ErrReconcileInProcessUnimplemented,
		"an out-of-range value is not the reserved in-process mode")

	// The zero value is rejected too, so an unset or defaulted mode is never
	// mistaken for a runnable one.
	var zero reconcile.ReconcileMode
	assert.Error(t, zero.Validate(), "the zero value must be rejected")
	assert.NotErrorIs(t, zero.Validate(), reconcile.ErrReconcileInProcessUnimplemented)
}
