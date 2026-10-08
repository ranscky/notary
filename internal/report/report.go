// Package report renders a slice of the ledger as a small set of static,
// self-contained HTML pages: an index, one page per memory, and one page per
// record. The reader is the compliance lead the design names -- someone who
// must explain an agent's memory decisions without reading code or running a
// command -- so every page carries fields the ledger already holds, phrased for
// a human, and nothing this package invented.
//
// Four properties shape it:
//
//   - Every page is rendered from export.Line. export.Render already decides
//     the phrasing, the visibility tier, the content-withholding rule and every
//     hash, so a page and an exported line cannot describe the same record
//     differently. The package therefore imports internal/export and never
//     internal/phrase: a report can reach no language model (design D10).
//
//     The one field read from the record instead is the reason's evidence, and
//     spec §12.4 is why it may be: export.Line carries ReasonKind and describes
//     no evidence at all, so the rule's purpose -- one field, one description --
//     is not touched, and the rule's opposite -- re-deriving a field the line
//     does carry -- is what the report never does. What the page shows is the
//     reason's own canonical encoding, Reason.Encode, verbatim: those are the
//     bytes the record's hash was computed over (record.CanonicalBytes writes
//     that encoding into the hashed bytes), and the encoding is complete -- it
//     carries the source and payload of an Observed reason, the basis, rule,
//     rule version and confidence of a Reconstructed one, and the note of an
//     Internal one.
//
//   - A page's NAME is derived, never taken from an id. Record ids in this
//     codebase carry '#' and ':' and '/' (corr-search-1#1,
//     add_resolved:stored_by_mem0:<event-id>), and ids from outside carry '..'
//     and control characters, so a name built by pasting an id is both
//     non-portable (':' is illegal on Windows) and a path-traversal risk. A
//     page is named from a sequence number and the first eight hex digits of
//     record.ContentHash(id) -- the project's existing, domain-separated,
//     length-prefixed digest, reused rather than reinvented -- and the id is
//     only ever displayed, escaped, by the template engine.
//
//   - It is a read path and one directory of output. It never touches the
//     ledger or the gap log, never walks the chain, never verifies anything,
//     never holds a key and never signs. The chain state arrives in Request
//     already computed by the caller through the same break collection
//     `notary verify` uses, so a page and the command cannot disagree.
//
//   - It is deterministic. Nothing here calls time.Now: the run is dated from
//     Request.VerifiedAt, so two renders of one Request write byte-identical
//     pages, and a report can be regenerated and compared.
package report

import (
	"bytes"
	"context"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"notary/internal/export"
	"notary/internal/ledger"
	"notary/internal/record"
)

// templateFS holds the page templates, compiled into the binary. A report is
// opened from a memory stick, an email attachment or a ticket, so the pages
// travel with the tool that wrote them rather than with the reader's machine,
// and no page is read from the filesystem at render time.
//
//go:embed templates/*.html
var templateFS embed.FS

// The page templates, by file. Each file is parsed into one template named
// after its base name, so a page's template name and its file name are the same
// string: a page cannot be written under a template that does not render it.
const (
	// indexFile is the report's entry point: the run, the slice, and one row
	// per memory.
	indexFile = "index.html"
	// memoryFile renders one memory's lifecycle.
	memoryFile = "memory.html"
	// recordFile renders one record's story.
	recordFile = "record.html"

	// memoryDir and recordDir hold the two page kinds, so the report's root
	// stays a short list of links however long the slice is.
	memoryDir = "memory"
	recordDir = "record"

	templateGlob = "templates/*.html"
	templateRoot = "report"
)

// The link prefixes the pages use. A page at the report's root links straight
// to a sibling directory; a page one directory down steps up first. They are
// two constants rather than assembled per page, so no template builds a path of
// its own.
const (
	rootBase = "."
	subBase  = ".."
)

