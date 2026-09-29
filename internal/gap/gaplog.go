// Package gap is Notary's hash-chained gap log: an append-only, newline-
// delimited JSON record of audit gaps -- spans of Mem0 activity that Notary
// could not capture in the ledger because the store was unreachable or broken.
//
// The log deliberately does NOT use internal/store. It exists for exactly the
// case where the store is failing, so sharing a failure domain with it would
// defeat the whole point: if the two shared a store, a store outage would
// silence both the ledger and the record that a gap occurred. For the same
// reason this package reads and writes the log as a plain file, with O_APPEND
// and a flush after every write, so it survives the very failure it is meant to
// report.
//
// The package imports internal/record (for the shared types) and internal/sign
// (for signed head checkpoints over the log, see checkpoint.go). Importing
// internal/sign does not reintroduce the shared failure domain: signing uses
// local key material from an env var, a 0600 file, or (unimplemented) the OS
// keychain -- it never touches the record store. The store outage a gap log
// exists to survive does not disable signing, so a checkpoint can still be
// written and checked while the store is broken.
package gap

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"notary/internal/record"
)

// gapDomain separates a gap digest from every other use of SHA-256, so a digest
// produced here can never be confused with a bare hash of the same bytes
// computed elsewhere.
const gapDomain = "notary/gap/v1"

// Entry is one gap-log record: a statement that a span of activity could not be
// audited, durable enough to reconcile back into the ledger once the store
// recovers.
//
// The three chain fields -- Counter, PrevHash, Hash -- are assigned by Log and
// are never taken from the caller: Log.Record overwrites any value supplied
// here, so no caller can forge a link or choose a position. This mirrors how
// internal/ledger rejects a caller-supplied Seq.
type Entry struct {
	// Counter is this entry's position in the gap chain, assigned by Log.
	Counter uint64
	// At is when the gap occurred. It is caller-supplied and normalised to UTC
	// when hashed and stored.
	At time.Time
	// Kind names the kind of event the gap concerns.
	Kind record.EventType
	// Scope identifies the caller context the gap concerns.
	Scope record.Scope
	// CorrelationID ties the gap to the record it stands in for. It is the
	// missing record's ID: when the store recovers, the record can be found by
	// this ID (Task 19 reconciles on it).
	CorrelationID string
	// Detail is a human-readable description of why the gap occurred.
	//
	// Detail must NEVER carry raw memory text, customer content, or any other
	// sensitive material. It is a description of the failure, not a copy of the
	// data that could not be audited; storing memory text here would put the
	// very content the ledger exists to protect into a plaintext side-channel
	// file. Describe, do not quote.
	Detail string
	// PrevHash links this entry to its predecessor: the previous entry's Hash,
	// or the all-zero hash for the first entry. It is assigned by Log and any
	// caller-supplied value is ignored.
	PrevHash record.Hash
	// Hash is this entry's own digest. It is assigned by Log and any
	// caller-supplied value is ignored.
	Hash record.Hash
}

// Log is an append-only, hash-chained gap log backed by a single file. It is
// safe for concurrent use: Record, Verify, and Close are serialised by one
// mutex, so the counter and the chain link can never be interleaved between
// callers.
type Log struct {
	mu   sync.Mutex
	path string
	f    *os.File
	// next is the counter the next entry will receive; prev is the hash the
	// next entry will link to. Both are recovered by Open and advanced by
	// Record, under mu.
	next uint64
	prev record.Hash
}

// Open opens (creating if needed) the gap log at path and recovers the chain
// head so a restart continues the chain rather than restarting at 0. The file
// is opened O_APPEND|O_CREATE|O_WRONLY with mode 0600 and kept open until
// Close.
//
// Recovery reads only complete, newline-terminated lines: a partially written
// tail is never treated as the last good entry, so it cannot resurrect a bogus
// counter. One exception is a torn fragment that is a *complete* JSON entry
// with a correct hash and chain link -- a write that finished but lost only its
// terminating newline -- which is adopted so the chain continues without a
// spurious break. If the file ends in a torn (unterminated) line, Open appends
// a single newline to terminate it, so the fragment remains in the file as
// evidence for Verify and the next append starts its own line.
//
// Open returns a wrapped error and never panics.
func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("gap: open log %s: %w", path, err)
	}
	l := &Log{path: path, f: f}
	if err := l.recover(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return l, nil
}

