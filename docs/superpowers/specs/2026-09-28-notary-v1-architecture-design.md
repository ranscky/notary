# Notary v1 — Architecture Design

**Date:** 2026-09-28
**Status:** Approved for planning
**Scope:** v1 (library mode). Proxy mode is Phase 9 of the same build but is not the subject of this document. (Corrected after implementation: Phase 9 shipped, so v1 is no longer library-mode only; "(library mode)" scopes this document, not v1.)

---

## 1. What Notary is

Notary is a Go CLI and library that sits between an application and Mem0 and produces a signed, tamper-evident, tier-labeled audit trail explaining **why** a memory was kept, dropped, or surfaced. Its audience is a compliance lead who must explain an agent's memory decisions without reading code or pulling in an engineer.

### Goals

- Every claim about *why* a memory did something is recorded, signed, and hash-chained.
- Every such claim carries an **epistemic tier** recording how Notary came to know it.
- Mem0's own internal decisions are reported as opaque rather than guessed at.
- Auditability never becomes an uptime risk: the observation path fails **open but loud**.
- A tamper-evident record is verifiable without trust in Notary's runtime.

### Non-goals for v1

- **No enforcement.** `FailClosed` exists as a config value and is never implemented; v1 has no function that can block or deny a memory operation.
- **No external transparency log.** Truncation defence is local (signed checkpoints). Publishing to an append-only external sink is future work.
- **No PII classification.** Sensitivity is supplied by the caller or by config rules; Notary does not detect it.
- **No self-hosted Mem0 adapter.** v1 targets the hosted platform. The OSS server is a later adapter behind the same client interface.
- **No point-in-time mutation model.** `replay` answers "what existed", not "what the state was and how it mutated".

---

## 2. Locked decisions

| # | Decision | Choice |
|---|---|---|
| D1 | Tier scope | All three tiers are implemented in v1; the reconciler is a first-class component |
| D2 | Tier semantics | `Observed` / `Reconstructed` / `Internal` — see §3 |
| D3 | Mem0 surface | Hosted platform, mixed endpoint versions: `v3` for `add`/`search`/`get_all`, `v1` for per-memory `history` and event status |
| D4 | Content at rest | Content is stored; `Sensitive` is caller/config-supplied; redaction happens at render time |
| D5 | Reconciliation | Both modes (scheduled CLI and in-process loop) behind a `reconcile-mode` config knob |
| D6 | Core architecture | One hash chain, two writers (interceptor and reconciler both persist signed claims) |
| D7 | `Observed` scope | Means "directly witnessed by Notary"; the source (`mem0_response` or `notary_instrumentation`) is recorded on the evidence |
| D8 | Truncation | Signed head checkpoints in v1; `verify --checkpoint` detects a shortened chain |
| D9 | `replay --at` | Knowledge time — filters on `RecordedAt <= T`, inclusive |
| D10 | LLM provider dependency | **Corrected (2026-10-03):** providers are pluggable through the OpenAI-compatible chat-completions schema and no provider SDK is added. The load-bearing half survives: `internal/phrase` is isolated to the phrasing pass, which no core package imports. This row originally read "Anthropic dependency — approved, isolated to `internal/phrase`, which no core package imports" |

**Assumption:** `go.mod` declares `go 1.25.0`, which satisfies `.clinerules`' "Go 1.23+" floor; `GOTOOLCHAIN=auto` fetches that toolchain on first build, so builds require network on a machine whose local toolchain is older.

---

## 3. The tier model

A tier labels the epistemic status of a **why** claim. It is not a confidence score and not a severity.

| Tier | Means | Required payload |
|---|---|---|
| `Observed` | Notary witnessed this directly | `ObservedEvidence{Source, Payload}` where `Source` is `mem0_response` or `notary_instrumentation` |
| `Reconstructed` | Notary inferred this | `ReconstructedEvidence{Basis []RecordID, Rule, RuleVersion, Confidence}` |
| `Internal` | Mem0 decided internally and no reason was exposed | `InternalNote{Opaque}` — a statement of opacity, never a guess |

Plain-language rendering must never blend them:

- `Observed`/`mem0_response` → "Mem0 returned this, score 0.83."
- `Observed`/`notary_instrumentation` → "Notary itself observed this: the audit store was unreachable at 12:04."
- `Reconstructed` → "Notary inferred this: memory `m-123` appeared in the 12:00 listing and is absent from the 12:05 search with `top_k=10`."
- `Internal` → "Mem0 removed this internally. No reason was exposed."