// Reader is the ledger read path a report renders: a range, and one memory's
// lifecycle. *ledger.Ledger satisfies it through ListRecords and
// ListRecordsByMemory, so a test renders from a fake and the command passes the
// real ledger.
type Reader interface {
	// ListRecords returns the records whose At lies within [from, to].
	ListRecords(from, to time.Time) ([]record.Record, error)
	// ListRecordsByMemory returns the records whose subject memory is memoryID,
	// in Seq order.
	ListRecordsByMemory(memoryID string) ([]record.Record, error)
}

// The real ledger satisfies Reader, so the command hands *ledger.Ledger to New
// unchanged and this package needs no adapter. The assertion makes an interface
// drift a build failure rather than a surprise in the command.
var _ Reader = (*ledger.Ledger)(nil)

// Request names the slice a report renders and what its pages may claim about
// it.
type Request struct {
	// MemoryID selects one memory: every record whose subject memory is
	// MemoryID. It and From/To are two ways of naming one subject, and a
	// request names exactly one; naming neither, or both, is refused.
	MemoryID string
	// From and To bound a range: the records whose At lies within [From, To],
	// narrowed by Scope. Both are required, because a store returns nothing for
	// a zero bound, and an unbounded read would render as a silently empty
	// report.
	From, To time.Time
	// Scope narrows a range exactly as it does for export and replay, through
	// export.ScopeMatches, so the three range views compose identically. It is
	// meaningless with MemoryID, where the subject already identifies the
	// scope.
	Scope record.Scope
	// IncludeSensitive renders the stored text of a record marked sensitive.
	// When it is false -- the safe default -- the text is withheld and the page
	// says so, with the marker export.Render sets. It changes only what is
	// printed, never a hash: rendering is a read path.
	IncludeSensitive bool

	// Verify reports whether a verification ran for this report, and Breaks is
	// what the shared break collection found. Verify true with no breaks
	// ("checked, clean") and Verify false with no breaks ("not checked") are
	// different facts, and len(Breaks) alone cannot tell them apart, which is
	// why Verify exists beside it.
	Verify bool
	// Breaks is the chain and gap-log state the caller computed, through the
	// same break collection `notary verify` uses, so a page and the command
	// cannot disagree. The renderer never walks the chain itself.
	Breaks []ledger.Break
	// VerifiedAt is the instant the run is attributed to: a page dates itself
	// from it and from nothing else, because the renderer never calls time.Now.
	// A zero VerifiedAt renders pages with no instant rather than a fabricated
	// one.
	VerifiedAt time.Time
}

// validate refuses a request this renderer cannot honour: one that names no
// subject or two, or a range that is half-bound or backwards. It refuses before
// anything is read or written, so a refused request leaves no directory and no
// page behind.
//
// The command refuses the same shapes first, naming the flags the operator
// typed; this is the package's own guard against a caller that reads a range
// backwards or hands over a memory id and a window at once, which would
// otherwise silently render one of the two slices the request names.
func (r Request) validate() error {
	switch {
	case r.MemoryID == "" && r.From.IsZero():
		return errors.New("report: no subject: set MemoryID, or From and To")
	case r.MemoryID != "" && (!r.From.IsZero() || !r.To.IsZero()):
		return fmt.Errorf("report: MemoryID %q and a range name two subjects; set exactly one", r.MemoryID)
	case r.MemoryID == "" && r.To.IsZero():
		return errors.New("report: a range needs both From and To")
	case r.To.Before(r.From):
		return fmt.Errorf("report: To %s is before From %s",
			r.To.UTC().Format(time.RFC3339Nano), r.From.UTC().Format(time.RFC3339Nano))
	}
	return nil
}

// Result counts what one Render wrote: how much of the slice its pages carry,
// and how much of that was withheld.
//
// It is meaningful in full only on success. On the error path from Render it
// carries the pages written before the failure, so a caller must check the
// error before trusting it.
type Result struct {
	// Memories is the number of memory pages written: the memories with at
	// least one record in the slice.
	Memories int
	// Records is the number of record pages written.
	Records int
	// Redacted is the number of records whose content was withheld because the
	// stored record marked it sensitive and IncludeSensitive was false. A
	// record that carried no content is not counted: nothing was hidden.
	Redacted int
	// Pages is every file written into the output directory.
	Pages int
}

