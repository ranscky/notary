package report_test

// This file exercises the report renderer through its exported API and the
// files it writes. Nothing here inspects an unexported name: the properties
// that matter -- the names a page is given, the tree a render creates, the text
// a page carries -- are properties of the files, so they are asserted on the
// files.
//
// The fixture vocabulary is deliberately tiny and shared: fakeReader stands in
// for *ledger.Ledger, newTestRecord builds a valid record whose phrasing
// export.Phrase specifies, and the tree helpers read back what a render wrote.

import (
	"context"
	"encoding/hex"
	"fmt"
	"html"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"notary/internal/export"
	"notary/internal/record"
	"notary/internal/report"
)

// sliceFrom and sliceTo bound the range most tests render: wide enough to hold
// every fixture record.
var (
	sliceFrom = time.Date(2024, 5, 1, 0, 0, 0, 0, time.UTC)
	sliceTo   = time.Date(2024, 5, 31, 0, 0, 0, 0, time.UTC)
)

// pageNameRe is the only shape a generated page's name may have: the fixed
// index, or a sequence number and eight hex digits inside one of the two page
// directories. An id can reach a name in no other way.
var pageNameRe = regexp.MustCompile(`^(index\.html|memory/[0-9]+-[0-9a-f]{8}\.html|record/[0-9]+-[0-9a-f]{8}\.html)$`)

// hrefRe finds the links on a page, so a test can follow them.
var hrefRe = regexp.MustCompile(`href="([^"]*)"`)

// fakeReader is an in-memory report.Reader. It answers like the store does --
// the records whose At lies in [from, to], and a memory's own records -- and it
// remembers what it was asked for, so a test can assert the renderer asked for
// the slice it was told to render.
type fakeReader struct {
	records []record.Record
	err     error

	askedFrom, askedTo time.Time
	askedMemory        string
}

