package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"notary/internal/export"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/replay"
)

// asOfData is the As-of page's payload.
type asOfData struct {
	pageData

	// The reader's own input, echoed back into the form so a rerun does not have
	// to be retyped. These values are untrusted and are rendered through
	// html/template like any other.
	Date string
	Time string
	Zone string

	// Scope is the four scope filters, composed with AND.
	Scope record.Scope

	// Err is a plain-language message about the reader's input. It is never a
	// storage error: a bad date is the reader's business.
	Err string

	// Submitted reports that the reader asked for something, which is what
	// distinguishes "no instant given" from "an instant was given and was wrong".
	Submitted bool

	// Zones is the list the form offers. The parser accepts any ±HH:MM, so this
	// is a convenience rather than the grammar.
	Zones []string

	// Ran reports that a replay ran, so the page renders results.
	Ran bool
	// NotVerified reports that no trusted keys are configured. A replay that
	// cannot be verified is not evidence, so none is run and the page says so in
	// the same words the Overview card uses.
	NotVerified bool
	// At is the instant this page understood, and Echo is that instant in UTC as
	// RFC3339 -- the form the reader can check and paste into the CLI.
	At   time.Time
	Echo string
	// Table is the replayed records.
	Table recordTable
	// Redacted counts the shown rows whose content was withheld as sensitive.
	Redacted int
	// Breaks is the integrity state of the prefix, from the same verdict every
	// other view uses.
	Breaks []ledger.Break
	// Reproduce is the exact command that answers the same question.
	Reproduce string
	// Reveal is the view's own reveal control, built from the request that was
	// actually served so toggling it never changes the question.
	Reveal revealControl
}

// asOfRun is what one replay produced, before it is arranged for the page.
type asOfRun struct {
	Rows     []recordRow
	Redacted int
	Breaks   []ledger.Break
}

// asOfZones is the zone list the form offers: UTC plus a short spread of fixed
// offsets. The parser accepts any ±HH:MM, so this list is a convenience rather
// than the grammar.
var asOfZones = []string{"UTC", "-08:00", "-05:00", "+00:00", "+01:00", "+05:30", "+10:00"}

// parseAsOfInstant turns the reader's date, time and zone into one instant.
//
// All three are required, and each refusal names the field at fault. There is no
// default and no fallback: a time without a zone is a different instant depending
// on where it is read, and assuming the server's or the browser's zone is the bug
// this page exists to avoid.
func parseAsOfInstant(date, clock, zone string) (time.Time, error) {
	loc, err := parseAsOfZone(zone)
	if err != nil {
		return time.Time{}, err
	}

	day, err := time.Parse("2006-01-02", date)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"the date must be written as YYYY-MM-DD (for example 2026-09-28), got %q", date)
	}

	var parsed time.Time
	for _, layout := range []string{"15:04", "15:04:05"} {
		if t, perr := time.Parse(layout, clock); perr == nil {
			parsed = t
			break
		}
	}
	if parsed.IsZero() {
		return time.Time{}, fmt.Errorf(
			"the time must be written as HH:MM or HH:MM:SS (for example 12:00), got %q", clock)
	}

	return time.Date(day.Year(), day.Month(), day.Day(),
		parsed.Hour(), parsed.Minute(), parsed.Second(), 0, loc), nil
}

// parseAsOfZone resolves the zone field: UTC, or a fixed offset written ±HH:MM.
func parseAsOfZone(zone string) (*time.Location, error) {
	switch zone {
	case "":
		return nil, errors.New(
			"choose a time zone: a time without one is a different instant depending on where it is read, " +
				"so this page will not guess")
	case "UTC", "Z":
		return time.UTC, nil
	}

	if len(zone) != 6 || (zone[0] != '+' && zone[0] != '-') || zone[3] != ':' {
		return nil, fmt.Errorf(
			"the time zone must be UTC or an offset written +HH:MM or -HH:MM (for example +05:30), got %q", zone)
	}
	hours, err1 := strconv.Atoi(zone[1:3])
	minutes, err2 := strconv.Atoi(zone[4:6])
	if err1 != nil || err2 != nil || hours > 23 || minutes > 59 {
		return nil, fmt.Errorf(
			"the time zone offset %q is not a valid offset (hours 00-23, minutes 00-59)", zone)
	}

	offset := hours*3600 + minutes*60
	if zone[0] == '-' {
		offset = -offset
	}
	return time.FixedZone(zone, offset), nil
}

// loadAsOf runs the real replay.Replayer over the request and decodes what it
// wrote.
//
// It is deliberately a renderer of replay's output rather than a peer of it:
// running replay.Replayer is what keeps the inclusive boundary, the chain-prefix
// verification, the scope conjunction and the redaction in the one package that
// owns them. The buffer is how a JSONL writer becomes a table.
func (s *Server) loadAsOf(ctx context.Context, req replay.Request) (asOfRun, error) {
	var buf bytes.Buffer
	res, err := replay.New(s.ledger, s.verifier).Replay(ctx, req, &buf)
	if err != nil {
		return asOfRun{}, fmt.Errorf("serve: replay: %w", err)
	}

	rows := make([]recordRow, 0, res.Records)
	dec := json.NewDecoder(&buf)
	for {
		var line export.Line
		err := dec.Decode(&line)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Only reachable if replay's encoder and export.Line disagree, which is
			// an internal fault: never the reader's business.
			return asOfRun{}, fmt.Errorf("serve: decode replayed line: %w", err)
		}
		rows = append(rows, recordRow{Line: line, Tier: badgeFor(line.Tier)})
	}

	return asOfRun{Rows: rows, Redacted: res.Redacted, Breaks: res.Breaks}, nil
}

// reproduceLine is the exact CLI command that answers the same question.
//
// The instant is this page's own, already validated, so it is emitted bare; the
// scope values are the reader's input and are always shell-quoted, so a value
// containing a quote, a space or a semicolon lands as one argument and cannot
// close the quoting around it.
func reproduceLine(at time.Time, scope record.Scope) string {
	parts := []string{"notary", "replay", "--at", at.UTC().Format(time.RFC3339)}
	for _, dim := range []struct {
		flag  string
		value string
	}{
		{"--user-id", scope.UserID},
		{"--agent-id", scope.AgentID},
		{"--app-id", scope.AppID},
		{"--run-id", scope.RunID},
	} {
		if dim.value != "" {
			parts = append(parts, dim.flag, shellQuote(dim.value))
		}
	}
	return strings.Join(parts, " ")
}

// shellQuote wraps s in single quotes, escaping any it contains, so a shell reads
// it as exactly one word whatever it holds.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