// Reporter renders ledger slices as pages. It holds the reader and nothing a
// Render mutates, so one Reporter may render many requests, and no state is
// shared between them.
type Reporter struct {
	reader Reader
}

// New returns a Reporter that reads the slices it renders through r.
//
// There is no error to return: the page templates are embedded in the binary
// and are parsed per render, so a malformed template -- which would mean a
// broken build rather than a bad argument -- is reported by Render instead of
// panicking, and a Reporter cannot be constructed in a state that cannot
// render.
func New(r Reader) *Reporter {
	return &Reporter{reader: r}
}

// Render reads req's slice and writes the report's pages into dir, creating dir
// and the subdirectories the pages need.
//
// Render verifies nothing, walks no chain, reads no gap log and holds no key:
// the chain state is req's, already computed by the caller. It writes only
// inside dir, and the pages it names are derived from a sequence number and a
// content digest, never from an id.
//
// It is a pure function of the records it reads, req and dir -- no call to
// time.Now, no map iteration order, no ambient state -- so two runs over the
// same slice with the same Request write byte-identical pages. The one page
// that names where the report was written is the index, which records the
// command that produced it.
//
// Pages are written memory first, record second, index last, so a run that
// fails part-way leaves pages but no index claiming a complete report. A
// request the renderer refuses writes nothing at all, not even dir. On error
// the returned Result is not zero: it carries the pages written before the
// failure, so a caller must check the error before trusting it.
//
// A nil ctx is treated as "no cancellation" rather than dereferenced.
func (rp *Reporter) Render(ctx context.Context, req Request, dir string) (Result, error) {
	var res Result

	if ctx == nil {
		ctx = context.Background()
	}
	if rp == nil || rp.reader == nil {
		return res, errors.New("report: no ledger reader (build the Reporter with New)")
	}
	if err := req.validate(); err != nil {
		return res, err
	}
	if dir == "" {
		return res, errors.New("report: no output directory")
	}

	tmpl, err := parseTemplates()
	if err != nil {
		return res, err
	}

	records, err := rp.read(req)
	if err != nil {
		return res, err
	}
	rendered, err := renderRecords(ctx, records, req.IncludeSensitive)
	if err != nil {
		return res, err
	}
	pages, built, err := buildPages(req, dir, rendered)
	if err != nil {
		return res, err
	}
	res = built

	// The tree is created only once the slice has been read and every page
	// named, so a read failure leaves no empty directory behind.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return res, fmt.Errorf("report: create output directory %s: %w", dir, err)
	}
	subdirs := map[string]bool{}
	for _, p := range pages {
		parent := path.Dir(p.name)
		if parent == "." || subdirs[parent] {
			continue
		}
		subdirs[parent] = true
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(parent)), 0o755); err != nil {
			return res, fmt.Errorf("report: create %s: %w", parent, err)
		}
	}

	for _, p := range pages {
		if err := ctx.Err(); err != nil {
			return res, fmt.Errorf("report: %w", err)
		}
		if err := writePage(tmpl, dir, p); err != nil {
			return res, err
		}
		res.Pages++
	}
	return res, nil
}

// parseTemplates parses every embedded page template into one set, so the three
// page kinds share the snippet definitions (the document head and foot, and
// the content block) that keep them from drifting apart.
//
// It is a function rather than package state: no mutable global holds a
// template, so no caller can hold one another caller reconfigures, and a
// template parse failure is a value Render returns rather than a panic a
// library should never raise.
func parseTemplates() (*template.Template, error) {
	tmpl, err := template.New(templateRoot).ParseFS(templateFS, templateGlob)
	if err != nil {
		return nil, fmt.Errorf("report: parse page templates: %w", err)
	}
	return tmpl, nil
}

