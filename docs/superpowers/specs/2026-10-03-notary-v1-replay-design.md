# Notary v1 — Phase 7 design: `replay`

**Status:** draft for review. Approved in brainstorming: the three decisions in §3–§5, and
the boundaries in §9. This document is the spec; per the brainstorming gate, no
implementation starts until it is reviewed and a plan is written.

**Date:** 2026-10-03
**Depends on:** Phases 0–6, complete and CI-green.
**Phase table:** architecture spec §7, row 7 (`replay`), which moves the original plan's
phase 6 to 7.

---

## 1. What `replay` is for

`replay` answers **what Notary knew at time T**. It is a *knowledge-time* view: which
records had been written into the ledger by then — not what the world looked like at T,
and not how any state mutated.

Architecture spec **D9** fixes the semantics: *"`replay --at` — Knowledge time — filters
on `RecordedAt <= T`, inclusive"*, and §9 adds the ordering and one worked example:
*"returns records with `RecordedAt <= T`, inclusive, ordered by `Seq`. A `Reconstructed`
claim written later is correctly absent from an earlier replay."*

The operator-facing question it answers is the auditor's: *what did the tool know on the
date it signed off?* A `Reconstructed` record written afterwards must not appear.

## 2. Correction to the Phase 7 brainstorming note

The note at `.superpowers/phase-7-replay-design-notes.md` said *"`Verify` (which already
works over a record slice)"*. **That is false, and this spec supersedes it.**
`ledger.Verify(v *sign.Verifier)` (internal/ledger/verify.go:73) takes no slice — it reads
`store.SeqEntries()`, the entire chain, and walks it. Verifying a *prefix* therefore needs
a small refactor (§5), not a call. Flagging it because it is the same defect class as
Phase 6's four overclaims: a prose claim written with more confidence than the code
supported, caught only by checking.

The note also said "the four scope flags", which is correct — `--user-id`, `--agent-id`,
`--app-id`, `--run-id` (cmd/notary/export.go:92–95).

## 3. The read path (decision 1)

`ListRecords` cannot serve this phase. It queries
`WHERE at >= ? AND at <= ? ORDER BY at ASC, seq ASC` (internal/store/sqlite.go:283) — the
**event** time, and ordered by it. Nothing in `store` or `ledger` currently reads
`recorded_at`.

Add, at both levels:

```go
// internal/store
func (s *SQLiteStore) ListRecordsAsOf(t time.Time) ([]record.Record, error)
// WHERE recorded_at <= ? ORDER BY seq ASC

// internal/ledger
func (l *Ledger) ListRecordsAsOf(t time.Time) ([]record.Record, error)  // delegates
```

Three properties this must have, each of which is a trap if left unstated:

- **Ordered by `seq`, not by `recorded_at`.** §9 requires `Seq` order, and `Seq` is the
  chain's own order. Ordering by the timestamp would be a different sequence whenever the
  clock is not perfectly monotone.
- **The string comparison is correct, and that is not an accident.** `recorded_at` is
  written by `formatTime` (internal/store/sqlite.go:574) using `instantLayout`
  (internal/store/sqlite.go:570), whose own doc says: *"Exactly nine fractional digits are always written, so every stored value has
  the same width and SQLite's BINARY collation orders the text chronologically."* The same
  doc records why `time.RFC3339Nano` cannot be used: it trims trailing zeros, and `'Z'`
  sorts after `'.'`, so lexical and chronological order disagree. **Say this in the new
  query's comment**, because the comparison reads like a bug to anyone who has not read
  that doc, and "fixing" it into a parse-and-compare would break index-ability.
- **Pass `formatTime(t)` as the bound**, not `t` — it is what applies the `.UTC()`
  normalisation the layout depends on.

**A row that cannot be decoded is an error here, not a break.** `SeqEntries` is
deliberately tolerant of an undecodable row (it returns it with `DecodeErr` set) because
verification must be able to *name* a tampered row. Replay is different: it cannot render
what it cannot decode, and silently omitting it would be exactly the loss this tool
refuses to hide. So `ListRecordsAsOf` fails loudly, like `ListRecords` does.

