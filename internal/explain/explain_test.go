package explain_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/explain"
	"notary/internal/export"
	"notary/internal/record"
)

// fakeReader is an in-memory Reader. It returns fixed records so a test can
// drive Explain without a database and prove it reads the right subject, renders
// what it was handed, and counts exactly what it withheld. It counts the calls
// it received so a test can prove the reader was left alone.
type fakeReader struct {
	byID      map[record.RecordID]record.Record
	byMem     map[string][]record.Record
	getErr    error
	listErr   error
	getCalls  int
	listCalls int
}

func (f *fakeReader) GetRecord(id record.RecordID) (record.Record, error) {
	f.getCalls++
	if f.getErr != nil {
		return record.Record{}, f.getErr
	}
	rec, ok := f.byID[id]
	if !ok {
		return record.Record{}, fmt.Errorf("record %s not found", id)
	}
	return rec, nil
}

func (f *fakeReader) ListRecordsByMemory(memoryID string) ([]record.Record, error) {
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.byMem[memoryID], nil
}

// explainAt is a fixed instant the fixtures are stamped with, so prose carries a
// deterministic recorded-at.
var explainAt = time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

// reasonFor builds a valid Reason for kind, choosing the tier constructor the
// kind's AllowedTier fixes, so the test does not re-encode the tier mapping.
// It mirrors internal/export/phrase_test.go's reasonFor.
func reasonFor(t *testing.T, kind record.ReasonKind) record.Reason {
	t.Helper()
	tier, ok := kind.AllowedTier()
	require.Truef(t, ok, "kind %q has no allowed tier", kind)

	switch tier {
	case record.Observed:
		ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"ok":true}`))
		require.NoError(t, err)
		r, err := record.NewObservedReason(kind, ev)
		require.NoError(t, err)
		return r
	case record.Reconstructed:
		ev, err := record.NewReconstructedEvidence([]record.RecordID{"basis-1"}, "rule", "1", 0)
		require.NoError(t, err)
		r, err := record.NewReconstructedReason(kind, ev)
		require.NoError(t, err)
		return r
	case record.Internal:
		note, err := record.NewInternalNote("note")
		require.NoError(t, err)
		r, err := record.NewInternalReason(kind, note)
		require.NoError(t, err)
		return r
	default:
		t.Fatalf("unhandled tier %v for kind %q", tier, kind)
		return record.Reason{}
	}
}

// mkRec builds a renderable record: a genuine reason so Render can phrase it,
// and a populated subject. It carries no chain fields, which Render does not
// need.
func mkRec(t *testing.T, id record.RecordID, seq uint64, event record.EventType, kind record.ReasonKind, memID string, recordedAt time.Time, content *record.Content) record.Record {
	t.Helper()
	var ch record.Hash
	for i := range ch {
		ch[i] = 0x11
	}
	return record.Record{
		ID:         id,
		Seq:        seq,
		At:         recordedAt.Add(-time.Second),
		RecordedAt: recordedAt,
		Event:      event,
		Reason:     reasonFor(t, kind),
		Subject:    record.Subject{MemoryID: memID, Scope: record.Scope{UserID: "u1"}, ContentHash: ch},
		Content:    content,
	}
}

// --- single-record view -----------------------------------------------------

// TestExplainSingleRecordRendersItsPhrasedClaim pins the core of the single
// record view: the sentence export.Phrase produces for that record appears in
// the prose, and GetRecord -- not ListRecordsByMemory -- was the read used.
func TestExplainSingleRecordRendersItsPhrasedClaim(t *testing.T) {
	rec := mkRec(t, "rec-1", 1, record.EventMemoryKept, record.ReasonStoredByMem0, "mem-A", explainAt, nil)
	r := &fakeReader{byID: map[record.RecordID]record.Record{"rec-1": rec}}
	e := explain.New(r)

	var buf bytes.Buffer
	res, err := e.Explain(context.Background(), explain.Request{RecordID: "rec-1"}, &buf)
	require.NoError(t, err)

	sentence, ok := export.Phrase(rec)
	require.True(t, ok, "the fixture must be a phraseable record")
	assert.Contains(t, buf.String(), sentence, "the phrased claim must appear in the prose")
	assert.Contains(t, buf.String(), "rec-1", "the prose must name the record it explains")

	assert.Equal(t, 1, res.Records)
	assert.Equal(t, 0, res.Redacted)
	assert.Equal(t, 1, r.getCalls, "a single-record view reads through GetRecord")
	assert.Equal(t, 0, r.listCalls, "a single-record view must not list by memory")
}

// TestExplainSingleRecordRejectsAnUnphraseableRecord is the falsifier: a record
// export.Phrase cannot word must fail loudly, not print a stub line whose prose
// reads as a missing record. The error must name the record and the missing
// phrasing, and nothing may be written to out. The pair is built the way
// internal/export/phrase_test.go builds it: an event outside the vocabulary
// with an otherwise-valid reason, so Render reaches Phrase and it returns false.
func TestExplainSingleRecordRejectsAnUnphraseableRecord(t *testing.T) {
	rec := mkRec(t, "rec-bad", 1, record.EventMemoryKept, record.ReasonStoredByMem0, "mem-A", explainAt, nil)
	rec.Event = record.EventType("not_an_event")
	r := &fakeReader{byID: map[record.RecordID]record.Record{"rec-bad": rec}}
	e := explain.New(r)

	var buf bytes.Buffer
	res, err := e.Explain(context.Background(), explain.Request{RecordID: "rec-bad"}, &buf)
	require.Error(t, err, "an unphrased claim is an error, never a rendered line")
	assert.Contains(t, err.Error(), "rec-bad", "the error must name the record")
	assert.Contains(t, err.Error(), "no phrasing", "the error must name the missing phrasing")
	assert.Contains(t, err.Error(), "not_an_event", "the error must name the offending event")
	assert.Empty(t, buf.String(), "nothing may be written when the record cannot be explained")
	assert.Equal(t, explain.Result{}, res)
}

// --- memory view ------------------------------------------------------------

// TestExplainMemoryRendersATimelineInSeqOrder pins Review Focus 3 for prose: a
// memory's records render one line each, in the Seq order the reader returned
// them, and each line carries its event, reason and tier.
func TestExplainMemoryRendersATimelineInSeqOrder(t *testing.T) {
	r1 := mkRec(t, "rec-1", 1, record.EventAddResolved, record.ReasonStoredByMem0, "mem-A", explainAt, nil)
	r2 := mkRec(t, "rec-2", 2, record.EventMemorySurfaced, record.ReasonReturnedBySearch, "mem-A", explainAt.Add(time.Hour), nil)
	r3 := mkRec(t, "rec-3", 3, record.EventMemoryDropped, record.ReasonRemovedByMem0, "mem-A", explainAt.Add(2*time.Hour), nil)
	r := &fakeReader{byMem: map[string][]record.Record{"mem-A": {r1, r2, r3}}}
	e := explain.New(r)

	var buf bytes.Buffer
	res, err := e.Explain(context.Background(), explain.Request{MemoryID: "mem-A"}, &buf)
	require.NoError(t, err)

	out := buf.String()
	assert.Equal(t, 3, res.Records)

	// In Seq order: the earlier record's line precedes the later one's.
	i1, i2, i3 := strings.Index(out, "rec-1"), strings.Index(out, "rec-2"), strings.Index(out, "rec-3")
	require.Truef(t, i1 >= 0 && i2 >= 0 && i3 >= 0, "every record must appear in the timeline")
	assert.Less(t, i1, i2, "records must render in Seq order")
	assert.Less(t, i2, i3, "records must render in Seq order")

	// One line each: no line carries two records.
	recordLines := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "rec-1") || strings.Contains(line, "rec-2") || strings.Contains(line, "rec-3") {
			recordLines++
		}
	}
	assert.Equal(t, 3, recordLines, "each record renders on its own line")

	// Each line carries the event, the reason kind and the tier.
	assert.Contains(t, out, string(record.EventAddResolved))
	assert.Contains(t, out, string(record.ReasonStoredByMem0))
	assert.Contains(t, out, record.Observed.String(), "the tier must be stated")
	assert.Equal(t, 1, r.listCalls, "a memory view reads through ListRecordsByMemory")
	assert.Equal(t, 0, r.getCalls)
}

// TestExplainEmptyMemoryViewWritesNothingAndDoesNotError pins the empty-memory
// view in both shapes: a memory with no records produces NO output at all -- no
// header, and, in JSON, no empty document -- and Result is the zero value with a
// nil error. The package does not own the no-records failure (the CLI does), but
// it must not put a success-shaped document on stdout for the CLI's non-zero
// exit to contradict.
func TestExplainEmptyMemoryViewWritesNothingAndDoesNotError(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		name := "prose"
		if jsonMode {
			name = "json"
		}
		t.Run(name, func(t *testing.T) {
			e := explain.New(&fakeReader{byMem: map[string][]record.Record{}})

			var buf bytes.Buffer
			res, err := e.Explain(context.Background(), explain.Request{MemoryID: "mem-none", JSON: jsonMode}, &buf)
			require.NoError(t, err, "an empty view is not a package error; the CLI owns that")
			assert.Equal(t, explain.Result{}, res)
			assert.Empty(t, buf.String(), "an empty view must write nothing, in prose or JSON")
		})
	}
}

// --- subject guard ----------------------------------------------------------

// TestExplainRejectsAnEmptySubjectBeforeReading pins Review Focus 4's guard: an
// empty MemoryID and an empty RecordID is a missing subject, refused before any
// read -- the reader is never called.
func TestExplainRejectsAnEmptySubjectBeforeReading(t *testing.T) {
	r := &fakeReader{}
	e := explain.New(r)

	var buf bytes.Buffer
	res, err := e.Explain(context.Background(), explain.Request{}, &buf)
	require.Error(t, err, "an empty subject must be refused")
	assert.Contains(t, err.Error(), "subject")
	assert.Equal(t, 0, r.getCalls, "the reader must not be called for an empty subject")
	assert.Equal(t, 0, r.listCalls, "the reader must not be called for an empty subject")
	assert.Empty(t, buf.String())
	assert.Equal(t, explain.Result{}, res)
}

// TestExplainRecordIDTakesPrecedenceOverMemoryID pins the decided precedence:
// when both subjects are set, RecordID wins and MemoryID is ignored. A
// both-set request is the CLI's usage error (Task 3), so the package resolves
// it rather than refusing it.
func TestExplainRecordIDTakesPrecedenceOverMemoryID(t *testing.T) {
	rec := mkRec(t, "rec-1", 1, record.EventMemoryKept, record.ReasonStoredByMem0, "mem-A", explainAt, nil)
	other := mkRec(t, "rec-2", 2, record.EventMemoryKept, record.ReasonStoredByMem0, "mem-A", explainAt, nil)
	r := &fakeReader{
		byID:  map[record.RecordID]record.Record{"rec-1": rec},
		byMem: map[string][]record.Record{"mem-A": {rec, other}},
	}
	e := explain.New(r)

	var buf bytes.Buffer
	res, err := e.Explain(context.Background(), explain.Request{RecordID: "rec-1", MemoryID: "mem-A"}, &buf)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Records, "RecordID takes precedence when both are set")
	assert.Equal(t, 1, r.getCalls)
	assert.Equal(t, 0, r.listCalls, "MemoryID must be ignored when RecordID is set")
}

// --- redaction --------------------------------------------------------------

// TestExplainWithholdsSensitiveContentUnlessIncluded pins Review Focus 5: a
// sensitive record is withheld by default -- visibly, not silently -- and shown
// under IncludeSensitive. Result.Redacted counts only the withheld record.
func TestExplainWithholdsSensitiveContentUnlessIncluded(t *testing.T) {
	sens := mkRec(t, "rec-s", 1, record.EventMemoryKept, record.ReasonStoredByMem0, "mem-A", explainAt,
		&record.Content{Text: "the secret value", Sensitive: true})
	r := &fakeReader{byID: map[record.RecordID]record.Record{"rec-s": sens}}
	e := explain.New(r)

	var withheld bytes.Buffer
	res, err := e.Explain(context.Background(), explain.Request{RecordID: "rec-s"}, &withheld)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Redacted, "a withheld sensitive record must be counted")
	assert.NotContains(t, withheld.String(), "the secret value")
	assert.Contains(t, strings.ToLower(withheld.String()), "withheld",
		"the prose must say the content was withheld, not stay silent")

	var shown bytes.Buffer
	res, err = e.Explain(context.Background(), explain.Request{RecordID: "rec-s", IncludeSensitive: true}, &shown)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Redacted, "nothing hidden means nothing counted")
	assert.Contains(t, shown.String(), "the secret value")
}

// TestExplainShowsNonSensitiveContent pins the ordinary path: content that is
// not sensitive is printed and never counted as redacted.
func TestExplainShowsNonSensitiveContent(t *testing.T) {
	rec := mkRec(t, "rec-p", 1, record.EventMemoryKept, record.ReasonStoredByMem0, "mem-A", explainAt,
		&record.Content{Text: "a public note", Sensitive: false})
	r := &fakeReader{byID: map[record.RecordID]record.Record{"rec-p": rec}}
	e := explain.New(r)

	var buf bytes.Buffer
	res, err := e.Explain(context.Background(), explain.Request{RecordID: "rec-p"}, &buf)
	require.NoError(t, err)
	assert.Contains(t, buf.String(), "a public note")
	assert.Equal(t, 0, res.Redacted)
}

// TestExplainDoesNotCountOrClaimWithholdingForAHasNoContentRecord pins the
// distinction Review Focus 5 rests on: a record that carried no content was not
// withheld. It is not counted in Redacted, and the prose must not claim its
// content was withheld.
func TestExplainDoesNotCountOrClaimWithholdingForAHasNoContentRecord(t *testing.T) {
	rec := mkRec(t, "rec-nc", 1, record.EventMemoryDropped, record.ReasonRemovedByMem0, "mem-A", explainAt, nil)
	r := &fakeReader{byID: map[record.RecordID]record.Record{"rec-nc": rec}}
	e := explain.New(r)

	var buf bytes.Buffer
	res, err := e.Explain(context.Background(), explain.Request{RecordID: "rec-nc"}, &buf)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Redacted, "a record with no content was not withheld")
	assert.NotContains(t, strings.ToLower(buf.String()), "withheld",
		"a record with no content must not claim its content was withheld")
}

// --- JSON view --------------------------------------------------------------

// TestExplainJSONAndProseAgreeOnTheSameRecordsInOrder pins Review Focus 3's
// cross-view agreement: the JSON view and the prose view of the same fixture
// carry the same records in the same Seq order.
func TestExplainJSONAndProseAgreeOnTheSameRecordsInOrder(t *testing.T) {
	r1 := mkRec(t, "rec-1", 1, record.EventAddResolved, record.ReasonStoredByMem0, "mem-A", explainAt, nil)
	r2 := mkRec(t, "rec-2", 2, record.EventMemorySurfaced, record.ReasonReturnedBySearch, "mem-A", explainAt.Add(time.Hour), nil)
	r3 := mkRec(t, "rec-3", 3, record.EventMemoryDropped, record.ReasonRemovedByMem0, "mem-A", explainAt.Add(2*time.Hour), nil)
	feed := func() *fakeReader {
		return &fakeReader{byMem: map[string][]record.Record{"mem-A": {r1, r2, r3}}}
	}

	var prose bytes.Buffer
	resProse, err := explain.New(feed()).Explain(context.Background(), explain.Request{MemoryID: "mem-A"}, &prose)
	require.NoError(t, err)

	var js bytes.Buffer
	resJSON, err := explain.New(feed()).Explain(context.Background(), explain.Request{MemoryID: "mem-A", JSON: true}, &js)
	require.NoError(t, err)

	assert.Equal(t, resProse, resJSON, "both views must agree on the counts")

	var view struct {
		Records []struct {
			ID       string `json:"id"`
			Sentence string `json:"sentence"`
		} `json:"records"`
	}
	require.NoError(t, json.Unmarshal(js.Bytes(), &view))
	require.Len(t, view.Records, 3)
	assert.Equal(t, []string{"rec-1", "rec-2", "rec-3"},
		[]string{view.Records[0].ID, view.Records[1].ID, view.Records[2].ID},
		"the JSON records must be in Seq order")

	// Same order in prose, and the same sentences the JSON carries.
	p := prose.String()
	assert.Less(t, strings.Index(p, "rec-1"), strings.Index(p, "rec-2"))
	assert.Less(t, strings.Index(p, "rec-2"), strings.Index(p, "rec-3"))
	for _, got := range view.Records {
		assert.Contains(t, p, got.Sentence, "both views must carry the same sentence")
	}
}

// TestExplainJSONIsItsOwnShapeNotAnExportLine pins Decision 1: explain's JSON is
// its own small shape, not export.Line. It carries a sentence and none of the
// export wire fields a single-subject consumer has no use for.
func TestExplainJSONIsItsOwnShapeNotAnExportLine(t *testing.T) {
	rec := mkRec(t, "rec-1", 1, record.EventMemoryKept, record.ReasonStoredByMem0, "mem-A", explainAt, nil)
	e := explain.New(&fakeReader{byID: map[record.RecordID]record.Record{"rec-1": rec}})

	var buf bytes.Buffer
	_, err := e.Explain(context.Background(), explain.Request{RecordID: "rec-1", JSON: true}, &buf)
	require.NoError(t, err)

	var view struct {
		Records []map[string]any `json:"records"`
	}
	require.NoError(t, json.Unmarshal(buf.Bytes(), &view))
	require.Len(t, view.Records, 1)
	got := view.Records[0]

	_, hasSentence := got["sentence"]
	assert.True(t, hasSentence, "explain's JSON shape carries its own sentence field")
	for _, forbidden := range []string{"hash", "prev_hash", "signature", "signer_key_id", "phrasing"} {
		_, present := got[forbidden]
		assert.Falsef(t, present, "explain's JSON must not carry export.Line's %q field", forbidden)
	}
}

// --- errors and cancellation ------------------------------------------------

// errReadFailed is the sentinel a failing reader returns, so the test can prove
// Explain's %w wrap preserves it rather than merely returning some error.
var errReadFailed = errors.New("ledger read failed")

// TestExplainWrapsReadError pins that a read failure is wrapped, not swallowed,
// and that nothing is written when the read fails.
func TestExplainWrapsReadError(t *testing.T) {
	var buf bytes.Buffer

	_, err := explain.New(&fakeReader{getErr: errReadFailed}).
		Explain(context.Background(), explain.Request{RecordID: "rec-1"}, &buf)
	require.ErrorIs(t, err, errReadFailed, "the %w wrap must preserve the reader's sentinel")
	assert.Empty(t, buf.String(), "nothing is written when the read fails")

	_, err = explain.New(&fakeReader{listErr: errReadFailed}).
		Explain(context.Background(), explain.Request{MemoryID: "mem-A"}, &buf)
	require.ErrorIs(t, err, errReadFailed, "the %w wrap must preserve the reader's sentinel")
	assert.Empty(t, buf.String(), "nothing is written when the read fails")
}

// TestExplainRejectsCancelledContext pins the context check between records: a
// cancelled context fails the view rather than quietly producing output.
func TestExplainRejectsCancelledContext(t *testing.T) {
	rec := mkRec(t, "rec-1", 1, record.EventMemoryKept, record.ReasonStoredByMem0, "mem-A", explainAt, nil)
	r := &fakeReader{byMem: map[string][]record.Record{"mem-A": {rec}}}
	e := explain.New(r)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	_, err := e.Explain(ctx, explain.Request{MemoryID: "mem-A"}, &buf)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, buf.String())
}
