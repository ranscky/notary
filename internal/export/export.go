package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"notary/internal/phrase"
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

// Paraphraser is the optional language-model pass the exporter may run. It is
// the one method of phrase.Client, expressed here as an interface so
// internal/export can be exercised with a fake -- no provider, no network, no
// key -- and so the CLI supplies the real client only when --phrase is given.
//
// It returns phrase.Paraphrase, a display-only type no decision package can
// import (design D7): the value flows to a JSONL line and nowhere else.
type Paraphraser interface {
	Paraphrase(ctx context.Context, records []record.Record) (phrase.Paraphrase, error)
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

// WithParaphraser sets the pass a Request.Phrase export runs. Without it, a
// request that sets Phrase fails rather than silently omitting the paraphrase
// the operator asked for: a paraphrase that is quietly dropped is exactly the
// loss the design refuses to hide.
//
// The paraphraser is a dependency rather than a Request field because it is a
// client -- a provider, a credential, a cost -- not a value that varies per
// export. The CLI supplies the real phrase.Client only under --phrase; a test
// supplies a fake.
func WithParaphraser(p Paraphraser) Option {
	return func(e *Exporter) { e.paraphraser = p }
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

	// Phrase requests a paraphrase pass: when set, the exporter runs the
	// configured paraphraser (see WithParaphraser) over the records it renders
	// and attaches the result to each line. It is off by default, so no provider
	// is billed unless it is asked for (design §8). A failure of the pass never
	// fails the export; it is reported through Result.ParaphraseFailed and
	// Result.ParaphraseError.
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
	// ParaphraseFailed reports that a paraphrase pass was requested and the
	// provider call failed. The failure never fails the export (design §8): the
	// structured records are rendered unchanged and Export returns no error, so
	// a phrasing outage cannot cost an operator their audit output. It is the
	// summary the CLI reports on stderr; the reason is in ParaphraseError.
	ParaphraseFailed bool
	// ParaphraseError is the reason a requested paraphrase pass failed, empty on
	// success or when no pass ran. It is carried out of the exporter so the
	// failure can be reported with WHY it happened rather than as a bare flag,
	// because a loss the tool cannot explain is a loss it should not hide.
	ParaphraseError string
}

// Exporter renders a range of the ledger as JSONL.
type Exporter struct {
	reader      Reader
	signer      *sign.Signer
	paraphraser Paraphraser
	now         func() time.Time
}

// New returns an Exporter that reads the range through r. opts configure it for
// its whole life; WithSigner supplies the key a checkpoint is signed with,
// WithClock the instant it is stamped with, and WithParaphraser the pass a
// Request.Phrase export runs. A nil option is tolerated, so a caller cannot
// crash construction by passing one.
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
// render/write loop is incremental. When a paraphrase pass runs it narrows that
// slice to the records it will render first, so the pass and the loop agree on
// what is in the export; that narrowing is a view of the already-held slice and
// adds no scaling beyond the read.
//
// It fails loudly on an invalid request (including Phrase with no paraphraser),
// a read error, or a write error. On error the returned Result is NOT zero: it
// carries the counts as they stood when the failure occurred (the lines already
// written), so a caller must check the error before trusting the Result. A
// range with no records is a success with zero lines (design §9).
//
// A failed paraphrase pass is deliberately NOT such an error: it degrades the
// prose, never the evidence.
//
// When req.Phrase is set, a paraphrase pass runs over the records the export
// renders -- after the scope filter, so it never bills for a record the reader
// cannot see. The pass is attempted once for the whole run, and its result is
// attached to every line it covers. A failed pass never fails the export: the
// records render unchanged, the failure is reported through
// Result.ParaphraseFailed and Result.ParaphraseError, and Export returns no
// error (design §8). A run with no rendered records is not passed to the
// paraphraser at all, because the client errors on an empty request rather than
// making a pointless billed call.
//
// When req.CheckpointOut names a file, a signed checkpoint of the ledger HEAD is
// written there after the lines, and Result.Checkpoint carries it. The head, not
// the last record in the range, is attested: a shortened tail is what a hash
// chain cannot see, and a checkpoint is what lets a later verify catch it.
func (e *Exporter) Export(ctx context.Context, req Request, out io.Writer) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	if req.Phrase && e.paraphraser == nil {
		return Result{}, errors.New("export: Phrase requested but no paraphraser is set (use WithParaphraser)")
	}

	records, err := e.reader.ListRecords(req.From, req.To)
	if err != nil {
		return Result{}, fmt.Errorf("export: read ledger: %w", err)
	}

	// Narrow to the records this export renders. The paraphrase pass is handed
	// only these, so it cannot bill for -- or describe -- a record the reader
	// never sees. The slice is a filtered view of records, which the read side
	// already holds whole, so this adds no scaling beyond the read.
	var matched []record.Record
	for _, rec := range records {
		if scopeMatches(req.Scope, rec.Subject.Scope) {
			matched = append(matched, rec)
		}
	}

	var res Result
	// Run the paraphrase pass once, before rendering, so every line carries the
	// same run result. An empty run is skipped: the client errors on zero
	// records rather than making a pointless billed call, so calling it here
	// would turn every empty range into a spurious failure.
	var para *phrase.Paraphrase
	if req.Phrase && len(matched) > 0 {
		p, perr := e.paraphraser.Paraphrase(ctx, matched)
		if perr != nil {
			res.ParaphraseFailed = true
			res.ParaphraseError = perr.Error()
		} else {
			para = &p
		}
	}

	enc := json.NewEncoder(out)
	// Content is text a human reads: keep <, >, and & as themselves rather than
	// \u003c escapes, so the export stays greppable.
	enc.SetEscapeHTML(false)

	for _, rec := range matched {
		// ctx stops a long range between records. The read itself is NOT
		// cancellable: Reader.ListRecords takes no context, so a cancel is
		// observed only after the read returns, record by record.
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("export: %w", err)
		}
		line, err := Render(rec, req.IncludeSensitive)
		if err != nil {
			return res, err
		}
		// The paraphrase is a guest on the line: set beside the rendered record,
		// and only on success, so a failed pass leaves the line exactly as it
		// would have been without --phrase.
		line.Paraphrase = para
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
