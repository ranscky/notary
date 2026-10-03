// Package replay renders the ledger as it stood at a chosen instant -- what
// Notary knew at time T -- as JSONL, byte-identical to an export.
//
// It is a knowledge-time view (design §1): the records whose RecordedAt is at
// or before T, in Seq order. The Reader it is given verifies the prefix before
// returning it (ledger.ReplayAsOf, design §4), because an as-of read that
// happened to have a hole would otherwise be presented as "what Notary knew"
// when it is not. replay selects and renders; it does not verify, sign, or
// write any file but the one it is handed.
package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"notary/internal/export"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
)

// Reader is the ledger as replay uses it: an as-of read that returns the
// verified prefix and any breaks it found. *ledger.Ledger satisfies it, and a
// test can supply an in-memory fake, exactly as export.Reader and
// reconcile.Reader are shaped.
//
// A break is not an error. The read fails only when the ledger cannot be read;
// an integrity problem is reported as a break so the caller decides whether to
// refuse to act on it, exactly as ledger.ReplayAsOf does.
type Reader interface {
	ReplayAsOf(t time.Time, v *sign.Verifier) ([]record.Record, []ledger.Break, error)
}

// Request is one replay invocation.
type Request struct {
	// At is the knowledge instant: records whose RecordedAt is at or before it
	// are in the view, inclusive. It is required; there is no default, because
	// silently replaying "now" for an unparsed instant is the wrong question.
	At time.Time

	// Scope, when any field is non-zero, restricts the replay to records whose
	// subject scope matches every non-zero field. An all-zero Scope is no
	// filter. It is export.ScopeMatches, the one matcher, so the two read paths
	// compose identically.
	Scope record.Scope

	// IncludeSensitive, when true, allows sensitive content to be rendered.
	// When false -- the safe default -- a record whose stored content is marked
	// sensitive has its text withheld and its line states redacted="sensitive".
	// The flag changes only what is printed; it never touches a hash, because
	// rendering is a read path.
	IncludeSensitive bool
}

// Result reports what a replay did.
//
// It is meaningful in full only on success. On the error path it carries the
// counts as they stood when the failure occurred (the lines already written),
// matching export.Result, so a caller must check the error before trusting it.
type Result struct {
	// Records is the number of JSONL lines written.
	Records int
	// Redacted is the number of lines whose content was withheld because the
	// stored record marked it sensitive and IncludeSensitive was false. A
	// record that carried no content is not counted: nothing was hidden.
	Redacted int
	// Breaks is every integrity break the prefix check found. It is not an
	// error: replay still renders the prefix it was given, and the caller
	// decides how to surface the breaks. A clean prefix leaves this empty.
	Breaks []ledger.Break
}

// Replayer renders a verified ledger prefix as JSONL through the shared export
// renderer and encoder.
type Replayer struct {
	reader Reader
	v      *sign.Verifier
}

// New returns a Replayer that reads through r and verifies the prefix with v.
// v is passed through to the reader unchanged; a nil verifier reports a
// signature break for every record, which is the reader's decision to make,
// not this constructor's.
func New(r Reader, v *sign.Verifier) *Replayer {
	return &Replayer{reader: r, v: v}
}

// Replay reads the ledger as of req.At, narrows it by req.Scope, and writes one
// JSONL line per record to out, through export.Render and the encoder
// export.NewLineEncoder. Records and Redacted are tallied exactly as
// Exporter.Export does, and ctx is checked between records the same way.
//
// The prefix's breaks go in Result.Breaks and are NOT an error: replay renders
// the records the reader returned, and the caller decides how to surface the
// breaks. Replay fails only on a zero At, a read error, a render error, a write
// error, or a cancelled context.
//
// Replay does not paraphrase, does not sign, and writes to no file but out. It
// exists so a replayed line is byte-identical to an exported one by
// construction (design §6): both paths render through export.Render and encode
// through export.NewLineEncoder.
func (rp *Replayer) Replay(ctx context.Context, req Request, out io.Writer) (Result, error) {
	// A zero At is a missing instant, not midnight of year one: reject it here
	// rather than silently replaying an empty prefix, mirroring Export's
	// rejection of a zero bound. Task 5's CLI always parses --at, but the
	// package is reusable and closing the footgun is cheap.
	if req.At.IsZero() {
		return Result{}, errors.New("replay: At must be set; a zero instant would silently replay an empty prefix")
	}

	records, breaks, err := rp.reader.ReplayAsOf(req.At, rp.v)
	if err != nil {
		return Result{}, fmt.Errorf("replay: read ledger: %w", err)
	}

	// Carry the breaks even when the scope filter renders nothing: they are a
	// property of the prefix, not of what this caller chose to print.
	res := Result{Breaks: breaks}

	// Narrow to the records this replay renders, exactly as Export does before
	// its loop, so the counting and the ctx check cover the same set.
	var matched []record.Record
	for _, rec := range records {
		if export.ScopeMatches(req.Scope, rec.Subject.Scope) {
			matched = append(matched, rec)
		}
	}

	enc := export.NewLineEncoder(out)
	for _, rec := range matched {
		// ctx stops a long replay between records. The read itself is not
		// cancellable -- Reader.ReplayAsOf takes no context -- so a cancel is
		// observed only after the read returns, record by record.
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("replay: %w", err)
		}
		line, err := export.Render(rec, req.IncludeSensitive)
		if err != nil {
			return res, err
		}
		if err := enc.Encode(line); err != nil {
			return res, fmt.Errorf("replay: write record %s: %w", rec.ID, err)
		}
		res.Records++
		if line.Redacted != "" {
			res.Redacted++
		}
	}

	return res, nil
}
