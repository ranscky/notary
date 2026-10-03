package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"notary/internal/record"
	"notary/internal/sign"
)

// defaultMaxSpan caps an export's range when Request.MaxSpan is unset. The cost
// of an export scales with the range, and the failure mode of a compliance
// export should be an error rather than an out-of-memory kill (design §9).
const defaultMaxSpan = 366 * 24 * time.Hour

// Reader is the ledger as the exporter uses it: the read side it renders, plus
// the head attestation a checkpoint needs. It is the existing ledger read and
// checkpoint methods, so the exporter depends only on a small interface and a
// test can supply an in-memory fake, exactly as reconcile.Reader does.
//
// Checkpoint is the read side's companion, not a second concern bolted on: an
// export that carries a checkpoint must be able to attest the ledger HEAD, and
// the head may lie beyond the exported range, so it cannot be derived from
// ListRecords. ledger.Ledger implements both methods, so the CLI still wires the
// exporter with a single reference.
type Reader interface {
	ListRecords(from, to time.Time) ([]record.Record, error)
	// Checkpoint signs a statement attesting the ledger's current head, the same
	// method ledger.Ledger.Checkpoint provides. It signs with sg and stamps the
	// checkpoint with now. An empty ledger has no head to attest to and errors.
	Checkpoint(sg *sign.Signer, now time.Time) (sign.Checkpoint, error)
}

// Option configures an Exporter at construction. It is applied once by New; it
// is a construction-time decision, not a per-request one, so a signer and a
// clock are set for the exporter's whole life rather than carried on Request.
type Option func(*Exporter)

// WithSigner sets the signer a Request.CheckpointOut export uses to sign its
// head checkpoint. Without it, a request that asks for a checkpoint fails
// rather than writing an unsigned file: an unsigned artifact named "checkpoint"
// looks like evidence and is not, so producing nothing is strictly safer.
//
// The signer is a dependency rather than a Request field because a signer is a
// process-level key, not a value that varies per export.
func WithSigner(sg *sign.Signer) Option {
	return func(e *Exporter) { e.signer = sg }
}

// WithClock sets the clock a checkpoint is stamped with, so an export can be
// deterministic in a test. A nil now is ignored, leaving the default (time.Now),
// so a caller cannot disable the clock.
func WithClock(now func() time.Time) Option {
	return func(e *Exporter) {
		if now != nil {
			e.now = now
		}
	}
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
	// When false -- the safe default -- a record whose stored content is marked
	// sensitive has its text withheld and its line states redacted="sensitive".
	// The flag changes only what is printed; it never touches a hash, because
	// rendering is a read path (design §3 D4, §6).
	IncludeSensitive bool

	// CheckpointOut names the file a signed head checkpoint is written to after
	// the lines, when non-empty. It attests the ledger head at export time, in
	// the same format verify --checkpoint reads, so a later verify can detect a
	// shortened tail. Empty means no checkpoint and no file.
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
	// Redacted is the number of lines whose content was withheld because the
	// stored record marked it sensitive and IncludeSensitive was false. A
	// record that carried no content is not counted: nothing was hidden.
	Redacted int
	// Checkpoint is the signed head checkpoint, set when a request named
	// CheckpointOut and the checkpoint was written. It is nil when no checkpoint
	// was requested, and nil on the error path.
	Checkpoint *sign.Checkpoint
}

// Exporter renders a range of the ledger as JSONL.
type Exporter struct {
	reader Reader
	signer *sign.Signer
	now    func() time.Time
}

// New returns an Exporter that reads the range through r. opts configure it for
// its whole life; WithSigner supplies the key a checkpoint is signed with and
// WithClock the instant it is stamped with. A nil option is tolerated, so a
// caller cannot crash construction by passing one.
func New(r Reader, opts ...Option) *Exporter {
	e := &Exporter{reader: r, now: time.Now}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	return e
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
//
// When req.CheckpointOut names a file, a signed checkpoint of the ledger HEAD is
// written there after the lines, and Result.Checkpoint carries it. The head, not
// the last record in the range, is attested: a shortened tail is what a hash
// chain cannot see, and a checkpoint is what lets a later verify catch it.
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
		if line.Redacted != "" {
			res.Redacted++
		}
	}

	// The checkpoint is written last, after every line, so a broken export does
	// not leave a checkpoint implying a clean one. It attests the ledger HEAD,
	// not the last record in the range: the truncation a chain cannot see is a
	// removed TAIL, and a checkpoint taken here is what makes that detectable
	// later (design §9, parent §7).
	if req.CheckpointOut != "" {
		cp, err := e.writeCheckpoint(req.CheckpointOut)
		if err != nil {
			return res, err
		}
		res.Checkpoint = &cp
	}
	return res, nil
}

// writeCheckpoint signs a checkpoint for the ledger's current head and writes
// its canonical JSON to path, returning the signed checkpoint. The bytes are
// sign.MarshalCheckpoint's encoding -- the exact format cmd/notary's
// --write-checkpoint writes and its --checkpoint flag reads back through
// sign.UnmarshalCheckpoint -- so the artifact an export produces is immediately
// usable by a command that already exists. There is deliberately no second
// encoding: two artifacts with one name and two shapes is the failure mode this
// avoids.
func (e *Exporter) writeCheckpoint(path string) (sign.Checkpoint, error) {
	if e.signer == nil {
		return sign.Checkpoint{}, errors.New("export: --checkpoint-out requires a signer (use WithSigner)")
	}
	cp, err := e.reader.Checkpoint(e.signer, e.now())
	if err != nil {
		return sign.Checkpoint{}, fmt.Errorf("export: create checkpoint: %w", err)
	}
	data, err := sign.MarshalCheckpoint(cp)
	if err != nil {
		return sign.Checkpoint{}, fmt.Errorf("export: marshal checkpoint: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return sign.Checkpoint{}, fmt.Errorf("export: write checkpoint %s: %w", path, err)
	}
	return cp, nil
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
