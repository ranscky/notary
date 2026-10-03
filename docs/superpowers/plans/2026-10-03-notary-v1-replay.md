# Notary v1 Phase 7 — `replay`: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `notary replay --at <T>`, answering what Notary knew at time T — the records with `RecordedAt <= T`, in `Seq` order, verified as a chain prefix before they are returned.

**Architecture:** A new `recorded_at` read path in `internal/store` and `internal/ledger`; `internal/replay` selects and verifies a prefix, then renders it through `internal/export`'s existing `Render` and a shared encoder, so a replayed line is byte-identical to an exported one. No new dependencies, no CLI surface that lies about what it can attest.

**Tech Stack:** Go 1.25, Cobra, `modernc.org/sqlite` (pure Go), stdlib `crypto/ed25519`, testify.

**Spec:** `docs/superpowers/specs/2026-10-03-notary-v1-replay-design.md`

## Global Constraints

- Go 1.25; no new dependencies and **no change to `go.mod`**.
- Stdlib `crypto/ed25519` only; `modernc.org/sqlite` stays pure Go.
- Never panic in library code; wrap errors `fmt.Errorf("...: %w", err)`.
- No global mutable state. No hardcoded secrets. Never log key material.
- The record is the source of truth; renders are views and never touch a hash.
- Idempotent writes; fail loud, never fail silent.
- The whole suite runs `-p 1` (`go test -p 1 ./... -count=1`); `internal/ledger` fails spuriously in parallel because of SQLITE_BUSY.
- `gofmt -l .` must be empty and `go vet ./...` clean before each commit.

## Review Focus

The spec implies these; no task's own tests necessarily cover them, and each gets its test in the task named.

1. **`--at` with sub-second precision** — T carrying nanoseconds must select correctly against the fixed-width `recorded_at` text. A user passing `2026-10-03T12:00:00.5Z` expects the records at that instant, not a silently empty or over-broad range. (Task 1)
2. **An empty ledger, and a T before every record** — replay must print nothing, report zero breaks, and exit successfully. Not expecting a live ledger to always be non-empty. (Task 4)
3. **T exactly equal to a record's `RecordedAt`** — the comparison is `<=`, inclusive, per D9. An off-by-one here silently hides the most recent record, which is the one an auditor is most likely to be asking about. (Task 1)
4. **A record whose `RecordedAt` is out of order with its `Seq`** — the prefix has a hole; replay must report a break rather than print a plausible-looking prefix. Without this the verification is unfalsified. (Task 2, Task 4)
5. **An unparseable `--at`** — must fail loudly and never be treated as "now". A typo'd date silently meaning the present turns a point-in-time query into the wrong question. (Task 5)

---

### Task 1: The as-of read path

**Files:**
- Modify: `internal/store/sqlite.go` (add `ListRecordsAsOf` beside `ListRecords` at line 283)
- Test: `internal/store/sqlite_test.go`

**Interfaces:**
- Produces: `func (s *SQLiteStore) ListRecordsAsOf(t time.Time) ([]record.Record, error)`

- [ ] **Step 1: Write the failing tests**

Three cases, in the existing store test style (an open `SQLiteStore` with records appended through `AppendChained`):
- a record with `RecordedAt` exactly `t` **is** returned (inclusive, Review Focus 3);
- a record with `RecordedAt` one nanosecond after `t` is **not** returned (Review Focus 1);
- with `recorded_at` order deliberately opposite to `seq` order, the result is in **`seq`** order.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/store/ -run TestListRecordsAsOf -count=1`
Expected: FAIL — `s.ListRecordsAsOf undefined`.

- [ ] **Step 3: Implement `ListRecordsAsOf`**

`SELECT ` + `selectColumns` + ` FROM records WHERE recorded_at <= ? ORDER BY seq ASC`, binding `formatTime(t)` (which applies the `.UTC()` the layout needs). Copy `ListRecords`'s scanning, error wrapping and `rows.Err()` handling. The doc comment must state why the string comparison is correct — `instantLayout`'s nine fixed fractional digits make SQLite's BINARY collation chronological — so nobody "fixes" it into a parse-and-compare.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/store/ -count=1`
Expected: PASS, including every pre-existing store test.