## 4. Why a replayed set must be verified (decision 3)

`RecordedAt` is stamped inside the append transaction and `Seq` is monotone, so
`RecordedAt <= T` should select a **contiguous chain prefix**: some first N records, seq 1
through N, with no holes.

Holes are possible. Nothing forces `RecordedAt` to be monotone with `Seq`: each append
takes the wall clock, and NTP can step it backwards. If record 5 received an *earlier*
`RecordedAt` than record 4, then at that instant the filter selects `{1,2,3,5}` — a set
with a gap, silently presented as "what Notary knew at T".

A hole in an audit prefix is worse than an error, so **replay verifies the prefix it is
about to return and fails loudly on a break.** This is what makes a replay evidence
rather than a view.

The checks are the ones `Verify` already performs, over exactly the records being
returned: recomputed hash, `prev_hash` linkage (genesis at the start), signature, and —
the one that catches the scenario above — **seq continuity from 1**.

## 5. Prefix verification: the refactor, stated precisely

`Verify`'s walk (internal/ledger/verify.go, from its definition at line 73) reads
`[]store.SeqEntry` — rows that may carry a `DecodeErr` — and checks hash, link, signature
and, in `if e.Seq != expected { … }`, continuity from `expected = 0` (the first record is seq 0, not 1).

The replay path holds `[]record.Record`, already decoded, because `ListRecordsAsOf` fails
rather than tolerating an undecodable row (§3). Those are different inputs, so the refactor
is:

- Extract the walk into a slice-based helper in `internal/ledger`, taking the decoded
  records, and keep `Verify` as the `SeqEntries`-based entry point that calls it. **`Verify`'s
  behaviour must not change** — including its tolerance of undecodable rows, which the
  helper must not swallow.
- Add `func (l *Ledger) VerifyPrefix(records []record.Record, v *sign.Verifier) []Break`
  — or an equivalent signature the implementer settles during the plan — verifying exactly
  the records given, **expecting seq to start at 0 and increment by one**.

**The "starts at 0" assumption is the whole check and must be documented.** A prefix of a
chain that starts at 0 starts at 0; a set with a hole fails because `expected` outruns the
next record's `Seq`. A future caller passing an arbitrary mid-chain slice would get false
breaks — so the doc comment must say the input is a prefix, not "some records".

## 6. Output: reuse the renderer, and share the encoder by construction (decision 2)

`internal/replay` selects; `internal/export` draws.

- Each record renders through `export.Render(rec, includeSensitive) (Line, error)`
  (internal/export/line.go:116) — the reviewed path that applies the fixed field set,
  redaction and phrasing. Redaction's hash-invariance is already proven there.
- Lines are written with the **same encoder configuration** the exporter uses:
  `json.NewEncoder(out)` with `SetEscapeHTML(false)` (internal/export/export.go:263–266),
  the latter so `<`, `>` and `&` stay greppable.

**Byte-identity must be structural, not duplicated.** The design requires a replayed line
to be byte-identical to an exported one. Two copies of a two-line encoder setup is exactly
how that quietly stops being true, so this spec requires the setup to be shared: extract it
in `internal/export` as a small exported constructor (e.g. `NewLineEncoder(w io.Writer)
*json.Encoder`), have `Export` use it, and have `replay` use it. The byte-identity test in
§10 then guards one definition rather than comparing two.

### Why not reuse `Exporter` wholesale

`export.New` takes a `Reader` (internal/export/export.go:36), which is `ListRecords(from,
to)` plus `Checkpoint(sg, now)`. Supplying a replay `Reader` would reuse everything with
almost no code — and it is **rejected**, for two reasons:

1. `Reader.ListRecords` is documented as bounding on `At`, the event time. An as-of reader
   would violate that contract while satisfying its signature — a lie in the type system,
   which is the failure mode this project keeps producing.
2. `Request.validate()` *requires* both bounds, so a `Reader`-based replay would have to
   invent a `From` and a `To` it does not mean.