---

## 4. Architecture

### The core invariant

Exactly one write path exists, and it is the only thing that touches the chain. Interceptor observations and reconciler inferences are the same kind of object — a signed, hash-chained `Record` — and the tier is what distinguishes them.

```
cmd/notary ─ version · verify · export · replay · explain · reconcile
   │
   ├─ internal/interceptor ── Interceptor iface ─┬─ library/mem0.go   (inline, Observed)
   │                                             └─ proxy/proxy.go    (Phase 9)
   ├─ internal/reconcile ──── Reconciler                     (deferred; Observed/Reconstructed/Internal)
   ├─ internal/export   internal/replay   internal/explain   internal/phrase    (read paths)
   │
   ├─ internal/ledger ─────── the ONLY writer
   │     ├─ internal/record    Record, VisibilityTier, Reason, chain
   │     ├─ internal/sign      ed25519, keyring, checkpoints
   │     └─ internal/store     sqlite
   ├─ internal/mem0 ───────── thin REST client (shared by interceptor and reconciler)
   └─ internal/gap ────────── hash-chained fallback gap log
config/config.go
```

### Interfaces

```go
type Ledger interface {
    Append(Claim) (RecordID, error)          // chain + sign + idempotent upsert, one transaction
    GetRecord(id RecordID) (Record, error)
    ListRecords(from, to time.Time) ([]Record, error)
}

type Interceptor interface { /* deployment mode; writes Observed inline */ }
type Reconciler  interface { Reconcile(ctx context.Context, window TimeRange) ([]Claim, error) }
```

`Interceptor` and `Reconciler` are deliberately distinct. A deployment mode wraps live traffic; a reconciler reads history. Collapsing them would force proxy mode and reconcile mode to share a lifetime they do not have.

`Claim` is the pre-chain input — event, reason, subject, content, idempotency key. `Ledger.Append` is what assigns `Seq`, computes `Hash`, and produces the signature, so no caller can supply a chain position or a hash.

### Dependency direction

`store` and `sign` depend on `record`; `ledger` depends on `store` and `sign`; `interceptor` and `reconcile` depend on `ledger`; `export`, `replay`, and `explain` read through `ledger`. `internal/phrase` talks to an LLM through a small OpenAI-compatible HTTP client — it adds no SDK — and is imported **only** by `internal/export`. No core package imports `internal/phrase`, so "the audit core has no LLM dependency" is enforced by the compiler rather than by convention. **Corrected (2026-10-03):** this sentence originally read "`internal/phrase` imports the Anthropic SDK"; providers are now pluggable and no SDK is added.

---

## 5. Record schema and canonical hashing

### The tier type

```go
type VisibilityTier struct{ v tierValue }   // unexported field: unforgeable outside the package

var (
    Observed      = VisibilityTier{tierObserved}
    Reconstructed = VisibilityTier{tierReconstructed}
    Internal      = VisibilityTier{tierInternal}
)

func (t VisibilityTier) Valid() bool
func (t VisibilityTier) String() string
func (t VisibilityTier) MarshalJSON() ([]byte, error)  // "observed" | "reconstructed" | "internal"
```

The zero value is deliberately invalid, so a defaulted tier can never pass as a real one. `VisibilityTier{v: 99}` does not compile outside `record`.

**Honest limitation:** Go permits the zero-value composite literal `record.VisibilityTier{}` and `record.Reason{}` from any package; no type design can make those a compile error. The guarantees that are achievable, and that are tested, are:

1. **Unforgeable.** The unexported field means no other package can produce a valid tier; the three package variables are the only valid values.
2. **Unavoidable, and non-swappable.** Three constructors with disjoint evidence types, rather than one builder taking a tier:

   ```go
   func NewObservedReason(kind ReasonKind, ev ObservedEvidence) Reason
   func NewReconstructedReason(kind ReasonKind, ev ReconstructedEvidence) Reason
   func NewInternalReason(kind ReasonKind, note InternalNote) Reason
   ```

   `Reason` has no exported fields and no setters, so a `Reconstructed` value cannot be relabelled `Observed`, and a single code path cannot emit more than one tier. `ObservedEvidence` is constructible only from `internal/mem0` response types, so an `Observed` claim cannot be fabricated without a real response.
