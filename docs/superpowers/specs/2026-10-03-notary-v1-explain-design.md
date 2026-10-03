# Notary v1 — Phase 8 design: `explain`

**Status:** draft for review. Approved in brainstorming: the three decisions in §4–§6, and the
boundaries in §8. This is the spec; per the brainstorming gate, no implementation starts until it is
reviewed and a plan is written.

**Date:** 2026-10-03
**Depends on:** Phases 0–7, complete, pushed and CI-green (`master` at `b77b795`).
**Phase table:** architecture spec §7, row 8, marked NEW — added mid-project.

---

## 1. What `explain` is for

`explain` is the **single-subject view**. Architecture §9 frames it against `export` in one line:
*"**`explain` is the single-subject view; `export` is the range view.**"* It answers *"what happened
to this memory, and why"* for one record or one memory, rather than rendering a range.

The constraint that shapes every choice below is §1's stated audience: *"a compliance lead who must
explain an agent's memory decisions without reading code or pulling in an engineer."* A range of
JSONL does not serve that person; one record's story does.

## 2. A claim retracted before it reached this spec

A brainstorming note asserted that Phase 8 owed a refactor extracting the tier phrasing out of
`internal/export`, citing design D6. **D6 is *"One hash chain, two writers"* — nothing to do with
phrasing.** Nothing in the architecture spec asks for that code to move, and `internal/phrase` is
imported only by `internal/export` by design (§4, D10).

A second note asserted the memory id is not a column and that `--memory` would therefore be a
Go-side scan. **It is a column** — see §4.

Both were flagged as unverified when written, which is why neither became a spec section asserting a
mechanism. Recorded here because this project's recurring defect is prose outliving the code it
describes, and both of these were that.

## 3. What the architecture spec already fixes

- §9's read-path table: `notary explain <record-id>` / `--memory <mem0-id>` — "one record, or one
  memory's lifecycle".
- §4's package tree: a new `internal/explain/`, "the per-record and per-memory lifecycle view".
- §4's dependency rule: `explain` reads through `ledger`.
- **D10:** `internal/phrase` is the only LLM-touching package and no core package imports it.

## 4. The read paths (decision 1, and the corrected finding)

**`explain <record-id>` needs nothing new.** `ledger.GetRecord(id)` exists (internal/ledger/ledger.go:201)
and already wraps `store.ErrNotFound`, so a missing record is already distinguishable from a read
failure. This is the whole of the single-record path.

**`--memory <mem0-id>` needs one new query, and the shape is now known.** The schema has
`memory_id TEXT NOT NULL DEFAULT ''` as a real column, alongside `user_id`, `agent_id`, `app_id` and
`run_id` (internal/store/sqlite.go:35-39); `PutRecord` writes them from `r.Subject` (:196-201),
`scanRecord` reads them back into `r.Subject` (:552), and `selectColumns` lists them (:56-57).

So this is a `WHERE memory_id = ?` query, not a Go-side filter:

```go
// internal/store
func (s *SQLiteStore) ListRecordsByMemory(memoryID string) ([]record.Record, error)
// WHERE memory_id = ? ORDER BY seq ASC

// internal/ledger
func (l *Ledger) ListRecordsByMemory(memoryID string) ([]record.Record, error)  // delegates
```

Three requirements, each of which is a trap if left unstated:

- **Ordered by `seq`**, matching `replay` and matching §9's ordering discipline. A lifecycle read in
  any other order is not a lifecycle.
- **An empty `memoryID` is rejected**, not executed. A caller passing `""` would match every record
  whose column took its `DEFAULT ''`, which is not a memory — a silent whole-ledger read. The
  caller-side check belongs in `internal/explain`, and the store must not be the only guard.
- **An undecodable row is an error**, matching `ListRecords` and `ListRecordsAsOf`, not `SeqEntries`'
  deliberate tolerance. `explain` cannot explain what it cannot read, and skipping it would hide a
  record — the same reasoning Phase 7 recorded in its §3.

**No index.** `memory_id` is unindexed today; the query is correct without one. If it ever needs an
index that is a separate change with its own measurement, exactly as Phase 7 ruled for `recorded_at`.

## 5. The output: prose by default, `--json` opt-in (decision 2)

The default output is prose written for §1's compliance lead. `--json` emits the same facts in a
machine-readable form for callers who need them.