func (f *fakeReader) ListRecords(from, to time.Time) ([]record.Record, error) {
	f.askedFrom, f.askedTo = from, to
	if f.err != nil {
		return nil, f.err
	}
	var out []record.Record
	for _, rec := range f.records {
		if rec.At.Before(from) || rec.At.After(to) {
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

func (f *fakeReader) ListRecordsByMemory(memoryID string) ([]record.Record, error) {
	f.askedMemory = memoryID
	if f.err != nil {
		return nil, f.err
	}
	var out []record.Record
	for _, rec := range f.records {
		if rec.Subject.MemoryID == memoryID {
			out = append(out, rec)
		}
	}
	return out, nil
}

// recordWith builds a valid fixture record from an event and a reason, filling
// the chain fields from the id so two fixture records never collide.
func recordWith(t *testing.T, id string, seq uint64, memoryID string, at time.Time, event record.EventType, reason record.Reason) record.Record {
	t.Helper()
	rec := record.Record{
		ID:             record.RecordID(id),
		Seq:            seq,
		At:             at,
		RecordedAt:     at.Add(3 * time.Second),
		Event:          event,
		Reason:         reason,
		Subject:        record.Subject{MemoryID: memoryID, Scope: record.Scope{UserID: "u1"}, ContentHash: record.ContentHash(id)},
		IdempotencyKey: record.IdemKey("idem-" + id),
		PrevHash:       record.ContentHash("prev", id),
		Hash:           record.ContentHash("hash", id),
		Signature:      []byte{0xde, 0xad, 0xbe, 0xef},
		SignerKeyID:    "key-1",
	}
	require.NoError(t, rec.Validate(), "the fixture must be a valid record")
	return rec
}

// observedReason builds an Observed reason from a raw payload, which
// record.NewObservedEvidence canonicalises.
func observedReason(t *testing.T, kind record.ReasonKind, raw string) record.Reason {
	t.Helper()
	ev, err := record.NewObservedEvidence(record.SourceMem0Response, []byte(raw))
	require.NoError(t, err)
	reason, err := record.NewObservedReason(kind, ev)
	require.NoError(t, err)
	return reason
}

// newTestRecord builds a valid record: a kept memory with reason
// stored_by_mem0, a pair export.Phrase has a specified sentence for, so a test
// can compare a page against the phrasing an export would print. The chain
// fields are derived from the id, so two fixture records never collide.
func newTestRecord(t *testing.T, id string, seq uint64, memoryID string, at time.Time) record.Record {
	t.Helper()
	return recordWith(t, id, seq, memoryID, at, record.EventMemoryKept,
		observedReason(t, record.ReasonStoredByMem0, `{"outcome":"ok"}`))
}

// observedRecord builds a record whose reason carries Observed evidence with
// the given raw payload.
func observedRecord(t *testing.T, id string, seq uint64, memoryID string, at time.Time, raw string) record.Record {
	t.Helper()
	return recordWith(t, id, seq, memoryID, at, record.EventMemoryKept,
		observedReason(t, record.ReasonStoredByMem0, raw))
}

// reconstructedRecord builds a record whose reason carries Reconstructed
// evidence: a non-empty basis, a rule and its version, and a confidence.
func reconstructedRecord(t *testing.T, id string, seq uint64, memoryID string, at time.Time) record.Record {
	t.Helper()
	ev, err := record.NewReconstructedEvidence(
		[]record.RecordID{"rec-basis-1", "rec-basis-2"}, "kept-by-content-match", "v2", 0.75)
	require.NoError(t, err)
	reason, err := record.NewReconstructedReason(record.ReasonKeptByContentMatch, ev)
	require.NoError(t, err)
	return recordWith(t, id, seq, memoryID, at, record.EventMemoryKept, reason)
}

// internalRecord builds a record whose reason carries an InternalNote.
func internalRecord(t *testing.T, id string, seq uint64, memoryID string, at time.Time) record.Record {
	t.Helper()
	note, err := record.NewInternalNote("the sweep removed a memory no search covered")
	require.NoError(t, err)
	reason, err := record.NewInternalReason(record.ReasonRemovedByMem0, note)
	require.NoError(t, err)
	return recordWith(t, id, seq, memoryID, at, record.EventMemoryDropped, reason)
}

// withContent returns rec carrying text, marked sensitive when sensitive.
func withContent(rec record.Record, text string, sensitive bool) record.Record {
	rec.Content = &record.Content{Text: text, Sensitive: sensitive}
	return rec
}

// page is the name a render must give a page: the sequence number and the first
// eight hex digits of record.ContentHash(id) -- the project's existing digest,
// reused rather than reinvented -- so a test derives the expected name from the
// pinned scheme instead of from the renderer.
func page(subdir string, seq uint64, id string) string {
	digest := record.ContentHash(id)
	return fmt.Sprintf("%s/%d-%s.html", subdir, seq, hex.EncodeToString(digest[:])[:8])
}

// tempRoot makes a fresh tree and returns it with an output directory inside
// it, at <root>/a/out: deep enough that a name escaping one or two levels would
// still land inside the tree the "nothing outside dir" assertions walk.
func tempRoot(t *testing.T) (root, out string) {
	t.Helper()
	root = t.TempDir()
	return root, filepath.Join(root, "a", "out")
}

// render renders req through a Reporter over reader into a fresh output
// directory, and returns that directory with every file under it, keyed by its
// slash-separated path relative to the directory.
func render(t *testing.T, reader report.Reader, req report.Request) (string, map[string]string, report.Result) {
	t.Helper()
	_, out := tempRoot(t)
	rp := report.New(reader)
	res, err := rp.Render(context.Background(), req, out)
	require.NoError(t, err)
	return out, readTree(t, out), res
}

// readTree returns every regular file under dir, keyed by its slash-separated
// path relative to dir.
func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, rel := range allFilesUnder(t, dir) {
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		require.NoError(t, err)
		files[rel] = string(body)
	}
	return files
}

// treeNames is readTree's keys, sorted, so a test can assert the created tree
// rather than only the absence of an error.
func treeNames(t *testing.T, dir string) []string {
	t.Helper()
	names := make([]string, 0)
	for name := range readTree(t, dir) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// allFilesUnder returns every regular file under root, as slash-separated paths
// relative to root, sorted. It is the whole tree, not just a page directory, so
// "nothing was written outside dir" is asserted over everything a run could
// have touched.
func allFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	require.NoError(t, err, "walking %s", root)
	sort.Strings(files)
	return files
}

// assertLinksResolve asserts that every link on a page resolves, relative to
// that page, to a file that exists under dir.
func assertLinksResolve(t *testing.T, dir, page string) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(page)))
	require.NoError(t, err)
	hrefs := hrefRe.FindAllStringSubmatch(string(body), -1)
	require.NotEmpty(t, hrefs, "%s carries no links at all", page)
	for _, m := range hrefs {
		href := m[1]
		if href == "" || strings.HasPrefix(href, "#") {
			continue
		}
		target := filepath.Join(dir, filepath.Dir(filepath.FromSlash(page)), filepath.FromSlash(href))
		info, serr := os.Stat(target)
		assert.NoError(t, serr, "%s links to %q, which does not exist", page, href)
		if serr == nil {
			assert.True(t, info.Mode().IsRegular(), "%s links to %q, which is not a file", page, href)
		}
	}
}

// reachableFromIndex follows every link from index.html and returns the pages
// it reached. A page nothing points at is a page a reader cannot get to, which
// is why the tree assertions compare against this and not only against the
// absence of a broken link.
func reachableFromIndex(t *testing.T, dir string) map[string]bool {
	t.Helper()
	seen := map[string]bool{"index.html": true}
	queue := []string{"index.html"}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		body, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(current)))
		require.NoError(t, err)
		for _, m := range hrefRe.FindAllStringSubmatch(string(body), -1) {
			href := m[1]
			if href == "" || strings.HasPrefix(href, "#") {
				continue
			}
			target := filepath.ToSlash(filepath.Join(filepath.Dir(filepath.FromSlash(current)), filepath.FromSlash(href)))
			if seen[target] {
				continue
			}
			seen[target] = true
			queue = append(queue, target)
		}
	}
	return seen
}

