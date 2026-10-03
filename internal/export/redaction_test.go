package export_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/export"
	"notary/internal/ledger"
	"notary/internal/record"
	"notary/internal/sign"
	"notary/internal/store"
)

// redactionTestSeed is a fixed 32-byte ed25519 seed, so the ledger these tests
// build signs deterministically. It is a different value from the ledger
// package's own seed only to keep the two suites independent.
var redactionTestSeed = []byte{
	0x20, 0x1f, 0x1e, 0x1d, 0x1c, 0x1b, 0x1a, 0x19,
	0x18, 0x17, 0x16, 0x15, 0x14, 0x13, 0x12, 0x11,
	0x10, 0x0f, 0x0e, 0x0d, 0x0c, 0x0b, 0x0a, 0x09,
	0x08, 0x07, 0x06, 0x05, 0x04, 0x03, 0x02, 0x01,
}

// redactionFixedNow is the deterministic clock the round-trip ledger uses.
var redactionFixedNow = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

// newSignedLedger builds a real ledger -- a temp SQLite store, a signer over a
// fixed seed, and a fixed clock -- so the round-trip test can assert against
// the actual stored record rather than a fixture that merely echoes whatever
// the renderer happens to produce.
func newSignedLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	t.Setenv("NOTARY_EXPORT_TEST_KEY", base64.StdEncoding.EncodeToString(redactionTestSeed))
	sg, err := sign.NewSigner(sign.KeySource{Kind: sign.KeySourceEnv, Ref: "NOTARY_EXPORT_TEST_KEY"})
	require.NoError(t, err)
	st, err := store.Open(filepath.Join(t.TempDir(), "ledger.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	return ledger.New(st, sg, func() time.Time { return redactionFixedNow })
}

// buildRecord returns a valid, appendable record. Its Reason is a genuine
// observed reason so Render can phrase it, and its content (when non-nil) is
// what redaction acts on.
func buildRecord(t *testing.T, id record.RecordID, at time.Time, content *record.Content) record.Record {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(`{"ok":true}`))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(record.ReasonReturnedBySearch, ev)
	require.NoError(t, err)

	var contentHash record.Hash
	for i := range contentHash {
		contentHash[i] = 0x40 + byte(i%8) + byte(len(id))
	}

	rec := record.Record{
		ID:      id,
		At:      at,
		Event:   record.EventMemorySurfaced,
		Reason:  reason,
		Subject: record.Subject{MemoryID: "mem-" + string(id), Scope: record.Scope{UserID: "u1"}, ContentHash: contentHash},
		Content: content,
	}
	require.NoError(t, rec.Validate(), "the fixture must be a valid record")
	return rec
}

// linesByID decodes JSONL output into one object per record, keyed by its id.
func linesByID(t *testing.T, out string) map[string]map[string]any {
	t.Helper()
	byID := map[string]map[string]any{}
	for _, l := range exportLines(t, out) {
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(l), &m))
		id, ok := m["id"].(string)
		require.True(t, ok, "every line must carry a string id")
		byID[id] = m
	}
	return byID
}

