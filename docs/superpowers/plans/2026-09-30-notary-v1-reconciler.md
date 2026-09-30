# Notary v1 Phase 5 — The Reconciler: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the reconciler — the component that goes back to Mem0 after an operation and derives the claims the ledger cannot observe directly (`add_resolved`, `memory_kept`, `memory_dropped`), as the first and only producer of `Reconstructed` and `Internal` records.

**Architecture:** A one-shot `notary reconcile` command derives its entire worklist from the existing ledger — no cursor, no queue, no side state — so re-running is a no-op by construction. Claims are keyed on their subject and the rule that justified them, never on the reconciliation run. Absence may only be claimed from an enumeration that has *proven* its own completeness (`len(collected) == count`), and that proof is enforced by a type only the paginating client call can construct.

**Tech Stack:** Go 1.25, `modernc.org/sqlite`, cobra, testify. No new dependency.

**Spec:** `docs/superpowers/specs/2026-09-30-notary-v1-reconciler-design.md` — read it in full. It is the source of truth for every decision here and explains *why* each one was taken. Parent spec for surrounding context: `docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md` §4–§10, §13.

## Global Constraints

- Go 1.25; module `notary`. Dependencies stay cobra, testify, `modernc.org/sqlite` — **no new dependency without asking the user first**.
- Never panic in library code. Wrap errors as `fmt.Errorf("...: %w", err)`.
- No global mutable state. No hardcoded secrets or signing keys — env vars or OS keychain only.
- Never log key material or raw sensitive memory content.
- Every record is written through `ledger.Append`, which alone assigns `Seq` and computes `Hash`. Nothing in `internal/reconcile` may set either.
- `VisibilityTier` must remain unconstructible without an explicit value; `record` is always the source of truth and no code path treats LLM text as authoritative.
- Fail-open-loud only — nothing implements fail-closed in v1. **Exception, deliberate:** the reconciler fails loudly and writes no `audit_gap` (spec §9.2).
- Tests never touch the network. The Mem0 client is tested against recorded fixtures in `internal/mem0/testdata/` over `httptest`; a live test is opt-in behind a build tag and is never a gate.
- All work on branch `feat/reconciler`. **Never commit to `master`.**
- Run packages individually, not `go test ./...`: `internal/ledger` takes ~30s on its own (its crash test SIGKILLs a writer) and the shell has a 2-minute limit.

## Review Focus

Failure modes the spec implies but no single task's tests fully exercise, most likely first:

1. **A `get_all` spanning more than one page.** `len(collected)` must equal `count` or the pass must error. A silently short list fabricates `absent_from_search` claims — the worst outcome in the system.
2. **`top_k == 0`, or a result count equal to `top_k`.** Must produce **no** `absent_from_search` claim. `0` is a real risk: `len(results) < 0` is never true, but an implementation that writes `<=` would fire on every search.
3. **An add whose event is still `PENDING`, or whose `EventStatus` call fails.** Must write nothing, and must not abort the whole pass in a way that leaves already-derived claims half-appended without explanation.
4. **A ledger holding `memory_surfaced` records for a search but no listing that ever knew the memory.** The reconciler must not invent a removal; `absent_from_search` requires a memory *known to exist*, not merely one that appeared once.
5. **The command run twice against unchanged state.** The second run must append zero records. This is the property that makes it safe to schedule, and the one most likely to regress silently.

---

### Task 1: Omit an unset `confidence` from the encoded reason

**Files:**
- Modify: `internal/record/reason.go` (the `reconstrEnvelope` struct, ~line 411)
- Test: `internal/record/reason_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces: no signature change. `NewReconstructedEvidence(basis []RecordID, rule, ruleVersion string, confidence float64)` keeps its signature and behaviour.

- [ ] **Step 1: Write the failing test**

In `internal/record/reason_test.go`, assert on the *encoded bytes*, not on the struct:

```go
func TestReconstructedReasonOmitsUnsetConfidence(t *testing.T) {
    ev, err := NewReconstructedEvidence([]RecordID{"r-1"}, "absent_from_search", "1", 0)
    require.NoError(t, err)
    r, err := NewReconstructedReason(ReasonAbsentFromSearch, ev)
    require.NoError(t, err)
    b, err := r.Encode()
    require.NoError(t, err)
    assert.NotContains(t, string(b), "confidence",
        "an unset confidence must be absent, not rendered as 0")
}

