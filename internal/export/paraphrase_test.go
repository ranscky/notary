package export_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/export"
	"notary/internal/phrase"
	"notary/internal/record"
)

// fakeParaphraser is an in-memory export.Paraphraser. It counts calls and
// records the records each call was given, so a test can prove the pass is off
// by default (zero calls), that it is skipped for an empty range, and that it
// is handed exactly the records the export renders. It never touches a network
// or needs a key: that is the whole point of the interface.
type fakeParaphraser struct {
	calls  int
	got    [][]record.Record
	result phrase.Paraphrase
	err    error
}

func (f *fakeParaphraser) Paraphrase(_ context.Context, records []record.Record) (phrase.Paraphrase, error) {
	f.calls++
	f.got = append(f.got, records)
	if f.err != nil {
		return phrase.Paraphrase{}, f.err
	}
	return f.result, nil
}

// TestPhraseIsOffByDefault pins the opt-in property: even with a paraphraser
// wired in, an export that does not set Phrase never calls it. "Off by
// default" must mean "no provider is ever billed", not merely "the output
// looks the same".
func TestPhraseIsOffByDefault(t *testing.T) {
	rec := recordAt(t, "r-1", rangeFrom.Add(time.Hour))
	fp := &fakeParaphraser{result: phrase.Paraphrase{Text: "must not be used", Model: "m"}}
	e := export.New(&fakeReader{records: []record.Record{rec}}, export.WithParaphraser(fp))

	var buf bytes.Buffer
	res, err := e.Export(context.Background(), export.Request{From: rangeFrom, To: rangeTo}, &buf)
	require.NoError(t, err)

	assert.Equal(t, 0, fp.calls, "a paraphraser must never be called unless Phrase is set")
	assert.False(t, res.ParaphraseFailed)
	assert.Empty(t, res.ParaphraseError)
	assert.NotContains(t, buf.String(), `"paraphrase"`, "no paraphrase object appears without --phrase")
	assert.Len(t, exportLines(t, buf.String()), 1)
}

// TestParaphraseAppearsBesideTheRecordNotInsteadOfIt is the display-only
// property: under Phrase the line carries a `paraphrase` object AND every
// structured field it restates. The prose is a guest; the evidence stays.
func TestParaphraseAppearsBesideTheRecordNotInsteadOfIt(t *testing.T) {
	rec := recordAt(t, "r-1", rangeFrom.Add(time.Hour))
	rec.Subject.MemoryID = "mem-1"
	at := time.Date(2024, 6, 1, 1, 2, 3, 0, time.UTC)
	fp := &fakeParaphraser{result: phrase.Paraphrase{Text: "the model's sentence", Model: "test-model", At: at}}
	e := export.New(&fakeReader{records: []record.Record{rec}}, export.WithParaphraser(fp))

	var buf bytes.Buffer
	res, err := e.Export(context.Background(), export.Request{From: rangeFrom, To: rangeTo, Phrase: true}, &buf)
	require.NoError(t, err)
	assert.Equal(t, 1, fp.calls)
	assert.False(t, res.ParaphraseFailed)
	assert.Empty(t, res.ParaphraseError)

	lines := exportLines(t, buf.String())
	require.Len(t, lines, 1)

	var line export.Line
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &line))

	// The paraphrase is present, as a labelled paraphrase object...
	require.NotNil(t, line.Paraphrase, "the paraphrase must be present beside the record")
	assert.Equal(t, "the model's sentence", line.Paraphrase.Text)
	assert.Equal(t, "test-model", line.Paraphrase.Model)
	assert.True(t, line.Paraphrase.At.Equal(at))

	// ...beside the record's own structured fields, which are untouched.
	assert.Equal(t, record.RecordID("r-1"), line.ID)
	assert.Equal(t, record.EventMemoryKept, line.Event)
	assert.Equal(t, record.ReasonStoredByMem0, line.ReasonKind)
	assert.Equal(t, "mem-1", line.MemoryID)
	assert.NotEmpty(t, line.Phrasing, "the deterministic phrasing still sits beside the paraphrase")
	assert.NotEmpty(t, line.ContentHash)
	assert.NotEmpty(t, line.Hash)
}