// detailsBlock returns a page's collapsed `<details class="…">` block, closing
// tag included. It is sliced out so a test can assert what a collapsed block
// carries -- and, just as important, that something is not inside it -- without
// asserting on the rest of the page.
func detailsBlock(t *testing.T, body, class string) string {
	t.Helper()
	open := `<details class="` + class + `">`
	start := strings.Index(body, open)
	require.NotEqual(t, -1, start, "the page carries no %s block", class)
	end := strings.Index(body[start:], "</details>")
	require.NotEqual(t, -1, end, "the %s block is not closed", class)
	return body[start : start+end+len("</details>")]
}

// chainBlock returns the record page's collapsed chain-fields block, so a test
// can compare the fields a verifier checks across two renders byte for byte,
// rather than only asserting that each contains a hash.
func chainBlock(t *testing.T, body string) string {
	t.Helper()
	return detailsBlock(t, body, "chain")
}

// canonicalIn returns the text a page carries inside its evidence block,
// exactly as the page writes it -- escaped by the template engine, and by
// nothing else. A test compares it against the bytes the renderer was given, so
// a re-encoding, a pretty-printer or a raw splice all fail here.
//
// Extraction stops at the first `</pre>`, which is the block's own closing tag:
// a reason whose own bytes carried a closing tag that reached the page raw
// would truncate the text and fail the comparison rather than pass it.
func canonicalIn(t *testing.T, block string) string {
	t.Helper()
	const open, close = `<pre class="canonical">`, `</pre>`
	start := strings.Index(block, open)
	require.NotEqual(t, -1, start, "the evidence block carries no encoding")
	rest := block[start+len(open):]
	end := strings.Index(rest, close)
	require.NotEqual(t, -1, end, "the evidence block is not closed")
	return rest[:end]
}

func TestReportRendersAnIndexAndAPagePerMemoryAndRecord(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	reader := &fakeReader{records: []record.Record{
		newTestRecord(t, "rec-a1", 0, "mem-a", base),
		newTestRecord(t, "rec-a2", 1, "mem-a", base.Add(time.Minute)),
		newTestRecord(t, "rec-b1", 2, "mem-b", base.Add(2*time.Minute)),
	}}

	out, files, res := render(t, reader, report.Request{
		From:       sliceFrom,
		To:         sliceTo,
		Verify:     true,
		VerifiedAt: base.Add(time.Hour),
	})

	// The range read asked for exactly the window it was given.
	assert.Equal(t, sliceFrom, reader.askedFrom)
	assert.Equal(t, sliceTo, reader.askedTo)

	// The created tree: the index, one page per memory, one per record.
	assert.Equal(t, []string{
		"index.html",
		page("memory", 0, "mem-a"),
		page("memory", 2, "mem-b"),
		page("record", 0, "rec-a1"),
		page("record", 1, "rec-a2"),
		page("record", 2, "rec-b1"),
	}, treeNames(t, out))

	// Every page is reachable from the index, and every link on every page
	// resolves to a page that exists. A page nothing links to, or a link to a
	// page that was never written, fails here.
	for _, name := range treeNames(t, out) {
		assert.True(t, reachableFromIndex(t, out)[name], "%s is not reachable from index.html", name)
		assertLinksResolve(t, out, name)
	}

	// The index reaches the memory pages; a memory page reaches its own
	// records' pages and no other memory's.
	index := files["index.html"]
	assert.Contains(t, index, page("memory", 0, "mem-a"))
	assert.Contains(t, index, page("memory", 2, "mem-b"))
	memA := files[page("memory", 0, "mem-a")]
	assert.Contains(t, memA, "rec-a1")
	assert.Contains(t, memA, "rec-a2")
	assert.NotContains(t, memA, "rec-b1")
	assert.Contains(t, memA, page("record", 0, "rec-a1"))

	assert.Equal(t, report.Result{Memories: 2, Records: 3, Pages: 6}, res)
}