- [ ] **Step 5: Commit**

```bash
git add internal/store/sqlite.go internal/store/sqlite_test.go
git commit -m "feat(store): read the ledger as of an instant"
```

### Task 2: Verifying a prefix, in the same read that produces it

**Files:**
- Modify: `internal/ledger/verify.go` (extract the walk from `Verify`, line 73)
- Modify: `internal/ledger/ledger.go` (add `ReplayAsOf` beside `ListRecords`, line 209)
- Test: `internal/ledger/verify_test.go`, `internal/ledger/ledger_test.go`

**Interfaces:**
- Consumes: `store.ListRecordsAsOf` (Task 1)
- Produces: `func (l *Ledger) ReplayAsOf(t time.Time, v *sign.Verifier) ([]record.Record, []Break, error)`, returning the prefix, its breaks, and an error only for a read failure.

**Decision, resolving spec §12 Q1:** one method, not a separate read plus verify. Replay must return *the prefix it verified*; two calls could interleave with a live writer and return a prefix that was never checked. The read and the check are one operation, so they are one function.

- [ ] **Step 1: Refactor `Verify` with no behaviour change**

Extract the existing walk — hash, `prev_hash` linkage, signature, and the `e.Seq != expected` continuity check from `expected = 1` — into a slice-based unexported helper. `Verify` keeps reading `store.SeqEntries()` and keeps tolerating a row with `DecodeErr`; the helper must not swallow that tolerance. **Existing `internal/ledger` tests must pass unchanged** — that they do is the evidence the refactor is behaviour-preserving.

- [ ] **Step 2: Write the failing tests for `ReplayAsOf`**

- a clean ledger at T returns the expected prefix and **zero** breaks;
- a record whose `RecordedAt` precedes its predecessor's, with T between them, returns a **hole** (`{1,2,4}`) and a **break** naming the missing seq (Review Focus 4);
- an empty ledger returns no records, no breaks, no error (Review Focus 2).

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/ledger/ -run TestReplayAsOf -count=1`
Expected: FAIL — `l.ReplayAsOf undefined`.

- [ ] **Step 4: Implement `ReplayAsOf`**

Read through `l.store.ListRecordsAsOf(t)`, pass the records to the helper, and return all three values. Its doc comment must state the prefix assumption — the records must start at seq 1 and step by one, which is true of an as-of view and false of an arbitrary mid-chain slice — because that assumption is the whole check.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/ledger/ -count=1`
Expected: PASS. `internal/ledger` takes 50–100s; run it alone, not in parallel with the rest.

- [ ] **Step 6: Commit**

```bash
git add internal/ledger/verify.go internal/ledger/ledger.go internal/ledger/verify_test.go internal/ledger/ledger_test.go
git commit -m "feat(ledger): replay a verified prefix as of an instant"
```

### Task 3: Share the line encoder instead of copying it

**Files:**
- Modify: `internal/export/export.go` (add `NewLineEncoder`; use it at line 263)
- Test: `internal/export/export_test.go`

**Interfaces:**
- Produces: `func NewLineEncoder(w io.Writer) *json.Encoder`, returning an encoder with `SetEscapeHTML(false)` set.

- [ ] **Step 1: Write the failing test**