For the same reason replay does **not** offer a checkpoint. `Exporter`'s checkpoint
attests the ledger **head** (export.go:292–296), which lies beyond T; an artifact named
"checkpoint" that describes a different instant than the replay would be misleading. Not
offering the flag is the honest form of that boundary.

## 7. The command

```
notary replay --at <RFC3339> [--include-sensitive] [--user-id id] [--agent-id id] [--app-id id] [--run-id id]
```

- `--at` is **required**. It takes an RFC3339 instant. An unparseable value fails; it is
  never silently treated as "now".
- The four scope flags match `export`'s names and semantics exactly, so the two read paths
  compose the same way.
- `--include-sensitive` matches `export`'s: off by default, and it changes only what is
  printed, never a hash.
- **Deliberately absent:** `--from`/`--to` (`--at` is a single instant), `--phrase` (§8 of
  the architecture spec makes the paraphrase export-only), `--checkpoint-out` (§6), and
  `--max-span` (a span cap guards a *range*; a single instant has none to cap).

## 8. Reporting and failure

On success, replay writes JSONL to stdout and reports the line and redacted counts the way
`export` does.

On a verification break, replay **must not exit quietly**. `notary verify`'s convention is
the model, and one half of it is confirmed: **it exits non-zero when there are breaks**
(cmd/notary/verify.go, at the comment *"gap.Verify reports exactly those breaks, so they
exit non-zero here"*). The implementer must **read `cmd/notary/verify.go` in full** and
match the rest of it — in particular whether the output is still written before the
non-zero exit — and say in the task report what that ordering actually is. This spec
asserts only the half it checked, because a spec that guesses is the defect of §2.

## 9. Boundaries (out of scope)

- **No point-in-time mutation model.** Architecture §3: replay answers "what existed", not
  "what the state was and how it mutated". Do not widen it into a world-state view.
- **No checkpoint** (§6).
- **No paraphrase** (§7).
- **No `replay --at now` special case**, no relative time parsing ("yesterday"), no
  incremental or streaming replay.
- **No new indexes.** If `recorded_at` ever needs one, that is a separate change with its
  own measurement; the query is correct without it.

## 10. Testing

- **The replay boundary** — architecture §12's item 8. The as-of instant is inclusive: a
  record whose `RecordedAt` equals T is returned; one a nanosecond later is not.
- **`Reconstructed` written later is absent** — §9's example, as an explicit test rather
  than an implication.
- **The non-prefix case, which is the point of §4.** A fixture with an out-of-order
  `RecordedAt` (record N+k stamped before record N) must make the prefix contain a hole and
  replay must report a break, not print a plausible-looking prefix. This test is what
  proves the verification is real; without it, §4 is an unfalsified claim.
- **Byte-identity with export**, over the same records, guarding §6's shared encoder.
- **Ordering is `seq`, not timestamp** — a fixture whose `recorded_at` order differs from
  its `seq` order must come out in `seq` order.
- **An undecodable row in range fails loudly** (§3), rather than being skipped.

## 11. Sequencing

1. The store query and the `ledger` pass-through (§3), with its tests.
2. The verifier refactor and `VerifyPrefix` (§5), with `Verify`'s existing tests passing
   unchanged — that they do is the evidence the refactor is behaviour-preserving.
3. `internal/replay` (§6), plus the shared encoder extracted from `export`.
4. The CLI (§7–§8).
5. Docs: the phase table row, the read-path table if it needs it, and the tests section.

## 12. Open questions for the reviewer

1. **`VerifyPrefix`'s shape.** `VerifyPrefix(records, v)` keeps reading and verifying
   separate, at the cost of a caller-visible two-step. A combined
   `ReplayAsOf(t, v) ([]record.Record, []Break, error)` reads once and cannot be called
   half-way, which is a coherence argument — the prefix it verifies is the prefix it
   returns. I lean combined and would like your call.
2. **Break output format.** Human-readable on stderr, or JSON? `verify` already has an
   answer; matching it is the presumption, but if `verify`'s is human-only and hard to
   consume, this is the moment to notice.
3. **`--at` in the future.** A T after the head returns the whole ledger. Worth an error,
   a note on stderr, or silence? Silence is defensible — it is a true answer.
