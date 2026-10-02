# Notary v1 Phase 5 — The Reconciler: Design

**Date:** 2026-09-30
**Branch:** `feat/reconciler`
**Parent spec:** [`2026-09-28-notary-v1-architecture-design.md`](./2026-09-28-notary-v1-architecture-design.md) §6, §7, §10, §13
**Status:** design approved, implementation plan not yet written

---

## 1. What this phase is for

Phases 0–4 built a ledger that records only what Notary **directly witnessed**: an add was
acknowledged, a search ran, a memory came back in the results. Those are `Observed` facts and they are
the easy half.

Phase 5 builds the reconciler: the component that goes back to Mem0 afterwards and works out what
actually became of those operations. It is the **first and only producer of `Reconstructed` and
`Internal` claims** — the "this memory was dropped, and here is the evidence and the rule that says
so" half, and the "Mem0 removed this and will not say why" half.

That makes this the phase where the product's central promise is either kept or broken. A ledger of
`Observed` facts is merely redundant with Mem0. The value is in claims Mem0 never made — which means
every one of them is an inference, and an inference in a compliance tool is only worth having if it
is honest about its limits.

---

## 2. The organising principle

Every decision below falls out of one rule, which is worth stating on its own because it is what the
rest of the document is defending:

> **Notary claims absence only when it can prove the enumeration was exhaustive.**

This has three instantiations, and none of them is optional:

| Claim | Exhaustiveness proof |
|---|---|
| A memory was absent from a search | the search returned **fewer** results than its `top_k` — so the store had nothing further above the threshold to give — **and** the memory in question was not among the results it did return |
| A memory was absent from a listing | **every page** of the enumeration was read |
| A memory was removed from the store | it is absent from a **complete** enumeration **and** its history shows a `DELETE`/`UPDATE` |

The first is why §5 of the parent spec insists the cause is never guessed. The second is why
pagination is a correctness requirement rather than a convenience — a partially-read listing turns
"we did not look everywhere" into "we looked everywhere and it was not there", which is a false claim
of exactly the kind this product exists to prevent. The third is why a store removal is `Internal`
rather than `Reconstructed`: the *event* is witnessed, the *reason* is not, and Notary does not
invent one.

---

## 3. Decisions taken during design

| # | Decision | Choice | Rationale |
|---|---|---|---|
| 1 | Operating model | CLI one-shot `notary reconcile`; `InProcess` defined but reserved | A compliance pass should be a schedulable job with its own exit code and logs, not a hidden goroutine inside a customer's application. Matches how `FailClosed` is already defined-and-unused. Scheduling stays in the operator's security domain. |
| 2 | Rule registry | Closed, code-internal, versioned; not operator-tunable | The inference logic *is* part of the evidence. Operator-tunable inference would let a deployment silently weaken what Notary is willing to claim. |
| 3 | Covering search | Saturation predicate, **plus** non-return: claim only when `len(results) < top_k` **and** the search did not return the memory | When the result set is truncated by `top_k`, absence proves nothing. When it is not, the store genuinely had nothing more above the threshold, so absence is a real fact about the threshold. Computable from the ledger alone — `top_k` and the result count are both already on the `search_performed` record. **Corrected after implementation:** saturation alone is NOT sufficient, and the original wording here ("the saturation predicate") was wrong. A saturated search can still have returned the very memory being claimed absent — a saturated search returns everything above the threshold, so a memory it returned was above the threshold and did not drop. Claiming absence then contradicts the reason kind's own meaning (`ReasonAbsentFromSearch` is "a memory expected to surface did not appear in a search's results"). Non-return is established from the `memory_surfaced` records, whose IDs are `<correlationID>#<rank>` while the `search_performed` record's ID *is* the correlation ID, so membership is a prefix test that cannot be confused by the `#` separator. |
| 4 | Claim identity | Per memory, keyed on memory id + content hash + rule + rule version | Makes re-running a no-op and keeps the ledger bounded. Also covers memories Notary never watched being added — a large blind spot otherwise. The link back to the originating add survives via the memory id that both the event-status response and `get_all` expose. |
| 5 | `confidence` | Left unset, and made explicit by **omission** from the encoded reason | See §8. |
| 6 | Exhaustiveness | Enforced by the type system, not convention | See §7. |
| 7 | Failure behaviour | Loud, non-zero exit; **not** fail-open, and no `audit_gap` | See §9. |

