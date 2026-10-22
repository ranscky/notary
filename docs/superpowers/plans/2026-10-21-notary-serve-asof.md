# `notary serve` — the As-of page: Implementation Plan

> **For an implementer:** one task per commit, each green before the next.

**Goal:** A browser form of `notary replay` at `GET /asof`: pick an instant, see the records Notary had
recorded at or before it — verified as a chain prefix by `internal/replay`, narrowed by the same scope
conjunction, redacted by the same rule.

**Architecture:** The console parses the form into a `replay.Request`, runs the real
`replay.Replayer.Replay` into a buffer, decodes its JSONL into `export.Line`, and renders those through the
Records view's own table partial. It reimplements none of replay's rules.

**Tech Stack:** Go 1.25, `net/http`, `html/template`, `go:embed`, testify. **No new module, no `go.mod`
change.**

**Spec:** `docs/superpowers/specs/2026-10-21-notary-serve-asof-design.md`

## Global Constraints

- Read-only and GET-only; no handler writes.
- No reimplementation of replay, the prefix check, the scope filter or redaction.
- No `template.HTML`; autoescaping never disabled. Every form value echoed back is untrusted.
- Security headers come from the existing outermost middleware only.
- 500s go through `internalError`; validation messages use `data.Err`.
- No absolute URL on any page.
- Never claim a verified or intact result without the run behind it.
- `gofmt -l .` empty and `go vet ./...` clean before each commit; suite runs `-p 1`.

## Review Focus

1. **A boundary that is off by one.** `RecordedAt <= at` inclusive. A record at the instant is in; one a
   nanosecond later is out. (Task 4)
2. **A default instant creeping in.** A missing or garbage instant must never resolve to now, and must never
   reach `internalError` — it is the reader's input, so it is a message. (Task 4)
3. **A zone that is silently the server's.** No default in the select, and an unparsed zone is a message.
   (Task 3)
4. **Quoting in the reproduce line.** A scope value containing a quote or a space must not break the
   command it is pasted into. (Task 5)
5. **The table drifting.** Both pages must render the same partial; the `data-*` attributes are the
   differential's contract. (Task 2, Task 5)

---

## File Structure

**New**

- `internal/serve/asof.go` — the request parser, the replay call, the view model.
- `internal/serve/templates/asof.html`
- `internal/serve/asof_test.go`

**Modified**

- `internal/serve/nav.go` — the `asof` section and its entry, between Records and Gaps.
- `internal/serve/routes.go` — `handleAsOf`, the route, `widerWindow` untouched.
- `internal/serve/partials.html` — the `recordTable` partial.
- `internal/serve/templates/index.html` — render the partial instead of inline markup.
- `internal/serve/assets/app.css` — the as-of form and echo.
- `internal/serve/routes_test.go`, `security_test.go` — pinned routes, offline sweep, headers.
- `README.md`, `docs/OPERATIONS.md`.

---

## Task 1: The instant parser

**Files:** `internal/serve/asof.go`, `internal/serve/asof_test.go`

**Produces:**

```go
// asOfFields is the reader's input, before validation.
type asOfFields struct { Date, Time, Zone string; Scope record.Scope }

// parseAsOfInstant turns date + time + zone into one instant.
func parseAsOfInstant(date, tm, zone string) (time.Time, error)
```

Rules: date `2006-01-02`; time `15:04` or `15:04:05`; zone `UTC` or `±HH:MM`. All three required. An
unparseable part names that part in the error.

**Step 1 — RED.** `TestParseAsOfInstantZonesNameTheSameMoment` (two zones, one instant),
`TestParseAsOfInstantRejectsAMissingZone`, `TestParseAsOfInstantRejectsAGarbageDate`. Expected **FAIL —
undefined**.

**Step 2 — GREEN.** Implement, parsing in `time.FixedZone` so the result carries the reader's offset.

Expected **PASS**. **Commit:** `feat(serve): parse an as-of instant from a date, a time and a zone`

---

## Task 2: The shared record table

**Files:** `partials.html`, `index.html`, `views.go` (a small `recordTable` type)

**Step 1 — RED.** No new assertion: the three existing records-table tests are the gate. Extract the
markup verbatim into `{{define "recordTable"}}` taking `{Caption string, Rows []recordRow}`, render it from
`index.html`.

Expected: **PASS** with the existing suite, `parseRows` included — the `data-*` attributes are unchanged.

**Commit:** `refactor(serve): one record table, rendered by every view`

---

## Task 3: Route, nav and handler skeleton

**Files:** `nav.go`, `routes.go`, `asof.html`, `routes_test.go`, `security_test.go`

**Step 1 — RED.** Pinned route list gains `/asof`; the nav test expects five entries in order; the header
sweep and the offline sweep gain `/asof`. Expected **FAIL**.

**Step 2 — GREEN.** Add `sectionAsOf`, the nav entry between Records and Gaps, the mux line, and a handler
that renders the form with no request.

**Commit:** `feat(serve): /asof, and its place in the nav`

---

## Task 4: The request, the replay call and the states

**Files:** `asof.go`, `asof.html`, `app.css`

**Produces:**

```go
func (s *Server) handleAsOf(w http.ResponseWriter, r *http.Request)
func (s *Server) loadAsOf(ctx context.Context, req replay.Request) (asOfResult, error)
```

`loadAsOf` runs `replay.New(s.ledger, s.verifier).Replay(ctx, req, &buf)`, decodes each line into
`export.Line`, and returns the rows plus `res.Records`, `res.Redacted` and `res.Breaks`.

The page renders, in order: the form; the verdict when the prefix has breaks; the echo; the explanatory
sentence; the count; the table (or the nothing-recorded sentence); the reproduce line.

**Step 1 — RED.** `TestAsOfBoundaryIsInclusive` — a record at the instant is shown, one a nanosecond later
is not. `TestAsOfWithNoKeyringRunsNothing`. Expected **FAIL**.

**Step 2 — GREEN.** Implement. Expected **PASS**.

**Commit:** `feat(serve): the As-of page, rendered through internal/replay`

---

## Task 5: The remaining tests

`TestAsOfMatchesReplayRejectsNothing` (differential via `parseRows`), `TestAsOfMissingAndGarbageInstants`,
`TestAsOfEchoesTheSameInstantForTwoZones`, `TestAsOfEscapesHostileInput`,
`TestAsOfRedactsSensitiveContentUntilRevealed`, `TestAsOfScopeFiltersComposeWithAND`,
`TestAsOfReproduceLineQuotesItsArguments`.

**Commit:** `test(serve): the As-of page's boundary, its states and its quoting`

---

## Task 6: Documentation

README: the As-of view in the reviewer's words — what it answers, that a record written later is correctly
absent, and that it needs trusted keys. `docs/OPERATIONS.md`: the route, the required zone, the replay call
and the buffer decode, and that the post-instant count is not shown because `replay.Result` does not expose
it.

**Commit:** `docs: the console can answer what Notary knew at a moment`
