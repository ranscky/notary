// Package explain is the single-subject view over the ledger: one record's
// reason, or one memory's lifecycle, rendered as prose (or a small JSON object)
// for a compliance lead who must not read code.
//
// It is a read path: it phrases and renders through internal/export and writes
// to no file but the writer it is handed. It offers no paraphrase and no
// --phrase -- internal/phrase is the only package that talks to a language
// model and no core package imports it (design D10) -- and its sentence is
// export.Phrase's, the same deterministic sentence the range view prints.
// Because it answers one subject, --json marshals its own small shape rather
// than export.Line, taking the sentence, the withholding decision and the tier
// from export so the two views cannot drift.
package explain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"notary/internal/export"
	"notary/internal/record"
)

// Reader is the ledger as explain uses it: one record by id, or one memory's
// records in Seq order. *ledger.Ledger satisfies it, and a test can supply an
// in-memory fake, exactly as export.Reader, replay.Reader and reconcile.Reader
// are shaped.
//
// ListRecordsByMemory does NOT reject an empty memoryID, so the guard that
// refuses an empty subject lives in Explain, the caller.
type Reader interface {
	GetRecord(id record.RecordID) (record.Record, error)
	ListRecordsByMemory(memoryID string) ([]record.Record, error)
}

// Request is one explain invocation.
type Request struct {
	// RecordID selects the single-record view: one record's reason. It takes
	// precedence over MemoryID, so a request setting both is resolved rather
	// than refused; a request setting NEITHER is a missing subject and an error.
	RecordID record.RecordID

	// MemoryID selects the memory view: one memory's lifecycle, one line per
	// record in Seq order. It is read only when RecordID is empty.
	MemoryID string

	// IncludeSensitive, when true, allows sensitive content to be rendered.
	// When false -- the safe default -- sensitive text is withheld and the
	// output says so, so a reader can tell "withheld" from "no content". The
	// flag changes only what is printed; it never touches a hash, because
	// rendering is a read path.
	IncludeSensitive bool

	// JSON selects the machine-readable view: one JSON object carrying the same
	// records, in the same order, as the prose view. It is explain's own small
	// shape, not export.Line.
	JSON bool
}

// Result reports what an explanation did.
//
// It is meaningful in full only on success. Explain fails on two kinds of path,
// and they differ. A missing subject, a read failure, a render failure and a
// cancelled context all happen while the whole view is built, BEFORE any byte is
// written, so on those paths out is untouched and Result is the zero value. A
// write failure is the other kind: it happens after the build, out may already
// be partially written, and Result then carries the counts as they stood when
// the failure occurred -- the records tallied before the write failed, matching
// export.Result and replay.Result. Either way a caller must check the error
// beside Result before trusting any field.
type Result struct {
	// Records is the number of records in the view.
	Records int
	// Redacted is the number of records whose content was withheld because the
	// stored record marked it sensitive and IncludeSensitive was false. A
	// record that carried no content is not counted: nothing was hidden.
	Redacted int
}

// Explainer renders a single ledger subject as prose or as its own JSON shape.
type Explainer struct {
	reader Reader
}

// New returns an Explainer that reads through r.
func New(r Reader) *Explainer {
	return &Explainer{reader: r}
}

