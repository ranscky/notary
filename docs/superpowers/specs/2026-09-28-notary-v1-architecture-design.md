# Notary v1 — Architecture Design

**Date:** 2026-09-28
**Status:** Approved for planning
**Scope:** v1 (library mode). Proxy mode is Phase 9 of the same build but is not the subject of this document.

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
| D10 | Anthropic dependency | Approved, isolated to `internal/phrase`, which no core package imports |

**Assumption:** `go.mod` declares `go 1.23` per `.clinerules`; `GOTOOLCHAIN=auto` fetches that toolchain on first build, so builds require network on a machine whose local toolchain is older.

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

`store` and `sign` depend on `record`; `ledger` depends on `store` and `sign`; `interceptor` and `reconcile` depend on `ledger`; `export`, `replay`, and `explain` read through `ledger`. `internal/phrase` imports the Anthropic SDK and is imported **only** by `internal/export`. No core package imports `internal/phrase`, so "the audit core has no LLM dependency" is enforced by the compiler rather than by convention.

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

`returned_by_search`, `stored_by_mem0`, `kept_by_content_match`, `absent_from_search`, `no_facts_extracted`, `removed_by_mem0`, `add_failed`, `audit_unavailable`.

Allowed tiers are fixed per kind: `returned_by_search`, `stored_by_mem0`, `add_failed`, `audit_unavailable` are `Observed`; `kept_by_content_match`, `absent_from_search`, `no_facts_extracted` are `Reconstructed`; `removed_by_mem0` is `Internal`.

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

- **Operations Mem0 identifies:** `H(kind ‖ scope ‖ event_id)`.
- **Operations Mem0 does not identify (`search`):** a caller-supplied `correlation_id` is required. Mem0's API already scopes by `run_id`, so this is idiomatic. The key is `H(kind ‖ scope ‖ correlation_id ‖ normalized_request)`. If the caller omits it, the library **errors** rather than inventing a key from a clock or a random source.
- **Derived claims:** `H(claim_kind ‖ basis_record_ids ‖ rule_version)`, so re-running `Reconcile` adds nothing.

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

- **`explain`** is the single-subject view; **`export`** is the range view.
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
    SensitivityRules []Rule
    Verbose          bool
}
```

`Rule` matches a scope and/or a Mem0 metadata key and marks matching content `Sensitive`. Rules are declarative configuration, never code.

No secrets in the repository; `.clinerules` guardrails apply unchanged.

---

## 11. Security and privacy

- The audit store is a durable second copy of the memories it describes. Content is stored in full so that hashes remain verifiable; sensitivity is caller/config-supplied and controls redaction only.
- `internal/phrase` is the sole importer of an LLM SDK, and the compiler enforces it.
- No core code path treats generated text as authoritative. `Paraphrase` is display-only.
- No secret or key material may appear in any log, error, or `%v` rendering; enforced by the redacting key type and a canary test.

---

## 12. Testing strategy

1. **Negative-compile fixtures** (`testdata/negative/`): forging a tier, building a `Reason` without a constructor, and fabricating `Observed` evidence must fail to compile.
2. **Hash and tamper:** mutate every field and assert the hash changes; edit a record by raw SQL and assert `verify` names the exact record and field.
3. **Chain:** continuity, `Seq` monotonicity, idempotent re-append is a no-op, and a subprocess killed between hash and commit leaves the chain intact.
4. **Truncation:** deleting the tail makes `verify --checkpoint` fail and leaves plain `verify` passing.
5. **Mem0:** `httptest` fixtures for `add`, `search`, `event`, `history`, and `get_all`. A live test is opt-in behind `//go:build mem0live` and `MEM0_API_KEY`, and is never a phase gate.
6. **Fail-open-loud:** inject a failing Store or Signer; assert the Mem0 call succeeds, the marker reaches both channels, and the gap log's own chain validates.
7. **Reconciler:** scripted sequences assert the exact tier emitted, and that a re-run adds zero records.
8. **Replay boundary**, **redaction round-trip**, and a **key-material canary** that must never appear in any output.

---

## 13. Phasing

| Phase | Deliverable |
|---|---|
| 0 | Scaffolding (complete) |
| 1 | `record` schema, tier constructors, `chain`, `store`; negative-compile tests |
| 2 | `sign`, keyring, `verify`, checkpoints |
| 3 | `interceptor`, `library/mem0`, `internal/mem0`, fail-open-loud, `internal/gap` |
| 4 | Idempotency; correlation ID required on id-less operations |
| 5 | **Reconciler** (new): event polling, `get_all` diffing, `kept`/`dropped`, rule registry, `reconcile-mode` |
| 6 | `export`, redaction, tier phrasing, `internal/phrase` |
| 7 | `replay` |
| 8 | **`explain`** (new) |
| 9 | Proxy mode |
| 10 | Polish, README, demo fixtures |

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
| `internal/gap/` | The hash-chained fallback gap log |
| `internal/phrase/` | The isolated Anthropic adapter |

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