3. **Rejected at the boundary.** `Ledger.Append` validates and returns an error on an invalid tier — never a panic — so a stray `Reason{}` cannot reach the store or the chain.
4. **Tested.** `testdata/negative/` fixtures assert that forging and omission snippets do not compile.

Additionally, the **(ReasonKind, tier)** pair is validated. Each kind declares the single tier it permits, so `returned_by_search` cannot be recorded as `Reconstructed`, and `removed_by_mem0` cannot be recorded as `Observed`.

### Event vocabulary

```go
type EventType string

const (
    EventAddRequested    EventType = "add_requested"
    EventAddResolved     EventType = "add_resolved"
    EventSearchPerformed EventType = "search_performed"
    EventMemorySurfaced  EventType = "memory_surfaced"
    EventMemoryKept      EventType = "memory_kept"
    EventMemoryDropped   EventType = "memory_dropped"
    EventAuditGap        EventType = "audit_gap"
)
```

### `ReasonKind` vocabulary

`search_performed`, `add_acknowledged`, `returned_by_search`, `stored_by_mem0`, `kept_by_content_match`, `absent_from_search`, `no_facts_extracted`, `removed_by_mem0`, `add_failed`, `audit_unavailable`.

Allowed tiers are fixed per kind: `search_performed`, `add_acknowledged`, `returned_by_search`, `stored_by_mem0`, `add_failed`, `audit_unavailable` are `Observed`; `kept_by_content_match`, `absent_from_search`, `no_facts_extracted` are `Reconstructed`; `removed_by_mem0` is `Internal`.

`search_performed` and `add_acknowledged` describe observing the *request* that produced a decision, as distinct from the decision's outcome. They were added during planning: every record carries a `Reason`, so without them a `search_performed` or `add_requested` record has no legal kind.

The vocabulary names only what v1 can actually establish. Attributing a non-surfaced memory to `threshold` versus `top_k` would require re-querying Mem0 with a widened window; v1 records the fact (`absent_from_search`) and the parameters as its basis, and does not guess the cause (§15).

### Record

```go
type Record struct {
    ID             RecordID
    Seq            uint64      // chain position — the authoritative order
    At             time.Time   // when the Mem0 event happened
    RecordedAt     time.Time   // when Notary wrote it
    Event          EventType   // enum, not a string
    Reason         Reason      // carries the VisibilityTier
    Subject        Subject     // Mem0 memory id, entity scope, content hash
    Content        *Content    // text + Sensitive flag
    IdempotencyKey IdemKey
    PrevHash       Hash
    Hash           Hash
    Signature      []byte
    SignerKeyID    string
}

type Subject struct {
    MemoryID    string // Mem0 memory id; empty when not applicable
    Scope       Scope  // user_id / agent_id / app_id / run_id
    ContentHash string // always present
}

type Content struct {
    Text      string
    Sensitive bool
}
```

`At` and `RecordedAt` are distinct because a reconciler writes records *about* an earlier event *later*. `Seq` is the authoritative order (`verify` walks it); `At` is a display and filtering order. A late-arriving `Reconstructed` claim is only honest if both are preserved.

### Canonical hashing

```
Hash = SHA-256( "notary/record/v1" ‖ canonical(record_without_hash_and_signature) ‖ Seq ‖ PrevHash )
```

`canonical` is an explicit encoder — length-prefixed fields, scores as fixed-precision decimal strings — not `encoding/json`. Mem0's `search` returns a float `score`, and JSON float formatting is a plausible source of a non-reproducible hash. The genesis record uses 32 zero bytes as `PrevHash`. Signing covers `Hash` only, since `Hash` already covers content and chain position.

---

## 6. Observation and reconciliation

### Inline observations (Interceptor, `Observed`, synchronous)

| Event | Captured |
|---|---|
| `add_requested` | scope, messages (content + `Sensitive`), `infer`, and `event_id`/`status: PENDING` from the response |
| `search_performed` | `query`, `filters`, `top_k`, `threshold`, `rerank`, result count |
| `memory_surfaced` | one record per returned memory: `memory_id`, `score`, rank |

Written on the request path. **The Mem0 call is never blocked, delayed, or failed by an audit-write outcome.**

### Deferred claims (Reconciler)