// Explain reads the subject named by req -- req.RecordID for one record, or
// req.MemoryID for one memory's lifecycle -- and writes it to out, as prose or,
// with req.JSON, as a single JSON object.
//
// A request that names neither subject is an error and the reader is not
// consulted: the empty-memoryID guard lives here because the store cannot tell
// "no filter" from "match the empty column", so an empty MemoryID would
// silently list the wrong rows. When both are set, RecordID wins; a both-set
// request is the CLI's usage error (design §7), not this package's to refuse.
//
// Phrase returning false -- an event or reason kind outside the vocabulary --
// is an ERROR here, not a stub line: such a claim reads as a missing record in
// a compliance view, and a tool whose purpose is to answer "why" must fail
// loudly rather than print a sentence that says nothing. Render reaches the same
// record and owns the canonical wording of the fault, so explain surfaces
// Render's error.
//
// Explain fails only on a missing subject, a read error, a render error (an
// invalid tier or an unphrased claim), a write error, or a cancelled context.
// It does not paraphrase, does not sign, and writes to no file but out.
func (e *Explainer) Explain(ctx context.Context, req Request, out io.Writer) (Result, error) {
	// Reject a missing subject before touching the reader: a zero RecordID and
	// an empty MemoryID name no record and no memory. This is the one guard the
	// store left to its caller.
	if req.RecordID == "" && req.MemoryID == "" {
		return Result{}, errors.New("explain: a subject is required: set RecordID or MemoryID")
	}

	records, err := e.read(req)
	if err != nil {
		return Result{}, err
	}

	// The whole view is built before a byte is written, so any failure while it
	// is built leaves out untouched rather than half-written.
	var res Result
	var prose strings.Builder
	jsonRecords := make([]jsonRecord, 0, len(records))

	if !req.JSON && req.RecordID == "" {
		fmt.Fprintf(&prose, "Memory %s\n", req.MemoryID)
	}

	for _, rec := range records {
		// ctx stops a long memory lifecycle between records; the read itself is
		// not cancellable, so a cancel is observed only after the read returns.
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("explain: %w", err)
		}

		// Phrase is the one source of the claim's sentence, shared with the
		// export view. A false result means the record is outside the vocabulary
		// and has no sentence; Render rejects the same record, so ask it for the
		// canonical error rather than inventing a second wording of the fault.
		sentence, ok := export.Phrase(rec)
		if !ok {
			_, err := export.Render(rec, req.IncludeSensitive)
			return Result{}, fmt.Errorf("explain: %w", err)
		}

		// Render, not a reimplementation, decides whether to withhold content
		// and what the tier is: there is exactly one redaction rule, in export.
		line, err := export.Render(rec, req.IncludeSensitive)
		if err != nil {
			return Result{}, fmt.Errorf("explain: %w", err)
		}

		switch {
		case req.JSON:
			jsonRecords = append(jsonRecords, toJSONRecord(rec, line, sentence))
		case req.RecordID != "":
			writeRecordProse(&prose, rec, line, sentence)
		default:
			writeTimelineLine(&prose, rec, line, sentence)
		}

		res.Records++
		if line.Redacted != "" {
			res.Redacted++
		}
	}

	if req.JSON {
		return res, writeJSON(out, jsonRecords)
	}
	return res, writeString(out, prose.String())
}

// read fetches the subject's records, choosing RecordID over MemoryID and
// wrapping any read error. Called only after the empty-subject guard, so it
// always has a subject to read.
func (e *Explainer) read(req Request) ([]record.Record, error) {
	if req.RecordID != "" {
		rec, err := e.reader.GetRecord(req.RecordID)
		if err != nil {
			return nil, fmt.Errorf("explain: get record %s: %w", req.RecordID, err)
		}
		return []record.Record{rec}, nil
	}
	records, err := e.reader.ListRecordsByMemory(req.MemoryID)
	if err != nil {
		return nil, fmt.Errorf("explain: list records for memory %s: %w", req.MemoryID, err)
	}
	return records, nil
}

