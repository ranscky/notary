# Notary v1 Phase 8 — `explain`: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `notary explain <record-id>` and `notary explain --memory <mem0-id>`, the single-subject view: one claim's reason, or one memory's lifecycle, in prose for the compliance lead the spec names.

**Architecture:** One new store query filtered on the `memory_id` column; a new `internal/explain` package reusing `export.Phrase` and `export.Render` the way `internal/replay` already does; and a CLI command following `export`'s pattern. The single-record path needs no new query — `ledger.GetRecord` already exists.

**Tech Stack:** Go 1.25, Cobra, `modernc.org/sqlite` (pure Go), stdlib `crypto/ed25519`, testify.

**Spec:** `docs/superpowers/specs/2026-10-03-notary-v1-explain-design.md`

## Global Constraints

- Go 1.25; no new dependencies and **no change to `go.mod`**.
- Stdlib `crypto/ed25519` only; `modernc.org/sqlite` stays pure Go.
- Never panic in library code; wrap errors `fmt.Errorf("...: %w", err)`.
- No global mutable state. No hardcoded secrets. Never log key material.
- The record is the source of truth; renders are views and never touch a hash.
- Idempotent writes; fail loud, never fail silent.
- The LLM is reachable only from `internal/export` (D10). `internal/explain` must not import `internal/phrase`.
- The whole suite runs `-p 1` (`go test -p 1 ./... -count=1`); `internal/ledger` fails spuriously in parallel.
- `gofmt -l .` empty and `go vet ./...` clean before each commit.

## Review Focus

Inputs and conditions the spec implies but no task's own tests necessarily cover. Each gets its test in the task named.

1. **`--memory ""`** — an empty memory id must be **rejected**, not executed. Every record whose `memory_id` took its column `DEFAULT ''` would match, so this is a whole-ledger read wearing a filter's clothes. (Task 1, Task 3)
2. **A memory id that has no records** — must fail loudly naming what was searched for, and must be distinguishable from a read failure (§6, §9). (Task 3)
3. **Two memories' records interleaved** — the filter must actually filter. A query that returns everything, or that returns the right count by accident, passes a single-memory fixture. (Task 1, Task 2)
4. **A record the vocabulary cannot phrase** — must fail loudly, not print a record with the explanation missing (§5). This is the one input where the phase-6 vocabulary gate is the difference between an error and a gap. (Task 2)
5. **A sensitive record's text with and without `--include-sensitive`** — withheld by default, rendered when asked, and the hash untouched either way. (Task 2, Task 3)

---

### Task 1: The by-memory read path

**Files:**
- Modify: `internal/store/sqlite.go` (add `ListRecordsByMemory` beside `ListRecordsAsOf`), `internal/store/store.go` (interface)
- Modify: `internal/ledger/ledger.go` (add the pass-through beside `ListRecords`)
- Test: `internal/store/sqlite_test.go`, `internal/ledger/ledger_test.go`

**Interfaces:**
- Produces: `func (s *SQLiteStore) ListRecordsByMemory(memoryID string) ([]record.Record, error)` and `func (l *Ledger) ListRecordsByMemory(memoryID string) ([]record.Record, error)`

- [ ] **Step 1: Write the failing tests**

Follow the store tests' existing `newOpenStore` + `PutRecord` fixture style. Three cases: a memory's own records come back in `seq` order; **two memories interleaved return only one memory's records** (Review Focus 3); and an undecodable row in range returns an **error**, matching `ListRecordsAsOf`'s policy rather than `SeqEntries`' tolerance.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/store/ -run TestListRecordsByMemory -count=1`
Expected: FAIL — `s.ListRecordsByMemory undefined`.

- [ ] **Step 3: Implement it**

`SELECT ` + `selectColumns` + ` FROM records WHERE memory_id = ? ORDER BY seq ASC`. Copy `ListRecordsAsOf`'s scanning, error wrapping and `rows.Err()` handling exactly. Add the method to the `Store` interface — the ledger holds `store.Store`, so it cannot call it otherwise, the same reason Task 1 of Phase 7 needed the interface.

