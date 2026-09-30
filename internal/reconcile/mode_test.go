package reconcile_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/config"
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

// TestConfigDefaultsToReconcileCommand verifies LoadFrom defaults the new field
// to the only mode implemented in v1. There is deliberately no environment
// variable for it.
func TestConfigDefaultsToReconcileCommand(t *testing.T) {
	cfg, err := config.LoadFrom(map[string]string{})
	require.NoError(t, err)
	require.NotNil(t, cfg)
	assert.Equal(t, reconcile.ReconcileCommand, cfg.ReconcileMode)
	assert.True(t, cfg.ReconcileMode.Valid(), "the default must be a valid mode")

	// An environment that names an unknown key leaves the default intact --
	// there is no env override for ReconcileMode.
	cfg, err = config.LoadFrom(map[string]string{
		"NOTARY_RECONCILE_MODE": "in_process",
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.ReconcileCommand, cfg.ReconcileMode,
		"there is no env override for ReconcileMode; the default must stand")
}
