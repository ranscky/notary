package main

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
)

// rootSubcommands is the set of subcommands newRootCmd attaches, as of the
// last phase of v1. It is a set -- not a count, and not a first-element check
// -- because a count passes when one command is swapped for another, and it is
// the count that goes stale when a command is added or renamed. Each name is
// the one the command answers to on the command line (`notary <name>`), taken
// from newRootCmd's AddCommand calls in cmd/notary/root.go.
var rootSubcommands = []string{
	"version",
	"verify",
	"gaps",
	"reconcile",
	"export",
	"replay",
	"explain",
	"proxy",
}

// TestNewRootCmdRegistersExactlyTheExpectedSubcommands pins the root command's
// subcommand set in BOTH directions: every expected name is attached, and
// nothing else is. Nothing else in the repository asserts this, so a command
// that silently vanished from newRootCmd -- a dropped AddCommand, a rename, a
// merge that lost a line -- would ship as a smaller CLI with no failing test;
// this is the one place that catches it.
//
// The failure messages name the offending command, so the test is actionable
// when someone adds the ninth: an unexpected name means either the new command
// is deliberately here (add it to rootSubcommands) or it was registered by
// accident.
//
// It pins what newRootCmd ATTACHES, which is not quite what `notary --help`
// lists: help and completion are Cobra's own defaults, added when the command
// tree is executed rather than by this constructor, and every subcommand's own
// test covers that command's surface.
//
// What a command *is* -- its flags, its short description, whether it needs a
// signing key -- belongs to that command's own test; this test deliberately
// enumerates none of it.
func TestNewRootCmdRegistersExactlyTheExpectedSubcommands(t *testing.T) {
	cmd := newRootCmd()

	registered := make(map[string]struct{})
	for _, sub := range cmd.Commands() {
		registered[sub.Name()] = struct{}{}
	}

	expected := make(map[string]struct{}, len(rootSubcommands))
	for _, name := range rootSubcommands {
		expected[name] = struct{}{}
	}

	var missing []string
	for _, name := range rootSubcommands {
		if _, ok := registered[name]; !ok {
			missing = append(missing, name)
		}
	}

	var unexpected []string
	for name := range registered {
		if _, ok := expected[name]; !ok {
			unexpected = append(unexpected, name)
		}
	}
	sort.Strings(unexpected)

	assert.Emptyf(t, missing,
		"newRootCmd does not register %v: every command must stay attached "+
			"(a missing command is a silently shrunk CLI), so add its AddCommand back in "+
			"cmd/notary/root.go", missing)
	assert.Emptyf(t, unexpected,
		"newRootCmd registers %v, which no test expects: if the command is deliberate, add "+
			"its name to rootSubcommands in cmd/notary/root_test.go", unexpected)
}