// read returns the slice's records in Seq order.
//
// The two subjects read different paths: one memory through
// ListRecordsByMemory, which is that memory's lifecycle, and a range through
// ListRecords narrowed by export.ScopeMatches, so this report narrows exactly
// as export and replay do.
//
// The records are ordered by Seq here rather than trusted to arrive that way.
// ListRecords orders by At -- the event time, which need not follow the ledger
// -- while a page's NAME follows Seq, so which file a reader is sent to must
// not depend on the store's ORDER BY. A memory's lifecycle is listed in Seq
// order on its page for the same reason. The read's slice is copied before it
// is ordered: reordering a slice a Reader handed over would change the reader's
// own view.
func (rp *Reporter) read(req Request) ([]record.Record, error) {
	var records []record.Record
	if req.MemoryID != "" {
		read, err := rp.reader.ListRecordsByMemory(req.MemoryID)
		if err != nil {
			return nil, fmt.Errorf("report: read memory %q: %w", req.MemoryID, err)
		}
		records = append(records, read...)
	} else {
		read, err := rp.reader.ListRecords(req.From, req.To)
		if err != nil {
			return nil, fmt.Errorf("report: read range: %w", err)
		}
		for _, rec := range read {
			if export.ScopeMatches(req.Scope, rec.Subject.Scope) {
				records = append(records, rec)
			}
		}
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].Seq < records[j].Seq })
	return records, nil
}

// renderedRecord is one record as the pages need it: its exported line, which
// every page renders, plus the one field the line does not carry -- the reason's
// evidence (spec §12.4). Pairing them here rather than reading the record again
// at the template keeps the two facts about one record travelling together.
type renderedRecord struct {
	line     export.Line
	evidence *evidenceView
}

// renderRecords renders every record as its export.Line, in order, and reads
// from each the one thing a line does not carry: the evidence its reason holds.
//
// A page cannot describe a record differently from an exported line, because
// every field it shows IS the exported line; the evidence is the single
// exception, and spec §12.4 is why it may be: Line describes no evidence field,
// so there is no field on which the report and an export could disagree.
//
// A reason that cannot be encoded fails the whole render rather than losing its
// evidence block: see evidenceOf.
func renderRecords(ctx context.Context, records []record.Record, includeSensitive bool) ([]renderedRecord, error) {
	rendered := make([]renderedRecord, 0, len(records))
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("report: %w", err)
		}
		line, err := export.Render(rec, includeSensitive)
		if err != nil {
			return nil, fmt.Errorf("report: render record %q: %w", rec.ID, err)
		}
		evidence, err := evidenceOf(rec)
		if err != nil {
			return nil, err
		}
		rendered = append(rendered, renderedRecord{line: line, evidence: evidence})
	}
	return rendered, nil
}

// evidenceView is the evidence a record's reason carries: the reason's own
// canonical encoding, as Reason.Encode returns it.
//
// The bytes are passed through with nothing added, removed or re-encoded. They
// are the bytes the record's hash was computed over -- record.CanonicalBytes
// writes Reason.Encode's output into the hashed bytes -- so pretty-printing,
// re-indenting or re-encoding them would put something on the page that the
// hash does not attest to, which in an evidence artefact is worse than showing
// nothing. The encoding carries the payload as raw JSON
// (observedEnvelope.Payload is a json.RawMessage), so the payload's own bytes
// survive inside it unmodified too. The template engine escapes the text, and
// the page collapses it, because it is unbounded.
//
// It is also complete, which ObservedEvidence.Payload() was not: Encode is the
// whole self-describing reason -- version tag, kind, tier, and the payload for
// that tier -- so an Observed reason shows its source AND its payload, a
// Reconstructed one shows its basis, rule, rule version and confidence, and an
// Internal one shows its note. Payload is the only exported accessor on any of
// the three evidence types, so reading the canonical encoding is also the only
// way to show two of the three kinds at all.
type evidenceView struct {
	// Canonical is the reason's canonical encoding, verbatim.
	Canonical string
}