func TestReportPageFilenamesAreDerivedNotTakenFromIds(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	const (
		// A memory id as an upstream system could supply it: a fragment marker,
		// a colon, path traversal, a control character.
		memoryID = "mem#1:../..\x01"
		// A record id in the shape this codebase already produces, plus
		// traversal and a fragment.
		recordID = "add_resolved:stored_by_mem0:../../escape#9"
	)
	reader := &fakeReader{records: []record.Record{newTestRecord(t, recordID, 0, memoryID, base)}}

	root, out := tempRoot(t)
	// A canary beside the output directory: the tree is compared whole, so a
	// write anywhere outside out -- a traversal, or a stray file -- shows up as
	// an unexpected path rather than going unnoticed.
	canary := filepath.Join(root, "a", "sibling.txt")
	require.NoError(t, os.MkdirAll(filepath.Dir(canary), 0o755))
	require.NoError(t, os.WriteFile(canary, []byte("untouched\n"), 0o644))

	_, err := report.New(reader).Render(context.Background(), report.Request{From: sliceFrom, To: sliceTo}, out)
	require.NoError(t, err)

	// The created tree is exactly the derived one: no part of an id reached a
	// name.
	assert.Equal(t, []string{
		"a/out/index.html",
		"a/out/" + page("memory", 0, memoryID),
		"a/out/" + page("record", 0, recordID),
		"a/sibling.txt",
	}, allFilesUnder(t, root))

	// Whatever the id was, every name has the id-free shape.
	for _, name := range treeNames(t, out) {
		assert.Regexp(t, pageNameRe, name)
		assert.NotContains(t, name, "mem#1")
		assert.NotContains(t, name, "escape")
		assert.NotContains(t, name, "..")
	}

	// The canary is untouched, byte for byte.
	body, err := os.ReadFile(canary)
	require.NoError(t, err)
	assert.Equal(t, "untouched\n", string(body))

	// The ids are displayed instead of being used as names, so a reader can
	// still see what the page is about.
	files := readTree(t, out)
	assert.Contains(t, files["index.html"], "mem#1:../..")
	assert.Contains(t, files[page("memory", 0, memoryID)], "mem#1:../..")
	assert.Contains(t, files[page("record", 0, recordID)], "../../escape#9")
}

func TestReportEscapesStoredText(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	const text = "<script>alert(1)</script> and </textarea>"

	// One record whose CONTENT is markup, and one whose MEMORY ID is: both are
	// stored text from outside Notary, and both are rendered into a document
	// this project writes.
	content := withContent(newTestRecord(t, "rec-x", 0, "mem-x", base), text, false)
	hostileID := newTestRecord(t, "rec-y", 1, "mem<script>alert(2)</script>", base.Add(time.Minute))

	_, files, _ := render(t, &fakeReader{records: []record.Record{content, hostileID}},
		report.Request{From: sliceFrom, To: sliceTo})

	// The pages that carry the content carry it as text.
	for _, name := range []string{page("memory", 0, "mem-x"), page("record", 0, "rec-x")} {
		assert.Contains(t, files[name], "&lt;script&gt;alert(1)&lt;/script&gt;", name)
		assert.Contains(t, files[name], "&lt;/textarea&gt;", name)
	}
	// The pages that carry the hostile memory id carry it as text too.
	for _, name := range []string{"index.html", page("memory", 1, "mem<script>alert(2)</script>")} {
		assert.Contains(t, files[name], "&lt;script&gt;alert(2)&lt;/script&gt;", name)
	}

	// And no generated file anywhere carries raw markup.
	for name, body := range files {
		assert.NotContains(t, body, "<script", "%s carries unescaped markup", name)
		assert.NotContains(t, body, "</textarea", "%s carries an unescaped close tag", name)
	}
}

func TestReportWithholdsSensitiveContentByDefault(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	const secret = "the butler did it"
	rec := withContent(newTestRecord(t, "rec-s", 0, "mem-s", base), secret, true)

	_, files, res := render(t, &fakeReader{records: []record.Record{rec}},
		report.Request{From: sliceFrom, To: sliceTo})

	// No generated file carries the withheld text.
	for name, body := range files {
		assert.NotContains(t, body, secret, "%s carries withheld content", name)
	}
	assert.Equal(t, 1, res.Redacted)

	// A withheld body says so, and names the marker the exported line carries,
	// so a reader can tell "withheld" from "empty": the block is the withheld
	// one, and it is neither the empty-content block nor the no-content one.
	line, err := export.Render(rec, false)
	require.NoError(t, err)
	require.NotEmpty(t, line.Redacted, "a sensitive record rendered without sensitive content must say so")
	for _, name := range []string{page("memory", 0, "mem-s"), page("record", 0, "rec-s")} {
		assert.Contains(t, files[name], line.Redacted, name)
		assert.Contains(t, files[name], `class="withheld"`, name)
		assert.NotContains(t, files[name], `class="empty"`, name)
		assert.NotContains(t, files[name], `class="none"`, name)
	}
}