| Event | Source | Tier |
|---|---|---|
| `add_resolved` | poll `GET /v1/event/{id}/` → `SUCCEEDED`/`FAILED` | `Observed` |
| `memory_kept` | `get_all` for the scope after the add resolves | `Observed` (`stored_by_mem0`) when the memory is seen in the listing; `Reconstructed` (`kept_by_content_match`) when inferred from a content-hash match |
| `memory_dropped` | see below | `Reconstructed` or `Internal` |
| `audit_gap` | the gap log | `Observed` / `notary_instrumentation` |

`memory_dropped` has two honestly different sources:

- **`Reconstructed`** — a memory known to exist in scope (from an earlier `get_all`, i.e. a `memory_kept` or listing record) that a covering `search` did not return. Basis = the prior listing record + this search record + its `top_k`/`threshold`; `Rule` and `RuleVersion` recorded.
- **`Internal`** — `GET /v1/memories/{id}/history/` shows an `UPDATE`/`DELETE` with no reason exposed. Notary reports *that* Mem0 removed it and marks the why as opaque.

The reconciler runs as a scheduled CLI command or an in-process loop, selected by `reconcile-mode`. It is the only producer of claims Mem0 did not report directly. Re-running it is a no-op (§7).

---

## 7. Idempotency and chain semantics

Idempotency keys are deterministic and content-derived — never wall-clock, never random.

- **Every operation keys on the caller-supplied `correlation_id`,** which is required. The key is `H(kind ‖ reason_kind ‖ scope ‖ correlation_id ‖ request_digest)`, the same form for adds and searches. If the caller omits the correlation ID, the library **errors** rather than inventing a key from a clock or a random source.
- **Notary deliberately does not key on Mem0's `event_id`.** Mem0 does return one for adds, but a *retried* add comes back with a **fresh** `event_id`, so keying on it would write a second audit record for one logical operation — the exact duplication an idempotency key exists to prevent. The `correlation_id` names the caller's operation, which is the thing that must not be recorded twice.
- **The reason kind is part of the key**, because it determines the tier (1:1 through `AllowedTier`). Without it, `memory_kept` observed as `stored_by_mem0` and reconstructed as `kept_by_content_match` would derive the same key, and the second claim — later knowledge — would be silently suppressed instead of appended.
- **Derived claims:** `H(claim_kind ‖ basis_record_ids ‖ rule_version)`, so re-running `Reconcile` adds nothing.
- **Keyless writes remain legal.** Uniqueness is a partial index over *non-empty* keys only. The interceptor always supplies one; the ledger tolerates absence rather than rejecting it.

**Chain writes.** `Ledger.Append` assigns `Seq` and computes the hash inside a single `BEGIN IMMEDIATE` transaction, so `Seq = max+1` cannot race and a crash between hash computation and commit is structurally impossible. An existing `IdempotencyKey` is a no-op returning the existing record ID; a different event or tier yields a different key, so later knowledge **appends** rather than mutates. One store file has one writer at a time, serialized by SQLite.

---

## 8. Trust: signing, verification, and fail-open-loud

### Key handling

ed25519. Loaded from an env var or the OS keychain; a file path is a dev convenience only — it must live outside the repository, be `0600`, and loading **refuses** if permissions are loose. A key is never generated silently. Key material is wrapped in a type whose `String`, `Format`, and `MarshalJSON` all emit `[redacted]`, so "never log key material" is a property of the type rather than a review that must be repeated. Each record names its `SignerKeyID` (a public-key fingerprint), and config supplies a set of trusted public keys so rotation does not invalidate history.

### Startup versus runtime

- **Config time:** no key configured → hard failure; the process refuses to start.
- **Call time:** a signer *failure* (key file became unreadable, keychain locked) is not a config error. The Mem0 call completes and an `audit_gap` is recorded.

### Fail-open-loud

On a Store or Signer failure the interceptor (a) lets the Mem0 call complete normally and (b) writes an `audit_gap` marker to two channels with **independent failure domains**: stderr, and a gap log file at a configurable path that is not the database path or volume.

The gap log has its own hash chain and a monotonic counter, so an outage cannot be silently erased — deleting an entry breaks the gap log's own chain. Entries carry `kind`, scope, correlation ID, timestamp, and failure reason, so a later `notary reconcile` can often close the gap by re-deriving the missing record.

### `notary verify`