// evidenceOf returns the evidence to show for a record, or nil when there is
// nothing to show.
//
// A reason that cannot be encoded is an error, not a missing block: Encode
// validates first, and a reason failing that validation is a fact about the
// data rather than a rendering detail, so the report refuses the record instead
// of describing it with its why removed. The path is reachable only through a
// Reader that is not the store: Encode also fails when encoding/json cannot
// marshal the reason (a NaN confidence, which NewReconstructedEvidence does not
// reject), and such a record cannot be hashed either -- record.ComputeHash
// hashes the encoding -- so no ledger can hold one. A record read from a ledger
// therefore always has an encoding, and a report that meets one that does not
// is looking at something the chain never attested to.
//
// The block's presence is otherwise structural: Encode writes the version tag,
// kind and tier unconditionally, so a valid reason never encodes to nothing,
// and the nil case keeps an empty block off the page rather than expressing a
// state a ledger can produce.
func evidenceOf(rec record.Record) (*evidenceView, error) {
	encoded, err := rec.Reason.Encode()
	if err != nil {
		return nil, fmt.Errorf("report: encode the reason of record %q: %w", rec.ID, err)
	}
	if len(encoded) == 0 {
		return nil, nil
	}
	return &evidenceView{Canonical: string(encoded)}, nil
}

// pageFile is one file a Render writes.
type pageFile struct {
	// name is the file's path relative to the report's root, slash-separated.
	// It is derived in this package from a sequence number and a content
	// digest -- never from an id -- so it carries no separator of its own.
	name string
	// template names the embedded template that renders the page.
	template string
	// data is the template's data.
	data any
}

// buildPages names every page one render writes, in write order: every memory
// page, every record page, and the index last.
//
// It reads nothing and writes nothing, so its failure modes are the ones the
// naming scheme admits: two subjects of the same kind claiming one page, which
// would silently overwrite one page with another. That cannot happen for a
// ledger, whose Seq is unique, and it is refused rather than assumed because a
// lost page in an evidence artefact is not a failure a reader could notice.
func buildPages(req Request, dir string, rendered []renderedRecord) ([]pageFile, Result, error) {
	var res Result

	used := map[string]string{}
	claim := func(name, what string) error {
		if prev, taken := used[name]; taken {
			return fmt.Errorf("report: %s and %s would both be written to %s", prev, what, name)
		}
		used[name] = what
		return nil
	}

	// rendered is Seq-ordered, so the first time a memory appears is its first
	// record: the Seq its page is named from.
	memoryPages := map[string]string{}
	var memoryOrder []string
	for _, rec := range rendered {
		line := rec.line
		if line.MemoryID == "" || memoryPages[line.MemoryID] != "" {
			continue
		}
		name := memoryPageName(line.Seq, line.MemoryID)
		if err := claim(name, fmt.Sprintf("memory %q", line.MemoryID)); err != nil {
			return nil, res, err
		}
		memoryPages[line.MemoryID] = name
		memoryOrder = append(memoryOrder, line.MemoryID)
	}

	views := make([]recordView, len(rendered))
	byMemory := map[string][]recordView{}
	for i, rec := range rendered {
		line := rec.line
		name := recordPageName(line.Seq, line.ID)
		if err := claim(name, fmt.Sprintf("record %q", line.ID)); err != nil {
			return nil, res, err
		}
		views[i] = newRecordView(rec, name, memoryPages[line.MemoryID])
		byMemory[line.MemoryID] = append(byMemory[line.MemoryID], views[i])
	}

	var files []pageFile
	for _, id := range memoryOrder {
		files = append(files, pageFile{
			name:     memoryPages[id],
			template: memoryFile,
			data: memoryPage{
				Title:    "Notary report: memory " + id,
				Base:     subBase,
				Home:     subBase + "/" + indexFile,
				MemoryID: id,
				Records:  byMemory[id],
			},
		})
	}
	res.Memories = len(memoryOrder)

	for _, view := range views {
		files = append(files, pageFile{
			name:     view.Page,
			template: recordFile,
			data: recordPage{
				Title:  "Notary report: record " + string(view.Line.ID),
				Base:   subBase,
				Home:   subBase + "/" + indexFile,
				Record: view,
			},
		})
		if view.Withheld {
			res.Redacted++
		}
	}
	res.Records = len(views)

	// The index is written last, and lists the slice it belongs to. A record
	// that names no memory is listed separately rather than dropped: it has a
	// page, and a page nothing links to is a page a reader cannot find.
	var memoryRows []memoryRow
	for _, id := range memoryOrder {
		memoryRows = append(memoryRows, memoryRow{
			ID:      id,
			Page:    memoryPages[id],
			Records: len(byMemory[id]),
		})
	}
	var memoryless []recordRow
	for _, view := range views {
		if view.Line.MemoryID != "" {
			continue
		}
		memoryless = append(memoryless, recordRow{
			ID:   string(view.Line.ID),
			Page: view.Page,
			At:   view.At,
		})
	}

	files = append(files, pageFile{
		name:     indexFile,
		template: indexFile,
		data: indexPage{
			Title:       "Notary report",
			Base:        rootBase,
			Command:     req.commandLine(dir),
			RunAt:       runAtText(req.VerifiedAt),
			Subject:     req.subjectText(),
			Scope:       scopeText(req.Scope.UserID, req.Scope.AgentID, req.Scope.AppID, req.Scope.RunID),
			Sensitivity: req.sensitivityText(),
			Counts:      countsText(len(memoryOrder), len(views), res.Redacted),
			Memories:    memoryRows,
			Memoryless:  memoryless,
		},
	})
	return files, res, nil
}