func TestReportRendersSensitiveContentWithTheFlag(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	const secret = "the butler did it"
	rec := withContent(newTestRecord(t, "rec-s", 0, "mem-s", base), secret, true)
	reader := &fakeReader{records: []record.Record{rec}}

	memPage, recPage := page("memory", 0, "mem-s"), page("record", 0, "rec-s")
	_, withheld, _ := render(t, reader, report.Request{From: sliceFrom, To: sliceTo})
	_, shown, res := render(t, reader, report.Request{From: sliceFrom, To: sliceTo, IncludeSensitive: true})

	assert.NotContains(t, withheld[memPage], secret)
	assert.NotContains(t, withheld[recPage], secret)
	assert.Contains(t, shown[memPage], secret)
	assert.Contains(t, shown[recPage], secret)
	assert.Zero(t, res.Redacted)

	// Showing the content changes no evidence: the chain fields a verifier
	// checks are byte-identical between the two runs, and they are the fields
	// export.Render put on the line -- including the signature, which the
	// report carries because it may be the only artefact that survives.
	withheldLine, err := export.Render(rec, false)
	require.NoError(t, err)
	shownLine, err := export.Render(rec, true)
	require.NoError(t, err)
	assert.Equal(t, withheldLine.Hash, shownLine.Hash)
	assert.Equal(t, withheldLine.PrevHash, shownLine.PrevHash)
	assert.Equal(t, chainBlock(t, withheld[recPage]), chainBlock(t, shown[recPage]))

	for _, want := range []string{shownLine.Hash, shownLine.PrevHash, shownLine.Signature, shownLine.SignerKeyID} {
		require.NotEmpty(t, want, "the fixture's line must carry every chain field")
		assert.Contains(t, withheld[recPage], want)
		assert.Contains(t, shown[recPage], want)
	}
}

func TestReportTellsWithheldEmptyAndAbsentContentApart(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	withheldRec := withContent(newTestRecord(t, "rec-1", 0, "mem-c", base), "the secret", true)
	emptyRec := withContent(newTestRecord(t, "rec-2", 1, "mem-c", base.Add(time.Minute)), "", false)
	noContentRec := newTestRecord(t, "rec-3", 2, "mem-c", base.Add(2*time.Minute))

	_, files, res := render(t, &fakeReader{records: []record.Record{withheldRec, emptyRec, noContentRec}},
		report.Request{From: sliceFrom, To: sliceTo})

	// Three records, three states, three visibly different blocks: a reader
	// must never read "you are not cleared for it" as "nothing was recorded",
	// or the other way round.
	withheld := files[page("record", 0, "rec-1")]
	empty := files[page("record", 1, "rec-2")]
	absent := files[page("record", 2, "rec-3")]

	assert.Contains(t, withheld, `class="withheld"`)
	assert.NotContains(t, withheld, "the secret")
	assert.Contains(t, empty, `class="empty"`)
	assert.NotContains(t, empty, `class="withheld"`)
	assert.Contains(t, absent, `class="none"`)
	assert.NotContains(t, absent, `class="withheld"`)
	assert.NotContains(t, absent, `class="empty"`)

	// Only the withheld record was counted as redacted: empty content and
	// absent content hide nothing.
	assert.Equal(t, 1, res.Redacted)
}

func TestReportMemoryPageMatchesThePhrasingExportProduces(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	rec := newTestRecord(t, "rec-p", 0, "mem-p", base)

	sentence, ok := export.Phrase(rec)
	require.True(t, ok, "the fixture record must have a phrasing")

	_, files, _ := render(t, &fakeReader{records: []record.Record{rec}},
		report.Request{From: sliceFrom, To: sliceTo})

	assert.Contains(t, files[page("memory", 0, "mem-p")], sentence)
	assert.Contains(t, files[page("record", 0, "rec-p")], sentence)
}