A test asserting `NewLineEncoder` does not escape `<`, `>` or `&` — encode a `Line` whose `Text` contains `<a & b>` and assert the bytes contain the literal characters and no `\u003c`.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/export/ -run TestNewLineEncoder -count=1`
Expected: FAIL — `NewLineEncoder undefined`.

- [ ] **Step 3: Implement it, and use it in `Export`**

Move the two lines at `export.go:263-266` into the exported constructor, and have `Export` call it. Doc comment: this is the one definition of the export line format, shared so a replayed line and an exported line cannot drift. **Every existing `internal/export` test must still pass** — they are the byte-level guard on `Export`'s output.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/export/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/export/export.go internal/export/export_test.go
git commit -m "refactor(export): one definition of the line encoder"
```

### Task 4: `internal/replay`

**Files:**
- Create: `internal/replay/replay.go`
- Test: `internal/replay/replay_test.go`

**Interfaces:**
- Consumes: `export.Render` (internal/export/line.go:116), `export.NewLineEncoder` (Task 3), `export.Line`, `ledger.Break`
- Produces:
  - `type Reader interface { ReplayAsOf(t time.Time, v *sign.Verifier) ([]record.Record, []Break, error) }` — satisfied by `*ledger.Ledger`, fakeable in tests
  - `type Request struct { At time.Time; Scope record.Scope; IncludeSensitive bool }`
  - `type Result struct { Records, Redacted int; Breaks []ledger.Break }`
  - `func New(r Reader, v *sign.Verifier) *Replayer`
  - `func (rp *Replayer) Replay(ctx context.Context, req Request, out io.Writer) (Result, error)`

- [ ] **Step 1: Write the failing tests**

Against a fake `Reader` returning fixed records, assert: records render as `export.Line` JSON; `Scope` narrows exactly as `export`'s `scopeMatches` does (call it, do not reimplement); `IncludeSensitive false` redacts a sensitive record and `Result.Redacted` counts it; breaks are surfaced in `Result.Breaks`; an empty prefix writes nothing and returns a zero `Result` with a nil error (Review Focus 2).

Then a test over a **byte-identical** comparison: append the same records to a real ledger, render once through `export.Export` and once through `Replay`, and assert the line bytes match. This is the guard on Task 3.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/replay/ -count=1`
Expected: FAIL — no such package.

- [ ] **Step 3: Implement `Replay`**

Call `Reader.ReplayAsOf(req.At, rp.v)`; for each returned record write `export.Render(rec, req.IncludeSensitive)` through `export.NewLineEncoder(out)`, honouring `ctx.Err()` between records as `Export` does; tally `Records` and `Redacted` the way `Export` does. Breaks are reported in `Result`, not swallowed — the CLI decides how to surface them. Do **not** run a paraphrase pass, do **not** sign anything, do **not** write any file but `out`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/replay/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/replay/
git commit -m "feat(replay): replay a verified prefix as JSONL"
```

### Task 5: `notary replay`

**Files:**
- Create: `cmd/notary/replay.go`
- Test: `cmd/notary/replay_test.go`

**Interfaces:**
- Consumes: `replay.New`, `replay.Request`, `replay.Result` (Task 4)

- [ ] **Step 1: Read `cmd/notary/verify.go` in full and write down its break convention**

Before writing code, establish and record: does it write its output before exiting non-zero, and what does it print? The one confirmed fact from the spec is that it exits non-zero when there are breaks (the comment at `cmd/notary/verify.go:167`). Report the rest in the task report rather than assuming it.

- [ ] **Step 2: Write the failing tests**

- `--at` unparseable exits non-zero with a message naming the value, and never consults "now" (Review Focus 5);
- a clean ledger prints JSONL to stdout and exits 0;
- a ledger with a hole prints the breaks and exits non-zero, matching `verify`'s convention from Step 1;
- the four scope flags filter as `export`'s do.

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./cmd/notary/ -run TestReplay -count=1`
Expected: FAIL — unknown command.

- [ ] **Step 4: Implement the command**

Register `replay` in the same way `export` is registered. Flags, and nothing more: `--at` (required, RFC3339), `--include-sensitive`, and `--user-id`, `--agent-id`, `--app-id`, `--run-id` — the same names and help text as `export`'s at `cmd/notary/export.go:92-95`. Deliberately **no** `--from`, `--to`, `--phrase`, `--checkpoint-out` or `--max-span`; the help text should not hint at them.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cmd/notary/ -count=1`
Expected: PASS.

