package export_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/export"
	"notary/internal/record"
)

// fakeReader is an in-memory Reader. Like the store, it returns only the
// records whose At lies within [from, to], so a test can prove the exporter
// asks for the right window and renders what it is given without a database.
// It records the bounds it was called with.
type fakeReader struct {
	records  []record.Record
	err      error
	lastFrom time.Time
	lastTo   time.Time
}

func (f *fakeReader) ListRecords(from, to time.Time) ([]record.Record, error) {
	f.lastFrom, f.lastTo = from, to
	if f.err != nil {
		return nil, f.err
	}
	var out []record.Record
	for _, r := range f.records {
		if r.At.Before(from) || r.At.After(to) {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

var (
	rangeFrom = time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC)
	rangeTo   = time.Date(2024, 6, 30, 0, 0, 0, 0, time.UTC)
)

// recordAt returns sampleRecord with its id and At/RecordedAt set.
func recordAt(t *testing.T, id string, at time.Time) record.Record {
	t.Helper()
	rec := sampleRecord(t)
	rec.ID = record.RecordID(id)
	rec.At = at
	rec.RecordedAt = at
	return rec
}

// exportLines splits JSONL output into its non-empty lines.
func exportLines(t *testing.T, out string) []string {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

func TestExportStreamsInRange(t *testing.T) {
	inA := recordAt(t, "in-a", rangeFrom.Add(time.Hour))
	inB := recordAt(t, "in-b", rangeFrom.Add(2*time.Hour))
	before := recordAt(t, "before", rangeFrom.Add(-time.Hour))
	after := recordAt(t, "after", rangeTo.Add(time.Hour))

	// The reader is handed the records out of order; the exporter must render
	// exactly those in range, in the order it reads them.
	fr := &fakeReader{records: []record.Record{before, inA, after, inB}}
	e := export.New(fr)

	var buf bytes.Buffer
	res, err := e.Export(context.Background(), export.Request{From: rangeFrom, To: rangeTo}, &buf)
	require.NoError(t, err)

	assert.Equal(t, rangeFrom, fr.lastFrom)
	assert.Equal(t, rangeTo, fr.lastTo)
	assert.Equal(t, 2, res.Records)

	lines := exportLines(t, buf.String())
	require.Len(t, lines, 2)
	assert.Contains(t, lines[0], `"id":"in-a"`)
	assert.Contains(t, lines[1], `"id":"in-b"`)
}

func TestEmptyRangeIsSuccess(t *testing.T) {
	e := export.New(&fakeReader{})

	var buf bytes.Buffer
	res, err := e.Export(context.Background(), export.Request{From: rangeFrom, To: rangeTo}, &buf)
	require.NoError(t, err)
	assert.Equal(t, 0, res.Records)
	assert.Empty(t, buf.String())
}

func TestScopeFlagsNarrowTheRange(t *testing.T) {
	u1 := recordAt(t, "u1-rec", rangeFrom.Add(time.Hour))
	u1.Subject.Scope.UserID = "u1"
	u2 := recordAt(t, "u2-rec", rangeFrom.Add(time.Hour))
	u2.Subject.Scope.UserID = "u2"

	e := export.New(&fakeReader{records: []record.Record{u1, u2}})

	var buf bytes.Buffer
	res, err := e.Export(context.Background(),
		export.Request{From: rangeFrom, To: rangeTo, Scope: record.Scope{UserID: "u1"}}, &buf)
	require.NoError(t, err)
	assert.Equal(t, 1, res.Records)
	assert.Contains(t, buf.String(), `"id":"u1-rec"`)
	assert.NotContains(t, buf.String(), `"id":"u2-rec"`)
}

func TestMaxSpanIsEnforced(t *testing.T) {
	e := export.New(&fakeReader{})

	var buf bytes.Buffer
	_, err := e.Export(context.Background(), export.Request{
		From:    rangeFrom,
		To:      rangeFrom.Add(48 * time.Hour),
		MaxSpan: 24 * time.Hour,
	}, &buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--max-span")
}

// TestRecordsWithoutContentRenderWithoutIt is Review Focus 1: a record with no
// content produces a line with no content field and no redacted field -- not an
// empty string, and not a redaction claim.
func TestRecordsWithoutContentRenderWithoutIt(t *testing.T) {
	rec := sampleRecord(t)
	rec.Content = nil

	got := renderJSON(t, rec, false)

	_, hasContent := got["content"]
	assert.False(t, hasContent, "a record with no content must have no content field")
	_, hasRedacted := got["redacted"]
	assert.False(t, hasRedacted, "no content is not the same as redacted content")

	data, err := json.Marshal(got)
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"content":""`, "an absent content must not render as an empty string")
}

// TestUnsensitiveLegacyRecordsRender is Review Focus 3: a record whose Sensitive
// is false, as every pre-Phase-6 row is, renders its text normally.
func TestUnsensitiveLegacyRecordsRender(t *testing.T) {
	rec := sampleRecord(t)
	rec.Content = &record.Content{Text: "legacy text", Sensitive: false}

	got := renderJSON(t, rec, false)

	assert.Equal(t, "legacy text", got["content"])
	_, hasRedacted := got["redacted"]
	assert.False(t, hasRedacted, "a false sensitivity flag is data, not an absence")
}
