# `notary serve` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `notary serve`, a read-only HTTP dashboard over the existing ledger, bound to loopback, that renders the records view, the per-record and per-memory explain views, the gaps view and the chain-verify status — thin enough over the existing read paths that it cannot disagree with the CLI.

**Architecture:** One new package (`internal/serve`) that owns the HTTP surface and the view models, rendering **every record from `export.Line`** (produced by `export.Render`, which owns the tier label and the redaction decision) and reading through `ledger`; one new command (`cmd/notary/serve.go`); and one extraction ahead of everything, so the gap report has a single definition the CLI and the dashboard share.

**Tech Stack:** Go 1.25, Cobra, `net/http`, `html/template` and `go:embed` (standard library), `modernc.org/sqlite`, testify. htmx 2.0.7 vendored as an asset — **no new Go module**.

**Spec:** `docs/superpowers/specs/2026-10-06-notary-serve-design.md`

## Global Constraints

- Go 1.25; **no new dependencies and no change to `go.mod`**. `net/http`, `html/template` and `go:embed` are stdlib; htmx is a vendored asset, not a module.
- Never panic in library code; wrap errors `fmt.Errorf("...: %w", err)`; exported names get doc comments; no global mutable state (templates live on `*Server`, built once in `New`).
- **`html/template` autoescaping is never turned off.** No value is ever `template.HTML`, `template.JS` or `template.URL`, and no stored string is ever concatenated into markup. Memory text, memory ids, gap details and phrasing sentences are all untrusted.
- **Redaction is presentation** (`internal/export`'s `redactedSensitive`, `line.go:91`): `export.Render(rec, false)` withholds, `true` reveals, and **no hash differs between the two renders**.
- **No writes.** The ledger is built with a **nil signer** (`ledger.New(st, nil, nil)`), so `Append` refuses; no handler calls `Append`, `gap.Open`, or reconcile. Every route is GET or HEAD.
- **No reimplementation of tiering, redaction or phrasing** — they come from `record`, `export.Render` and `export.Phrase`. **No second definition of "intact"** (`ledger.CollectBreaks`) or of "outstanding gaps" (Task 1's `OutstandingGaps`).
- **No import of `internal/phrase`** (D10). No paraphrase anywhere.
- **No key material in any page**, with `export`'s canary discipline. The private key never loads here.
- Bind is **`127.0.0.1` only**; the only flag is `--port` (default `4317`).
- `gofmt -l .` empty and `go vet ./...` clean before each commit; the suite runs `-p 1` (`go test -p 1 ./... -count=1`).
- **`cmd/notary/root_test.go` pins the subcommand list** (`rootSubcommands`, both directions). Task 6 extends it, and the test failing first is the expected red.
- Exact values from the spec: default window **30 days**; cap `export.DefaultMaxSpan`; params `from`, `to`, `user_id`, `agent_id`, `app_id`, `run_id`, `reveal`; tier classes `tier-observed` / `tier-reconstructed` / `tier-internal`; colours `#D9A441`, `#8593A5`, `#47505E`, `#707B8A` over `#0A0D12`/`#141A23` with text `#EEF1F5` and muted `#98A2B3`; **htmx 2.0.7**, 51,076 bytes, SHA-256 `60231ae6ba9db3825eb15a261122d5f55921c4d53b66bf637dc18b4ee27c79f9`.

## Review Focus

Inputs and conditions the spec implies but no task's own tests necessarily cover. Each gets its test in the task named.

1. **A record whose subject names no memory.** The store's `memory_id` column defaults to `''`, so such records exist. The records table and the record page must render **no memory link** for them, and `/memory?id=` with an empty value must **refuse**, not list the whole ledger wearing one memory's clothes — the footgun `explain` refuses. (Task 2, Task 5)
2. **A hostile id in every URL position.** Record ids carry `/`, `#`, `:` and can carry `..`, spaces, control characters and non-ASCII. Every rendered `href` and form value must be escaped, `..` must never become a path (there are no paths or filenames in this phase), and the id must round-trip through the server. (Task 4, Task 5)
3. **Markup and newlines arriving as data.** A memory's content, a memory id and a gap's `Detail` can all contain `<script>`, `</textarea>`, `</title>` and newlines. Each must render as text in the page's HTML *and* be correct when htmx swaps a fragment in. (Task 5)
4. **An empty range, an empty ledger, and a bad filter.** A range with no records, a ledger with no records at all, a reversed range, and a span over the cap must each say so in words and never look like a broken invocation or like redaction. (Task 4, Task 5)
5. **A non-loopback `Host` and a non-GET method.** A DNS-rebinding request (`Host: evil.example`) and any `POST`/`PUT`/`DELETE` must be refused **without leaking any ledger content**, and the 405 must carry no body. (Task 4)

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/ledger/gapreport.go` | The one definition of "what is outstanding" — the extraction of `runGaps`'s orchestration |
| `internal/serve/server.go` | `Options`, `Server`, `New`, the loopback listener and graceful shutdown |
| `internal/serve/views.go` | The `export.Line` projections: rows, tier badge, the filter, `loadRecords`/`loadRecord`/`loadMemory` |
| `internal/serve/status.go` | `loadGaps` (via `ledger.OutstandingGaps`) and `loadChain` (the three states via `ledger.CollectBreaks`) |
| `internal/serve/routes.go` | `Handler()`: the fixed route table, the method guard and the `Host` guard |
| `internal/serve/render.go` | Per-page template sets built once in `New`, executed per request |
| `internal/serve/embed.go` | `go:embed` for `templates/` and `assets/` |
| `internal/serve/templates/*.html` | `layout.html`, `partials.html`, `index.html`, `record.html`, `memory.html`, `gaps.html`, `verify.html` |
| `internal/serve/assets/` | `app.css`, `htmx.min.js`, `htmx.LICENSE` |
| `cmd/notary/serve.go` | `newServeCmd`, `runServe` |
| `cmd/notary/gaps.go` | Refactored to call `ledger.OutstandingGaps` |
| `cmd/notary/root.go`, `root_test.go` | Register `serve`; extend the pinned `rootSubcommands` |

**Template set shape (decided, to avoid a parse collision):** `layout.html` defines `layout` and calls `{{template "page" .}}`; `partials.html` defines the shared partials; each page file defines exactly **`page`**. `New` builds one set per page from `layout.html` + `partials.html` + that page's file, and `render` executes `layout`. Parsing all pages into one set would collide on the single `page` name.

---

### Task 1: The shared gap report

**Files:**
- Create: `internal/ledger/gapreport.go`
- Test: `internal/ledger/gapreport_test.go`
- Modify: `cmd/notary/gaps.go`
- Guard: `cmd/notary/gaps_test.go` (existing — **must keep passing unchanged**)

**Interfaces:**
- Consumes: `gap.Read`, `gap.Verify`, `store.Store.SeqEntries`, `ledger.GapBreaks`.
- Produces:
  - `type GapReport struct { Unreconciled []gap.Entry; Integrity []gap.Break }`
  - `func OutstandingGaps(st store.Store, gapPath string) (GapReport, error)`

**Notes the implementer needs:** this is spec §3 record 4 — the explicit `.clinerules` §5 ask, recorded in the spec and approved. The three stages to move are `runGaps`'s: read the log (`gap.Read`), load the decodable stored records (`st.SeqEntries`, keeping only `DecodeErr == nil`), decide the unreconciled set with `ledger.GapBreaks` (**the same function `verify` uses — never re-derive the match**), and `gap.Verify` for the log's own integrity. Preserve each stage's **error-wrapping text verbatim** from `runGaps`: `"reading gap log %s: %w"`, `"reading ledger records for gap check: %w"`, `"verifying gap log %s: %w"`. Read the store **only when the log has entries** — `runGaps` does, and an empty log must not touch the store. Join the `[]ledger.Break` back to entries by `Counter == Break.Seq` to produce `Unreconciled []gap.Entry`; `GapBreaks` emits one break per gap entry keyed by that entry's `Counter`, so the join always hits and `runGaps`'s unreachable fallback branch goes away with it. `OutstandingGaps` prints nothing and decides no policy — it returns the report, and each caller decides what to do with it, exactly as `CollectBreaks` does.

- [ ] **Step 1: Write the failing tests**

In `internal/ledger/gapreport_test.go`, over fixtures built the way this package's own tests build them (`ledger_test.go`'s `newStore`/`newLedger`; for a gap log use `gap.Open` on a path in the same temp dir):

- `TestOutstandingGapsIsEmptyOnACleanLedger` — no entries, no integrity breaks, no error.
- `TestOutstandingGapsIgnoresAMatchingEntry` — a gap entry whose `(Kind, Scope, CorrelationID)` a stored record accounts for produces no `Unreconciled` entry.
- **`TestOutstandingGapsReportsAnUnmatchedEntry`** — **the load-bearing one (Review Focus 1 of the spec's §11)**: a ledger whose records are all intact and whose gap log holds an entry naming a correlation id no record accounts for. Assert `Unreconciled` has exactly that entry, with its `Counter` and `CorrelationID`, **and** that `l.Verify(v)` alone returns no breaks — both, so the test fails if the extraction ever narrows to the chain walk.
- `TestOutstandingGapsReportsACorruptLog` — a log with a rewritten line yields a non-empty `Integrity`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/ledger/ -run TestOutstandingGaps -count=1`
Expected: FAIL — `undefined: OutstandingGaps`.

- [ ] **Step 3: Implement `OutstandingGaps` and `GapReport` in `internal/ledger/gapreport.go`**

Move the stages, keeping their order, their error wrapping and the comments that explain why the gap cross-check exists (`gaps.go` and `verify_test.go` carry that reasoning; keep it with the code that does it).

- [ ] **Step 4: Refactor `runGaps` to call it**

`runGaps` keeps opening the store, calling `OutstandingGaps`, and printing; it loses only the orchestration. Its printed output and its exit codes must be **byte-identical** — `cmd/notary/gaps_test.go` asserts them and must not be edited.

- [ ] **Step 5: Run the package and the command's tests**

Run: `go test -p 1 ./internal/ledger/ ./cmd/notary/ -count=1`
Expected: PASS, with `gaps_test.go` unmodified.

- [ ] **Step 6: Commit**

```bash
git add internal/ledger/gapreport.go internal/ledger/gapreport_test.go cmd/notary/gaps.go
git commit -m "refactor(ledger): one definition of what is outstanding, for gaps and the console"
```

---

### Task 2: `internal/serve` — the view layer, part 1: the `export.Line` projections

**Files:**
- Create: `internal/serve/server.go`, `internal/serve/views.go`
- Test: `internal/serve/views_test.go`

**Interfaces:**
- Consumes: `ledger.Ledger` reads (`ListRecords`, `GetRecord`, `ListRecordsByMemory`), `export.Render`, `export.ScopeMatches`, `export.DefaultMaxSpan`, `record.Scope`, `record.VisibilityTier`, `store.ErrNotFound`.
- Produces:
  - `type Options struct { Ledger *ledger.Ledger; Store store.Store; GapLogPath string; Verifier *sign.Verifier; KeyringPath string; Reveal io.Writer; Now func() time.Time }` and `func New(opts Options) *Server` **[Superseded:** `Options` no longer carries `Verifier *sign.Verifier`; it carries `Keyring map[string]ed25519.PublicKey` (with `KeyringPath` kept separately, display-only), and `serve.New` derives the verifier itself, and only when `len(Keyring) > 0`, so an empty keyring means the chain check does not run rather than accusing a healthy ledger.**]**
  - `type tierBadge struct { Name, Class string }` and `func badgeFor(t record.VisibilityTier) tierBadge`
  - `type recordRow struct { Line export.Line; Tier tierBadge; RecordHref, MemoryHref string }`
  - `type filter struct { From, To time.Time; Scope record.Scope; Reveal bool }`
  - `func (s *Server) parseFilter(q url.Values) (filter, error)`
  - `func (s *Server) loadRecords(f filter) ([]recordRow, error)` / `loadRecord(id record.RecordID, reveal bool) (recordRow, error)` / `loadMemory(id string, reveal bool) ([]recordRow, error)`
  - `var ErrMissingID = errors.New(...)` — a usage error the HTTP layer maps to 400; `store.ErrNotFound` maps to 404.

**Notes the implementer needs:** `New` normalises `opts.Now` to `time.Now` when nil and `opts.Reveal` to `io.Discard` when nil. **Every row is `export.Render(rec, reveal)`** — the tier label, the redaction decision and the phrasing sentence all come from there, so a viewer can never drift from `notary export`. `loadRecords` filters with `export.ScopeMatches(f.Scope, rec.Subject.Scope)` **after** the range read, exactly as `Exporter.Export` narrows, so the two agree. `badgeFor` maps `Observed`→`{"Observed", "tier-observed"}`, `Reconstructed`→`{"Reconstructed", "tier-reconstructed"}`, `Internal`→`{"Internal", "tier-internal"}` and **must not silently accept an invalid tier**: `export.Render` already rejects one, so an invalid tier never reaches `badgeFor`; make that the documented precondition rather than a fourth case. `MemoryHref` is `""` when `rec.Subject.MemoryID == ""` (Review Focus 1). `RecordHref` is built as a path plus a query value, e.g. `/record` + `?id=` + the id, with the id **kept raw** — `html/template` encodes it (spec §6); do not escape it here, or it is escaped twice. **[Superseded in Task 5:** this raw-id rule was wrong, and the `RecordHref`/`MemoryHref` fields were removed. `html/template`'s contextual escaper URL-encodes only its fixed set of URL attributes (`href`, `action`, `src`, …) and merely HTML-escapes a custom attribute such as `hx-get`, so a pre-joined URL interpolated into `hx-get` is **not** URL-escaped — a raw id there leaves `/`, `#` and `:` unescaped and starts a fragment. The shipped rule is the one Task 5's notes state: the id is `url.QueryEscape`d exactly once in Go, and the same string is handed to `href` and `hx-get` alike.**]** `loadMemory` and `loadRecord` return `ErrMissingID` for an empty id, before reading. `parseFilter`: `from`/`to` accept RFC3339 or a bare `2006-01-02`; a date-only `from` is that day `00:00:00Z` and a date-only `to` is the **end of that day** (`date.AddDate(0,0,1).Add(-time.Nanosecond)`), because the store's `ListRecords` is inclusive at both bounds; defaults are `from = s.now − 30*24h`, `to = s.now`; a reversed range is an error naming both bounds; a span over `export.DefaultMaxSpan` is an error naming the cap, **the same cap `export` applies**. Reveal is `q.Get("reveal") == "1"`.

- [ ] **Step 1: Write the failing tests**

In `internal/serve/views_test.go`, over a fixture built like `cmd/notary/verify_test.go`'s: a `store.Open(filepath.Join(t.TempDir(), "ledger.db"))`, a `*sign.Signer` from a fixed 32-byte seed through `sign.KeySourceEnv` (copy `newVerifySigner`'s shape, including `ed25519.NewKeyFromSeed(seed).Public()` for the keyring), a `ledger.New(st, sg, fixedClock)`, and appends of both content-carrying and content-free records (`record.NewObservedEvidence` → `record.NewObservedReason` → `record.Content{Text, Sensitive}`).

- `TestLoadRecordsMatchesTheExportPath` — **the differential (Task 4's Review Focus)**: for the same range and scope, `s.loadRecords(f)`'s `Line`s equal, field for field, the `export.Line`s that `export.New(l).Export(ctx, export.Request{From: f.From, To: f.To, Scope: f.Scope, IncludeSensitive: f.Reveal}, &buf)` writes, parsed as JSONL.
- `TestLoadRecordsPreservesLedgerOrder` — rows are in the ledger's order.
- `TestLoadRecordRefusesAnEmptyID` and `TestLoadMemoryRefusesAnEmptyID` — `ErrMissingID`.
- `TestLoadRecordReportsAMissingIDAsNotFound` — `errors.Is(err, store.ErrNotFound)`.
- `TestBadgeForNamesEachTier` — the three names and the three classes, exactly.
- `TestMemoryHrefIsEmptyWhenTheRecordNamesNoMemory` — a record appended with `Subject.MemoryID: ""` yields `MemoryHref == ""`.
- `TestLoadRecordAndMemoryRedactByDefaultAndRevealOnRequest` — a sensitive record's `Line.Redacted` is `"sensitive"` and `Line.Content` is nil by default; `Content` is present with `reveal`; **and no hash field differs between the two** (compare `PrevHash`, `Hash`, `Signature` across the two calls).
- `TestParseFilterDefaultsToTheLastThirtyDays`; `TestParseFilterAcceptsDateOnlyBoundsAtDayEdges` (a date-only `to` includes a record at `23:59:59Z` that day); `TestParseFilterRejectsAReversedRange`; `TestParseFilterRejectsASpanOverTheCap` naming `export.DefaultMaxSpan`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/serve/ -count=1`
Expected: FAIL — no such package / `undefined: New`.

- [ ] **Step 3: Implement `Options`, `Server`, `New` and the views**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./internal/serve/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/serve/server.go internal/serve/views.go internal/serve/views_test.go
git commit -m "feat(serve): the ledger as export.Line, the one shape every view renders"
```

---

### Task 3: `internal/serve` — the view layer, part 2: the gap report and the chain state

**Files:**
- Create: `internal/serve/status.go`
- Test: `internal/serve/status_test.go`

**Interfaces:**
- Consumes: `ledger.OutstandingGaps` and `ledger.GapReport` (Task 1), `ledger.CollectBreaks`, `gap.Entry`, `gap.Break`, `ledger.Break`, `sign.Verifier`.
- Produces:
  - `type gapsView struct { Unreconciled []gap.Entry; Integrity []gap.Break; OK bool }` and `func (s *Server) loadGaps() (gapsView, error)`
  - `type chainState int` with `chainNotVerified`, `chainClean`, `chainBroken`
  - `type chainView struct { State chainState; AsOf time.Time; Breaks []ledger.Break; KeyringPath string }` and `func (s *Server) loadChain() (chainView, error)`

**Notes the implementer needs:** `loadGaps` calls `ledger.OutstandingGaps(s.store, s.gapLogPath)` and sets `OK` when both lists are empty. `loadChain` has **exactly three states and they must never collapse**: when `s.verifier == nil` it returns `chainNotVerified` with `AsOf = s.now()` and **no** break list — it does not run a check, because `sign.NewVerifier(nil)` would report every record as a signature break and accusing a healthy ledger is the worst output this tool could produce. With a verifier it runs `ledger.CollectBreaks(s.ledger, s.store, s.gapLogPath, s.verifier)` and returns `chainClean` on no breaks, `chainBroken` with the breaks otherwise. `AsOf` is the instant the check ran, so the banner is never stale. `KeyringPath` is carried so the "not verified" state can name the fix. `loadChain` is called per request — the banner is fresh, never a snapshot — and its cost (a whole-chain walk per page view) is a recorded limitation, not a surprise.

- [ ] **Step 1: Write the failing tests**

In `internal/serve/status_test.go`, reusing Task 2's fixture builder:

- `TestLoadGapsMatchesTheSharedReport` — over three fixtures: intact, an entry matching no record, and a corrupt log, `s.loadGaps()` equals `ledger.OutstandingGaps(st, gapPath)` field for field, and `OK` is true only for the intact one.
- **`TestChainStateDistinguishesTheThreeStates`** — three servers: no verifier → `chainNotVerified` with no breaks; a verifier over an intact fixture → `chainClean`; a verifier over a fixture with an edited record (tamper the row directly, as `testdata/tamper/README.md` documents) → `chainBroken` naming the record id and field.
- `TestChainStateIsNeverCleanWithoutAKeyring` — the no-verifier state's `Breaks` is empty **and** `State != chainClean`, so "could not verify" can never render as "verified".

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/serve/ -run 'TestLoadGaps|TestChain' -count=1`
Expected: FAIL — `undefined: loadGaps`.

- [ ] **Step 3: Implement `loadGaps` and `loadChain` in `internal/serve/status.go`**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./internal/serve/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/serve/status.go internal/serve/status_test.go
git commit -m "feat(serve): the gap report and the chain state, each through the one shared definition"
```

---

### Task 4: The server, the records view, and the assets

**Files:**
- Create: `internal/serve/routes.go`, `internal/serve/render.go`, `internal/serve/embed.go`
- Create: `internal/serve/templates/layout.html`, `internal/serve/templates/partials.html`, `internal/serve/templates/index.html`
- Create: `internal/serve/assets/app.css`, `internal/serve/assets/htmx.min.js`, `internal/serve/assets/htmx.LICENSE`
- Test: `internal/serve/routes_test.go`

**Interfaces:**
- Consumes: Task 2's `Server`, `Options`, `filter`, `loadRecords`, `parseFilter`, `ErrMissingID`; Task 3's `loadChain`, `chainView`.
- Produces:
  - `func (s *Server) Handler() http.Handler`
  - `func (s *Server) Listen(port int) (net.Listener, error)`
  - `func (s *Server) Serve(ctx context.Context, ln net.Listener) error`
  - `func (s *Server) render(w io.Writer, page string, data any) error` (unexported is fine; name it consistently)

**Notes the implementer needs:** fetch htmx once with `curl -fsS https://unpkg.com/htmx.org@2.0.7/dist/htmx.min.js -o internal/serve/assets/htmx.min.js`, then confirm `sha256sum` is `60231ae6ba9db3825eb15a261122d5f55921c4d53b66bf637dc18b4ee27c79f9` and `wc -c` is `51076`; fetch its licence to `htmx.LICENSE`; add a header comment file or a `//go:embed` comment recording the version, digest and "Zero-Clause BSD (0BSD)". **The binary's bytes are unmodified.** `Handler` builds one `http.ServeMux` with exactly `/`, `/record`, `/memory`, `/gaps`, `/verify` and `/assets/`, wrapped by two middlewares **in this order**: the method guard (anything but GET/HEAD → `405` with `w.WriteHeader(405)` and no body) then the `Host` guard (a `Host` whose host part is not `127.0.0.1` or `localhost` → `403`, no body). `Access-Control-Allow-Origin` is **never** set anywhere. `/assets/` serves only the embedded files, by exact name, `Cache-Control: no-store`. `Listen` is `net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))` — a literal loopback host, never `":port"` — and a failure wraps the port and the likely cause. `Serve` runs an `http.Server` and, on `ctx.Done()`, calls `Shutdown` with a bounded timeout, returning nil for a clean shutdown. `render` writes to a `bytes.Buffer` first and only then to `w`, so a template failure cannot half-write a page. `New` builds the per-page template sets (see File Structure) and returns an error if a template does not parse; `Server` keeps them in a field, never in a package variable.

For the records view payload, pass a struct carrying `Chain chainView`, `Filter filter`, `Rows []recordRow`, `Err string` (the filter's usage error, so a 400 renders the page with a banner rather than a bare error) and the `Now` used, so the page can show the range it searched. The filter form is an ordinary GET form to `/` with the `from`, `to` and four scope inputs, enhanced with `hx-get="/" hx-target="#records" hx-select="#records" hx-swap="outerHTML"`. The chain banner is on every page via the shared partial and reads `As of <instant>, this tool's verification reported: …` with the re-run command beside it.

- [ ] **Step 1: Write the failing tests**

In `internal/serve/routes_test.go`, driving `s.Handler()` through `httptest.NewRecorder` and `httptest.NewRequest` over Task 2's fixture (add a helper that returns a `*Server` plus the fixture's `*store.SQLiteStore`, temp dir, and public key):

- `TestEveryRouteAnswers` — `GET /`, `GET /assets/app.css`, `GET /assets/htmx.min.js` are `200`.
- `TestRecordsViewMatchesTheExportPath` — **the differential**: parse the table's rows out of the rendered page and compare their ids, seqs, tiers and withheld/shown content against the JSONL `export` writes for the same range and scope. Parse the JSONL, not the HTML, for the expected side; assert the same ids appear in the same order.
- `TestRecordsViewShowsTheTierBadgeForEachTier` — a fixture with all three tiers renders `Observed`, `Reconstructed` and `Internal` as text and the matching `tier-observed` / `tier-reconstructed` / `tier-internal` classes.
- **`TestRecordsViewEscapesAHostileID`** (Review Focus 2) — a record whose id is `a/b#c d:e..f` renders in the row's link with `/`, `#`, `:` and space percent-encoded and **no** `../` path segment; and a follow-up `GET` of the rendered `href` finds that record (round-trip).
- **`TestNonGETMethodsAreRefusedWithNoBody`** (Review Focus 5) — `POST`, `PUT`, `DELETE`, `PATCH` on `/` are each `405` with an empty body.
- **`TestANonLoopbackHostIsRefused`** (Review Focus 5) — `Host: evil.example` is `403` with an empty body and body contains no record id.
- `TestNoResponseCarriesACORSHeader` — no response has `Access-Control-Allow-Origin`.
- `TestPagesAreOffline` — no rendered page contains `http://` or `https://` except the `http://127.0.0.1:<port>` the banner names as the re-run command (assert on the *host set* present, so a CDN URL fails the test).
- **`TestAnEmptyRangeSaysSoInWords`** (Review Focus 4) — a range with no records renders a sentence naming the range and how to widen it, and is not an empty table with no explanation. Also `TestAnEmptyLedgerRendersWithoutError`.
- **`TestABadFilterRendersTheViewWithABanner`** (Review Focus 4) — a reversed range and an over-cap span each render `400` with the page and a message naming the problem; the page still carries the chain banner.
- `TestListenBindsLoopbackOnly` and `TestListenReportsTheChosenPortForPortZero` — `s.Listen(0)`, assert `ln.Addr().(*net.TCPAddr).IP.IsLoopback()`.
- **`TestNoRouteWrites`** (Global Constraints) — build the ledger over a wrapper that embeds `*store.SQLiteStore` and fails the test from `PutRecord` and `AppendChained`; exercise every route; assert neither was called **and** that the ledger file's SHA-256 and the gap log's bytes are unchanged.
- `TestServeShutsDownOnContextCancel` — `Serve` in a goroutine with a cancellable context returns nil promptly after cancel.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/serve/ -run 'TestEveryRoute|TestRecords|TestNonGET|TestANonLoopback|TestPagesAreOffline|TestListen' -count=1`
Expected: FAIL — `undefined: Handler`.

- [ ] **Step 3: Implement `Handler`, `Listen`, `Serve`, `render`, the embed, and the records view + assets**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test ./internal/serve/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/serve/ && git commit -m "feat(serve): loopback-only server, GET-only routes, and the records view"
```

---

### Task 5: The remaining pages, and the reveal toggle

**Files:**
- Create: `internal/serve/templates/record.html`, `memory.html`, `gaps.html`, `verify.html`
- Modify: `internal/serve/templates/partials.html` (the reveal control), `internal/serve/routes.go` (the four handlers)
- Test: `internal/serve/routes_test.go` (additions)

**Interfaces:**
- Consumes: Task 2's `loadRecord`/`loadMemory`, Task 3's `loadGaps`/`loadChain`, Task 4's `render` and middlewares.
- Produces: the `/record`, `/memory`, `/gaps` and `/verify` handlers; `render` gains no new exported surface.

**Notes the implementer needs:** `/record` renders the phrased sentence, the event, the reason kind, the tier badge, `At` and `RecordedAt`, and the content or *"Content withheld — marked sensitive"* or *"No content recorded"*; the chain fields (`prev_hash`, `hash`, `signature`, `signer_key_id`) go in a collapsed `<details>` with a line saying the page verifies nothing by itself and naming `notary verify`. `/memory` renders one row per record in `Seq` order, the same claims `explain --memory` prints, plus a link back to the records view. `/gaps` renders each unreconciled entry's kind, scope, correlation id, `At` and `Detail`, then the integrity breaks, and says *"No outstanding gaps"* when there are none. `/verify` renders the full break list, or the clean state with the record count, or the not-verified state with the fix. **The reveal control is a real `<a>`** whose `href` carries `reveal=1` and which htmx enhances in place; with JavaScript off it is an ordinary navigation, and the console stays fully usable. Reveal is logged with `fmt.Fprintf(s.reveal, "serve: revealed sensitive content for %s\n", view)` — one line per revealing request, to the `Options.Reveal` writer (stderr in the command). Every `href` and every `hx-get` carrying an id hands the template the SAME `url.QueryEscape`d string, escaped **exactly once in Go** by `recordHref`/`memoryHref` — because `html/template`'s contextual escaper URL-encodes only its fixed set of URL attributes (`href`, `action`, `src`, …) and merely HTML-escapes a custom attribute such as `hx-get`, so a raw id there would leave `/`, `#` and `:` unescaped and start a fragment (spec §6) — **never** `urlquery` in the template, never concatenation.

- [ ] **Step 1: Write the failing tests**

Add to `internal/serve/routes_test.go`:

- `TestRecordViewShowsTheClaimAndTheChainFields` — the page carries the `export.Phrase` sentence for that record, its tier name, its `hash`, and the `notary verify` re-run line.
- `TestMemoryViewMatchesTheExplainPath` — **the differential**: for one memory, the page's rows carry the same sentences, tiers, and withheld-or-shown decisions that `notary explain --memory <id> --json` reports (parse both; compare the shared fields).
- `TestGapsViewMatchesTheSharedReport` — the page shows every unreconciled entry's correlation id and detail, and *"No outstanding gaps"* on the intact fixture.
- `TestVerifyViewRendersThreeDistinctStates` — clean, broken (naming the record id and field), and not-verified (naming the keyring fix), each rendered differently.
- **`TestStoredTextCannotBecomeMarkup`** (Review Focus 3) — `<script>`, `</textarea>` and a newline placed in a memory's content, in a memory id and in a gap entry's `Detail` render escaped on every page that shows them, and the same holds for the htmx fragment swap (assert on the fragment response, not only the full page).
- **`TestMemoryViewRefusesAnEmptyID`** (Review Focus 1) — `/memory?id=` is `400` and its body lists no record ids.
- **`TestRevealIsPerViewAndLogged`** (spec §3 record 3) — `/record?id=X` is redacted; `/record?id=X&reveal=1` shows the content and writes exactly one line to the `Reveal` writer naming the view; a `/memory` page's links to records carry **no** `reveal`, so following one re-redacts.
- `TestRevealChangesNoHash` — the `hash` and `signature` rendered on the page are identical with and without `reveal=1`.
- `TestNoKeyMaterialInAnyPage` — the fixture's signing key bytes and the base64 key never appear in any rendered page.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/serve/ -run 'TestRecordView|TestMemoryView|TestGapsView|TestVerifyView|TestReveal' -count=1`
Expected: FAIL — `404`/empty body from the unregistered routes.

- [ ] **Step 3: Implement the four handlers and their templates, and the reveal control**

- [ ] **Step 4: Run them to verify they pass**

Run: `go test -p 1 ./internal/serve/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/serve/ && git commit -m "feat(serve): the record, memory, gaps and verify views, and a deliberate reveal"
```

---

### Task 6: The `notary serve` command, and the docs

**Files:**
- Create: `cmd/notary/serve.go`, `cmd/notary/serve_test.go`
- Modify: `cmd/notary/root.go`, `cmd/notary/root_test.go`
- Modify: `docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md` (§9, §13, §14), `README.md`, `.clinerules`

**Interfaces:**
- Consumes: `serve.New`, `serve.Options`, `Server.Listen`, `Server.Serve`; `config.Config` (`DBPath`, `GapLogPath`, `TrustedKeysPath`); `sign.LoadTrustedKeys`, `sign.NewVerifier`; `store.Open`, `ledger.New`. **[Superseded:** the command no longer calls `sign.NewVerifier`; it passes `serve.Options.Keyring` (a `map[string]ed25519.PublicKey`) and `serve.New` derives the verifier itself, only when `len(Keyring) > 0`.**]**
- Produces: `func newServeCmd() *cobra.Command` and `func runServe(cmd *cobra.Command, cfg *config.Config) error`.

**Notes the implementer needs:** `--port` is the **only** flag, `Int`, default `4317`. No `--host`, no `--addr`, no `--include-sensitive`. `runServe` opens the store, builds `ledger.New(st, nil, nil)` (**nil signer — the structural read-only guarantee**), loads the keyring only when `cfg.TrustedKeysPath != ""` (a load failure is a plain-language error naming the path; an empty keyring means the check is not run, never a failure), builds `serve.Options` with `Reveal: cmd.ErrOrStderr()` and `KeyringPath: cfg.TrustedKeysPath`, then `Listen(port)`, prints

```
notary serve: dashboard on http://127.0.0.1:<port> (read-only; Ctrl-C to stop)
```

to **stderr** (stdout stays empty), and `Serve`s until `cmd.Context()` is done. `cmd.Context()` is nil for a command not run through `Execute`, so fall back to `signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)`. **[Superseded:** the shipped code wraps the context with `signal.NotifyContext` **unconditionally**, not only when `cmd.Context()` is nil: under `Execute` cobra supplies a plain `context.Background()`, so a nil check would never fire and SIGINT would be left to the runtime default (a hard kill). `runServe` falls back to `context.Background()` only when `cmd.Context()` is nil, then always wraps the result, so a signal is always a clean stop.**]** Register the command in `newRootCmd` and add `"serve"` to `rootSubcommands` in `root_test.go` — the test failing first is the expected red.

- [ ] **Step 1: Write the failing tests**

In `cmd/notary/serve_test.go`:

- `TestServeFlagsAreExactlyPort` — walk `cmd.Flags().VisitAll` and assert the set is exactly `{port}`, so a later `--host` or `--include-sensitive` cannot be added unnoticed.
- `TestServePortDefaultsTo4317` and `TestServeRejectsAPortThatIsNotANumber`.
- `TestServeCmdIsRegistered` — already covered by `root_test.go` once it is updated; assert it there.
- `TestServeCmdStartsTheDashboardOnALoopbackPort` — capture stderr in a buffer, run `runServe` in a goroutine on `--port 0` with a cancellable context, poll the buffer for the printed URL, `http.Get` it and assert `200` and a body containing `Observed`, then cancel and assert `runServe` returns nil. This pins the startup line's format and the loopback address.
- `TestServeCmdRefusesAnOccupiedPort` — occupy a port, point `--port` at it, and assert the error names the port and the likely cause.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cmd/notary/ -run TestServe -count=1`
Expected: FAIL — `undefined: newServeCmd`.

- [ ] **Step 3: Implement `newServeCmd`, `runServe`, the registration, and `rootSubcommands`**

- [ ] **Step 4: Run the command tests and the pinned list**

Run: `go test ./cmd/notary/ -count=1`
Expected: PASS.

- [ ] **Step 5: Update the docs**

`docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md`: §9's read-path table gains a `notary serve` row ("the whole ledger, live, read-only, over loopback" / answers "show me, in a browser, and let me set the range"); §13's phase table gains row 12; §14's delta table gains `internal/serve`. `README.md`: a `notary serve` bullet beside the other commands, noting it is read-only, loopback-only, and needs no key. `.clinerules`: the folder tree gains `internal/serve`.

- [ ] **Step 6: Run the whole gate**

Run: `gofmt -l . && go vet ./... && go test -p 1 ./... -count=1 && go test -p 1 -race ./internal/store/ ./internal/ledger/ ./internal/serve/ -count=1`
Expected: `gofmt` prints nothing, `vet` is silent, all tests PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/notary/serve.go cmd/notary/serve_test.go cmd/notary/root.go cmd/notary/root_test.go \
        docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md README.md .clinerules
git commit -m "feat(cmd): notary serve, the console a compliance lead can open"
```

---

## Self-Review

**Spec coverage.** §1–§2 context — no code owed. §3 record 1 (standalone) — no shared renderer is built; Task 2's `export.Line` projection is the data-level sharing. §3 record 2 (verify posture) — Task 3's three-state `loadChain` + Task 5's `TestVerifyViewRendersThreeDistinctStates`. §3 record 3 (reveal) — Task 5's `TestRevealIsPerViewAndLogged` + `TestRevealChangesNoHash`. §3 record 4 (extraction) — Task 1. §3 records 5–7 (query ids, Host check, 30-day default with the cap) — Task 2's `parseFilter`/hrefs, Task 4's guards. §4 (the command) — Task 6. §5 (routes and pages) — Tasks 4–5. §6 (rendering, redaction, reveal, escaping, styling) — Tasks 2, 4, 5. §7 (assets) — Task 4, with the digest asserted. §8 (read-only, GET-only, no CORS, no auth) — Task 4's `TestNoRouteWrites`, method and Host guards. §9 (must not do) — Global Constraints. §10 (limitations) — recorded in the spec; no code. §11 (testing) — mapped onto the tasks above. §12 (sequencing) — the task order.

**Type consistency.** `OutstandingGaps`/`GapReport` are defined once in Task 1 and used in Tasks 3 and 6's docs. `filter`, `recordRow`, `tierBadge`, `loadRecords`/`loadRecord`/`loadMemory` are defined in Task 2 and consumed unchanged in Tasks 3–5. `gapsView` and `chainView`/`chainState` are defined in Task 3 and consumed in Task 5. `Options` is defined in Task 2 and constructed in Task 6. `ErrMissingID` is defined in Task 2 and mapped to 400 in Tasks 4–5.

**Review Focus.** Each of the five lines names the task whose tests pin it: 1 → Tasks 2 and 5; 2 → Tasks 4 and 5; 3 → Task 5; 4 → Tasks 4 and 5; 5 → Task 4.