// TestReportRecordPageCarriesTheEvidenceVerbatim is spec §12.4's test.
//
// The record page reads ONE field outside export.Line: the reason's evidence.
// It may, because Line does not describe that field at all, so the report and
// an exported line cannot describe it differently -- which is the whole point of
// rendering from Line. What must hold is that the bytes on the page are the
// bytes the record's hash covers. They are the reason's own canonical encoding,
// Reason.Encode(), which record.CanonicalBytes writes into the hashed bytes
// verbatim and which carries the payload as raw JSON, so pretty-printing,
// re-indenting or re-encoding it would display something the hash does not
// attest to -- worse, in an evidence artefact, than showing nothing.
//
// The encoding is also complete in a way the payload alone is not: an Observed
// reason shows its source AND its payload, a Reconstructed one its basis, rule,
// rule version and confidence, and an Internal one its note. The payload is the
// only exported accessor on any of the three evidence types, so the narrower
// rendering showed nothing at all for two of the three kinds.
func TestReportRecordPageCarriesTheEvidenceVerbatim(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)

	// One slice, one record per evidence kind. The observed payload is chosen
	// so canonicalisation changes it (keys reordered, whitespace dropped,
	// markup escaped to \u003c) and so the engine has JSON quotes to escape.
	observed := observedRecord(t, "rec-observed", 0, "mem-ev", base,
		`{"note":"<script>alert(1)</script></pre> & 1+1=2 'q'","n":[1,2,{"deep":true}]}`)
	reconstructed := reconstructedRecord(t, "rec-recon", 1, "mem-ev", base.Add(time.Minute))
	internal := internalRecord(t, "rec-internal", 2, "mem-ev", base.Add(2*time.Minute))

	_, files, res := render(t, &fakeReader{records: []record.Record{observed, reconstructed, internal}},
		report.Request{From: sliceFrom, To: sliceTo})

	assert.Equal(t, report.Result{Memories: 1, Records: 3, Pages: 5}, res)

	// encodedOf is the bytes the page must carry, read the way the renderer
	// reads them: the reason's own canonical encoding.
	encodedOf := func(t *testing.T, rec record.Record) string {
		t.Helper()
		raw, err := rec.Reason.Encode()
		require.NoError(t, err, "the fixture's reason must encode")
		return string(raw)
	}
	// blockOf asserts the page carries the evidence inside a collapsed block and
	// returns what that block holds, decoded back out of the engine's escaping.
	// It takes the test it was called from, so a failed assertion stops the
	// subtest rather than the whole test.
	blockOf := func(t *testing.T, seq uint64, id string) string {
		t.Helper()
		block := detailsBlock(t, files[page("record", seq, id)], "evidence")
		text := canonicalIn(t, block)
		require.NotEmpty(t, text, "%s: the evidence block must never be empty", id)
		return html.UnescapeString(text)
	}

	t.Run("an observed reason shows its source and its payload", func(t *testing.T) {
		got := blockOf(t, 0, "rec-observed")
		assert.Equal(t, encodedOf(t, observed), got,
			"the block must carry Reason.Encode()'s bytes, not a re-encoding of them")
		assert.Contains(t, got, `"source":"mem0_response"`)
		// The payload lands inside the envelope as the bytes the hash covers.
		ev, ok := observed.Reason.Observed()
		require.True(t, ok, "the fixture must carry observed evidence")
		assert.True(t, strings.Contains(got, string(ev.Payload())),
			"the canonical payload must appear byte-for-byte inside the encoding")
		// Canonicalisation has already escaped the fixture's markup, so the
		// bytes on the page carry no raw tag.
		assert.Contains(t, got, `\u003cscript\u003e`)
		assert.NotContains(t, got, "<script")
	})

	t.Run("a reconstructed reason shows its basis, rule, rule version and confidence", func(t *testing.T) {
		got := blockOf(t, 1, "rec-recon")
		assert.Equal(t, encodedOf(t, reconstructed), got)
		assert.Contains(t, got, `"basis":["rec-basis-1","rec-basis-2"]`)
		assert.Contains(t, got, `"rule":"kept-by-content-match"`)
		assert.Contains(t, got, `"rule_version":"v2"`)
		assert.Contains(t, got, `"confidence":0.75`)
	})

	t.Run("an internal reason shows its note", func(t *testing.T) {
		got := blockOf(t, 2, "rec-internal")
		assert.Equal(t, encodedOf(t, internal), got)
		assert.Contains(t, got, "the sweep removed a memory no search covered")
	})

	t.Run("the encoding is escaped by the engine, and is one line", func(t *testing.T) {
		block := detailsBlock(t, files[page("record", 0, "rec-observed")], "evidence")
		text := canonicalIn(t, block)
		assert.NotContains(t, text, `"`, "the encoding's quotes must be escaped, not emitted raw")
		assert.Contains(t, text, "&#34;", "the escaped encoding must carry the entity, not a bare quote")
		// The canonical encoding is a single line: a pretty-printer would
		// break this, and its output would not be the bytes the hash covers.
		assert.NotContains(t, text, "\n")
		// A closing tag inside the reason cannot close the block: the block has
		// exactly the one <pre> the template wrote.
		assert.Equal(t, 1, strings.Count(block, "</pre>"))
		// And no file anywhere in the report carries raw markup.
		for name, body := range files {
			assert.NotContains(t, body, "<script", "%s carries unescaped markup", name)
		}
	})

	t.Run("a record carrying no evidence is refused, so no page can carry an empty block", func(t *testing.T) {
		// The only way to carry no evidence is the zero Reason, and it is not a
		// Reason this project admits: export.Render refuses it at the tier check
		// before the evidence is reached, so the report writes no page for it --
		// and therefore no empty evidence block. A record that DOES render has a
		// valid reason, whose canonical encoding is never empty (Encode writes
		// the version tag, kind and tier unconditionally), which is why the
		// template's guard is the structural half of "absent, not empty" and no
		// fixture can exercise a block that exists but holds nothing.
		noEvidence := record.Record{
			ID:         "rec-no-evidence",
			Seq:        0,
			At:         base,
			RecordedAt: base,
			Event:      record.EventMemoryKept,
			// Reason is the zero value: no observed, reconstructed or internal
			// evidence of any kind.
			Subject: record.Subject{MemoryID: "mem-none", ContentHash: record.ContentHash("rec-no-evidence")},
		}
		require.Error(t, noEvidence.Validate(), "and it is not even a valid record")

		_, out := tempRoot(t)
		_, err := report.New(&fakeReader{records: []record.Record{noEvidence}}).Render(
			context.Background(), report.Request{From: sliceFrom, To: sliceTo}, out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rec-no-evidence", "the refusal must name the record")
		assert.NoDirExists(t, out, "a refused render writes nothing")
	})

	t.Run("a reason that cannot be encoded is a render error, not a skipped block", func(t *testing.T) {
		// This path IS reachable, and only through a Reader that is not the
		// store. NewReconstructedEvidence does not validate its confidence, so a
		// NaN one builds a reason that passes Reason.Validate and export.Render
		// -- and then fails Reason.Encode, because encoding/json refuses NaN.
		// record.ComputeHash fails on the same record (it hashes the encoding),
		// so such a record can never have been appended to a ledger; a report
		// that rendered it would describe a record the chain never attested to.
		ev, err := record.NewReconstructedEvidence(
			[]record.RecordID{"rec-basis-1"}, "kept-by-content-match", "v2", math.NaN())
		require.NoError(t, err, "the confidence is not validated, which is how this is constructible")
		reason, err := record.NewReconstructedReason(record.ReasonKeptByContentMatch, ev)
		require.NoError(t, err)
		unencodable := recordWith(t, "rec-nan", 0, "mem-ev", base, record.EventMemoryKept, reason)
		require.NoError(t, unencodable.Validate(), "it is a valid record: the failure is in the encoding")

		// The line renders, so the failure is genuinely the evidence read's.
		_, err = export.Render(unencodable, false)
		require.NoError(t, err, "export.Render must succeed, or this test proves nothing")
		_, err = unencodable.Reason.Encode()
		require.Error(t, err, "the reason must not encode")
		_, err = record.ComputeHash(unencodable)
		require.Error(t, err, "and so it cannot be hashed, and cannot be a stored record")

		_, out := tempRoot(t)
		_, err = report.New(&fakeReader{records: []record.Record{unencodable}}).Render(
			context.Background(), report.Request{From: sliceFrom, To: sliceTo}, out)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rec-nan", "the failure must name the record")
		assert.Contains(t, err.Error(), "record: encode reason", "and must carry the cause, wrapped")
		assert.NoDirExists(t, out, "a failed render writes nothing")
	})
}
func TestReportRecordsTheCommandThatProducedIt(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	reader := &fakeReader{records: []record.Record{newTestRecord(t, "rec-c", 0, "mem-c", base)}}

	t.Run("a memory", func(t *testing.T) {
		out, files, _ := render(t, reader, report.Request{
			MemoryID:         "mem-c",
			IncludeSensitive: true,
			VerifiedAt:       base.Add(time.Hour),
		})
		index := files["index.html"]
		assert.Contains(t, index, "notary report --out "+out)
		assert.Contains(t, index, "--memory mem-c")
		assert.Contains(t, index, "--include-sensitive")
		// No verification ran, and the page says so through the command it
		// records rather than leaving a reader to assume it passed.
		assert.Contains(t, index, "--no-verify")
		assert.Contains(t, index, base.Add(time.Hour).UTC().Format(time.RFC3339Nano))
		assert.Equal(t, "mem-c", reader.askedMemory)
	})

	t.Run("a range", func(t *testing.T) {
		out, files, _ := render(t, reader, report.Request{
			From:       sliceFrom,
			To:         sliceTo,
			Scope:      record.Scope{UserID: "u1"},
			Verify:     true,
			VerifiedAt: base,
		})
		index := files["index.html"]
		assert.Contains(t, index, "notary report --out "+out)
		assert.Contains(t, index, "--from "+sliceFrom.UTC().Format(time.RFC3339Nano))
		assert.Contains(t, index, "--to "+sliceTo.UTC().Format(time.RFC3339Nano))
		assert.Contains(t, index, "--user-id u1")
		assert.NotContains(t, index, "--memory")
		assert.NotContains(t, index, "--no-verify")
	})
}