Walks `Seq` ascending and, per record, checks that `Hash` recomputes, `PrevHash` equals its predecessor's `Hash`, and the signature is valid under the named key. It reports the exact record ID **and field** at which anything breaks. It also cross-checks the gap log and reports gap entries with no corresponding record.

### Truncation

A hash chain cannot detect deletion of trailing records — what remains is self-consistent. `export` and `verify` therefore emit a signed `Checkpoint{Seq, Hash, At, SignerKeyID, Signature}`, and `verify --checkpoint <file>` fails loudly when the chain is shorter than the checkpoint. Publishing checkpoints to an external transparency log is documented as future work and is the only complete defence.

---

## 9. Read paths

| Command | Scope | Answers |
|---|---|---|
| `notary explain <record-id>` / `--memory <mem0-id>` | one record, or one memory's lifecycle | what happened to this memory, and why |
| `notary export --from --to [--include-sensitive]` | a time range, bulk | everything between these dates |
| `notary replay --at <ts>` | the ledger as of an instant | what Notary knew at time T |
| `notary verify [--checkpoint]` | the whole chain | whether anything was tampered with |
| `notary serve` | the whole ledger, live, read-only, over loopback | show me, in a browser, and let me set the range |
| `notary report --out DIR (--memory <mem0-id> \| --from <ts> [--to <ts>])` | one memory, or a range, written out as static pages | give me the slice as files I can attach and hand over |

