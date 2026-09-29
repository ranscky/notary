package gap

import (
	"errors"
	"fmt"
	"os"

	"notary/internal/record"
)

// Field names reported in a Break. They name the exact part of the gap chain
// that changed.
const (
	// fieldDecode reports that a line could not be decoded -- a torn or corrupt
	// write. It mirrors the record store's fieldDecode.
	fieldDecode = "decode"
	// fieldHash reports that an entry's stored Hash is not the hash recomputed
	// over the stored entry.
	fieldHash = "hash"
	// fieldChain reports that an entry's PrevHash is not its predecessor's Hash
	// (or the all-zero genesis hash at the start of the chain).
	fieldChain = "chain"
	// fieldCounter reports that an entry's Counter is not exactly one more than
	// its predecessor's -- a gap or a duplicate.
	fieldCounter = "counter"
)

// Break records one integrity failure found while verifying the gap log. It
// names the exact line and -- where known -- the entry and field that broke.
type Break struct {
	// Line is the 1-based line number the break was found on.
	Line int
	// Counter is the entry's chain position, when it could be decoded. It is 0
	// for a line that could not be decoded.
	Counter uint64
	// Field names the part of the entry that broke ("decode", "hash", "chain",
	// or "counter").
	Field string
	// Detail explains the failure in plain language.
	Detail string
}

// Verify walks the log's entries in order and reports every integrity break it
// finds -- the counter continuity, the chain link, the recomputed hash, and any
// line that will not decode -- rather than stopping at the first. A clean or
// empty log returns no breaks and a nil error; a missing file is an empty log.
//
// Verify never panics. It notes, but does not fix, that a hash chain cannot see
// its own removed tail: deleting the final line yields a shorter but still
// self-consistent chain, which only an external checkpoint could detect.
func (l *Log) Verify() ([]Break, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return verifyPath(l.path)
}

// Verify reads the gap log at path and reports every integrity break it finds,
// with the same semantics as (*Log).Verify. A missing file is an empty log and
// returns no breaks. It never panics.
func Verify(path string) ([]Break, error) {
	return verifyPath(path)
}

// Read returns every decodable entry in the log at path, in file order. Lines
// that cannot be decoded are skipped -- Verify is what reports them. A missing
// file is an empty log and returns no entries and a nil error. It never panics.
func Read(path string) ([]Entry, error) {
	data, err := readLog(path)
	if err != nil {
		return nil, err
	}
	lines, _, _ := splitLines(data)
	var out []Entry
	for _, raw := range lines {
		e, derr := decodeLine(raw)
		if derr != nil {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

// Entries returns every decodable entry in the OPEN log, in file order, with
// the same semantics as the package-level Read: lines that cannot be decoded
// are skipped (Verify is what reports them), and a missing or empty file yields
// no entries and a nil error. It exists so a caller that holds only a *gap.Log
// -- and not its unexported path -- can read the log back; it deliberately
// exposes no path and has no other obligation.
//
// Entries serialises with Record, Verify, and Close on the log's mutex, so it
// observes a consistent view and never races a concurrent append. It never
// panics.
func (l *Log) Entries() ([]Entry, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Read(l.path)
}

// readLog reads the whole file at path, treating a missing file as empty.
func readLog(path string) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("gap: read %s: %w", path, err)
	}
	return data, nil
}

// verifyPath reads and verifies the log at path.
func verifyPath(path string) ([]Break, error) {
	data, err := readLog(path)
	if err != nil {
		return nil, err
	}
	return verifyData(data), nil
}

// splitLines splits data into its newline-terminated lines plus any trailing
// unterminated remainder. lines excludes the remainder; torn is that remainder
// and hasTorn reports whether a non-empty one exists. Because every complete
// entry is written newline-terminated, any non-empty remainder is a torn
// (partially written) line.
func splitLines(data []byte) (lines [][]byte, torn []byte, hasTorn bool) {
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		return lines, data[start:], true
	}
	return lines, nil, false
}

// verifyData checks the bytes of a log and returns every break found. See
// (*Log).Verify for the semantics.
func verifyData(data []byte) []Break {
	lines, _, hasTorn := splitLines(data)

	var breaks []Break
	var prev record.Hash
	prevKnown := true
	var expected uint64
	lineNo := 0

	for _, raw := range lines {
		lineNo++
		e, err := decodeLine(raw)
		if err != nil {
			breaks = append(breaks, Break{Line: lineNo, Field: fieldDecode, Detail: err.Error()})
			// The line's hash is unknown, so the next line's link cannot be
			// checked.
			prevKnown = false
			continue
		}

		// (1) The counter must be exactly one past its predecessor.
		if e.Counter != expected {
			breaks = append(breaks, Break{
				Line: lineNo, Counter: e.Counter, Field: fieldCounter,
				Detail: fmt.Sprintf("expected counter %d, found %d", expected, e.Counter),
			})
		}
		expected = e.Counter + 1

		// (2) The entry must link to its predecessor (genesis at the start).
		if prevKnown && e.PrevHash != prev {
			breaks = append(breaks, Break{
				Line: lineNo, Counter: e.Counter, Field: fieldChain,
				Detail: fmt.Sprintf("prev_hash %x does not match predecessor hash %x", e.PrevHash[:], prev[:]),
			})
		}

		// (3) The stored hash must be the hash recomputed over the entry.
		if got := hashEntry(e); got != e.Hash {
			breaks = append(breaks, Break{
				Line: lineNo, Counter: e.Counter, Field: fieldHash,
				Detail: fmt.Sprintf("stored hash %x does not match hash recomputed over the entry: %x", e.Hash[:], got[:]),
			})
		}

		prev = e.Hash
		prevKnown = true
	}

	if hasTorn {
		lineNo++
		breaks = append(breaks, Break{
			Line: lineNo, Field: fieldDecode,
			Detail: "torn write: final line is not newline-terminated",
		})
	}
	return breaks
}