// TestParaphraseObjectKeysAreSnakeCase pins the WIRE keys of the `paraphrase`
// object. Every sibling key in the line is snake_case (content_hash,
// reason_kind, signer_key_id), so a paraphrase object marshalled with Go field
// names would be the one inconsistent key set in an otherwise uniform line.
//
// It decodes into a map[string]any, NOT into export.Line: Go's field matching
// is case-insensitive, so unmarshalling into the struct would accept {"Text"}
// and {"text"} alike and pin nothing about the wire form. The map sees the keys
// as they actually are.
func TestParaphraseObjectKeysAreSnakeCase(t *testing.T) {
	rec := recordAt(t, "r-1", rangeFrom.Add(time.Hour))
	at := time.Date(2024, 6, 1, 1, 2, 3, 0, time.UTC)
	fp := &fakeParaphraser{result: phrase.Paraphrase{Text: "the model's sentence", Model: "test-model", At: at}}
	e := export.New(&fakeReader{records: []record.Record{rec}}, export.WithParaphraser(fp))

	var buf bytes.Buffer
	_, err := e.Export(context.Background(), export.Request{From: rangeFrom, To: rangeTo, Phrase: true}, &buf)
	require.NoError(t, err)

	var line map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &line))
	obj, ok := line["paraphrase"].(map[string]any)
	require.True(t, ok, "the line must carry a `paraphrase` object")

	got := make([]string, 0, len(obj))
	for k := range obj {
		got = append(got, k)
	}
	assert.ElementsMatch(t, []string{"text", "model", "at"}, got,
		"the paraphrase object's keys must be snake_case, matching every sibling key in the line")
	assert.Equal(t, "the model's sentence", obj["text"])
	assert.Equal(t, "test-model", obj["model"])
	// `at` is a time.Time, so it encodes the same way the line's own `at` and
	// `recorded_at` do: an RFC3339 string. Consistency matters more than any
	// other choice here -- a consumer parses one date format across the line.
	assert.Equal(t, at.Format(time.RFC3339Nano), obj["at"])
}

// TestParaphraseFailureDegradesAndDoesNotFailTheExport is the rule that
// matters most. For each of the four provider failure modes the design names
// -- a network error, a non-2xx status, an empty choices array, and a body
// that is not JSON -- the export must: leave the record byte-for-byte as it
// would have been without the pass, add a note saying the paraphrase failed
// and why, and return NO error.
func TestParaphraseFailureDegradesAndDoesNotFailTheExport(t *testing.T) {
	modes := []struct {
		name string
		err  error
	}{
		{"network error", errors.New("phrase: POST /chat/completions: dial tcp 127.0.0.1:1: connect: connection refused")},
		{"non-2xx", &phrase.HTTPError{StatusCode: 502, Body: []byte(`{"error":"upstream unavailable"}`)}},
		{"empty choices", errors.New("phrase: response carried no choices")},
		{"non-JSON body", errors.New("phrase: decoding POST /chat/completions response: invalid character '<' looking for beginning of value")},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			rec := recordAt(t, "r-1", rangeFrom.Add(time.Hour))
			fp := &fakeParaphraser{err: m.err}
			e := export.New(&fakeReader{records: []record.Record{rec}}, export.WithParaphraser(fp))

			var buf bytes.Buffer
			res, err := e.Export(context.Background(), export.Request{From: rangeFrom, To: rangeTo, Phrase: true}, &buf)
			require.NoError(t, err, "a phrasing failure must never fail the export")

			// A note is present, and it says why.
			assert.True(t, res.ParaphraseFailed, "the failure must be reported, never silently omitted")
			assert.Contains(t, res.ParaphraseError, m.err.Error(), "the note must carry the reason")

			lines := exportLines(t, buf.String())
			require.Len(t, lines, 1, "the record is still exported")

			// The record renders exactly as it would with no paraphrase pass:
			// the structured evidence is untouched.
			want, err := export.Render(rec, false)
			require.NoError(t, err)
			wantJSON, err := json.Marshal(want)
			require.NoError(t, err)
			assert.JSONEq(t, string(wantJSON), lines[0], "a failed paraphrase leaves the record untouched")

			var line export.Line
			require.NoError(t, json.Unmarshal([]byte(lines[0]), &line))
			assert.Nil(t, line.Paraphrase, "a failed paraphrase yields no paraphrase object, not an empty one")
		})
	}
}

