package reconcile

import (
	"errors"
	"fmt"
)

// ReconcileMode selects how reconciliation is driven.
//
// The vocabulary is closed: a mode is either commanded by the CLI or run
// in-process. Only ReconcileCommand is implemented in v1 -- the CLI performs a
// one-shot pass and exits -- and ReconcileInProcess is DEFINED BUT RESERVED,
// exactly as interceptor.FailClosed is in this codebase: it is named so the
// vocabulary is complete and a future mode has something to be, but no code
// path in this build honours it. Validate rejects ReconcileInProcess
// explicitly, so a caller that runs only what this build implements asks
// Validate -- never Valid -- and gets a matchable error rather than silently
// entering a path with no implementation.
//
// The zero value is deliberately invalid (see the iota+1 below), so an unset
// or defaulted ReconcileMode can never be mistaken for a real one -- the same
// discipline as FailMode, VisibilityTier and CompleteEnumeration.
type ReconcileMode uint8

const (
	// ReconcileCommand is the only mode implemented in v1: a single reconcile
	// pass is commanded and the process exits. This is what config.LoadFrom
	// defaults to.
	ReconcileCommand ReconcileMode = iota + 1
	// ReconcileInProcess is RESERVED and never implemented in v1. It is
	// defined so the vocabulary is closed and a future, long-running driver has
	// a name to select; no code path in this build honours it. Valid reports
	// true for it -- it is a real name, not the zero value -- but Valid means
	// "is a member of the vocabulary", NOT "this build implements it". A caller
	// that runs only what this build implements goes through Validate, which
	// rejects this value.
	ReconcileInProcess
)

// String returns the lowercase name of the mode: "command" or "in-process".
// It returns "invalid" for the zero value and for any out-of-range value, so an
// unset or forged mode renders recognisably rather than as a plausible name.
func (m ReconcileMode) String() string {
	switch m {
	case ReconcileCommand:
		return "command"
	case ReconcileInProcess:
		return "in-process"
	default:
		return "invalid"
	}
}

// Valid reports whether m is a named member of the closed ReconcileMode
// vocabulary: true for ReconcileCommand and ReconcileInProcess, false for the
// zero value and for any out-of-range value.
//
// Valid is deliberately about VOCABULARY, not implementation: it returns true
// for ReconcileInProcess even though v1 implements no code path for it. That
// keeps "reserved" (a valid name) distinguishable from the zero value (not a
// name at all).
//
// Valid must NOT be used as an implementation check: it is true for
// ReconcileInProcess, which v1 does not implement. Use Validate for that -- it
// is the check that rejects a reserved mode this build cannot honour.
func (m ReconcileMode) Valid() bool {
	switch m {
	case ReconcileCommand, ReconcileInProcess:
		return true
	default:
		return false
	}
}

// ErrReconcileInProcessUnimplemented reports that the reserved in-process mode
// was requested. ReconcileInProcess is defined but not implemented in v1, so
// this error is returned by Validate rather than the mode being silently
// honoured. It mirrors interceptor.ErrFailClosedUnimplemented.
var ErrReconcileInProcessUnimplemented = errors.New("reconcile: in-process mode is not implemented")

// Validate reports whether m can actually be run by this build: nil for
// ReconcileCommand, ErrReconcileInProcessUnimplemented for ReconcileInProcess,
// and a non-nil error for any other value. The zero value is deliberately
// invalid, so an unset or defaulted mode is never mistaken for a runnable one.
//
// Valid reports VOCABULARY validity; Validate reports IMPLEMENTATION.
// ReconcileInProcess is valid vocabulary and is still rejected here, so a
// caller that runs only what is implemented asks Validate and gets an explicit,
// matchable error instead of silently entering a path with no implementation.
func (m ReconcileMode) Validate() error {
	switch m {
	case ReconcileCommand:
		return nil
	case ReconcileInProcess:
		return ErrReconcileInProcessUnimplemented
	default:
		return fmt.Errorf("reconcile: invalid mode %d", uint8(m))
	}
}