- [ ] **Step 4: Write the ledger pass-through and its test**

The ledger method delegates, like `ListRecords` does. Its test proves the delegation, not the SQL.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/store/ -count=1` then `go test ./internal/ledger/ -count=1` (the second takes 50–100s; run it alone).

- [ ] **Step 6: Commit**

```bash
git add internal/store/ internal/ledger/
git commit -m "feat(store): read a memory's records by id"
```

### Task 2: `internal/explain`

**Files:**
- Create: `internal/explain/explain.go`
- Test: `internal/explain/explain_test.go`

**Interfaces:**
- Consumes: `export.Phrase` (internal/export/phrase.go:41), `export.Render` (internal/export/line.go:116), `export.Line`, `ledger.ListRecordsByMemory` (Task 1)
- Produces:
  - `type Reader interface { GetRecord(id record.RecordID) (record.Record, error); ListRecordsByMemory(memoryID string) ([]record.Record, error) }` — satisfied by `*ledger.Ledger`
  - `type Request struct { RecordID record.RecordID; MemoryID string; IncludeSensitive bool; JSON bool }`
  - `type Result struct { Records int; Redacted int }`
  - `func New(r Reader) *Explainer`
  - `func (e *Explainer) Explain(ctx context.Context, req Request, out io.Writer) (Result, error)`

- [ ] **Step 1: Write the failing tests**

Against a fake `Reader`:
- a single record renders its phrased claim (assert on the `export.Phrase` output appearing in `out`);
- a memory's records render as a **timeline in `seq` order**, one line each, each with event, reason and tier (Review Focus 3);
  (Corrected after implementation: the line also carries both of the record's instants — `at <At>, recorded at <RecordedAt>`, rendered RFC3339 — for the reason §6 of `2026-10-03-notary-v1-explain-design.md` was corrected; the shape is pinned by `TestExplainMemoryRendersATimelineInSeqOrder`, and `TestExplainTimelineLineShowsTheGapBetweenEventAndWriteTime` pins the gap between the two.)
- **an empty `MemoryID` with an empty `RecordID` is rejected** before the reader is called;
- **a record `export.Phrase` cannot word fails loudly** — construct one with an event/reason pair outside the vocabulary and assert an error, not a stub line (Review Focus 4);
- a sensitive record is withheld by default and rendered under `IncludeSensitive` (Review Focus 5);
- `JSON: true` and prose mode agree on the same fixture — same records, same order (Review Focus 3).

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/explain/ -count=1`
Expected: FAIL — no such package.

- [ ] **Step 3: Implement `Explain`**

Single record: `GetRecord`, phrase it, write it. Memory: `ListRecordsByMemory`, phrase each, write in order. Prose is a small fixed template — the subject, then the phrased claim, then the recorded-at instant and tier — because the spec fixes that the output is prose and where it comes from, not the exact sentence. `--json` gets its **own small shape**, not `export.Line`: they are different views, and an explain consumer should not have to ignore fields the range view carries.

Two obligations the doc comments must state: that `Phrase` returning false is an **error** here and why (a missing explanation is the failure an audit tool must not produce), and that the empty-`MemoryID` check lives here because the store cannot distinguish "no filter" from "match the empty column".

Do not import `internal/phrase`. Do not write any file but `out`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/explain/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/explain/
git commit -m "feat(explain): the single-subject view"
```

### Task 3: `notary explain`

**Files:**
- Create: `cmd/notary/explain.go`
- Modify: `cmd/notary/root.go` (register)
- Test: `cmd/notary/explain_test.go`

**Interfaces:**
- Consumes: `explain.New`, `explain.Request`, `explain.Result` (Task 2)

- [ ] **Step 1: Write the failing tests**

- **exactly one subject is required**: both `<record-id>` and `--memory`, or neither, is a usage error naming the problem (Review Focus 1);
- **`--memory ""` is rejected** rather than reading the ledger (Review Focus 1);
- **a memory id with no records exits non-zero**, naming the id searched for and distinguishable from a read failure (Review Focus 2);
- a record id that does not exist exits non-zero naming the id — `ledger.GetRecord` wraps `store.ErrNotFound`, so the message can say "no such record" rather than "read failed";
- a clean record prints prose and exits 0; `--json` prints JSON and exits 0;
- `--include-sensitive` controls redaction (Review Focus 5).

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cmd/notary/ -run TestExplain -count=1`
Expected: FAIL — unknown command.