// TestRedactionRoundTripIsHashInvariant is spec §10's required test, the one
// that proves "presentation may never change the evidence".
//
// It exports the SAME ledger range twice -- once under the default (sensitive
// content withheld) and once with IncludeSensitive -- and asserts three
// independent things:
//
//  1. Every hash, prev_hash, signature AND content_hash is identical across
//     both outputs AND equal to the STORED record's, not merely to each other.
//     Two outputs agreeing proves only that rendering is deterministic;
//     agreeing with the stored record proves the export never touched the
//     evidence. content_hash is the record's commitment to the memory's text,
//     so redaction -- which removes the text -- must leave it untouched too.
//  2. The sensitive text appears in exactly one output, and the non-sensitive
//     text in both -- a redaction that redacted nothing fails (2), and a
//     redaction that redacted everything fails it too.
//  3. The withheld line says so: it carries redacted="sensitive" and no
//     content, and the same record under IncludeSensitive carries the text and
//     no redacted claim.
func TestRedactionRoundTripIsHashInvariant(t *testing.T) {
	l := newSignedLedger(t)

	const sensitiveText = "the secret value is hunter2"
	const publicText = "a public note"

	sens := buildRecord(t, "sens-rec", time.Date(2024, 6, 10, 0, 0, 0, 0, time.UTC),
		&record.Content{Text: sensitiveText, Sensitive: true})
	public := buildRecord(t, "public-rec", time.Date(2024, 6, 11, 0, 0, 0, 0, time.UTC),
		&record.Content{Text: publicText, Sensitive: false})
	// A record that carries no content at all (the shape an audit_gap has). It
	// can never be redacted, so it pins Result.Redacted against a record the
	// redaction path must not count -- the count is asserted, not merely
	// derived from line.Redacted by construction.
	gap := buildRecord(t, "gap-rec", time.Date(2024, 6, 12, 0, 0, 0, 0, time.UTC), nil)

	_, err := l.Append(sens)
	require.NoError(t, err)
	_, err = l.Append(public)
	require.NoError(t, err)
	_, err = l.Append(gap)
	require.NoError(t, err)

	// Read the records back from the ledger: this is what the export's hashes
	// must equal, so the test depends on storage, not on the renderer.
	storedSens, err := l.GetRecord("sens-rec")
	require.NoError(t, err)
	storedPublic, err := l.GetRecord("public-rec")
	require.NoError(t, err)
	storedGap, err := l.GetRecord("gap-rec")
	require.NoError(t, err)

	e := export.New(l)

	var withheld, shown bytes.Buffer
	withheldRes, err := e.Export(context.Background(), export.Request{From: rangeFrom, To: rangeTo}, &withheld)
	require.NoError(t, err)
	shownRes, err := e.Export(context.Background(), export.Request{From: rangeFrom, To: rangeTo, IncludeSensitive: true}, &shown)
	require.NoError(t, err)

	withheldLines := linesByID(t, withheld.String())
	shownLines := linesByID(t, shown.String())
	require.Len(t, withheldLines, 3)
	require.Len(t, shownLines, 3)

	// 1. Hash invariance, asserted against the STORED record.
	for _, tc := range []struct {
		id     string
		stored record.Record
	}{
		{"sens-rec", storedSens},
		{"public-rec", storedPublic},
		{"gap-rec", storedGap},
	} {
		wantHash := hex.EncodeToString(tc.stored.Hash[:])
		wantPrev := hex.EncodeToString(tc.stored.PrevHash[:])
		wantSig := hex.EncodeToString(tc.stored.Signature)
		// content_hash is the record's commitment to the memory text. Redaction
		// removes the text and must leave this untouched, in BOTH outputs.
		wantContentHash := hex.EncodeToString(tc.stored.Subject.ContentHash[:])

		assert.Equal(t, wantHash, withheldLines[tc.id]["hash"], "%s: hash under default", tc.id)
		assert.Equal(t, wantHash, shownLines[tc.id]["hash"], "%s: hash under IncludeSensitive", tc.id)
		assert.Equal(t, wantPrev, withheldLines[tc.id]["prev_hash"], "%s: prev_hash under default", tc.id)
		assert.Equal(t, wantPrev, shownLines[tc.id]["prev_hash"], "%s: prev_hash under IncludeSensitive", tc.id)
		assert.Equal(t, wantSig, withheldLines[tc.id]["signature"], "%s: signature under default", tc.id)
		assert.Equal(t, wantSig, shownLines[tc.id]["signature"], "%s: signature under IncludeSensitive", tc.id)
		assert.Equal(t, wantContentHash, withheldLines[tc.id]["content_hash"], "%s: content_hash under default", tc.id)
		assert.Equal(t, wantContentHash, shownLines[tc.id]["content_hash"], "%s: content_hash under IncludeSensitive", tc.id)
	}

	// 2. The sensitive text appears in exactly one output. This is the
	// assertion that stops the test being a lie: without it, a renderer that
	// redacted NOTHING satisfies (1) and (3) and passes.
	assert.Equal(t, 0, strings.Count(withheld.String(), sensitiveText),
		"the sensitive text must not appear in the default output")
	assert.Equal(t, 1, strings.Count(shown.String(), sensitiveText),
		"the sensitive text must appear exactly once, under IncludeSensitive")
	// And the non-sensitive text appears in both, so "redact everything" fails.
	assert.Equal(t, 1, strings.Count(withheld.String(), publicText),
		"non-sensitive text must survive the default export")
	assert.Equal(t, 1, strings.Count(shown.String(), publicText),
		"non-sensitive text must survive the IncludeSensitive export")

	// 3. The withheld line states that it was redacted, and carries no text.
	assert.Equal(t, "sensitive", withheldLines["sens-rec"]["redacted"],
		"the withheld line must name why it was redacted")
	_, withheldHasContent := withheldLines["sens-rec"]["content"]
	assert.False(t, withheldHasContent, "the withheld line must carry no content")
	_, shownHasRedacted := shownLines["sens-rec"]["redacted"]
	assert.False(t, shownHasRedacted, "the shown line must carry no redacted claim")
	assert.Equal(t, sensitiveText, shownLines["sens-rec"]["content"],
		"the shown line must carry the text")
	// The non-sensitive record is never redacted, either way.
	_, publicRedacted := withheldLines["public-rec"]["redacted"]
	assert.False(t, publicRedacted, "non-sensitive content is never redacted")
	// The no-content record is neither shown nor claimed redacted: there was
	// nothing to hide.
	_, gapHasContent := withheldLines["gap-rec"]["content"]
	assert.False(t, gapHasContent, "a record with no content renders no content field")
	_, gapRedacted := withheldLines["gap-rec"]["redacted"]
	assert.False(t, gapRedacted, "a record with no content is never claimed redacted")

	// Result.Redacted counts the records whose text was withheld. The gap
	// record proves the count is not simply "records minus shown": it is 1 of
	// 3, and the one is the sensitive record.
	assert.Equal(t, 1, withheldRes.Redacted, "exactly one record was redacted, not the no-content one")
	assert.Equal(t, 0, shownRes.Redacted, "nothing is redacted under IncludeSensitive")
	assert.Equal(t, 3, withheldRes.Records)
	assert.Equal(t, 3, shownRes.Records)
}