// jsonRecord is explain's own wire shape for one record: the fields a
// single-subject reader needs, and none of export.Line's range-view fields.
type jsonRecord struct {
	Seq        uint64                `json:"seq"`
	ID         record.RecordID       `json:"id"`
	MemoryID   string                `json:"memory_id,omitempty"`
	Event      record.EventType      `json:"event"`
	ReasonKind record.ReasonKind     `json:"reason_kind"`
	Tier       record.VisibilityTier `json:"tier"`
	At         time.Time             `json:"at"`
	RecordedAt time.Time             `json:"recorded_at"`
	// Sentence is export.Phrase's wording, the same sentence the prose view
	// prints; it sits beside the structured fields, never instead of them.
	Sentence string `json:"sentence"`
	// Content is the memory text, present only when shown; it is a pointer so
	// "no content" and "empty content" stay distinguishable.
	Content *string `json:"content,omitempty"`
	// Redacted names why content was withheld, "sensitive", matching
	// export.Render. It is absent whenever content was shown -- and, critically,
	// whenever the record carried no content -- so "nothing was recorded" and
	// "you are not cleared for it" never look alike.
	Redacted string `json:"redacted,omitempty"`
}

// jsonView is explain's own JSON document: one subject's records, in Seq order.
type jsonView struct {
	Records []jsonRecord `json:"records"`
}

// toJSONRecord projects a record and its rendered line into explain's wire
// shape.
func toJSONRecord(rec record.Record, line export.Line, sentence string) jsonRecord {
	return jsonRecord{
		Seq:        rec.Seq,
		ID:         rec.ID,
		MemoryID:   rec.Subject.MemoryID,
		Event:      rec.Event,
		ReasonKind: rec.Reason.Kind(),
		Tier:       line.Tier,
		At:         rec.At,
		RecordedAt: rec.RecordedAt,
		Sentence:   sentence,
		Content:    line.Content,
		Redacted:   line.Redacted,
	}
}

// writeRecordProse writes the single-record view: the subject, then the phrased
// claim, then the recorded-at instant and the tier, then the content or the
// fact that it was withheld.
func writeRecordProse(b *strings.Builder, rec record.Record, line export.Line, sentence string) {
	if rec.Subject.MemoryID != "" {
		fmt.Fprintf(b, "Record %s for memory %s\n", rec.ID, rec.Subject.MemoryID)
	} else {
		fmt.Fprintf(b, "Record %s\n", rec.ID)
	}
	fmt.Fprintf(b, "%s\n", sentence)
	fmt.Fprintf(b, "Recorded at %s, tier %s.\n", rec.RecordedAt.UTC().Format(time.RFC3339), line.Tier)
	writeContent(b, line)
}

// writeTimelineLine writes one memory-view line: a single line carrying the
// record's position, id, event, reason kind, tier and sentence, plus the
// content or the fact that it was withheld.
func writeTimelineLine(b *strings.Builder, rec record.Record, line export.Line, sentence string) {
	fmt.Fprintf(b, "%d. %s %s %s %s: %s", rec.Seq, rec.ID, rec.Event, rec.Reason.Kind(), line.Tier, sentence)
	switch {
	case line.Redacted != "":
		fmt.Fprintf(b, " [content withheld: %s]\n", line.Redacted)
	case line.Content != nil:
		fmt.Fprintf(b, " [content: %s]\n", *line.Content)
	default:
		b.WriteByte('\n')
	}
}

// writeContent appends the content line to the single-record prose. A withheld
// record says so; a record that carried no content adds nothing, so the two
// states never look alike.
func writeContent(b *strings.Builder, line export.Line) {
	switch {
	case line.Redacted != "":
		fmt.Fprintf(b, "Content withheld: %s.\n", line.Redacted)
	case line.Content != nil:
		fmt.Fprintf(b, "Content: %s\n", *line.Content)
	}
}

// writeJSON encodes the view as one JSON object, leaving <, > and & literal
// (SetEscapeHTML(false)) so the sentence's words are not rewritten.
func writeJSON(out io.Writer, records []jsonRecord) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(jsonView{Records: records}); err != nil {
		return fmt.Errorf("explain: write: %w", err)
	}
	return nil
}

// writeString writes the built prose to out.
func writeString(out io.Writer, s string) error {
	if _, err := io.WriteString(out, s); err != nil {
		return fmt.Errorf("explain: write: %w", err)
	}
	return nil
}