- [ ] **Step 6: Run the whole suite, serially**

Run: `go test -p 1 ./... -count=1`
Expected: all packages PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/notary/replay.go cmd/notary/replay_test.go
git commit -m "feat(cmd): notary replay"
```

### Task 6: The documented surface

**Files:**
- Modify: `docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md` (the phase table row 7, the read-path table near line 298, the tests list near line 351)

**Interfaces:** none — documentation only.

- [ ] **Step 1: Update the phase table**

Mark row 7 `replay` as complete, in the same form row 6 uses.

- [ ] **Step 2: Record the test the tests-list item 8 now has**

Item 8 names a "Replay boundary" test. Name the file and test that provides it, as the neighbouring entries in that list do.

- [ ] **Step 3: Correct anything in the existing prose that this phase made false**

Specifically, verify the claim at line 111 that `replay` "reads through `ledger`" is still true of the code as built, and fix the sentence if it is not. Do not add a new claim that has not been checked against the code.

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md
git commit -m "docs: record Phase 7"
```

---

## Risk, and where it is concentrated

- **Task 2 is the only task that touches working code on the verification path.** The mitigation is that `Verify`'s existing tests must pass unchanged; if the refactor is not behaviour-preserving, they fail, and that is the signal to stop rather than to adjust them.
- **Task 4's byte-identity test is the guard on Task 3.** If the encoder is not actually shared, that test fails. It is the reason Task 3 exists as its own task rather than being folded in.
- **The non-prefix fixture is the falsifier for the whole phase.** Spec §4 claims a backwards clock produces a hole; if the fixture does not produce a break, the claim is wrong and the phase's central premise needs revisiting, not the test.
- **Least certain:** `verify`'s exact break convention (Task 5 Step 1). It is read-and-match, not invented, and the task report must say what was found.

## Sequencing, and why the order is forced

1→2 (Task 2 consumes Task 1), 3→4 (Task 4 consumes the shared encoder), 5 (consumes Task 4), 6 (records what exists). Tasks 1 and 3 are independent of each other and could be parallelised, but the chain is short enough that serial is simpler to review.

## Spec coverage

| Spec section | Task |
| --- | --- |
| §3 read path, ordering, string comparison, decode policy | 1 |
| §4 why a prefix must be verified | 2, 4 |
| §5 the verifier refactor and the starts-at-1 assumption | 2 |
| §6 renderer reuse, shared encoder, why not `Exporter` wholesale | 3, 4 |
| §7 the command and its deliberate omissions | 5 |
| §8 reporting and failure | 5 Step 1–2 |
| §9 boundaries (no mutation model, no checkpoint, no paraphrase) | 4 Step 3, 5 Step 4 |
| §10 testing, including the non-prefix fixture | 1, 2, 4 |
| §11 sequencing | this plan's order |
| §12 Q1 (one method or two) | resolved in Task 2 |
| §12 Q2, Q3 (break format, `--at` in the future) | 5 Step 1; left to the CLI's convention |

## Type consistency

- `record.Record`, `record.Scope`, `sign.Verifier`, `sign.Signer` — existing, unchanged.
- `ledger.Break` — existing; `ReplayAsOf` returns `[]Break`, `replay.Result.Breaks` is `[]ledger.Break`.
- `export.Line`, `export.Render`, `export.NewLineEncoder` — `Render` existing, `NewLineEncoder` added in Task 3; Task 4 uses both and defines no second line type.
- `store.ListRecordsAsOf` → `ledger.ReplayAsOf` → `replay.Reader.ReplayAsOf` — one name, one signature, `(t, v) ([]record.Record, []Break, error)`, at every level.
