# `notary serve` — the Overview page: Implementation Plan

> **For an implementer:** checkboxes track progress. One task per commit, each green before the next.

**Goal:** Make the console's front page answer "can I trust this trail?" for a reviewer who is not an
engineer — a three-state verdict, three ledger facts, a gap count, and a first-run explanation — with the
records view moved to `/records`.

**Architecture:** One new page template and one new view model (`overview.go`), fed by the verdict the
console already computes (`loadChain` → `ledger.CollectBreaks`) and the gap report it already reads
(`ledger.OutstandingGaps`). The only new read is a single store aggregate for the ledger facts, which
`extent()` then delegates to so the console keeps one definition of "how many records and what span". The
nav becomes a list in one place and the routes move beneath it.

**Tech Stack:** Go 1.25, `net/http`, `html/template`, `go:embed`, `modernc.org/sqlite`, testify. **No new
Go module and no `go.mod` change.**

**Spec:** `docs/superpowers/specs/2026-10-21-notary-serve-overview-design.md`

## Global Constraints

- Read-only and GET-only. No handler appends, reconciles, or opens the gap log for writing.
- **No `template.HTML`, `template.JS` or `template.URL` anywhere.** Autoescaping is never disabled.
- **No handler sets security headers** — the outermost `securityHeaders` middleware does, and the new route
  is inside it.
- No new verification logic. The card reads `chainView`; the gap line reads `OutstandingGaps`.
- Whole-ledger status is **stated** as whole-ledger, never implied to be scoped to a view.
- Every figure names the command whose output contains it.
- No absolute URL in any rendered page.
- Plain labels on the page: "Seen directly", "Inferred", "Not visible to Notary" — the ledger's own
  `Observed` / `Reconstructed` / `Internal` stay in the CLI and the records table.
- `gofmt -l .` empty and `go vet ./...` clean before each commit; suite runs `-p 1`.

## Review Focus

Conditions the spec implies that no single task's tests necessarily cover. Each gets its test in the task
named.

1. **A verdict without a run.** Every card state must carry the instant of the run that produced it, and
   the re-run command. A card that renders "intact" with no `AsOf` would be the failure the banner rules
   exist to prevent. (Task 3)
2. **The empty ledger is not a zero row.** A ledger with no records must render the first-run state, not a
   facts table of zeroes or an "intact" card over nothing. (Task 3, Task 4)
3. **The route move has two paths.** The filter form is an ordinary GET that htmx upgrades, so `action`
   **and** `hx-get` both move. A mismatch makes the scripted and unscripted paths disagree. (Task 2)
4. **The records empty state's own link.** `widerWindow` builds `/?from=…`; it moves to `/records?…` or the
   offered range link 404s into the Overview. (Task 2)
5. **Stored text on a new surface.** The Overview renders no record content, but the memory id in a tier
   breakdown must still be escaped if it ever gains one; the injection test runs over every page, not just
   the Overview. (Task 4)

---

## File Structure

**New**

- `internal/store/stats.go` — `LedgerStats`, the one aggregate read.
- `internal/store/stats_test.go`
- `internal/serve/overview.go` — `overviewView`, `loadOverview`, and the three card states.
- `internal/serve/nav.go` — the nav entries and section keys, in one list.
- `internal/serve/templates/overview.html` — the page.
- `internal/serve/overview_test.go` — the page's own tests.

**Modified**

- `internal/store/sqlite.go` — nothing, unless the aggregate belongs beside its siblings (prefer `stats.go`).
- `internal/serve/views.go` — `extent()` delegates to `LedgerStats`.
- `internal/serve/routes.go` — `handleOverview` at `/`, records moved to `/records`, `widerWindow` links.
- `internal/serve/server.go` — the page template set gains "overview".
- `internal/serve/templates/layout.html` — nav from the list; the records links repointed.
- `internal/serve/templates/index.html` — form `action` and `hx-get`.
- `internal/serve/assets/app.css` — the card, the facts row, the first-run note.
- `internal/serve/routes_test.go` — the pinned route list and every `/`-as-records assertion.
- `README.md`, `docs/OPERATIONS.md` — the new front page and the moved route.

---

## Task 1: The aggregate read

**Files:** `internal/store/stats.go`, `internal/store/stats_test.go`, `internal/serve/views.go`

**Produces:**

```go
type Stats struct {
    Count    int
    Earliest time.Time
    Latest   time.Time
    ByTier   map[record.Tier]int
}

func (s *SQLiteStore) LedgerStats() (Stats, error)
```

**Step 1 — RED.** Write `stats_test.go` first:

```go
func TestLedgerStatsCountsSpanAndTiers(t *testing.T)
func TestLedgerStatsOnAnEmptyLedgerIsZeroAndNoError(t *testing.T)
```