// TestRedactedLineNeverClaimsToHideAbsentContent is Review Focus 1 from the
// redaction side: a record that carries no content must render no content field
// AND no redacted claim, under either flag. A consumer must never read
// "nothing was recorded" as "you are not cleared for it".
//
// The mutation this closes: setting redacted="sensitive" whenever the flag is
// false, regardless of whether there was content to hide. That would make every
// derived or audit_gap record falsely claim a redaction, and this test fails
// loudly on it.
func TestRedactedLineNeverClaimsToHideAbsentContent(t *testing.T) {
	rec := sampleRecord(t)
	rec.Content = nil

	for _, include := range []bool{false, true} {
		got := renderJSON(t, rec, include)

		_, hasContent := got["content"]
		assert.False(t, hasContent, "includeSensitive=%v: absent content renders no content field", include)
		_, hasRedacted := got["redacted"]
		assert.False(t, hasRedacted,
			"includeSensitive=%v: absent content must not render a redacted claim", include)
	}
}

// TestEmptySensitiveContentIsStillRedacted pins the boundary the previous test
// guards from the other side: content that EXISTS and is empty, but is marked
// sensitive, is redacted like any other sensitive content -- it is not confused
// with absent content.
func TestEmptySensitiveContentIsStillRedacted(t *testing.T) {
	rec := sampleRecord(t)
	rec.Content = &record.Content{Text: "", Sensitive: true}

	got := renderJSON(t, rec, false)

	_, hasContent := got["content"]
	assert.False(t, hasContent, "sensitive empty content is withheld, so no content field")
	assert.Equal(t, "sensitive", got["redacted"],
		"content that exists and was withheld must say so, even when empty")
}