// TestParaphraseIsSkippedForAnEmptyRange pins the empty-range guard. The client
// errors on zero records rather than making a pointless billed call, so a
// per-export pass must not be attempted when nothing is rendered -- otherwise
// every empty range would report a spurious paraphrase failure.
func TestParaphraseIsSkippedForAnEmptyRange(t *testing.T) {
	fp := &fakeParaphraser{result: phrase.Paraphrase{Text: "unused"}}
	e := export.New(&fakeReader{}, export.WithParaphraser(fp))

	var buf bytes.Buffer
	res, err := e.Export(context.Background(), export.Request{From: rangeFrom, To: rangeTo, Phrase: true}, &buf)
	require.NoError(t, err)
	assert.Equal(t, 0, fp.calls, "an empty range must not call the provider")
	assert.False(t, res.ParaphraseFailed, "an empty range must not report a spurious paraphrase failure")
	assert.Empty(t, res.ParaphraseError)
	assert.Empty(t, buf.String())
}

// TestParaphraseReceivesOnlyTheRenderedRecords pins that the pass is handed the
// records the export actually renders -- after the scope filter -- so it cannot
// bill for records that never appear and cannot describe records the reader
// cannot see.
func TestParaphraseReceivesOnlyTheRenderedRecords(t *testing.T) {
	u1 := recordAt(t, "u1-rec", rangeFrom.Add(time.Hour))
	u1.Subject.Scope.UserID = "u1"
	u2 := recordAt(t, "u2-rec", rangeFrom.Add(2*time.Hour))
	u2.Subject.Scope.UserID = "u2"

	fp := &fakeParaphraser{result: phrase.Paraphrase{Text: "sentence"}}
	e := export.New(&fakeReader{records: []record.Record{u1, u2}}, export.WithParaphraser(fp))

	var buf bytes.Buffer
	_, err := e.Export(context.Background(),
		export.Request{From: rangeFrom, To: rangeTo, Scope: record.Scope{UserID: "u1"}, Phrase: true}, &buf)
	require.NoError(t, err)

	require.Equal(t, 1, fp.calls)
	require.Len(t, fp.got, 1)
	require.Len(t, fp.got[0], 1, "only the records the export renders are paraphrased")
	assert.Equal(t, record.RecordID("u1-rec"), fp.got[0][0].ID)
}

// TestPhraseWithoutAParaphraserIsAnError pins that asking for a paraphrase with
// nothing able to provide one fails loudly rather than silently omitting it: a
// request the exporter cannot honour is an error, not a quiet no-op.
func TestPhraseWithoutAParaphraserIsAnError(t *testing.T) {
	rec := recordAt(t, "r-1", rangeFrom.Add(time.Hour))
	e := export.New(&fakeReader{records: []record.Record{rec}})

	var buf bytes.Buffer
	_, err := e.Export(context.Background(), export.Request{From: rangeFrom, To: rangeTo, Phrase: true}, &buf)
	require.Error(t, err, "requesting a paraphrase with no paraphraser must fail loudly")
	assert.Contains(t, err.Error(), "paraphras")
}