// writePage renders one page and writes it. The page is rendered into memory
// first, so a template failure cannot leave a half-written page behind, and the
// target is checked to be inside dir before anything is created there.
func writePage(tmpl *template.Template, dir string, p pageFile) error {
	target, err := targetPath(dir, p.name)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, p.template, p.data); err != nil {
		return fmt.Errorf("report: render %s: %w", p.name, err)
	}
	if err := os.WriteFile(target, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("report: write %s: %w", p.name, err)
	}
	return nil
}

// targetPath joins a page's name onto dir and refuses any name that is not a
// path inside dir.
//
// Page names are derived in this package and cannot leave dir today, so this is
// a second lock on the same door: the property "nothing is written outside the
// output directory" then holds by construction rather than by every future
// naming change being careful, and Review Focus 2's path-traversal edge has a
// guard at the point of the write rather than only at the point of the name.
func targetPath(dir, name string) (string, error) {
	local := filepath.FromSlash(name)
	if name == "" || !filepath.IsLocal(local) {
		return "", fmt.Errorf("report: refusing page name %q: it is not a path inside the report", name)
	}
	return filepath.Join(dir, local), nil
}

// pageName builds a page's name -- its path relative to the report's root --
// from a sequence number and the first eight hex digits of a content digest.
//
// The id NEVER reaches the name. What is left is a fixed directory, a decimal
// sequence number, a hyphen, eight hex digits and ".html", so whatever id the
// name was derived from it can carry no separator, no ".." and no control
// character. Two ids share a name only if they collide in record.ContentHash --
// the project's existing domain-separated, length-prefixed digest, which is
// reused here rather than reinvented.
func pageName(subdir string, seq uint64, id string) string {
	digest := record.ContentHash(id)
	return subdir + "/" + strconv.FormatUint(seq, 10) + "-" + hex.EncodeToString(digest[:])[:8] + ".html"
}

// recordPageName names one record's page: its own Seq, and the digest of its
// id.
func recordPageName(seq uint64, id record.RecordID) string {
	return pageName(recordDir, seq, string(id))
}

// memoryPageName names one memory's page: the Seq of its first record in the
// slice -- the record the memory first appears in -- and the digest of the
// memory id.
func memoryPageName(firstSeq uint64, id string) string {
	return pageName(memoryDir, firstSeq, id)
}