func TestReportIsDeterministicAcrossRuns(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	reader := &fakeReader{records: []record.Record{
		withContent(newTestRecord(t, "rec-d1", 0, "mem-d", base), "first note", false),
		withContent(newTestRecord(t, "rec-d2", 1, "mem-d", base.Add(time.Minute)), "second note", true),
		newTestRecord(t, "rec-d3", 2, "", base.Add(2*time.Minute)),
	}}
	req := report.Request{
		From:       sliceFrom,
		To:         sliceTo,
		Verify:     true,
		VerifiedAt: base.Add(time.Hour),
	}
	rp := report.New(reader)

	_, first := tempRoot(t)
	firstRes, err := rp.Render(context.Background(), req, first)
	require.NoError(t, err)
	before := readTree(t, first)

	// A second run over the same slice writes the same bytes: nothing in the
	// render path reads a clock, a map's iteration order or any other ambient
	// state.
	secondRes, err := rp.Render(context.Background(), req, first)
	require.NoError(t, err)
	assert.Equal(t, before, readTree(t, first), "two renders of one Request must be byte-identical")
	assert.Equal(t, firstRes, secondRes)

	// Only the index names the output directory, so every other page is
	// byte-identical when the report is written somewhere else entirely.
	_, elsewhere := tempRoot(t)
	_, err = rp.Render(context.Background(), req, elsewhere)
	require.NoError(t, err)
	elsewhereTree := readTree(t, elsewhere)
	require.Equal(t, len(before), len(elsewhereTree))
	for name, body := range elsewhereTree {
		if name == "index.html" {
			continue
		}
		assert.Equal(t, before[name], body, "%s must not depend on where the report was written", name)
	}
}

