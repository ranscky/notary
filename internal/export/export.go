package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"notary/internal/record"
	"notary/internal/sign"
)

// defaultMaxSpan caps an export's range when Request.MaxSpan is unset. The cost
// of an export scales with the range, and the failure mode of a compliance
// export should be an error rather than an out-of-memory kill (design §9).
const defaultMaxSpan = 366 * 24 * time.Hour

// Reader is the read side of the ledger the exporter renders. It is the
// existing ledger read method, so the exporter depends only on a small
// interface and a test can supply an in-memory fake, exactly as
// reconcile.Reader does.
type Reader interface {
	ListRecords(from, to time.Time) ([]record.Record, error)
}

// Request is one export invocation.
type Request struct {
	// From and To bound the export on a record's At -- the event time -- not
	// RecordedAt, matching reconcile --since. A record reconstructed later
	// appears in the window its event belongs to.
	From time.Time
	To   time.Time

	// Scope, when any field is non-zero, restricts the export to records whose
	// subject scope matches every non-zero field. An all-zero Scope is no
	// filter.
	Scope record.Scope

	// IncludeSensitive, when true, allows sensitive content to be rendered.
	// Redaction itself lands in Task 6; this task renders all content.
	IncludeSensitive bool

	// CheckpointOut names the file a signed head checkpoint is written to. The
	// checkpoint lands in Task 7.
	CheckpointOut string

	// MaxSpan caps To-From. A non-positive value selects defaultMaxSpan.
	MaxSpan time.Duration

	// Phrase requests a paraphrase pass. It lands in Task 10.
	Phrase bool
}

// Result reports what an export did.
//
// It is meaningful in full only on success. On the error path from Export it
// records how far the export got before it failed: Records is the lines already
// written and Checkpoint is nil regardless. A non-zero Result is therefore not
// by itself a success signal -- a caller must check the error returned beside
// it before trusting any field.
type Result struct {
	// Records is the number of JSONL lines written. On the error path it is the
	// count written before the failure.
	Records int
	// Redacted is the number of lines whose content was withheld. It is always
	// zero in this task: redaction lands in Task 6.
	Redacted int
	// Checkpoint is the signed head checkpoint, when one was requested and
	// written. It is always nil in this task (the checkpoint lands in Task 7),
	// and it is nil on the error path.
	Checkpoint *sign.Checkpoint
}

// Exporter renders a range of the ledger as JSONL.
type Exporter struct {
	reader Reader
}

// New returns an Exporter that reads the range through r.
func New(r Reader) *Exporter {
	return &Exporter{reader: r}
}

// Export renders every record in req's range, in ledger order, as one JSONL
// line each, writing to out.
//
// The write side streams: each record is rendered and written as it is reached,
// and no slice of Lines is ever collected, so output memory does not scale with
// the range. The READ side does not stream -- Reader.ListRecords returns the
// whole range as a slice, which is the ledger's read interface -- so only the
// render/write loop is incremental.
//
// It fails loudly on an invalid request, a read error, or a write error. On
// error the returned Result is NOT zero: it carries the counts as they stood
// when the failure occurred (the lines already written), so a caller must check
// the error before trusting the Result. A range with no records is a success
// with zero lines (design §9).
func (e *Exporter) Export(ctx context.Context, req Request, out io.Writer) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}

	records, err := e.reader.ListRecords(req.From, req.To)
	if err != nil {
		return Result{}, fmt.Errorf("export: read ledger: %w", err)
	}

	enc := json.NewEncoder(out)
	// Content is text a human reads: keep <, >, and & as themselves rather than
	// \u003c escapes, so the export stays greppable.
	enc.SetEscapeHTML(false)

	var res Result
	for _, rec := range records {
		// ctx stops a long range between records. The read itself is NOT
		// cancellable: Reader.ListRecords takes no context, so a cancel is
		// observed only after the read returns, record by record.
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("export: %w", err)
		}
		if !scopeMatches(req.Scope, rec.Subject.Scope) {
			continue
		}
		line, err := Render(rec, req.IncludeSensitive)
		if err != nil {
			return res, err
		}
		if err := enc.Encode(line); err != nil {
			return res, fmt.Errorf("export: write record %s: %w", rec.ID, err)
		}
		res.Records++
	}
	return res, nil
}

// validate rejects an invocation the exporter cannot honour: a missing bound, a
// reversed range, or a span over the cap. The store returns nothing when a
// bound is the zero time, so an unbounded request must fail here rather than
// silently export an empty range.
func (r Request) validate() error {
	if r.From.IsZero() || r.To.IsZero() {
		return errors.New("export: --from and --to are required")
	}
	if r.To.Before(r.From) {
		return fmt.Errorf("export: --to %s is before --from %s",
			r.To.Format(time.RFC3339Nano), r.From.Format(time.RFC3339Nano))
	}
	max := r.MaxSpan
	if max <= 0 {
		max = defaultMaxSpan
	}
	if span := r.To.Sub(r.From); span > max {
		return fmt.Errorf("export: range %s exceeds --max-span %s", span, max)
	}
	return nil
}

// scopeMatches reports whether a record's scope satisfies want. A zero field in
// want is a wildcard; every non-zero field must match. It is the same rule
// reconcile's Window uses.
func scopeMatches(want record.Scope, got record.Scope) bool {
	if want.UserID != "" && want.UserID != got.UserID {
		return false
	}
	if want.AgentID != "" && want.AgentID != got.AgentID {
		return false
	}
	if want.AppID != "" && want.AppID != got.AppID {
		return false
	}
	if want.RunID != "" && want.RunID != got.RunID {
		return false
	}
	return true
}