**Rejected alternatives.** A job queue enqueued by the interceptor (approach B) would give smaller
scans at the cost of mutable state outside the hash chain, and a queue that diverges from the ledger
is a new failure mode in a tool whose central promise is that the record is the source of truth.
Pure derivation with no Mem0 calls (approach C) cannot resolve an add's outcome or observe the store
at all, so it cannot produce most of what this phase exists to produce.

---

## 4. Surfaces

**New package `internal/reconcile`** — reads through `internal/ledger` and depends on
`internal/record` and `internal/mem0`. It does **not** depend on `internal/interceptor`: the parent
spec's §4 makes `interceptor` and `reconcile` peers that both depend on `ledger`, and collapsing that
would couple the reconciler to a sibling implementation.

**New command.**

```
notary reconcile [--since <RFC3339>] [--user-id <id>] [--agent-id <id>] [--app-id <id>] [--run-id <id>] [--dry-run]
```

| Flag | Purpose |
|---|---|
| `--since` | only consider records whose **`At`** (the Mem0 event time) is at/after this time; bounds the pass cost |
| `--user-id` / `--agent-id` / `--app-id` / `--run-id` | restrict the pass to one entity scope; combinable, and combined with AND |
| `--dry-run` | compute and report what it *would* claim; write nothing |

`--since` filters on `At` and **deliberately not** on `RecordedAt`. Work items are Mem0 events, and a
record written late *about* an old event still belongs to that old event's window. The consequence is
worth stating because it is a real footgun: an operator who sets `--since` more recently than an
unresolved add's event time will skip that add. The default is no bound, so this only affects
operators who set it deliberately.

Four explicit scope flags rather than a single `--scope <dimension>=<value>`: `Scope` is a four-field
struct, and four named flags keep `--help` self-describing and remove any question of how to pass a
value. An earlier draft of this document specified `--scope <user_id|agent_id|…>`, which was
genuinely ambiguous about how the dimension and value were supplied.

### 4.1 Public API

Matching the parent spec's §4 interface sketch, the reconciler computes claims and does not itself
write to the chain:

```go
// Window bounds a pass. Since is zero for "no bound".
type Window struct { Since time.Time; Scope record.Scope }

// Reconcile derives the claims warranted by the current state of Mem0 and the
// ledger. Returned records carry a zero Seq and zero Hash: Ledger.Append is what
// assigns chain position and computes the hash, so nothing here can supply either.
func Reconcile(ctx context.Context, w Window) ([]record.Record, error)
```

The command appends each returned record via `ledger.Append`. `--dry-run` prints them instead. This
split is why `--dry-run` is trivial rather than a special mode threaded through the whole
reconciler, and why the reconciler is testable without a ledger.

### 4.2 Reading evidence the interceptor wrote

The worklist cannot be derived without reading three Observed evidence payloads that the interceptor
writes: `addPayload` (event id, status), `searchPerformedPayload` (`top_k`, `count`), and
`memorySurfacedPayload` (score, rank). The first two are load-bearing — the event id identifies
unresolved adds, and `top_k` versus `count` *is* the saturation predicate.

Those types are currently unexported in `internal/interceptor/library`. That leaves only bad options:
depend on a sibling implementation (contradicting §4 of the parent spec, where `interceptor` and
`reconcile` are peers that both depend on `ledger`), or re-declare the same JSON shapes in
`reconcile` so the two can drift silently.

**Decision:** move the three payload types into `internal/mem0` as exported types. They are
projections of Mem0's own response types, and both consumers already import `mem0` — indeed
`searchPerformedPayload` already embeds `mem0.Filters`. This keeps a single definition and preserves
the sibling relationship.

**Hard constraint, because these payloads are hashed.** `ObservedEvidence.Payload` is embedded in
`Reason.Encode()` output, which `CanonicalBytes` writes directly into the hash input. So the encoded
JSON of these payloads is part of every record's hash. `add_requested`, `search_performed` and
`memory_surfaced` records already exist in real ledgers written by Phases 3–4, so a changed field
**name or tag** would make every existing record of that kind fail verification.