- [ ] **Step 3: Implement the command**

Register as `export` does. Flags: `--memory`, `--json`, `--include-sensitive` — and deliberately **no** `--phrase`, `--checkpoint-out`, or scope flags (§7), with the help text naming none of them.

Enforce mutual exclusion **explicitly rather than with Cobra's declarative helper**, so the error text can say which of the two was missing and what to do — the spec's §11 Q2 leaned this way and this plan fixes it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cmd/notary/ -count=1`
Expected: PASS.

- [ ] **Step 5: Run the whole suite, serially**

Run: `go test -p 1 ./... -count=1`
Expected: all packages PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/notary/
git commit -m "feat(cmd): notary explain"
```

### Task 4: The documented surface

**Files:**
- Modify: `docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md` (row 8 of the phase table, the read-path table, the tests list)

**Interfaces:** none — documentation only.

- [ ] **Step 1: Mark the phase table's row 8 complete**, in row 7's form. Verify what row 7 says and match it rather than assuming.

- [ ] **Step 2: Check the read-path table's `explain` row** against what shipped and correct it if the command's surface differs.

- [ ] **Step 3: Name the test that provides the phase's testing** where the tests list predicts one, as Phase 7's Task 6 did.

- [ ] **Step 4: Correct anything this phase made false**, checking each sentence against the code before changing it. Do not add a claim that has not been checked.

- [ ] **Step 5: Commit**

```bash
git add docs/
git commit -m "docs: record Phase 8"
```

---

## Risk, and where it is concentrated

- **Task 2's fail-loudly-on-unphraseable test is the phase's sharpest edge.** If `Phrase` returning false is handled by printing something, the command will quietly emit an explanation-shaped line with no explanation — the exact failure the vocabulary gate exists to prevent. That test is the falsifier.
- **Task 1's interleaved-memories test is the guard on the filter.** A single-memory fixture passes against a query that ignores `memory_id` entirely.
- **Least certain:** the empty-`MemoryID` enforcement point. It is checked in `internal/explain` and again at the CLI, which is redundant by design — the store cannot tell "no filter" from "match the empty column", so neither layer alone is sufficient.

## Sequencing, and why the order is forced

1→2 (explain consumes the query; the single-record path needs only `GetRecord`), 2→3, 4 records what exists. Task 1 is independent of nothing and everything depends on it.

## Spec coverage

| Spec section | Task |
| --- | --- |
| §4 the single-record path via `GetRecord` | 2, 3 |
| §4 the `memory_id` query, ordering, empty-id rejection, decode policy | 1 |
| §5 prose from `export.Phrase`; fail loudly when it cannot word | 2, 3 |
| §5 no paraphrase (D10) | 2 Step 3 |
| §6 the timeline, not a summary or a narrative | 2 |
| §7 the command, and the flags deliberately absent | 3 |
| §8 boundaries | 1 (no index), 2, 3 |
| §9 testing | 1, 2, 3 |
| §10 sequencing | this plan's order |
| §11 Q1 prose wording | 2 Step 3 (fixed template) |
| §11 Q2 mutual exclusion | 3 Step 3 (explicit) |
| §11 Q3 `--json` shape | 2 Step 3 (its own) |

## Type consistency

- `record.Record`, `record.RecordID`, `record.Scope` — existing, unchanged.
- `export.Phrase(record.Record) (string, bool)`, `export.Render(record.Record, bool) (Line, error)` — existing; Task 2 calls both and defines no second phrasing.
- `store.ListRecordsByMemory` → `ledger.ListRecordsByMemory` → `explain.Reader.ListRecordsByMemory` — one name, one signature, `(string) ([]record.Record, error)`, at every level.
- `ledger.GetRecord(record.RecordID) (record.Record, error)` — existing; Task 2 consumes it through `Reader`.