func TestReportNumbersPagesByLedgerSeqNotByReadOrder(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	// The store returns a range ordered by event time, which need not be the
	// ledger's order: here seq 1 happened BEFORE seq 0, so the read hands them
	// over reversed. A page's name follows Seq regardless, or which file a
	// reader is sent to would depend on the store's ORDER BY.
	first := newTestRecord(t, "rec-early", 1, "mem-o", base)
	second := newTestRecord(t, "rec-late", 0, "mem-o", base.Add(time.Minute))

	out, files, _ := render(t, &fakeReader{records: []record.Record{first, second}},
		report.Request{From: sliceFrom, To: sliceTo})

	assert.Equal(t, []string{
		"index.html",
		page("memory", 0, "mem-o"),
		page("record", 0, "rec-late"),
		page("record", 1, "rec-early"),
	}, treeNames(t, out))

	// The memory's own page is its lifecycle in Seq order, not in read order.
	timeline := files[page("memory", 0, "mem-o")]
	early, late := strings.Index(timeline, page("record", 1, "rec-early")), strings.Index(timeline, page("record", 0, "rec-late"))
	require.NotEqual(t, -1, early)
	require.NotEqual(t, -1, late)
	assert.Less(t, late, early, "the memory page must list its records in Seq order")
}

func TestReportRendersRecordsThatBelongToNoMemory(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	rec := newTestRecord(t, "add_requested#1", 0, "", base)

	out, files, res := render(t, &fakeReader{records: []record.Record{rec}},
		report.Request{From: sliceFrom, To: sliceTo})

	// A record with no memory gets a record page and no memory page, and the
	// index still reaches it: a page nothing links to is a page a reader
	// cannot find.
	assert.Equal(t, []string{"index.html", page("record", 0, "add_requested#1")}, treeNames(t, out))
	assert.Equal(t, report.Result{Records: 1, Pages: 2}, res)
	assert.Contains(t, files["index.html"], page("record", 0, "add_requested#1"))
	assertLinksResolve(t, out, "index.html")
	assertLinksResolve(t, out, page("record", 0, "add_requested#1"))
}

func TestReportScopesARangeTheSameWayExportDoes(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	mine := newTestRecord(t, "u1-rec", 0, "u1-mem", base)
	theirs := newTestRecord(t, "u2-rec", 1, "u2-mem", base.Add(time.Minute))
	theirs.Subject.Scope.UserID = "u2"

	out, files, res := render(t, &fakeReader{records: []record.Record{mine, theirs}},
		report.Request{From: sliceFrom, To: sliceTo, Scope: record.Scope{UserID: "u1"}})

	assert.Equal(t, []string{
		"index.html",
		page("memory", 0, "u1-mem"),
		page("record", 0, "u1-rec"),
	}, treeNames(t, out))
	assert.Equal(t, report.Result{Memories: 1, Records: 1, Pages: 3}, res)
	assert.NotContains(t, files["index.html"], "u2-mem")
}

func TestReportRefusesTwoRecordsThatWouldShareOnePage(t *testing.T) {
	base := time.Date(2024, 5, 2, 9, 0, 0, 0, time.UTC)
	rec := newTestRecord(t, "rec-dupe", 0, "mem-a", base)

	// A reader that hands the same record over twice would name one page twice.
	// Refusing is the point: writing the second copy over the first would lose
	// a record from an artefact whose whole purpose is that nothing is lost,
	// and the run would still report two record pages.
	_, out := tempRoot(t)
	_, err := report.New(&fakeReader{records: []record.Record{rec, rec}}).Render(
		context.Background(), report.Request{From: sliceFrom, To: sliceTo}, out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rec-dupe")
	assert.NoDirExists(t, out)
}

func TestReportRefusesARequestThatNamesNoSingleSubject(t *testing.T) {
	_, out := tempRoot(t)
	rp := report.New(&fakeReader{})

	_, err := rp.Render(context.Background(), report.Request{}, out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subject")

	_, err = rp.Render(context.Background(), report.Request{MemoryID: "mem-a", From: sliceFrom, To: sliceTo}, out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mem-a")

	// A memory id with half a window is the same two-subjects mistake: the
	// window would be silently ignored, so it is refused rather than dropped.
	_, err = rp.Render(context.Background(), report.Request{MemoryID: "mem-a", To: sliceTo}, out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mem-a")

	_, err = rp.Render(context.Background(), report.Request{From: sliceFrom}, out)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "To")

	// A refused request writes nothing at all: no directory, no page, no
	// partial report an operator could mistake for a complete one.
	assert.NoDirExists(t, out)
}