The prose is built from **`export.Phrase(rec record.Record) (string, bool)`** (internal/export/phrase.go:41),
which is deterministic and not the LLM pass. This is an **import of `internal/export` by
`internal/explain`**, and it is consistent with an edge that already exists and was reviewed:
`internal/replay` imports `internal/export` for `Render`, `NewLineEncoder` and `ScopeMatches`. One
definition of a phrased claim, two readers of it.

Where `Phrase` returns `false` — a record whose event/reason pair the vocabulary cannot word —
**`explain` fails loudly** rather than printing a record with a missing explanation. That is the same
discipline `Render` already enforces for the export path, and it is the whole point of the phase-6
vocabulary gate: an audit tool's failure mode must be an error, not a gap.

**The LLM pass is not available here.** `--phrase` does not exist on this command: D10 keeps
`internal/phrase` imported only by `internal/export`, so `explain` cannot reach a provider even if
someone wanted it to.

## 6. The memory timeline (decision 3)

`--memory <mem0-id>` renders every record whose subject is that memory, in `Seq` order, each with its
event, reason and tier — the lifecycle as the ledger holds it, including a `Reconstructed` claim
written long after the event it describes.

It is deliberately **not** a summary of current state, which hides how the memory got there, and
**not** a generated narrative: this tool's value is that its claims are recorded rather than
generated, and a synthesized paragraph about a memory would be the one thing in the output that no
row attests.

## 7. The command

```
notary explain <record-id> [--json] [--include-sensitive]
notary explain --memory <mem0-id> [--json] [--include-sensitive]
```

- Exactly one of `<record-id>` or `--memory` is required. Both, or neither, is a usage error.
- `--include-sensitive` matches `export`'s and `replay`'s: off by default, and it changes only what
  is printed, never a hash.
- **Deliberately absent:** `--phrase` (§5), `--checkpoint-out` (an explanation attests nothing), and
  the four scope flags — a single subject already identifies its scope, so filtering by one would be
  a no-op at best and a confusion at worst.

## 8. Boundaries (out of scope)

- **No range view.** That is `export`. Do not add `--from`/`--to`.
- **No paraphrase**, per D10 (§5).
- **No new index** (§4).
- **No memory-subject search by content**, no fuzzy or prefix matching on ids, no "explain the last
  N records".
- **No writing.** `explain` is a read path; it touches no file but its output writer.

## 9. Testing

- **A record id that does not exist fails loudly**, naming the id searched for, and is distinct from
  a read failure — the `store.ErrNotFound` distinction §4 relies on.
- **The memory timeline** returns that memory's records in `seq` order, and a fixture with two
  memories' records interleaved proves the filter actually filters rather than returning everything.
- **An empty `--memory` is rejected** (§4's second requirement) — the one input that would otherwise
  silently read the whole ledger.
- **A record the vocabulary cannot phrase fails loudly** rather than printing a stub (§5).
- **`--json` and the prose agree** on the same fixture: same records, same order, same reasons.
- **A sensitive record's text is withheld by default** and rendered under `--include-sensitive`,
  matching `export`'s render-time redaction.
- **Exactly one subject is required** — both or neither is a usage error (§7).

## 10. Sequencing

1. The store query and the `ledger` pass-through (§4), with its tests.
2. `internal/explain` (§5–§6), following `internal/replay`'s shape — a `Reader` interface so the
   package is testable against a fake.
3. `cmd/notary/explain.go` (§7), following `export`'s registration and flag pattern.
4. Docs: the phase table's row 8, the read-path table if it needs it, and the tests list.

## 11. Open questions for the reviewer

1. **The prose's shape.** The spec fixes *that* the default is prose and *where* it comes from
   (`export.Phrase`), not the exact sentence. Is a small fixed template — subject, then the phrased
   claim, then the timestamp and tier — the right level of specification for the plan, or does this
   spec owe the wording?
2. **Where the "exactly one subject" check lives.** Cobra's `Args`/`MarkFlagsMutuallyExclusive` can
   enforce it declaratively, or the command can check it explicitly and control the message. The
   latter is more code and a better error; the former is idiomatic. I lean explicit, for the error
   text.
3. **Should `--json` be the `export.Line` shape or its own?** Reusing `Line` would let a consumer
   share a parser between the range and single-subject views, at the cost of shipping fields an
   explain consumer may not want. They are different views; I lean toward its own small shape, but
   it is a real fork.