A correction to an earlier draft of this section, which claimed that declaration order was hashed
too "because `encoding/json` emits fields in declaration order". That is wrong, and verifying it
matters: `NewObservedEvidence` decodes the payload into an `any` with `UseNumber()` and **re-marshals
it**, so for a JSON object the keys come out **sorted**. The bytes that reach the hash are therefore
canonical, sorted-key JSON, and *declaration order does not affect any record hash*. Renaming a field
or editing a tag does; reordering does not. `internal/record/reason.go` states this ("insignificant
key order and whitespace … cannot perturb a downstream record hash") and
`TestNewObservedEvidenceCanonicalises` asserts it.

The move therefore must preserve field names and all JSON tags exactly. A **golden-bytes regression
test** pins the exact encoded payload for all three, which is deliberately *stricter* than the hash
requires — it also pins declaration order and the absence of `omitempty` — so a careless refactor
cannot invalidate existing ledgers.

`--dry-run` is how an operator inspects the inference logic against their real store before letting
it sign claims into the chain. It is not a debugging convenience; for a tool that writes to an
append-only signed ledger, it is the preview gate.

**`config`** gains `ReconcileMode` with values `Command` and `InProcess`. Only `Command` is
implemented. `InProcess` is defined and reserved, matching the precedent of `FailClosed` (§14 of the
parent spec).

**`internal/mem0`** gains a paginating enumeration (§7). The existing one-page `GetAll` stays exactly
as it is, because the tests rely on it and because "return one page" is a truthful description of the
HTTP contract.

**No changes to `internal/ledger`.** The reconciler folds the ledger in memory using the existing
`ListRecords` and derives its worklist from that. This is deliberately the smaller design: bespoke
query methods would couple `ledger` to the reconciler's needs, and correctness does not depend on
incremental queries because re-runs are idempotent regardless. Total blast radius: one new package,
plus small edits to `config`, `cmd/notary`, `internal/mem0`, and one wire-format change in
`internal/record` (§8).

---

## 5. The claim catalogue

Everything Phase 5 can produce. Nothing outside this table is written by the reconciler.

| # | Event | Trigger | Reason kind | Tier |
|---|---|---|---|---|
| 1 | `add_resolved` | event status terminal **SUCCEEDED** | `stored_by_mem0` | `Observed` |
| 2 | `add_resolved` | event status terminal **FAILED** | `add_failed` | `Observed` |
| 3 | `add_resolved` | SUCCEEDED, **zero** memories produced | `no_facts_extracted` | `Reconstructed` |
| 4 | `memory_kept` | present in a complete enumeration, and **either** its id matches the add's produced id **or** it was identified by content match (row 5) | `stored_by_mem0` | `Observed` |
| 5 | `memory_kept` | present in a complete enumeration, **id differs** but content hash matches | `kept_by_content_match` | `Reconstructed` |
| 6 | `memory_dropped` | known memory absent from a **covering** search (saturated **and** did not return it) | `absent_from_search` | `Reconstructed` |

**Rows 4 and 5 fire together, and that is deliberate.** A content match tells us *which* memory the add produced; the memory being present in the enumeration we just read is separately witnessed, so the Observed claim is written in the SAME pass rather than a later one. Writing it later would mean the first re-run appended a record, which is exactly the idempotence promise (`re-running against an unchanged store appends nothing`) this design makes to operators who schedule the command.
| 7 | `memory_dropped` | absent from a **complete** enumeration, history shows `DELETE`/`UPDATE` | `removed_by_mem0` | `Internal` |

### 5.1 The `add_resolved` reason kinds (resolves a gap in the parent spec)

§6 of the parent spec lists `add_resolved` as a deferred claim and lists `memory_kept` as the
producer of `stored_by_mem0`, but the §5 vocabulary has no "add succeeded" kind — `add_failed` covers
only failure. A successful `add_resolved` therefore uses `stored_by_mem0`: Mem0 finished and stored a
memory.

Two events sharing a reason kind is legal. `DeriveIdemKey` takes the event kind as an input, so the
two never collide.

`no_facts_extracted` is `Reconstructed` and not `Observed` because the *observation* is "the results
array was empty"; the claim "Mem0 extracted no facts" is an interpretation of that, and the tier
vocabulary already maps it correctly (`ReasonNoFactsExtracted → Reconstructed`).

### 5.2 Why the reconciler writes `memory_dropped` and never "evicted" or "pruned"

`absent_from_search` and `removed_by_mem0` are the only two things v1 can establish. A memory absent
from a covering search may have fallen below the threshold, or been removed — Notary does not and
cannot know which without re-querying with a widened window, which §5 forbids. It records the fact
and the parameters and stops there.

---

## 6. The rule registry

`internal/reconcile/rules.go` holds one table. A rule has a stable name, a version, the reason kind
it justifies, and the tier that implies.

| Rule | Version | Justifies |
|---|---|---|
| `kept_by_content_match` | 1 | memory present in a complete enumeration whose id differs from the add's produced id, but whose content hash equals the submitted text |
| `absent_from_search` | 1 | known memory absent from a search that was **saturated** (`len(results) < top_k`) **and** did not return that memory |
| `no_facts_extracted` | 1 | event resolved `SUCCEEDED` with an empty `results` array |
| `removed_by_mem0` | 1 | absent from a complete enumeration, and history shows `DELETE`/`UPDATE` exposing no reason |

**Versioning contract.** A version bumps **only** when the rule's meaning changes — never for a
refactor, a rename, or a message reword. Because `rule` and `ruleVersion` are recorded inside the
signed record, both are permanent: renaming a rule would leave every earlier record unexplainable, so
a name is never reused for a different meaning. The registry documents what each version means, so a
reader years later can establish what "rule `absent_from_search` v1" was actually asserting.

**Enforcement.** `basis`, `rule`, and `ruleVersion` are already required non-empty by both
`NewReconstructedEvidence` and `ReconstructedEvidence.validate`, so a claim cannot be written without
them. A test additionally asserts that (a) registry names are unique and (b) every rule name the
producers record exists in the registry. That test is what stops a rename from silently entering
signed records.

---

## 7. The exhaustiveness guarantee

### 7.1 How Mem0 paginates (verified against primary sources)

The client as written cannot paginate at all: `GetAllRequest` carries only `Filters`, so there is no
way to ask for a second page. Its comment — "`Next` and `Previous` are opaque cursors" — is also
wrong about the wire format.

Per Mem0's official API reference and the `get-memories.mdx` source, `POST /v3/memories/` paginates by
**offset, through URL query parameters**:

| Parameter | Rules |
|---|---|
| `page` | integer, **1-indexed**, minimum 1 |
| `page_size` | integer, 1–200; a community MCP wrapper reports the default as 10 |

`next` and `previous` are **ready-to-follow full URLs**, not cursor tokens, and `count` is the
**total number of memories matching the filters** — not the size of the current page.

Source: <https://docs.mem0.ai/api-reference/memory/get-memories> and
<https://github.com/mem0ai/mem0/blob/3e6ab394/docs/api-reference/memory/get-memories.mdx>.

Two caveats, recorded deliberately. The query-parameter mechanism, the `count` semantics, and the
URL form of `next` are corroborated by Mem0's own documentation. The default `page_size` of 10 comes
from a third-party wrapper and is **not** relied on. And this project's own history is the reason for
saying so: in Phase 3, five documented response-shape guesses were proven wrong by recording real
fixtures. Nothing here is re-guessed silently — the design below does not depend on any of these
details being right, because it verifies completeness instead of assuming it.

### 7.2 Completeness is proven, not assumed

`internal/mem0` gains a paginating enumeration. It:

1. Requests `page_size` at the documented maximum (200) to minimise round trips, iterating `page`
   until `next` is null, under a page cap.
2. **Verifies `len(collected) == count`** — the authoritative total Mem0 reports for the filter — and
   returns an **explicit error** on any mismatch, or when the page cap is hit.

Step 2 is the point. Rather than trusting that the loop followed every page, the enumeration checks
its result against the number Mem0 itself says should be there, and requires that number to stay
stable across pages. A mismatch, a page that disagrees with the first page's count, or a repeated
memory id means the store changed underneath us: completeness cannot be established, so it fails
loudly. Failing in that direction is always safe — a spurious error costs a retry, a spurious absence
claim corrupts the audit trail.

**What this check is, stated honestly.** It is a *consistency* check, not immunity to concurrent
writes, and the difference matters. With offset pagination a writer that deletes near the front while
we read can shift offsets so that an item is never returned while the total still matches; a
delete-plus-insert can even preserve the total while doing so. No count-based check can rule that out,
and claiming otherwise would be exactly the kind of overstatement this product exists to avoid. So the
guarantee is one direction only: *the enumeration is accepted only if the pages it read do not
contradict one another.* It is emphatically **not** "a consistent snapshot was read" — the
delete-at-front/insert-at-back case passes every check while an item is never returned, so pages that
agree are evidence of consistency, never proof of completeness. The residual risk is a scope being
written to during the walk, which is why the check fails closed instead of guessing, and why absence
claims remain `Reconstructed` with their basis recorded rather than being asserted as observed fact.

The existing one-page `GetAll` stays exactly as it is, with its misleading comment corrected, because
the tests rely on it and "return one page" is a truthful description of the HTTP contract.

### 7.3 The guarantee is enforced in the type system

Absence rules take a `CompleteEnumeration` value that only the paginating call can construct. An
incomplete or single-page listing therefore cannot be passed to an absence rule: the invalid claim
does not compile.

This mirrors the trick already used for `VisibilityTier`, where three constructors with disjoint
evidence types make "record a guess as a fact" unrepresentable. The same reasoning applies here — the
product's honesty property becomes structural rather than a convention someone has to remember.

**Cost accepted:** roughly fifteen lines of ceremony (the type, its single constructor, and accessors)
plus a negative-compile fixture in the existing `testdata/negative/` harness.

---

## 8. The `confidence` wire change

`CanonicalBytes` writes `Reason.Encode()` output directly into the hash input
(`internal/record/chain.go:70`). `confidence` lives in that envelope, so **its rendering is part of
the signed wire contract** and is therefore frozen the moment Phase 5 signs a real record.

Today it renders as `"confidence": 0`, which a compliance reader can reasonably read as "0% confident"
— an actively misleading number on a signed attestation, and the opposite of what this design is
defending.

**Change:** an unset `confidence` is omitted from the encoded reason. `confidence` is therefore
explicitly absent when Notary is making no probabilistic claim, and a future non-zero value would
still appear. The rule name and version carry the evidential weight instead.

**Why now, and not later.** Verified before deciding: the interceptor produces only `Observed` reasons
(`audit_unavailable`, `add_acknowledged`, `returned_by_search`, `search_performed`), and no non-test
code path anywhere writes a `Reconstructed` record. So no ledger in existence contains a `confidence`
value, and the format is still free to change. Once Phase 5 has signed records, the same change would
require a `notary/reason/v2` encoding bump — a real compatibility event rather than a one-line edit.

**Scope note:** this touches `internal/record/reason.go`, which is outside Phase 5's nominal package
list. That is deliberate and explicitly approved. The change must not alter any other part of the
envelope, and the existing round-trip test (asserting basis, rule, rule version and confidence all
survive `Encode` → `ParseReason`) must continue to pass unchanged.

---

## 9. Worklist derivation and failure behaviour

### 9.1 Worklist, in dependency order

Derived entirely from the ledger — no cursor, no queue, no side state. This is what makes re-running
safe.

1. **Unresolved adds** — `add_requested` records whose event id has no corresponding `add_resolved`.
   The event id is read from the `addPayload` Observed evidence (§4.2); both the request and the
   resolution record carry it, so the match is exact rather than heuristic. Poll `EventStatus`. A
   response that is still `PENDING` writes **nothing** and is reported as pending, so a stuck add is
   visible to an operator without polluting the chain.
2. **Scopes to enumerate** — scopes appearing in any memory-bearing record.
3. **Coverage candidates** — known memories versus later same-scope searches, filtered by the
   saturation predicate.
4. **Removal candidates** — known memories missing from the complete enumeration, corroborated
   against `History`.

Stages run in this order because later stages consume earlier results.

> **Corrected after execution (live verification against real Mem0).** That sentence was necessary
> but not sufficient, and the implementation got it wrong in a way no fixture caught. Folding the
> worklist ONCE before stage 1 ran meant stage 1's own discovery — the id of the memory a resolved
> add produced — was invisible to the stages that need it, so a fresh add took **two** passes: pass 1
> wrote `add_resolved`, pass 2 wrote `memory_kept`. Every fixture hid this, because each already
> carried a `memory_kept` record and so began with the chain pre-satisfied.
>
> `Reconcile` now re-folds after stage 1, over the window-filtered records **plus** that stage's own
> output, taken unfiltered. Stage 1 is not re-run, so the pass cannot loop, and re-folding only when
> stage 1 produced something leaves a pass with no adds exactly as it was.
>
> **A correction to this note, from code review.** It first justified taking that output unfiltered
> by claiming that re-filtering "would discard the very record that establishes the memory, because
> those records carry `At` = the EVENT time, which may precede `--since`". That is **false**:
> `buildAddResolved` copies `At` verbatim from the add record (`adds.go:140`), and the add only ever
> came from the filtered set, so `w.filter` over stage 1's output is a no-op. The behaviour is right
> either way — taking the output unfiltered avoids depending on that coincidence — but the reason
> given was wrong, so it is corrected here rather than quietly dropped.

### 9.2 Failure behaviour

The reconciler is **not** fail-open, and this is a deliberate departure from the interceptor.

Fail-open-loud exists to protect a customer's request path: an audit failure must never break an
application. The reconciler has no request to protect, and its entire output *is* the audit trail. A
silent partial pass there would produce a confusing state with no record of why, so a Mem0 or store
error fails the pass loudly with a non-zero exit code.

It writes **no `audit_gap`**. A gap entry means "a customer-visible operation happened that we failed
to record", which is not this situation. Inventing one would blur the meaning of the gap log, which
is itself audited.

**Partial progress is harmless and needs no rollback.** Each claim is appended in its own
transaction, so a mid-pass failure leaves a consistent chain, and the next pass resumes idempotently
by construction (§3, decision 4).

> **Corrected after execution (live verification).** The CLI reported `wrote N claim(s)` using the
> number of DERIVED claims rather than the number appended. Because `Append` deduplicates on the
> idempotency key and returns the stored record either way, a re-run printed `wrote 1 claim(s)` while
> appending nothing — on a tool whose scheduling story rests on idempotency, an operator could not
> tell whether the ledger had changed. The report now reads the chain position either side of each
> `Append` and distinguishes `appended` from `present`, with a summary separating derived from
> appended. That same count corrects the mid-pass failure message, which over-reported by the same
> reasoning.

### 9.3 What this phase does not do about gaps

§8 of the parent spec says "a later `notary reconcile` can often close the gap by re-deriving the
missing record". That capability already exists as `AuditWriter.ReplayGaps`, but it takes the caller's
records through a lookup function, so it cannot be driven from a CLI — the caller holds the record,
not Notary. Phase 5 therefore leaves gap replay as a library API and **does not pretend** that
`notary reconcile` performs it. `notary reconcile` closes gaps in *knowledge*, not gap-log replay.

---

## 10. Testing strategy

**Unit tests** drive each rule against the existing recorded fixtures over `httptest`. No network, no
API key, consistent with the parent spec's acceptance criterion that a live test is opt-in and never a
gate.

**Integrity properties**, asserted directly rather than assumed:

1. Re-running `notary reconcile` against an unchanged store appends **zero** records. This is the
   property that makes the command safe to schedule, and it is the one most likely to regress
   silently.
2. No absence claim is reachable from an incomplete enumeration — guaranteed by the type, proven by a
   negative-compile fixture.
3. Every `Reconstructed` claim carries a registry rule name and a non-empty basis.
4. A still-`PENDING` add produces no record.
5. `--dry-run` leaves the chain head and `Seq` unchanged.
6. The three interceptor-written evidence payloads encode to byte-identical JSON after the type move
   into `internal/mem0` (§4.2). This is the guard that keeps ledgers written by Phases 3–4
   verifiable, and it is the test most likely to be skipped by someone who has not read why the
   payload bytes are hashed.

**Mutation tests**, in the project's established style — deliberately break the code and require the
tests to fail, so the suite is proven non-vacuous rather than merely green: flip the saturation
predicate to `<=` and require the absence tests to fail; attempt to pass a partial page set to an
absence rule and require a compile error.

---

## 11. Deltas to the parent spec

The parent spec remains the source of truth; Phase 5 writes these back into it.

| Parent section | Delta |
|---|---|
| §5 / §6 | "Covering search" is defined as the saturation predicate. §6 previously said "a covering `search`" without definition, and the entire `absent_from_search` claim rests on it. |
| §6 | The `add_resolved` reason kinds are specified (§5.1 here): `stored_by_mem0` on success, `add_failed` on failure, `no_facts_extracted` when nothing was extracted. |
| §7 | The claim-identity rule is made explicit. "Derived claims … so re-running Reconcile adds nothing" only holds if the reconciliation *run* never becomes part of the key. |
| §7 / §8 | The `confidence` encoding change (§8 here), including the note that an unset value is omitted rather than rendered as zero. |
| §10 | `ReconcileMode` is implemented as `Command` only, with `InProcess` reserved — documented the same way `FailClosed` already is. |
| §13 | Phase 5 status updated on completion. |
| §5 | `Reason`'s rule/version permanence stated as a contract: names are stable and versions bump only on a change of meaning, because both are recorded inside signed records. |

---

## 12. Out of scope

Not built in this phase, and not to be reported as missing:

- The in-process reconciliation loop (`ReconcileMode.InProcess` is reserved)
- `explain`, `export`, `replay`
- Sensitivity rules and redaction (`SensitivityRules`, §10 of the parent spec) — Phase 6
- CLI-driven gap replay (§9.3)
- Any mutation or deletion of existing records. The reconciler only ever appends.