// recordView is one record as the pages render it. Every field is taken from
// the export.Line or computed from it, so no page can describe a record
// differently from an exported line. The fields that are not Line data are the
// two page names, which are navigation rather than claims, and Evidence, which
// the line does not describe at all (spec §12.4).
type recordView struct {
	// Line is the record's exported form: the phrasing, the tier, the content
	// decision and every hash, exactly as an export would carry them.
	Line export.Line
	// Page is this record's page, relative to the report's root, and
	// MemoryPage is its memory's page, empty for a record that names no memory.
	Page       string
	MemoryPage string

	// Evidence is the evidence the record's reason carries, or nil when this
	// package can read none -- which is what makes the block absent rather than
	// an empty one on the page.
	Evidence *evidenceView

	// At and RecordedAt are Line.At and Line.RecordedAt in RFC3339Nano, the
	// rendering a range and a record's own canonical encoding use. They are
	// formatted here rather than in a template so that an instant is rendered
	// one way in this project rather than two.
	At         string
	RecordedAt string
	// ScopeText names the scope dimensions the line carries.
	ScopeText string

	// Content is the line's content, present only when the line carries any.
	Content string
	// Withheld reports that the content was withheld, with Line.Redacted
	// naming why; HasContent reports that the line carries content at all, and
	// ContentEmpty that the content it carries is empty. The three states a
	// reader must be able to tell apart -- withheld, empty, and none -- are
	// therefore distinguishable on every page that shows content.
	Withheld     bool
	HasContent   bool
	ContentEmpty bool
}

// newRecordView renders one record for the pages, with its own page's name and
// its memory's.
func newRecordView(rec renderedRecord, page, memoryPage string) recordView {
	line := rec.line
	view := recordView{
		Line:       line,
		Page:       page,
		MemoryPage: memoryPage,
		Evidence:   rec.evidence,
		At:         line.At.UTC().Format(time.RFC3339Nano),
		RecordedAt: line.RecordedAt.UTC().Format(time.RFC3339Nano),
		ScopeText: scopeText(
			line.Scope.UserID, line.Scope.AgentID, line.Scope.AppID, line.Scope.RunID),
		Withheld: line.Redacted != "",
	}
	if line.Content != nil {
		view.HasContent = true
		view.Content = *line.Content
		view.ContentEmpty = view.Content == ""
	}
	return view
}

// indexPage is index.html's data: the run, the slice, and one row per memory.
type indexPage struct {
	Title string
	// Base is the link prefix for a page at the report's root.
	Base string

	// Command is the notary report invocation this Request describes, in the
	// command's flag order. It is reconstructed from the Request -- the
	// renderer is not handed the operator's argv -- so it is the run's
	// equivalent command: flags that changed no page (--force, a database path)
	// are not echoed, and the flags that did change one (--include-sensitive,
	// --no-verify) are.
	Command string
	// RunAt is Request.VerifiedAt, or "" when the run is attributed to no
	// instant. The renderer never calls time.Now, so a page dates itself from
	// the Request or not at all.
	RunAt string
	// Subject, Scope and Sensitivity describe the slice, one phrase each.
	Subject     string
	Scope       string
	Sensitivity string
	// Counts is the summary: the counts with their nouns.
	Counts string

	// Memories is one row per memory in the slice; Memoryless is one row per
	// record that names no memory, which is why it has no memory page.
	Memories   []memoryRow
	Memoryless []recordRow
}

// memoryRow is one memory in the index's table.
type memoryRow struct {
	// ID is the memory's id, displayed -- escaped by the template engine --
	// and never used as a name.
	ID string
	// Page is the memory's page, relative to the report's root.
	Page string
	// Records is how many of the slice's records belong to this memory.
	Records int
}

// recordRow is one record the index lists outside the memory table.
type recordRow struct {
	// ID is the record's id, displayed and never used as a name.
	ID string
	// Page is the record's page, relative to the report's root.
	Page string
	// At is the record's event instant, in the pages' rendering.
	At string
}