- **`explain`** is the single-subject view; **`export`** is the range view; **`report`** takes either subject and writes it as a folder of files rather than a stream.
- **`serve`** is the browser view over the same records: read-only, loopback-only (`127.0.0.1`, no host or address flag), and it needs no signing key and no keyring. Its ledger is opened with a nil signer, so it can never write; content is redacted by default and revealed per view, and revealing never changes a hash.
- **`report`** is the artefact view over the same records: one subject, **bounded by construction** — `--memory <mem0-id>`, or `--from`/`--to` (on each record's `At`, the event time) narrowed by the four scope flags through the same `export.ScopeMatches` matcher `export` and `replay` use — with no `--all`, and `--out DIR` refused when the directory already holds files unless `--force` is given. It is a read path that writes only inside `--out` and holds **no signing key**: the trusted public keyring only, and none at all with `--no-verify`. It refuses a ledger path with **no ledger at it, or a zero-byte file**, rather than creating one, and it exits non-zero when the chain state it rendered is broken **while still writing the pages**.
- **`report`'s chain state is `verify`'s answer, not a second one.** All three checks `notary verify` runs without a checkpoint file — the chain walk, the gap cross-check against the store, and the gap log's own integrity — live in one function, `ledger.CollectBreaks` (`internal/ledger/breaks.go`), so a report and the command cannot disagree about "is this ledger intact?". It is a whole-ledger property, not slice-scoped.
- **`report`'s evidence block is not filtered by `--include-sensitive`.** The flag governs a record's stored **content**; the evidence block is the record's own stored reason, printed exactly as the ledger hashed it (`Reason.Encode`), and `reconcile` records whole upstream objects rather than curated fields — so such a reason can quote memory text the sensitivity rules never marked. The index, every record page and the help text say so, and narrowing what `reconcile` records is recorded as work for a phase of its own, because it changes what records hash.
- **Redaction is presentation, never storage.** Full content is stored, the hash covers the real content, and redaction happens at render. Every redacted entry states that it was redacted. `--include-sensitive` reveals content and does not change any hash.
- **`replay --at T`** returns records with `RecordedAt <= T`, inclusive, ordered by `Seq`. A `Reconstructed` claim written later is correctly absent from an earlier replay.
- **The LLM phrasing pass** is export-only, opt-in, and lives in `internal/phrase`. Its output is a distinct `Paraphrase` type that is never an input to any decision function, is always displayed alongside the structured record and its tier, and is labelled a paraphrase. Failure degrades to the structured record plus a note.
- **Input validation** rejects malformed dates and IDs with plain-language errors, bounds ranges, and paginates.

---

## 10. Configuration

```go
type Config struct {
    Mem0APIKey       string        // env only
    Mem0BaseURL      string        // default https://api.mem0.ai
    DBPath           string
    SigningKeySource KeySource     // env | keychain | file(dev)
    TrustedKeys      []string      // public keys, for rotation
    FailMode         FailMode      // FailOpenLoud (default); FailClosed reserved, unused in v1
    ReconcileMode    ReconcileMode // Command | InProcess
    GapLogPath       string
    Verbose          bool
}
```

`Rule` matches a scope and/or a Mem0 metadata key and marks matching content `Sensitive`. Rules are declarative configuration, never code.

Sensitivity rules are deliberately **not** a `Config` field (corrected during Phase 6). They are loaded from a path given by `NOTARY_SENSITIVITY_RULES`, and `config.LoadSensitivityRules` is a **write-path** loader: rules mark content at *write* time, so they belong to whatever writes records — an application that constructs the interceptor, and `notary proxy`, which writes records on the live path. There is deliberately no `Config.SensitivityRules` field and no CLI flag for the path. (Corrected after implementation: this paragraph originally called the loader *application-facing* and said *no `notary` command writes records, so the rules belong to whatever application constructs the interceptor rather than to the CLI* — Phase 9's `notary proxy` writes records and reads this variable.)

No secrets in the repository; `.clinerules` guardrails apply unchanged.

---

## 11. Security and privacy

- The audit store is a durable second copy of the memories it describes. Content is stored in full so that hashes remain verifiable; sensitivity is caller/config-supplied and controls redaction only.
- `internal/phrase` is the only package that talks to an LLM, whatever provider it is pointed at, and the compiler enforces that no core package imports it. **Corrected (2026-10-03):** this bullet originally read "is the sole importer of an LLM SDK"; no SDK is added.
- No core code path treats generated text as authoritative. `Paraphrase` is display-only.
- No secret or key material may appear in any log, error, or `%v` rendering; enforced by the redacting key type and a canary test.

---

## 12. Testing strategy

1. **Negative-compile fixtures** (`testdata/negative/`): forging a tier, building a `Reason` without a constructor, and fabricating `Observed` evidence must fail to compile.
2. **Hash and tamper:** mutate every field and assert the hash changes; edit a record by raw SQL and assert `verify` names the exact record and field.
3. **Chain:** continuity, `Seq` monotonicity, idempotent re-append is a no-op, and a subprocess killed between hash and commit leaves the chain intact.
4. **Truncation:** deleting the tail makes `verify --checkpoint` fail and leaves plain `verify` passing.
5. **Mem0:** `httptest` fixtures for `add`, `search`, `event`, `history`, and `get_all`. A live test is opt-in behind `//go:build mem0live` and `NOTARY_MEM0_API_KEY`, and is never a phase gate. (Corrected after implementation: this line originally named `MEM0_API_KEY`, a variable nothing reads. The test is `internal/reconcile/live_mem0_test.go`; run it with `-count=1`, because without it a re-run replays a cached PASS that never touched Mem0.)
6. **Fail-open-loud:** inject a failing Store or Signer; assert the Mem0 call succeeds, the marker reaches both channels, and the gap log's own chain validates.
7. **Reconciler:** scripted sequences assert the exact tier emitted, and that a re-run adds zero records.
8. **Replay boundary**, **redaction round-trip**, and a **key-material canary** that must never appear in any output. (Corrected after implementation: the replay boundary's as-of inclusivity is `TestListRecordsAsOfIncludesRecordAtExactInstant` and `TestListRecordsAsOfExcludesRecordOneNanosecondLater` in `internal/store/sqlite_test.go`, and the non-prefix hole is `TestReplayAsOfReportsABreakForAHoleInThePrefix` in `internal/ledger/ledger_test.go`. Phase 8's explain surface is guarded by `TestExplainSingleRecordRejectsAnUnphraseableRecord` in `internal/explain/explain_test.go`; the interleaved-memory filter by `TestListRecordsByMemoryFiltersToOneMemoryOnly` in `internal/store/sqlite_test.go` and `TestListRecordsByMemoryDelegatesAndFilters` in `internal/ledger/ledger_test.go`; and the CLI's refusal of an empty `--memory` and its non-zero exit for a memory with no records by `TestExplainRejectsEmptyMemoryRatherThanReadingTheWholeLedger` and `TestExplainMemoryWithNoRecordsExitsNonZeroNamingTheID` in `cmd/notary/explain_test.go`.)

---

## 13. Phasing

| Phase | Deliverable | Status |
|---|---|---|
| 0 | Scaffolding | ✅ complete |
| 1 | `record` schema, tier constructors, `chain`, `store`; negative-compile tests | ✅ complete |
| 2 | `sign`, keyring, `verify`, checkpoints | ✅ complete |
| 3 | `interceptor`, `library/mem0`, `internal/mem0`, fail-open-loud, `internal/gap` | ✅ complete |
| 4 | Idempotency; correlation ID required on every observation | ✅ complete |
| 5 | **Reconciler** (new): event polling, `get_all` diffing, `kept`/`dropped`, rule registry, `reconcile-mode` | ✅ complete |
| 6 | `export`, redaction, tier phrasing, `internal/phrase` | ✅ complete |
| 7 | `replay` | ✅ complete |
| 8 | **`explain`** (new) | ✅ complete |
| 9 | Proxy mode | ✅ complete |
| 10 | Polish, README, demo fixtures | ✅ complete |
| 11 | **`report` and `doctor`** (new, post-v1): the static evidence report, and the setup diagnosis | ✅ complete |
| 12 | **`notary serve`** (new): the read-only loopback dashboard | ✅ complete |

Phases 0–12 are implemented and merged to `master`. Phase 11 was designed first and numbered then, and
`notary serve` was built and merged as Phase 12 while it was still in progress, so row 11 sits between 10 and
12 because its number does, not because it landed first. Each phase that needs design work of its own gets a
dated design spec and implementation plan under `docs/superpowers/`; the reconciler's are
`specs/2026-09-30-notary-v1-reconciler-design.md` and `plans/2026-09-30-notary-v1-reconciler.md`. This
table is updated as each phase lands, so a ⬜ here means genuinely not built — not merely undocumented.
Phase 10 was a polish pass over existing documents, tests and fixtures rather than new code, so it was run as
a bounded batch with a short in-chat design instead of a dated plan; its record is the ledger under
`.superpowers/sdd/2026-10-04-notary-v1-proxy/`.

The reconciler precedes `export` because otherwise the export phase has nothing but `Observed` records to display.

Against the original Cline plan: phases 0–4 keep their intent and numbering; `replay` moves 6→7, proxy moves 7→9, and polish moves 8→10 to make room for the reconciler (new Phase 5) and `explain` (new Phase 8). No original phase is dropped.

---

## 14. Deltas to `.clinerules`

The folder structure gains packages the original layout did not account for:

| Package | Why |
|---|---|
| `internal/ledger/` | The single writer; chain + sign + upsert belong together, not in `store` |
| `internal/mem0/` | The thin REST client, shared by `interceptor/library` and `reconcile` |
| `internal/reconcile/` | The reconciler |
| `internal/explain/` | The per-record and per-memory lifecycle view |
| `internal/serve/` | The read-only loopback dashboard over the ledger |
| `internal/report/` | The static evidence report: the pages, their derived filenames, and the two embedded assets |
| `internal/doctor/` | The deployment diagnosis: the checks, the findings, and the setup page |
| `internal/gap/` | The hash-chained fallback gap log |
| `internal/phrase/` | The only package that talks to an LLM, whatever provider it is pointed at. **Corrected (2026-10-03):** this row originally read "The isolated Anthropic adapter" |

`.clinerules` wording that this design refines rather than contradicts: "idempotent from Phase 4 onward" now holds for phases 4+, with `Append` idempotent by construction; "fail-open-loud only" is preserved, with `FailClosed` still defined and unused; the `VisibilityTier` guardrail is implemented as described in §5, with the compile-time limitation stated plainly rather than overclaimed.

---

## 15. Risks and deferred work

| Risk | Mitigation |
|---|---|
| Local truncation defence only | Signed checkpoints in v1; external transparency log deferred and documented |
| `Observed` coverage is narrower than the product's framing implies — Mem0 exposes no first-class reason | Tiers make the limit explicit rather than papering over it; most "why" claims are correctly `Reconstructed` |
| Fine-grained drop attribution (`threshold` vs `top_k`) is not established in v1 | Recorded as `absent_from_search` with the search parameters as basis; a re-query probe to attribute the cause is deferred |
| Reconciler depends on stable platform response shapes | Fixtures pin the shapes; live tests are opt-in so drift is detected without gating the build |
| Caller must supply `correlation_id` for searches | Documented as a required integration step; omission is a loud error, not a silent duplicate |
| Go 1.23 toolchain fetched on demand | Documented; builds require network on machines with an older local toolchain |