// recover restores l.next and l.prev from the complete lines already in the
// file, and heals a torn tail. It is called with l.f open for append and no
// concurrent caller (Open is not yet returned).
func (l *Log) recover() error {
	data, err := os.ReadFile(l.path)
	if err != nil {
		return fmt.Errorf("gap: read log %s: %w", l.path, err)
	}

	lines, torn, hasTorn := splitLines(data)

	// Recover the head from the complete, newline-terminated lines only. A
	// line that will not decode is corruption: Verify reports it, and recovery
	// skips it, so a bogus counter is never fabricated from it.
	l.next = 0
	l.prev = record.GenesisHash
	var last Entry
	found := false
	for _, raw := range lines {
		e, derr := decodeLine(raw)
		if derr != nil {
			continue
		}
		last = e
		found = true
	}
	if found {
		l.next = last.Counter + 1
		l.prev = last.Hash
	}

	if hasTorn {
		// A torn tail is a write that did not finish. A *partial* fragment is
		// not valid JSON, never decodes, and is never trusted -- it must not
		// resurrect a bogus counter. The one exception is a fragment that is a
		// complete JSON entry with a correct hash and chain link: the write
		// finished and only its terminating newline was lost. That entry is
		// adopted so the chain continues without a spurious break.
		if e, ok := tornHead(torn, last, found); ok {
			l.next = e.Counter + 1
			l.prev = e.Hash
		}
		// Terminate the torn tail either way, so the next append is its own
		// line and the fragment stays visible to Verify as evidence.
		if _, werr := l.f.Write([]byte("\n")); werr != nil {
			return fmt.Errorf("gap: heal torn tail of %s: %w", l.path, werr)
		}
		if serr := l.f.Sync(); serr != nil {
			return fmt.Errorf("gap: heal torn tail of %s: %w", l.path, serr)
		}
	}
	return nil
}

// tornHead reports whether the torn (unterminated) tail fragment is a fully
// valid entry that legitimately continues the chain from prev, and returns it
// when so. It is trusted only when the fragment decodes, its stored hash
// matches its recomputed hash, and it links to prev at the expected position --
// so a truncated or tampered fragment is never adopted as the head.
func tornHead(torn []byte, prev Entry, hasPrev bool) (Entry, bool) {
	e, err := decodeLine(torn)
	if err != nil {
		return Entry{}, false
	}
	if hashEntry(e) != e.Hash {
		return Entry{}, false
	}
	wantPrev := record.GenesisHash
	var wantCounter uint64
	if hasPrev {
		wantPrev = prev.Hash
		wantCounter = prev.Counter + 1
	}
	if e.PrevHash != wantPrev || e.Counter != wantCounter {
		return Entry{}, false
	}
	return e, true
}

// Record appends e to the gap log, assigning its chain position: it sets
// Counter, links PrevHash to the current head (the all-zero genesis hash for
// the first entry), computes Hash over the final entry, writes the entry as one
// newline-terminated JSON line, and flushes it. Any Counter, PrevHash, or Hash
// supplied by the caller is cleared -- chain state is the log's to assign.
//
// Record is serialised by a mutex covering both the counter/link assignment and
// the write, so concurrent callers can never interleave counters or corrupt a
// line. It returns a wrapped error and never panics.
func (l *Log) Record(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.recordLocked(e)
}

// recordLocked is Record's body, run with l.mu held.
func (l *Log) recordLocked(e Entry) error {
	if l.f == nil {
		return fmt.Errorf("gap: record entry: log is closed")
	}

	// Chain state is the log's to assign; a caller cannot forge a link.
	e.Counter = l.next
	e.PrevHash = l.prev
	e.Hash = hashEntry(e)

	line, err := encodeEntry(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	if _, err := l.f.Write(line); err != nil {
		return fmt.Errorf("gap: write entry %d: %w", e.Counter, err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("gap: flush entry %d: %w", e.Counter, err)
	}

	l.next = e.Counter + 1
	l.prev = e.Hash
	return nil
}

// Close flushes and closes the log's file. It is safe to call more than once.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	if err := f.Close(); err != nil {
		return fmt.Errorf("gap: close log %s: %w", l.path, err)
	}
	return nil
}

// WriteChannels returns the writers a gap should be reported on at once, so a
// gap is never silent. The first is always a writer that appends to the gap log
// file at path; when w is non-nil the second is w itself (for example os.Stderr,
// so the gap is loud). The two writers are distinct.
//
// When w is nil, only the file writer is returned: there is no second sink to
// report on, and returning the same writer twice would falsely suggest a second
// channel. The file writer opens the file per write (O_APPEND|O_CREATE|0o600)
// and surfaces any open or write error from Write rather than panicking, which
// is why WriteChannels itself returns no error.
func WriteChannels(path string, w io.Writer) []io.Writer {
	chans := []io.Writer{&fileAppender{path: path}}
	if w != nil {
		chans = append(chans, w)
	}
	return chans
}

// fileAppender is an io.Writer that appends each write to a file opened fresh
// per call, so it holds no shared mutable state and needs no lock.
type fileAppender struct {
	path string
}

// Write appends p to the file. It returns a wrapped error on any failure and
// never panics.
func (a *fileAppender) Write(p []byte) (int, error) {
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, fmt.Errorf("gap: open channel %s: %w", a.path, err)
	}
	defer func() { _ = f.Close() }()

	n, err := f.Write(p)
	if err != nil {
		return n, fmt.Errorf("gap: write channel %s: %w", a.path, err)
	}
	if err := f.Sync(); err != nil {
		return n, fmt.Errorf("gap: flush channel %s: %w", a.path, err)
	}
	return n, nil
}