The first appends a known set (two observed, one reconstructed, one internal, with distinct `at` values),
then asserts `Count == 4`, `Earliest`/`Latest` are the extremes, and `ByTier` holds the three counts. The
second asserts a zero `Stats` and a nil error on a fresh file — no `time.Time{}` leaking into a page as
"year 1".

Run `go test ./internal/store/ -run 'LedgerStats' -v`. Expected: **FAIL — `s.LedgerStats` undefined**.

**Step 2 — GREEN.** One method, two aggregates, parsed with the store's existing time helper:

```sql
SELECT COUNT(*), MIN(at), MAX(at) FROM records
SELECT tier, COUNT(*) FROM records GROUP BY tier
```

`MIN`/`MAX` come back as text; parse them with the same helper `scanRawRow` uses, so a malformed `at`
is an error rather than a silently wrong bound. An empty table yields count 0 with NULL bounds — return a
zero `Stats` and no error, never a parse failure on NULL.

Run the same test. Expected: **PASS**.

**Step 3 — `extent()` delegates.** Replace `extent()`'s row walk with a call to `LedgerStats`, mapping
`Stats` onto `ledgerExtent` (`ok` is `Count > 0`). Its doc comment changes: it is no longer "called only
on the empty path", because the Overview calls the same read on every load — say so.

Run `go test ./internal/serve/ ./internal/store/ -count=1`. Expected: **PASS**, including the existing
empty-state tests.

**Commit:** `feat(store): one aggregate for the ledger's count, span and tiers`

---

## Task 2: The nav list and the route move

**Files:** `internal/serve/nav.go`, `internal/serve/routes.go`, `templates/layout.html`,
`templates/index.html`, `internal/serve/routes_test.go`

**Step 1 — RED.** Extend the pinned route test: every page renders the nav in the order
Overview, Records, Gaps, Verify, and marks exactly one entry current; `GET /records` serves the records
view and `GET /` serves the Overview; the filter form's `action` and `hx-get` are both `/records`.

Expected: **FAIL** — `/records` is a 404 and `/` is still the table.

**Step 2 — GREEN.**

- `nav.go`: `type navEntry struct{ Href, Label, Section string }` and `var navEntries = []navEntry{…}`.
  The layout ranges over it and marks `aria-current="page"` on the section match — the mechanism the
  existing nav already uses.
- `routes.go`: `handleOverview` at `/`, `handleRecords` at `/records`; the `r.URL.Path != "/"` guard moves
  to the Overview handler. `widerWindow` builds `/records?…`.
- `index.html`: `action="/records"` and `hx-get="/records"`.
- Every "back to records" link repointed.

Expected: **PASS**.

**Commit:** `feat(serve): the records view moves to /records, and the nav becomes one list`

---

## Task 3: The Overview page

**Files:** `internal/serve/overview.go`, `templates/overview.html`, `server.go`, `app.css`

**Step 1 — RED.** `overview_test.go`:

```go
func TestOverviewCardStatesAreDistinct(t *testing.T)        // intact / not verified / problem found
func TestOverviewIntactAgreesWithNotaryVerify(t *testing.T) // differential
func TestOverviewEmptyLedgerShowsTheFirstRunNote(t *testing.T)
func TestOverviewNamesACommandForEveryFigure(t *testing.T)
```

Expected: **FAIL** — no template, no handler.

**Step 2 — GREEN.** `overview.go` builds the view from `loadChain`, `loadGaps`, `LedgerStats` and
`gapsView`; `overview.html` renders, in order: the status card, the facts, the gaps line — or, when the
ledger is empty, the explanation and the `<textarea readonly>` note. The card carries `AsOf` and the
re-run command in every state. Include the run's own instant in each state, not only the intact one.

Expected: **PASS**.

**Commit:** `feat(serve): the Overview page, and the verdict it exists to give`

---

## Task 4: Cross-cutting tests

**Files:** `internal/serve/overview_test.go`, `internal/serve/routes_test.go`

- **No absolute URLs** on any page: no `http://`, `https://` or `//` in rendered output.
- **Stored text cannot become markup**: a fixture whose memory text is `<script>alert(1)</script>` renders
  escaped and adds no `script` element.
- **Tier counts match the ledger**, counted from the fixture's records rather than from the page.
- **Nav on every page**, current section marked, four entries.

Expected red first where the assertion is new; green after the relevant task above.

**Commit:** `test(serve): the Overview's states, and the console's escaping and offline guarantees`

---

## Task 5: Documentation

**Files:** `README.md`, `docs/OPERATIONS.md`

- README: the console now opens on the Overview — what it answers, the three states, and that the records
  table is one click away. Plain language, no flags, no env vars except where the reviewer-facing note needs
  them. Keep the "Reading the trail in a browser" section's structure.
- OPERATIONS: `/` is the Overview, `/records` is the table; the aggregate read and its behaviour on a
  corrupt row; the nav list's location.

**Commit:** `docs: the console opens on the Overview`