func TestReconstructedReasonKeepsNonZeroConfidence(t *testing.T) {
    // same construction with confidence 0.5
    assert.Contains(t, string(b), `"confidence":0.5`)
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/record/ -run TestReconstructedReasonOmitsUnsetConfidence -v`
Expected: FAIL — `confidence` is present as `"confidence":0`.

- [ ] **Step 3: Change the tag**

In the `reconstrEnvelope` struct change `Confidence  float64 \`json:"confidence"\`` to carry `,omitempty`. Change nothing else about the envelope — its field order and the other tags are part of the hashed format.

- [ ] **Step 4: Run the whole package, not just the new test**

Run: `go test ./internal/record/ -v`
Expected: PASS, including the existing round-trip test asserting basis, rule, rule version and confidence all survive `Encode` → `ParseReason` (an absent `confidence` decodes back to 0). If a golden-bytes assertion elsewhere in the package pins the old form, update it and say so in the commit message — that is the one acceptable edit outside the new test.

- [ ] **Step 5: Commit**

```bash
git add internal/record/reason.go internal/record/reason_test.go
git commit -m "fix(record): omit an unset confidence from the encoded reason

The reconstruction envelope is inside the hashed region, so an unset
confidence rendered as \"confidence\":0, which reads as \"0% confident\" on a
signed claim. Verified free to change now: no non-test code path has ever
written a Reconstructed record."
```

---

### Task 2: Move the evidence payload types into `internal/mem0`

**Files:**
- Create: `internal/mem0/evidence.go`
- Modify: `internal/interceptor/library/mem0.go` (remove the three local types ~lines 482–508; use the `mem0` ones)
- Test: `internal/interceptor/library/evidence_golden_test.go` (new), `internal/mem0/evidence_test.go` (new)

**Interfaces:**
- Consumes: `mem0.Filters` (already imported by `interceptor/library`).
- Produces: exported types in package `mem0`, with these field names, **order**, and tags exactly:

```go
type AddPayload struct {
    EventID string `json:"event_id"`
    Status  string `json:"status"`
}

type SearchPerformedPayload struct {
    Query     string  `json:"query"`
    Filters   Filters `json:"filters"`
    TopK      int     `json:"top_k"`
    Threshold float64 `json:"threshold"`
    Rerank    bool    `json:"rerank"`
    Count     int     `json:"count"`
}

type MemorySurfacedPayload struct {
    Score float64 `json:"score"`
    Rank  int     `json:"rank"`
}
```

**Why this task is a refactor with a byte-identity guard.** `ObservedEvidence.Payload` is embedded in `Reason.Encode()` output, which `CanonicalBytes` writes straight into the hash input. These three payloads are therefore already hashed inside real ledgers written by Phases 3–4. Go's `encoding/json` emits struct fields in declaration order, so reordering a field would change the bytes and make every one of those records fail `verify`. The golden test is the guard, and it is written against the *old* types first so it characterises current behaviour rather than restating the intended one.

- [ ] **Step 1: Write a characterisation test against the current types**

`internal/interceptor/library/evidence_golden_test.go`, white-box (package `library`), marshalling the existing unexported types with fixed values and pinning the exact JSON:

```go
func TestEvidencePayloadGoldenBytes(t *testing.T) {
    add, err := json.Marshal(addPayload{EventID: "evt-1", Status: "PENDING"})
    require.NoError(t, err)
    assert.Equal(t, `{"event_id":"evt-1","status":"PENDING"}`, string(add))
    // plus one case each for searchPerformedPayload and memorySurfacedPayload,
    // with every field set to a non-zero value so field order is fully pinned
}
```

- [ ] **Step 2: Run it — it should pass unchanged**

Run: `go test ./internal/interceptor/library/ -run TestEvidencePayloadGoldenBytes -v`
Expected: PASS. This is a characterisation test: it documents today's bytes. If it fails, the expected strings are wrong, not the code.

- [ ] **Step 3: Move the types and update their users**

Create `internal/mem0/evidence.go` with the three exported types exactly as specified above. Delete the unexported ones from `internal/interceptor/library/mem0.go` and update its call sites to `mem0.AddPayload`, `mem0.SearchPerformedPayload`, `mem0.MemorySurfacedPayload`.

- [ ] **Step 4: Re-point the golden test at the moved types and require identical bytes**

Replace the test's constructors with the `mem0.*` types, using the **same field values**. The expected strings must not change — that is the entire assertion.

Run: `go test ./internal/mem0/ -run TestEvidencePayloadGoldenBytes -v`
Expected: PASS with the same expected strings as Step 1.

- [ ] **Step 5: Prove the guard actually guards (mutation check)**

Temporarily swap `Status` and `EventID` in the struct declaration, run the golden test, and confirm it FAILS. Revert. Report the observed output in the task report — this is what shows the test is not vacuous.

- [ ] **Step 6: Run the affected packages**

Run: `go test ./internal/mem0/ ./internal/interceptor/... ./internal/ledger/`
Expected: all PASS. `internal/ledger` is the one that would catch a byte change, because its fixtures are real records.

- [ ] **Step 7: Commit**

```bash
git add internal/mem0/evidence.go internal/mem0/evidence_test.go \
        internal/interceptor/library/mem0.go internal/interceptor/library/evidence_golden_test.go
git commit -m "refactor(mem0): move evidence payloads so the reconciler can read them

These payload bytes are inside the canonical hash, so the move preserves
field order and tags exactly; a golden-bytes test pins them."
```

---

### Task 3: Paginating enumeration and `CompleteEnumeration`

**Files:**
- Create: `internal/mem0/enumeration.go`
- Modify: `internal/mem0/client.go` (`GetAllRequest` gains page fields; `GetAll` sends query params; correct the wrong "opaque cursors" comment on `GetAllResponse`)
- Create: `internal/mem0/enumeration_test.go`

**Interfaces:**
- Consumes: `GetAllResponse{Count int, Next *string, Previous *string, Results []Memory}`, `Memory`.
- Produces:

```go
// CompleteEnumeration is a listing of one scope whose completeness has been
// verified against the total Mem0 reported. The zero value is invalid: only
// GetAllComplete can construct a usable one.
type CompleteEnumeration struct{ e *enumeration }

func (c CompleteEnumeration) Valid() bool
func (c CompleteEnumeration) Items() []Memory   // nil when !Valid
func (c CompleteEnumeration) Len() int
func (c CompleteEnumeration) Count() int        // the count Mem0 reported

var ErrEnumerationIncomplete = errors.New("mem0: enumeration incomplete")

type GetAllRequest struct {
    Filters  Filters `json:"filters"`
    Page     int     `json:"-"`
    PageSize int     `json:"-"`
}

func (c *Client) GetAllComplete(ctx context.Context, req GetAllRequest) (CompleteEnumeration, error)
```

`Page` and `PageSize` are `json:"-"` because Mem0 takes them as **URL query parameters**, not body fields — sending them in the body would silently change nothing and make the missing pagination invisible.

- [ ] **Step 1: Write the failing tests**

In `internal/mem0/enumeration_test.go`, over `httptest` servers (no network):

```go
func TestGetAllCompleteWalksEveryPage(t *testing.T)
    // server returns page 1 of 2 with next set, then page 2 with next null,
    // and count=3 on both; expect Len()==3, Valid()==true

func TestGetAllCompleteRejectsShortRead(t *testing.T)
    // server reports count=5 but pages yield 3; expect ErrEnumerationIncomplete
    // and a zero (invalid) CompleteEnumeration

func TestGetAllCompleteRejectsExcessivePages(t *testing.T)
    // server always returns a non-null next; expect ErrEnumerationIncomplete
    // once the page cap is reached

func TestZeroCompleteEnumerationIsInvalid(t *testing.T)
    assert.False(t, mem0.CompleteEnumeration{}.Valid())
    assert.Nil(t, mem0.CompleteEnumeration{}.Items())
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/mem0/ -run 'GetAllComplete|ZeroComplete' -v`
Expected: FAIL — `GetAllComplete` is undefined.

- [ ] **Step 3: Implement**

`GetAllComplete` requests pages with `page_size` at the documented maximum of 200, iterating `page` from 1 until `Next` is null, under a page cap constant (`maxEnumerationPages`). It then **verifies `len(collected) == Count`** and returns `ErrEnumerationIncomplete` on mismatch or when the cap is hit. Only after that check passes does it construct the value. Accessors on an invalid value return zero values rather than panicking.

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./internal/mem0/ -v`
Expected: all PASS, including the pre-existing one-page `GetAll` tests.

- [ ] **Step 5: Commit**

```bash
git add internal/mem0/enumeration.go internal/mem0/enumeration_test.go internal/mem0/client.go
git commit -m "feat(mem0): add a paginating enumeration that proves its own completeness

count is the total Mem0 reports, so len(collected)==count is a real
completeness check rather than an assumption. Also corrects the GetAllResponse
comment: next/previous are ready-to-follow URLs, not opaque cursors."
```

---

### Task 4: The rule registry

**Files:**
- Create: `internal/reconcile/rules.go`, `internal/reconcile/rules_test.go`

**Interfaces:**
- Consumes: `record.ReasonKind`, `record.VisibilityTier` (via `AllowedTier()`).
- Produces:

```go
type Rule struct {
    Name    string
    Version string            // "1"
    Kind    record.ReasonKind
    Summary string            // what the rule asserts, for a reader years later
}

const (
    RuleKeptByContentMatch = "kept_by_content_match"
    RuleAbsentFromSearch   = "absent_from_search"
    RuleNoFactsExtracted   = "no_facts_extracted"
    RuleRemovedByMem0      = "removed_by_mem0"
)

func Rules() []Rule
func LookupRule(name string) (Rule, bool)
```

- [ ] **Step 1: Write the failing test**

```go
func TestRulesAreWellFormed(t *testing.T)
    // Names unique; Version non-empty; Summary non-empty; and -- the property
    // that matters -- every rule's Kind.AllowedTier() is Reconstructed or
    // Internal. A rule can never justify an Observed claim, which has no rule.

func TestRegistryCoversEveryDerivedKind(t *testing.T)
    // the four derived reason kinds are each reachable by exactly one rule
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/reconcile/ -run TestRules -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement the registry**

One table with the four v1 entries, each carrying the `Summary` text from spec §6. No rule may be tunable from config; this is a constant table in code.

- [ ] **Step 4: Run and confirm pass**

Run: `go test ./internal/reconcile/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/reconcile/rules.go internal/reconcile/rules_test.go
git commit -m "feat(reconcile): add the versioned rule registry"
```

---

### Task 5: Claims keyed on their subject, plus `Window` and the ledger fold

**Files:**
- Modify: `internal/record/idem.go` (add `DeriveClaimIdemKey`)
- Create: `internal/reconcile/reconcile.go`, `internal/reconcile/worklist.go`, `internal/reconcile/reconcile_test.go`

**Interfaces:**
- Consumes: `ledger` read methods, `mem0.Client`, `record` types.
- Produces:

```go
// record/idem.go -- a distinct domain so derived keys can never collide with
// request-path keys.
const claimIdemDomain = "notary/idem/claim/v1"

func DeriveClaimIdemKey(kind EventType, reasonKind ReasonKind,
    memoryID, contentHash, ruleVersion string) (IdemKey, error)

// reconcile
type Window struct {
    Since time.Time     // zero for no bound; filters on At, NOT RecordedAt
    Scope record.Scope  // zero fields for no scope filter
}

type Reader interface {
    ListRecords(from, to time.Time) ([]record.Record, error)
}

type Reconciler struct { /* unexported */ }

func New(r Reader, c *mem0.Client) *Reconciler
func (rc *Reconciler) Reconcile(ctx context.Context, w Window) ([]record.Record, error)
```

Nothing about the reconciliation run may enter `DeriveClaimIdemKey` — that is what makes a second pass a no-op.

- [ ] **Step 1: Write the failing test**

```go
func TestDeriveClaimIdemKeyIsStableAndSubjectSpecific(t *testing.T)
    // same inputs -> identical key across calls
    // different memory id, content hash, rule version, or reason kind -> different key
    // a key from DeriveClaimIdemKey never equals one from DeriveIdemKey
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./internal/record/ -run TestDeriveClaimIdemKey -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement `DeriveClaimIdemKey`**

Same shape as the existing `DeriveIdemKey`: a distinct domain constant, length-prefixed fields, lowercase hex, `ErrIdemKeyUnavailable` on failure. Follow the existing function rather than inventing a second encoding.

- [ ] **Step 4: Write the failing tests for the fold**

```go
func TestWorklistFindsUnresolvedAdds(t *testing.T)
    // an add_requested with no matching add_resolved is unresolved
func TestWorklistExcludesResolvedAdds(t *testing.T)
func TestWindowSinceFiltersOnAt(t *testing.T)
    // a record whose At is before Since but whose RecordedAt is after is EXCLUDED
func TestWindowScopeFilter(t *testing.T)
```

- [ ] **Step 5: Run them and watch them fail**, then implement `New`, `Reconcile`, and the fold.

`Reconcile` reads via `Reader.ListRecords`, folds the ledger into a worklist in dependency order (unresolved adds → scopes to enumerate → coverage candidates → removal candidates), calls the per-stage producers, and returns the accumulated records. Every returned record has a **zero `Seq` and zero `Hash`**.

- [ ] **Step 6: Run the package and commit**

Run: `go test ./internal/record/ ./internal/reconcile/ -v`

```bash
git add internal/record/idem.go internal/reconcile/
git commit -m "feat(reconcile): derive the worklist from the ledger, keyed per subject"
```

---

### Task 6: The `add_resolved` producers

**Files:**
- Create: `internal/reconcile/adds.go`, `internal/reconcile/adds_test.go`

**Interfaces:**
- Consumes: `mem0.AddPayload` (Task 2), `mem0.EventStatusResponse{Status, Results []EventResult, CompletedAt}`, `Reconcile`/`Reconciler` (Task 5), `RuleNoFactsExtracted` (Task 4).
- Produces: `func (rc *Reconciler) resolveAdd(ctx context.Context, add record.Record) ([]record.Record, error)` — returns 0 or 1 records.

Emits, per spec §5 rows 1–3:

| Event status | Record |
|---|---|
| `SUCCEEDED`, results non-empty | `EventAddResolved` + `ReasonStoredByMem0`, `Observed` |
| `FAILED` | `EventAddResolved` + `ReasonAddFailed`, `Observed` |
| `SUCCEEDED`, results empty | `EventAddResolved` + `ReasonNoFactsExtracted`, `Reconstructed` (rule `no_facts_extracted` v1, basis = the add record id) |
| anything else (e.g. still `PENDING`) | **nothing** |

- [ ] **Step 1: Write the failing tests**

```go
func TestResolveAddWritesNothingWhilePending(t *testing.T)
func TestResolveAddRecordsObservedSuccess(t *testing.T)
func TestResolveAddRecordsObservedFailure(t *testing.T)
func TestResolveAddInfersNoFactsExtractedFromEmptyResults(t *testing.T)
    // tier is Reconstructed, rule is no_facts_extracted, version "1",
    // basis contains the add record id, and confidence is left at zero
```

Each test builds a fixture `EventStatusResponse` served over `httptest`.

- [ ] **Step 2: Run them and watch them fail**, then implement.

Read the event id from the add record's `mem0.AddPayload`; call `EventStatus`; map the status as the table above. The `Observed` records carry the event-status response as their evidence payload; the `Reconstructed` one carries basis, rule and version and **no confidence**.

- [ ] **Step 3: Run and commit**

Run: `go test ./internal/reconcile/ -v`

```bash
git add internal/reconcile/adds.go internal/reconcile/adds_test.go
git commit -m "feat(reconcile): resolve add outcomes from event status"
```

---

### Task 7: The `memory_kept` producers

**Files:**
- Create: `internal/reconcile/kept.go`, `internal/reconcile/kept_test.go`

**Interfaces:**
- Consumes: `CompleteEnumeration` (Task 3), `mem0.SearchPerformedPayload`, `RuleKeptByContentMatch`.
- Produces: `func (rc *Reconciler) resolveKept(ctx context.Context, scope record.Scope, known []knownMemory) ([]record.Record, error)`.

Emits, per spec §5 rows 4–5:

| Condition | Record |
|---|---|
| the add's produced memory id appears in the complete enumeration | `EventMemoryKept` + `ReasonStoredByMem0`, `Observed` |
| the id is absent but a listed memory's content hash equals the submitted text | `EventMemoryKept` + `ReasonKeptByContentMatch`, `Reconstructed` (rule v1, basis = the add record id and the listing) |

- [ ] **Step 1: Write the failing tests**

```go
func TestKeptObservedWhenMemoryIDMatches(t *testing.T)
func TestKeptReconstructedWhenOnlyContentHashMatches(t *testing.T)
    // asserts tier Reconstructed, rule kept_by_content_match, version "1"
func TestKeptWritesNothingWhenMemoryIsNeitherMatchedNorPresent(t *testing.T)
func TestKeptOnlyClaimsFromACompleteEnumeration(t *testing.T)
    // first page omits the memory and count disagrees -> the pass errors and
    // writes NO memory_kept claim
```

- [ ] **Step 2: Run them and watch them fail**, then implement.

- [ ] **Step 3: Run and commit**

Run: `go test ./internal/reconcile/ -v`

```bash
git add internal/reconcile/kept.go internal/reconcile/kept_test.go
git commit -m "feat(reconcile): derive memory_kept from a complete enumeration"
```

---

### Task 8: The `absent_from_search` producer

**Files:**
- Create: `internal/reconcile/absent.go`, `internal/reconcile/absent_test.go`

**Interfaces:**
- Consumes: `mem0.SearchPerformedPayload` (Task 2), `RuleAbsentFromSearch`, `DeriveClaimIdemKey`.
- Produces: `func (rc *Reconciler) resolveAbsent(known []knownMemory, searches []record.Record) ([]record.Record, error)`.

Emits `EventMemoryDropped` + `ReasonAbsentFromSearch`, `Reconstructed`, rule `absent_from_search` v1, basis = the prior listing record and the search record.

**The saturation predicate is the whole rule:** claim only when `search.Count < search.TopK`. When the result set was truncated by `top_k`, absence proves nothing and no claim is written. Read `TopK` and `Count` from the `searchPerformedPayload`.

- [ ] **Step 1: Write the failing tests**

```go
func TestAbsentClaimedWhenResultsAreFewerThanTopK(t *testing.T)   // Count 2, TopK 5
func TestAbsentNotClaimedWhenResultsFillTopK(t *testing.T)        // Count 5, TopK 5
func TestAbsentNotClaimedWhenTopKIsZero(t *testing.T)             // Count 0, TopK 0
func TestAbsentNotClaimedForAMemoryThatWasNeverKnown(t *testing.T)
    // a memory that only ever appeared in search results, never in a listing,
    // must not produce an absence claim
func TestAbsentClaimRecordsRuleAndParameters(t *testing.T)
    // rule absent_from_search, version "1", basis non-empty
```

- [ ] **Step 2: Run them and watch them fail**, then implement.

- [ ] **Step 3: Mutation-check the predicate**

Temporarily change `<` to `<=` in the predicate and confirm `TestAbsentNotClaimedWhenResultsFillTopK` FAILS. Revert. Record the observed output in the task report.

- [ ] **Step 4: Run and commit**

Run: `go test ./internal/reconcile/ -v`

```bash
git add internal/reconcile/absent.go internal/reconcile/absent_test.go
git commit -m "feat(reconcile): claim absence only from a saturated search"
```

---

### Task 9: The `removed_by_mem0` producer

**Files:**
- Create: `internal/reconcile/removed.go`, `internal/reconcile/removed_test.go`

**Interfaces:**
- Consumes: `mem0.CompleteEnumeration`, `mem0.HistoryResponse` / `HistoryEvent{Event, OldMemory, NewMemory}`, `RuleRemovedByMem0`.
- Produces: `func (rc *Reconciler) resolveRemoved(ctx context.Context, e mem0.CompleteEnumeration, known []knownMemory) ([]record.Record, error)`.

Emits `EventMemoryDropped` + `ReasonRemovedByMem0`, **`Internal`**, rule `removed_by_mem0` v1, only when a known memory is absent from the complete enumeration **and** its history shows a `DELETE`/`UPDATE`. The record's `InternalNote` states that Mem0 exposed no reason.

- [ ] **Step 1: Write the failing tests**

```go
func TestRemovedInternalWhenHistoryShowsDelete(t *testing.T)
    // asserts tier Internal, and that the note is an opacity marker
func TestRemovedNotClaimedWhenMemoryIsStillListed(t *testing.T)
func TestRemovedNotClaimedFromAnIncompleteEnumeration(t *testing.T)
func TestRemovedRecordsNoGuessAtWhy(t *testing.T)
    // the note must not contain "threshold", "expired", "duplicate" or any
    // other invented cause
```

- [ ] **Step 2: Run them and watch them fail**, then implement.

- [ ] **Step 3: Run and commit**

Run: `go test ./internal/reconcile/ -v`

```bash
git add internal/reconcile/removed.go internal/reconcile/removed_test.go
git commit -m "feat(reconcile): record store removals as Internal, without guessing why"
```

---

### Task 10: `config.ReconcileMode`

**Files:**
- Create: `internal/reconcile/mode.go`, `internal/reconcile/mode_test.go`
- Modify: `config/config.go` (`Config` gains `ReconcileMode`; `LoadFrom` defaults it)

**Interfaces:**
- Produces:

```go
type ReconcileMode uint8

const (
    ReconcileCommand ReconcileMode = iota + 1
    ReconcileInProcess
)

func (m ReconcileMode) String() string
func (m ReconcileMode) Valid() bool
```

`config.Config` gains `ReconcileMode reconcile.ReconcileMode`, defaulted by `LoadFrom` to `ReconcileCommand`. This mirrors how `FailMode` already lives in `internal/interceptor` with `config` importing it.

- [ ] **Step 1: Write the failing tests**

```go
func TestReconcileModeZeroValueIsInvalid(t *testing.T)
func TestReconcileInProcessIsReservedNotImplemented(t *testing.T)
    // InProcess is Valid()==true but no code path implements it; assert the
    // String() forms so a future implementer sees the intent
func TestConfigDefaultsToReconcileCommand(t *testing.T)
```

- [ ] **Step 2: Run them and watch them fail**, then implement. Document in the type's comments that `ReconcileInProcess` is defined but reserved, matching `FailClosed`.

- [ ] **Step 3: Run and commit**

Run: `go test ./internal/reconcile/ ./config/ -v`

```bash
git add internal/reconcile/mode.go internal/reconcile/mode_test.go config/config.go
git commit -m "feat(config,reconcile): add ReconcileMode with InProcess reserved"
```

---

### Task 11: The `notary reconcile` command

**Files:**
- Create: `cmd/notary/reconcile.go`, `cmd/notary/reconcile_test.go`
- Modify: `cmd/notary/root.go` (register the command — one `cmd.AddCommand(newReconcileCmd())`)

**Interfaces:**
- Consumes: `reconcile.New`, `(*Reconciler).Reconcile`, `config.LoadFrom`, `ledger.Append`, `sign`.
- Produces: `func newReconcileCmd() *cobra.Command` and `func runReconcile(cmd *cobra.Command, cfg *config.Config) error` — a testable seam, matching the existing `runVerify`/`runGaps` pattern. Flags are read with `cmd.Flags().GetString(...)`, never from a global.

Flags: `--since`, `--user-id`, `--agent-id`, `--app-id`, `--run-id`, `--dry-run`.

- [ ] **Step 1: Write the failing tests**

```go
func TestReconcileRequiresASigningKey(t *testing.T)
    // by spec 8, config time with no key is a hard failure: the process
    // refuses to start rather than writing unsigned records
func TestReconcileDryRunAppendsNothing(t *testing.T)
    // assert the ledger head and Seq are unchanged after a --dry-run pass
func TestReconcileSinceFiltersOnAtNotRecordedAt(t *testing.T)
func TestReconcileHelpListsItsFlags(t *testing.T)
```

- [ ] **Step 2: Run them and watch them fail**, then implement.

The command builds a store, signer and ledger from config, constructs `reconcile.New(ledger, mem0.NewClient(cfg.Mem0BaseURL, cfg.Mem0APIKey, nil))`, calls `Reconcile`, and — unless `--dry-run` — appends each record through `ledger.Append`, printing one line per claim. It exits non-zero on any error (spec §9.2) and writes no `audit_gap`.

A missing `NOTARY_MEM0_API_KEY` must fail with an error naming the variable, not with a bare HTTP 401.

- [ ] **Step 3: Run and commit**

Run: `go test ./cmd/notary/ -v`

```bash
git add cmd/notary/reconcile.go cmd/notary/reconcile_test.go cmd/notary/root.go
git commit -m "feat(cmd): add notary reconcile"
```

---

### Task 12: End-to-end integrity tests, the negative fixture, and the README

**Files:**
- Create: `internal/reconcile/integrity_test.go`, `internal/reconcile/negative_test.go`, `internal/reconcile/testdata/negative/construct_complete_enumeration.go.txt`
- Modify: `README.md` (Status section)

**Interfaces:**
- Consumes: everything above.
- Produces: no new API.

- [ ] **Step 1: Write the property tests**

```go
func TestSecondPassAppendsZeroRecords(t *testing.T)
    // the headline property: reconcile, append everything, then reconcile
    // again over the SAME store and ledger; every derived key must already
    // exist, so the second pass appends nothing
func TestDryRunThenRealPassIsStable(t *testing.T)
func TestNoAbsenceClaimIsProducedFromAnIncompleteEnumeration(t *testing.T)
func TestEveryReconstructedClaimNamesARegistryRule(t *testing.T)
    // walks all produced records: each Reconstructed claim's rule+version must
    // be in the registry, and its basis must be non-empty
```

- [ ] **Step 2: Add the negative-compile harness and fixture**

Mirror `internal/record/negative_test.go`: compile the fixture with the Go toolchain and require it to FAIL, matching a specific compiler message. The fixture attempts `mem0.CompleteEnumeration{e: ...}` and must fail with an unknown-field error.

Also assert the runtime half: `mem0.CompleteEnumeration{}` is `!Valid()` and passing it to an absence rule yields an error rather than a claim.

Run: `go test ./internal/reconcile/ -run Negative -v`
Expected: PASS, having observed a genuine compile failure.

- [ ] **Step 3: Run the affected packages**

Run: `go test ./internal/reconcile/ ./internal/mem0/ ./cmd/notary/ ./config/ -v`
Then, separately: `go test ./internal/ledger/` (~30s, run on its own).
Expected: all PASS.

- [ ] **Step 4: Update the README Status section**

Move the reconciler out of "Not built yet" into the working list, stating plainly that `ReconcileMode.InProcess` remains reserved and that gap replay is still a library API with no CLI. Do not claim anything the tests do not show.

- [ ] **Step 5: Commit**

```bash
git add internal/reconcile/ README.md
git commit -m "test(reconcile): pin idempotency, exhaustiveness and rule provenance"
```

---

## Self-Review

**Spec coverage.** §4 surfaces → Tasks 3, 10, 11 (plus 5 for the API). §5 catalogue rows 1–3 → Task 6; rows 4–5 → Task 7; row 6 → Task 8; row 7 → Task 9. §6 registry → Task 4, with the provenance test in Task 12. §7 exhaustiveness → Task 3 and the negative fixture in Task 12. §8 confidence → Task 1. §9.1 worklist → Task 5; §9.2 failure behaviour → Task 11. §10 testing → Tasks 5–9 and 12. §4.2 evidence reading → Task 2. §12 out-of-scope items appear in no task.

**Type consistency.** `CompleteEnumeration` is constructed only by `GetAllComplete` (Task 3) and consumed by Tasks 7 and 9. `Rule`/`Rules()`/`LookupRule` are defined once in Task 4. `DeriveClaimIdemKey` is defined in Task 5 and used by Tasks 6–9. `knownMemory` is introduced in Task 5's fold and consumed by Tasks 7–9. `Window`/`Reader`/`New`/`Reconcile` are defined in Task 5 and used in Task 11. `ReconcileMode` is defined in Task 10 and consumed by config and Task 11.

**One consequence worth stating explicitly, not fixed here.** Because claims are keyed on memory plus rule (spec §3 decision 4), a memory that several later covering searches each omit produces **one** `absent_from_search` claim, whose basis is the first such covering search. The searches themselves remain recorded as `search_performed`, so the reader can still see they happened; only the derived claim is single. That is the deliberate cost of keeping the ledger bounded. If per-search absence claims are wanted instead, the fix is to add the search record id to `DeriveClaimIdemKey` — one line, but it changes the ledger's growth characteristics, so it is flagged rather than assumed.

**Places a reviewer should look hardest:** Task 3's completeness check (the only thing standing between a short read and fabricated absence claims), Task 8's predicate direction (`<` not `<=`), and Task 12's idempotency property (the promise that makes the command safe to schedule).