// memoryPage is memory.html's data: one memory's lifecycle, in Seq order.
type memoryPage struct {
	Title string
	// Base is the link prefix for a page one directory down; Home is the
	// index's link, relative to this page.
	Base string
	Home string
	// MemoryID is the memory's id, displayed and never used as a name.
	MemoryID string
	// Records is the slice's records for this memory, in Seq order, each
	// linking to its own page.
	Records []recordView
}

// recordPage is record.html's data: one record's story.
type recordPage struct {
	Title string
	// Base is the link prefix for a page one directory down; Home is the
	// index's link, relative to this page.
	Base string
	Home string
	// Record is the record, as its exported line renders it.
	Record recordView
}

// subjectText names the slice: the memory, or the range.
func (r Request) subjectText() string {
	if r.MemoryID != "" {
		return "one memory: " + r.MemoryID
	}
	return fmt.Sprintf("records whose event happened between %s and %s",
		r.From.UTC().Format(time.RFC3339Nano), r.To.UTC().Format(time.RFC3339Nano))
}

// sensitivityText says what was done about sensitive content, which is a
// rendering decision and never a hash.
func (r Request) sensitivityText() string {
	if r.IncludeSensitive {
		return "included (--include-sensitive)"
	}
	return "withheld: a record marked sensitive prints no text"
}

// commandLine reconstructs the notary report invocation this Request describes,
// in the order the command's own flags are listed. Values are shown as the
// operator gave them, not shell-quoted: a page prints a command to read and
// re-run, not one this renderer executed or a shell is asked to parse.
//
// dir is the output directory the report is being written to. It is the one
// value on the index that no other page carries, which is why the index is the
// only page that differs when a report is written somewhere else.
func (r Request) commandLine(dir string) string {
	var b strings.Builder
	b.WriteString("notary report --out ")
	b.WriteString(dir)

	if r.MemoryID != "" {
		b.WriteString(" --memory ")
		b.WriteString(r.MemoryID)
	} else {
		b.WriteString(" --from ")
		b.WriteString(r.From.UTC().Format(time.RFC3339Nano))
		b.WriteString(" --to ")
		b.WriteString(r.To.UTC().Format(time.RFC3339Nano))
		for _, dim := range []struct{ flag, value string }{
			{"--user-id", r.Scope.UserID},
			{"--agent-id", r.Scope.AgentID},
			{"--app-id", r.Scope.AppID},
			{"--run-id", r.Scope.RunID},
		} {
			if dim.value == "" {
				continue
			}
			b.WriteString(" " + dim.flag + " " + dim.value)
		}
	}
	if r.IncludeSensitive {
		b.WriteString(" --include-sensitive")
	}
	if !r.Verify {
		b.WriteString(" --no-verify")
	}
	return b.String()
}

// runAtText renders the run's instant, or "" when the request carries none.
func runAtText(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	return at.UTC().Format(time.RFC3339Nano)
}

// scopeText names a caller scope, in the flags' order, showing only the
// dimensions the record actually carried: an empty dimension is omitted rather
// than printed with nothing after it, and a scope with no dimensions at all
// reads as "any scope" instead of as an empty cell a reader would have to
// interpret.
func scopeText(user, agent, app, run string) string {
	var parts []string
	for _, dim := range []struct{ name, value string }{
		{"user", user},
		{"agent", agent},
		{"app", app},
		{"run", run},
	} {
		if dim.value != "" {
			parts = append(parts, dim.name+" "+dim.value)
		}
	}
	if len(parts) == 0 {
		return "any scope"
	}
	return strings.Join(parts, ", ")
}

// countsText renders the index's summary, so a page reads as English rather
// than as "1 memories".
func countsText(memories, records, withheld int) string {
	text := plural(memories, "memory", "memories") + ", " + plural(records, "record", "records")
	if withheld > 0 {
		text += fmt.Sprintf(", %d with content withheld", withheld)
	}
	return text
}

// plural renders n with the noun's singular or plural form.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
