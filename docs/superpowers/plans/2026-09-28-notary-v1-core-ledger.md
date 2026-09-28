# Notary v1 Core Ledger Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build Notary's core ledger — the signed, hash-chained, tier-typed record store that everything else in Notary writes through — covering Phases 1–4 of the v1 build.

**Architecture:** One write path (`internal/ledger`) owns the chain. Records are immutable, hash-chained, and signed with ed25519; the chain position `Seq` is assigned inside a single SQLite transaction. A `VisibilityTier` that cannot be forged outside `internal/record` labels every "why" claim, enforced by three constructors with disjoint evidence types rather than one builder. The Mem0 interceptor writes only what it witnesses inline, and fails open but loud.

**Tech Stack:** Go 1.25, `github.com/spf13/cobra`, `modernc.org/sqlite` (pure-Go, no CGO), stdlib `crypto/ed25519`, `github.com/stretchr/testify`.

**Spec:** `docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md`

## Global Constraints

- `go.mod` declares `go 1.25.0`, using the **current** toolchain (`go1.25.0`, downloaded via `GOTOOLCHAIN=auto`) and current dependency versions. This supersedes an earlier `go 1.22` pin.
- Module path is `notary`. If Notary is to be imported by other modules this must become a repository URL **before** any external consumer exists.
- Dependencies at their current versions: `github.com/spf13/cobra v1.10.2`, `github.com/stretchr/testify v1.12.1`, `modernc.org/sqlite v1.59.0`. **The Anthropic SDK is not used in this plan.** Do not add any other dependency without asking.
- The Go module cache (`/home/ranscky/go/pkg/mod`) and the checksum-db cache are **not writable by subagents**. Module provisioning and any version bump is done by the controller, never inside a task; implementers only read the cache and write `/tmp` (the `GOCACHE`).
- Never `panic` in library code. Every fallible function returns an error.
- Wrap errors with context: `fmt.Errorf("writing record: %w", err)`.
- No global mutable state; dependencies are passed through constructors.
- No hardcoded secrets or signing keys. Env vars or OS keychain only; a key file is dev-only, must sit outside the repo, and must be `0600`.
- Never log key material or raw sensitive memory content.
- The `VisibilityTier` zero value is invalid and must never reach the store.
- The structured record is the source of truth. No code path treats generated text as authoritative.
- Every store write is idempotent, implemented in `Ledger.Append` from Phase 4.
- Fail-open-loud only. `FailClosed` is a defined value that is never implemented.
- File length guidance is ~300 lines as a split signal, not a hard ceiling.
- `snake_case` only for SQL column names; camelCase unexported, PascalCase exported.

## Review Focus

The spec says what Notary must do, not everything it will meet. These are the input classes and failure modes most likely to bite a user, each pinned by a test in the task that owns the code:

1. **An empty store** — `verify` on a store with zero records, and `Head()` on an empty table, must report "nothing to verify", not error or panic. (Task 6, Task 10)
2. **A store whose tail was deleted** — plain `verify` must still pass (the remaining chain is self-consistent) while `verify --checkpoint` must fail loudly. Confusing these two is the whole point of the feature. (Task 11)
3. **A record edited directly in SQLite** — `verify` must name the exact record ID *and* field, not just report "invalid". (Task 10)
4. **A Store or Signer that fails during a Mem0 call** — the Mem0 call must still succeed *and* a gap marker must land in both channels. (Task 15, Task 16, Task 19)
5. **Key material reaching any output** — a canary private key must never appear in a log, an error string, or a `%v`/`%+v` rendering. (Task 7)

---

### Task 0: Establish Phase 0 scaffolding

Phase 0 is described as complete in the Cline phase plan, but the repository currently contains only `.clinerules` and the spec. This task establishes it. If a Phase 0 build exists elsewhere, port it and skip to Task 1.

**Files:**
- Create: `go.mod`, `cmd/notary/main.go`, `cmd/notary/root.go`, `cmd/notary/version.go`, `config/config.go`, `config/config_test.go`
- Create: `.gitignore` (ignore `*.db`, `*.db-wal`, `*.db-shm`, `/notary`)

**Interfaces:**
- Produces: `config.Config`, `config.Load() (*Config, error)`, `config.LoadFrom(env map[string]string) (*Config, error)`; the `notary` root command with persistent flags `--verbose` and `--config`.

- [ ] **Step 1: Confirm the module and create the folders**

The controller has already run `go mod init notary` and pinned the three dependencies, because the module cache is not writable from your sandbox. Verify, do not re-run:

```bash
cat go.mod                      # expect `module notary`, `go 1.25.0`, and the three requires
go build ./... && echo "module OK"
mkdir -p cmd/notary internal/record internal/store internal/sign internal/ledger internal/interceptor/library config
```

If `go.mod` is missing or the build fails, stop and report `BLOCKED` — do not attempt `go get`, which cannot write the module cache from your sandbox.

- [ ] **Step 2: Write the failing config test** in `config/config_test.go`

Assert: `LoadFrom(map[string]string{})` returns a `*Config` with non-empty `DBPath` default; `LoadFrom` with `NOTARY_DB_PATH=/tmp/x.db` returns `DBPath == "/tmp/x.db"`; `LoadFrom` with no `MEM0_API_KEY` does **not** error (the key is only required when a Mem0 call is attempted).

- [ ] **Step 3: Run it and confirm it fails**

Run: `go test ./config/ -run TestLoadFrom -v`
Expected: FAIL — `undefined: config.LoadFrom`

- [ ] **Step 4: Implement `config/config.go`**

```go
type Config struct {
    Mem0APIKey   string
    Mem0BaseURL  string
    DBPath       string
    GapLogPath   string
    Verbose      bool
}

func Load() (*Config, error)
func LoadFrom(env map[string]string) (*Config, error)
```

Defaults: `Mem0BaseURL` = `https://api.mem0.ai`, `DBPath` = `notary.db` in the working directory, `GapLogPath` = `notary-gaps.log`. Every value overridable by env (`NOTARY_MEM0_API_KEY`, `NOTARY_MEM0_BASE_URL`, `NOTARY_DB_PATH`, `NOTARY_GAP_LOG_PATH`). No secret is ever defaulted.

- [ ] **Step 5: Implement the CLI root and `version`**

`cmd/notary/root.go` defines `notary` with persistent `--verbose` and `--config`. `cmd/notary/version.go` defines `notary version` printing a version constant. `cmd/notary/main.go` calls `root.Execute()` and exits non-zero on error.

- [ ] **Step 6: Create the SQLite file at startup if absent**

In `root.go`'s `PersistentPreRunE`, if `Config.DBPath` does not exist, create it via `modernc.org/sqlite` and close it. A failure prints a plain-language error; the binary must not panic with no config present.

- [ ] **Step 7: Run the acceptance checks**

```bash
go build ./... && go test ./config/ -v && go run ./cmd/notary version && go run ./cmd/notary --help
```
Expected: build clean; config tests PASS; `version` prints a version string; `--help` lists the root command with `--verbose` and `--config`.

- [ ] **Step 8: Commit**

```bash
git add -A && git commit -m "chore: establish Phase 0 scaffolding (module, config, cobra root, version)"
```

---

### Task 1: The `VisibilityTier` type

**Files:**
- Create: `internal/record/tier.go`, `internal/record/ids.go`
- Test: `internal/record/tier_test.go`

**Interfaces:**
- Produces: `record.VisibilityTier`, `record.Observed`, `record.Reconstructed`, `record.Internal`; `(VisibilityTier) Valid() bool`, `String() string`, `MarshalJSON() ([]byte, error)`, `(*VisibilityTier) UnmarshalJSON([]byte) error`; and the shared identifiers `record.RecordID` (`string`), `record.Hash` (`[32]byte`), `record.IdemKey` (`string`).

- [ ] **Step 1: Write the failing test** in `internal/record/tier_test.go`

Assert: `Observed.Valid()`, `Reconstructed.Valid()`, `Internal.Valid()` are all true; `VisibilityTier{}.Valid()` is false; `Observed.String() == "observed"`; `Observed != Reconstructed`; `json.Marshal(Observed) == []byte(`"observed"`)`; and `json.Unmarshal([]byte(`"bogus"`), &t)` returns a non-nil error while leaving `t.Valid() == false`.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/record/ -run TestVisibilityTier -v`
Expected: FAIL — `undefined: record.VisibilityTier`

- [ ] **Step 3: Implement `tier.go`**

```go
type tierValue uint8

const (
    tierInvalid       tierValue = iota // zero value; never valid
    tierObserved
    tierReconstructed
    tierInternal
)

type VisibilityTier struct{ v tierValue }

var (
    Observed      = VisibilityTier{tierObserved}
    Reconstructed = VisibilityTier{tierReconstructed}
    Internal      = VisibilityTier{tierInternal}
)
```

The struct field is unexported so no other package can construct a valid tier. `MarshalJSON` writes the `String()` form; `UnmarshalJSON` accepts only those three strings and returns an error otherwise, leaving the receiver invalid.

`ids.go` declares only `type RecordID string`, `type Hash [32]byte`, and `type IdemKey string` — the shared identifiers the rest of the package builds on. Both files are tiny; they live together because `tier.go` and `ids.go` are the package's foundation and nothing in the package predates them.

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/record/ -run TestVisibilityTier -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/record/ && git commit -m "feat(record): add unforgeable VisibilityTier type"
```

---

### Task 2: `ReasonKind`, evidence types, and the three constructors

**Files:**
- Create: `internal/record/reason.go`
- Test: `internal/record/reason_test.go`

**Interfaces:**
- Consumes: `record.VisibilityTier` and its three values, plus `record.RecordID` and `record.Hash`, all from Task 1.
- Produces: `record.ReasonKind` and its constants; `(ReasonKind) AllowedTier() (VisibilityTier, bool)`; `record.EvidenceSource` (`SourceMem0Response`, `SourceNotaryInstrumentation`); `record.ObservedEvidence`, `record.ReconstructedEvidence`, `record.InternalNote`; `record.Reason`; `NewObservedReason(ReasonKind, ObservedEvidence) (Reason, error)`, `NewReconstructedReason(ReasonKind, ReconstructedEvidence) (Reason, error)`, `NewInternalReason(ReasonKind, InternalNote) (Reason, error)`; `(Reason) Kind() ReasonKind`, `Tier() VisibilityTier`, `Observed() (ObservedEvidence, bool)`, `Reconstructed() (ReconstructedEvidence, bool)`, `InternalNote() (InternalNote, bool)`, `Validate() error`; and the serialization pair `(Reason) Encode() ([]byte, error)` / `ParseReason([]byte) (Reason, error)`.

`Encode`/`ParseReason` exist because `internal/store` is a **different package** and cannot reach `Reason`'s unexported fields — encoding is the record package's responsibility, and the store persists opaque bytes. `Encode` emits the tier, the kind, and the tier's payload; `ParseReason` rebuilds through the constructors so a row tampered with in SQLite fails validation rather than silently becoming a different claim.

- [ ] **Step 1: Write the failing test** in `internal/record/reason_test.go`

Assert:
- `ReasonReturnedBySearch.AllowedTier() == (Observed, true)`; `ReasonAbsentFromSearch.AllowedTier() == (Reconstructed, true)`; `ReasonRemovedByMem0.AllowedTier() == (Internal, true)`; `ReasonKind("nonsense").AllowedTier()` returns `ok == false`.
- `NewObservedReason(ReasonReturnedBySearch, ev)` succeeds and `r.Tier() == Observed`.
- `NewObservedReason(ReasonAbsentFromSearch, ev)` returns an error — the kind forbids the tier.
- `NewObservedReason(ReasonReturnedBySearch, ObservedEvidence{})` returns an error — zero evidence is invalid.
- `NewReconstructedReason(ReasonAbsentFromSearch, ev)` with an empty `Basis` returns an error.
- `NewInternalReason(ReasonRemovedByMem0, note)` with an empty `Opaque` returns an error.
- `Reason{}.Validate()` returns a non-nil error.
- **Canonicalisation:** `NewObservedEvidence(SourceMem0Response, []byte(`{"b":1,"a":2.50}`))` and `NewObservedEvidence(SourceMem0Response, []byte(`{"a":2.50,"b":1}`))` produce evidence with byte-identical payloads.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/record/ -run TestReason -v`
Expected: FAIL — `undefined: record.NewObservedReason`

- [ ] **Step 3: Implement the kind vocabulary and the allowed-tier matrix**

```go
type ReasonKind string

const (
    ReasonSearchPerformed    ReasonKind = "search_performed"
    ReasonAddAcknowledged    ReasonKind = "add_acknowledged"
    ReasonReturnedBySearch   ReasonKind = "returned_by_search"
    ReasonStoredByMem0       ReasonKind = "stored_by_mem0"
    ReasonKeptByContentMatch ReasonKind = "kept_by_content_match"
    ReasonAbsentFromSearch   ReasonKind = "absent_from_search"
    ReasonNoFactsExtracted   ReasonKind = "no_facts_extracted"
    ReasonRemovedByMem0      ReasonKind = "removed_by_mem0"
    ReasonAddFailed          ReasonKind = "add_failed"
    ReasonAuditUnavailable   ReasonKind = "audit_unavailable"
)
```

`AllowedTier` is a single `switch` mapping each kind to exactly one tier: `Observed` for `search_performed`, `add_acknowledged`, `returned_by_search`, `stored_by_mem0`, `add_failed`, `audit_unavailable`; `Reconstructed` for `kept_by_content_match`, `absent_from_search`, `no_facts_extracted`; `Internal` for `removed_by_mem0`.

**Spec amendment — `search_performed` and `add_acknowledged` are not in the spec.** The spec's §5 vocabulary names the outcomes of memory decisions but not the observation of the *request* that produced them. Since every record carries a `Reason` (§5), a `search_performed` or `add_requested` record had no legal kind. These two `Observed` kinds fill that hole; the spec's §5 vocabulary list and allowed-tier matrix should be amended to match.

- [ ] **Step 4: Implement the evidence types with canonicalising constructors**

```go
type EvidenceSource uint8

const (
    SourceInvalid EvidenceSource = iota
    SourceMem0Response
    SourceNotaryInstrumentation
)

type ObservedEvidence struct{ source EvidenceSource; payload []byte }
type ReconstructedEvidence struct{ basis []RecordID; rule string; ruleVersion string; confidence float64 }
type InternalNote struct{ opaque string }
```

`NewObservedEvidence(src EvidenceSource, payload []byte) (ObservedEvidence, error)` rejects `SourceInvalid` and rejects a non-JSON payload. It **canonicalises** the payload: decode with `json.NewDecoder(bytes.NewReader(payload))` plus `dec.UseNumber()`, then re-`json.Marshal`. Go sorts map keys on marshal and `json.Number` preserves the original numeric literal, so key order and float formatting cannot perturb the record hash (spec §5).

- [ ] **Step 5: Implement `Reason` and the three constructors**

```go
type Reason struct {
    kind  ReasonKind
    tier  VisibilityTier
    obs   *ObservedEvidence
    rec   *ReconstructedEvidence
    inter *InternalNote
}
```

Each constructor checks `kind.AllowedTier()` against its own tier and returns `fmt.Errorf("reason %s requires tier %s, got %s: %w", ...)` on mismatch, then validates its payload. `Validate()` returns an error when `tier` is invalid, when `kind` is unknown, or when the payload pointer matching the tier is nil. There are no setters and no exported fields, so a `Reconstructed` value cannot be relabelled `Observed`.

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test ./internal/record/ -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/record/ && git commit -m "feat(record): add ReasonKind matrix, evidence types, per-tier constructors"
```

---

### Task 3: Negative-compile fixtures

`VisibilityTier` cannot be forged and a `Reason` cannot be built without a constructor — but Go still permits the zero literal. This task proves the forgeable part is closed, which is how the spec's "compile error" criterion (§5) is actually satisfied.

**Files:**
- Create: `internal/record/negative_test.go`
- Create: `testdata/negative/forge_tier.go.txt`, `testdata/negative/bare_reason.go.txt`, `testdata/negative/observed_from_nothing.go.txt`

**Interfaces:**
- Consumes: `record.VisibilityTier`, `record.Reason`, `record.NewObservedReason` from Tasks 1–2.
- Produces: nothing. Test-only.

- [ ] **Step 1: Write the fixtures as `.go.txt` files**

`forge_tier.go.txt` — must not compile:
```go
package fixture
import "notary/internal/record"
var _ = record.VisibilityTier{v: 3}
```
`bare_reason.go.txt`:
```go
package fixture
import "notary/internal/record"
var _ = record.Reason{}
var _ = record.Reason{kind: record.ReasonReturnedBySearch}
```
`observed_from_nothing.go.txt`:
```go
package fixture
import "notary/internal/record"
var _ = record.ObservedEvidence{source: record.SourceMem0Response, payload: []byte(`{}`)}
```

`.go.txt` is used so `go build ./...` never sees the fixtures and the directory is not a broken package.

- [ ] **Step 2: Write the failing test** in `internal/record/negative_test.go`

`TestNegativeFixturesDoNotCompile` reads each `testdata/negative/*.go.txt`, writes it to `t.TempDir()/pkg/fixture.go`, runs `go build ./...` in that temp module (with a `go.mod` requiring `notary` via a `replace` directive pointing at the repo root), and asserts a **non-zero** exit. It also asserts the failure is a compile error, not a missing-module error, by requiring the output to contain `record`.

- [ ] **Step 3: Run it and confirm it passes**

Run: `go test ./internal/record/ -run TestNegativeFixturesDoNotCompile -v`
Expected: PASS — all three fixtures fail to compile.

- [ ] **Step 4: Sanity-check the harness is not vacuous**

Temporarily add a fourth fixture that *should* compile (`var _ = record.Observed`) and confirm the test **fails** on it; then delete it. A negative-compile harness that passes on everything is worthless.

- [ ] **Step 5: Commit**

```bash
git add internal/record/negative_test.go testdata/ && git commit -m "test(record): prove tier forging and bare reasons do not compile"
```

---

### Task 4: `EventType`, `Record`, and its component types

**Files:**
- Create: `internal/record/event.go`
- Test: `internal/record/event_test.go`

**Interfaces:**
- Consumes: `record.Reason` (Task 2), `record.VisibilityTier` (Task 1), and the identifiers `record.RecordID`, `record.Hash`, `record.IdemKey` (Task 1).
- Produces: `record.EventType` and its constants, `record.Scope`, `record.Subject`, `record.Content`, `record.Record`, `(Record) Validate() error`.

- [ ] **Step 1: Write the failing test** in `internal/record/event_test.go`

Assert: a fully-populated `Record` returns `nil` from `Validate()`; a `Record` whose `Reason` is the zero value returns an error; a `Record` with an empty `ID` returns an error; a `Record` with an empty `Subject.ContentHash` returns an error; `Record` with `Content == nil` validates (content is optional for events that carry none).

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/record/ -run TestRecordValidate -v`
Expected: FAIL — `undefined: record.Record`

- [ ] **Step 3: Implement the types**

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

type Scope struct{ UserID, AgentID, AppID, RunID string }
type Subject struct{ MemoryID string; Scope Scope; ContentHash Hash }
type Content struct{ Text string; Sensitive bool }

type Record struct {
    ID             RecordID
    Seq            uint64
    At             time.Time
    RecordedAt     time.Time
    Event          EventType
    Reason         Reason
    Subject        Subject
    Content        *Content
    IdempotencyKey IdemKey
    PrevHash       Hash
    Hash           Hash
    Signature      []byte
    SignerKeyID    string
}
```

`At` is when the Mem0 event happened; `RecordedAt` is when Notary wrote it. Both are stored as UTC. `Validate` rejects an empty `ID`, an unknown `EventType`, an invalid `Reason`, and an all-zero `Subject.ContentHash`; it deliberately does **not** check `Seq`, `Hash`, or `Signature`, because those are assigned by `Ledger.Append` (Task 9).

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/record/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/record/ && git commit -m "feat(record): add EventType, Record, and component types"
```

---

### Task 5: Canonical encoding and the record hash

**Files:**
- Create: `internal/record/chain.go`
- Test: `internal/record/chain_test.go`, `internal/record/chain_property_test.go`

**Interfaces:**
- Consumes: `record.Record`, `record.Hash` (Task 4).
- Produces: `record.GenesisHash` (`Hash`, 32 zero bytes); `CanonicalBytes(Record) ([]byte, error)`; `ComputeHash(Record) (Hash, error)`; `LinkOK(prev, cur Record) bool`.

- [ ] **Step 1: Write the failing test** in `internal/record/chain_test.go`

Assert: `ComputeHash(r)` is stable across repeated calls; two records differing only in `Seq` hash differently; two records differing only in `PrevHash` hash differently; changing `Hash` or `Signature` on the input does **not** change the computed hash (both are excluded from the canonical form); `GenesisHash` is 32 zero bytes; `LinkOK(genesis, first)` is false when `first.PrevHash != GenesisHash`.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/record/ -run TestComputeHash -v`
Expected: FAIL — `undefined: record.ComputeHash`

- [ ] **Step 3: Implement the canonical encoder and hash**

```go
var GenesisHash Hash // 32 zero bytes

func CanonicalBytes(r Record) ([]byte, error)
func ComputeHash(r Record) (Hash, error)
func LinkOK(prev, cur Record) bool
```

`CanonicalBytes` writes length-prefixed fields (`binary.BigEndian.PutUint32` length, then bytes) in a fixed order: `ID`, `Event`, `Reason` (kind, tier, then the tier's payload — for `Observed`, the already-canonicalised bytes; for `Reconstructed`, basis IDs in their given order, rule, rule version, and `confidence` via `strconv.FormatFloat(confidence, 'f', 6, 64)`; for `Internal`, the opaque string), `Subject`, `Content`, `IdempotencyKey`, `At` and `RecordedAt` as `time.RFC3339Nano` in UTC, and `SignerKeyID`. It excludes `Hash` and `Signature`.

`ComputeHash` returns `sha256("notary/record/v1" ‖ CanonicalBytes(r) ‖ uint64 BE r.Seq ‖ r.PrevHash[:])`.

Do **not** use `encoding/json` for the record body. The evidence payload is already canonicalised (Task 2), and `json` struct encoding would leave float formatting of `confidence` uncontrolled.

- [ ] **Step 4: Run the test and confirm it passes**

Run: `go test ./internal/record/ -run TestComputeHash -v`
Expected: PASS

- [ ] **Step 5: Write the failing property test** in `internal/record/chain_property_test.go`

`TestEveryFieldChangesHash` builds a baseline `Record`, then for each of ~10 mutations — `ID`, `Seq`, `At`, `RecordedAt`, `Event`, `Reason` (swap kind/tier via a valid second construction), `Subject.MemoryID`, `Subject.ContentHash`, `Content.Text`, `Content.Sensitive`, `IdempotencyKey`, `SignerKeyID` — asserts `ComputeHash(mutated) != ComputeHash(baseline)`. Also assert the empty store case: `ComputeHash` on a record with `Seq == 0` and `PrevHash == GenesisHash` does not error.

- [ ] **Step 6: Run it and confirm it passes**

Run: `go test ./internal/record/ -run TestEveryFieldChangesHash -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/record/ && git commit -m "feat(record): add canonical encoding and chain hash"
```

---

### Task 6: The SQLite store

**Files:**
- Create: `internal/store/store.go`, `internal/store/sqlite.go`
- Test: `internal/store/sqlite_test.go`

**Interfaces:**
- Consumes: `record.Record`, `record.RecordID`, `record.IdemKey` (Task 4).
- Produces: `store.Store` interface; `store.Open(path string) (*SQLiteStore, error)`; `store.ErrNotFound`; `store.ErrDuplicateIdemKey`.

```go
type Store interface {
    PutRecord(record.Record) error
    GetRecord(record.RecordID) (record.Record, error)
    ListRecords(from, to time.Time) ([]record.Record, error)
    Head() (record.Record, bool, error)   // last record by Seq; false when empty
    ByIdemKey(record.IdemKey) (record.Record, bool, error)
    Close() error
}
```

- [ ] **Step 1: Write the failing test** in `internal/store/sqlite_test.go`

Assert: `Open` on a fresh temp path creates the schema and `Head()` returns `false` with no error (Review Focus #1); `PutRecord` then `GetRecord` round-trips every field, including the `Reason` tier and evidence payload; `GetRecord` on an unknown ID returns `ErrNotFound`; `ListRecords` returns records ordered by `At` ascending; `ListRecords` is inclusive at both range bounds; `PutRecord` of a second record with the same `IdempotencyKey` returns `ErrDuplicateIdemKey`.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/store/ -v`
Expected: FAIL — `undefined: store.Open`

- [ ] **Step 3: Implement `sqlite.go`**

Enable WAL (`PRAGMA journal_mode=WAL`) and `PRAGMA foreign_keys=ON` on open. Schema, snake_case columns:

```sql
CREATE TABLE IF NOT EXISTS records (
  seq              INTEGER PRIMARY KEY,
  id               TEXT    NOT NULL UNIQUE,
  at               TEXT    NOT NULL,
  recorded_at      TEXT    NOT NULL,
  event            TEXT    NOT NULL,
  tier             TEXT    NOT NULL,
  reason_kind      TEXT    NOT NULL,
  reason_payload   BLOB    NOT NULL,
  memory_id        TEXT    NOT NULL DEFAULT '',
  user_id          TEXT    NOT NULL DEFAULT '',
  agent_id         TEXT    NOT NULL DEFAULT '',
  app_id           TEXT    NOT NULL DEFAULT '',
  run_id           TEXT    NOT NULL DEFAULT '',
  content_hash     BLOB    NOT NULL,
  content_text     TEXT,
  content_sensitive INTEGER NOT NULL DEFAULT 0,
  idempotency_key  TEXT    NOT NULL DEFAULT '',
  prev_hash        BLOB    NOT NULL,
  hash             BLOB    NOT NULL,
  signature        BLOB    NOT NULL,
  signer_key_id    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_records_at ON records(at);
-- Partial, not a column constraint: only non-empty keys are unique. Phase 3
-- deliberately writes records before idempotency exists (spec §13), and a
-- plain UNIQUE would reject the second keyless record.
CREATE UNIQUE INDEX IF NOT EXISTS idx_records_idem ON records(idempotency_key)
  WHERE idempotency_key <> '';
```

Times are stored as a **fixed-width UTC instant**, formatted with the layout `2006-01-02T15:04:05.000000000Z07:00` after calling `.UTC()` (which renders the literal `Z`). Fixed width is load-bearing: SQLite compares TEXT with BINARY collation, so lexicographic order equals chronological order **only** when every stored instant has identical width. `time.RFC3339Nano` trims trailing zeros, which makes fractional width vary — a whole-second value sorts after a sub-second one (`'Z'` 0x5A > `'.'` 0x2E), so an `at <= ?` bound would silently drop in-range records. Use `Z07:00` rather than a literal `Z` in the layout: a literal `Z` is emitted verbatim and would silently mis-render a non-UTC time, whereas `Z07:00` renders the real offset and makes a forgotten `.UTC()` visible.
Parse with the matching layout. This affects only `at`/`recorded_at` storage and range queries; the record hash is unaffected because `CanonicalBytes` formats times itself (Task 5). `reason_payload` holds the bytes from `Reason.Encode()` (Task 2); the tier and kind are also stored as their string forms so a row is readable and so `verify` can report a field name without decoding. `GetRecord` and `ListRecords` rebuild the `Reason` via `record.ParseReason`, so a row tampered with in SQLite surfaces as a decode error rather than as a silently different claim. `PutRecord` returns `ErrDuplicateIdemKey` only for a duplicate **non-empty** key; an empty key is stored as-is. `ByIdemKey("")` reports not-found.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/store/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/store/ && git commit -m "feat(store): add SQLite store with schema, chain-aware accessors"
```

---

### Task 7: Signing keys

**Files:**
- Create: `internal/sign/sign.go`
- Modify: `config/config.go` (add strings only: `SigningKeyEnv` and `TrustedKeysPath`)
- Test: `internal/sign/sign_test.go`, `internal/sign/canary_test.go`

**Interfaces:**
- Consumes: `record.Hash` (Task 4).
- Produces: `sign.KeySource` (`KeySourceEnv`, `KeySourceFile`, `KeySourceKeychain`); `sign.Signer`; `sign.NewSigner(KeySource) (*Signer, error)`; `(*Signer) KeyID() string`; `(*Signer) Sign([]byte) ([]byte, error)`; `sign.Verifier`; `sign.NewVerifier(keyring map[string]ed25519.PublicKey) *Verifier`; `(*Verifier) Verify(keyID string, msg, sig []byte) error`; `sign.ErrNoKeyConfigured`; `sign.ErrKeychainUnsupported`; `sign.ErrLoosePerms`; plus `sign.ErrUnknownKey` and `sign.ErrInvalidSignature` so `Verify` can distinguish an unknown key ID from a bad signature.

**Redaction must live on `Signer` itself, with value receivers** — `String`, `Format`, `GoString`, and `MarshalJSON`. Putting them only on the inner key-material type does not work: `fmt` never consults a nested type's `GoStringer` for `%#v` on an outer struct, and pointer-receiver methods do not apply to embedded values. `Signer` must therefore implement `fmt.Formatter`/`fmt.Stringer`/`fmt.GoStringer`/`json.Marshaler` in its own right, or `%v`, `%+v`, `%x`, and `%#v` all print the raw private key. `KeyID()` stays readable — only private key material is secret.

**`config` must not carry crypto types or import `internal/sign`** (that would create a cycle once `sign` needs config). It holds only the strings `SigningKeyEnv` and `TrustedKeysPath`; `sign` translates them into its own `KeySource`.

The canary test (§12, Review Focus #5) must assert **positively** that every verb renders exactly `[redacted]`, not merely that a list of secret spellings is absent — an enumerated list misses the `0x`-prefixed Go-syntax form that `%#v` emits, which is precisely the leak this task exists to prevent.

- [ ] **Step 1: Write the failing tests**

`TestNewSignerFailsWithoutKey` — `NewSigner(KeySource{Kind: KeySourceEnv, Ref: "NOTARY_SIGNING_KEY"})` with the env unset returns `ErrNoKeyConfigured` and a nil signer. It must **never** generate a throwaway key (Review Focus #5's companion risk).

`TestNewSignerRejectsLoosePerms` — a key file written `0644` returns `ErrLoosePerms`; the same file rewritten `0600` loads.

`TestKeychainUnsupportedOnThisPlatform` — `KeySourceKeychain` returns `ErrKeychainUnsupported` on Linux rather than silently falling back to a file.

`TestKeyIDIsStableAndDistinct` — two signers from the same seed share a `KeyID()`; a different seed differs; `KeyID()` is the base64 of the SHA-256 of the public key.

`TestSignVerifyRoundTrip` — `Verifier.Verify` accepts a genuine signature and rejects a message tampered by one byte and a signature tampered by one byte.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/sign/ -v`
Expected: FAIL — `undefined: sign.NewSigner`

- [ ] **Step 3: Implement `sign.go`**

Load the private key from `NOTARY_SIGNING_KEY` as base64 (32-byte seed or 64-byte private key) or from a `0600` file whose path comes from config. Wrap the loaded key in a `secretKey` type implementing `String() string`, `Format(fmt.State, rune)`, and `MarshalJSON() ([]byte, error)` that all emit `[redacted]`, so no `%v`, `%+v`, `%s`, or `json.Marshal` can leak it. Name the wrapped key's field `secretKey`, never return it from an exported method, and never include it in an error.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/sign/ -v`
Expected: PASS

- [ ] **Step 5: Write the failing canary test** in `internal/sign/canary_test.go`

`TestKeyMaterialNeverAppearsInOutput` (Review Focus #5) — generate a key from a fixed canary seed, derive its base64, then assert that string is absent from: `fmt.Sprintf("%v", signer)`, `fmt.Sprintf("%+v", signer)`, `fmt.Sprintf("%#v", signer)`, `json.Marshal(signer)`, `fmt.Sprintf("%v", keySource)`, `fmt.Sprintf("%+v", err)` for every error `NewSigner` can return, and a `log.Logger` writing to a `bytes.Buffer` after logging the signer.

- [ ] **Step 6: Run it and confirm it passes**

Run: `go test ./internal/sign/ -run TestKeyMaterialNeverAppearsInOutput -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/sign/ && git commit -m "feat(sign): add ed25519 key loading with redacting key type and keyring verifier"
```

---

### Task 8: Signed head checkpoints

**Files:**
- Create: `internal/sign/checkpoint.go`
- Test: `internal/sign/checkpoint_test.go`

**Interfaces:**
- Consumes: `sign.Signer`, `sign.Verifier` (Task 7); `record.Hash`, `record.RecordID` (Task 4).
- Produces: `sign.Checkpoint`; `sign.NewCheckpoint(seq uint64, hash record.Hash, at time.Time, signer *Signer) (Checkpoint, error)`; `(*Verifier) VerifyCheckpoint(Checkpoint) error`; `sign.MarshalCheckpoint(Checkpoint) ([]byte, error)`; `sign.UnmarshalCheckpoint([]byte) (Checkpoint, error)`.

```go
type Checkpoint struct {
    Seq         uint64
    Hash        record.Hash
    At          time.Time
    SignerKeyID string
    Signature   []byte
}
```

- [ ] **Step 1: Write the failing test** in `internal/sign/checkpoint_test.go`

Assert: a checkpoint round-trips through `MarshalCheckpoint`/`UnmarshalCheckpoint`; `VerifyCheckpoint` accepts a genuine one; changing `Seq`, `Hash`, or `At` after signing makes `VerifyCheckpoint` fail; a checkpoint signed by an unknown `SignerKeyID` fails; `UnmarshalCheckpoint` rejects malformed JSON with an error rather than a partial struct.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/sign/ -run TestCheckpoint -v`
Expected: FAIL — `undefined: sign.NewCheckpoint`

- [ ] **Step 3: Implement `checkpoint.go`**

The signed message is exactly `"notary/checkpoint/v1" ‖ uint64 BE Seq ‖ Hash[:] ‖ At.UTC().Format(RFC3339Nano)` — **no length prefix on the timestamp.** The prefix is unnecessary and must not be added: the first segment of the message is a fixed 19+8+32 = 59 bytes, and the timestamp is last, so everything after byte 59 is unambiguous. Adding a prefix would change the signed artifact away from the form documented here, and a verifier implemented from this document would then reject valid checkpoints.

**`Checkpoint` must carry `MarshalJSON`/`UnmarshalJSON` that delegate to `MarshalCheckpoint`/`UnmarshalCheckpoint`.** Without them the type is a footgun: `record.Hash` is `[32]byte`, so a bare `json.Marshal(Checkpoint)` emits the hash as a 32-element number array, which `UnmarshalCheckpoint` then rejects. Any caller embedding a checkpoint in a larger JSON document — including the `notary verify --write-checkpoint` wiring in Task 11 — would produce an artifact this package cannot read. Make the obvious path the correct one.

`MarshalCheckpoint` writes indented JSON via a struct with exported fields and a `json:"..."` tag on each — the checkpoint is a human-inspectable artifact an auditor may read, so indentation is deliberate. Hex-encode the `Hash` and `Signature` in that wire struct and document the format.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/sign/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/sign/ && git commit -m "feat(sign): add signed head checkpoints"
```

---

### Task 9: The ledger — the single write path

**Files:**
- Create: `internal/ledger/ledger.go`
- Test: `internal/ledger/ledger_test.go`

**Interfaces:**
- Consumes: `store.Store` (Task 6), `sign.Signer`/`sign.Verifier` (Task 7), `record.*` (Tasks 1–5).
- Produces: `ledger.Ledger`; `ledger.New(st store.Store, sg *sign.Signer, now func() time.Time) *Ledger`; `(*Ledger) Append(record.Record) (record.RecordID, error)`; `(*Ledger) GetRecord(record.RecordID) (record.Record, error)`; `(*Ledger) ListRecords(from, to time.Time) ([]record.Record, error)`; `(*Ledger) Head() (record.Record, bool, error)`; `ledger.ErrInvalidTier`.

- [ ] **Step 1: Write the failing test** in `internal/ledger/ledger_test.go`

Assert: `Append` of a valid record returns its ID and the stored record has `Seq == 0`, `PrevHash == record.GenesisHash`, a `Hash` equal to `record.ComputeHash`, a non-empty `Signature`, and a `SignerKeyID` matching the signer; a second `Append` gets `Seq == 1` and `PrevHash` equal to the first record's `Hash`; `Append` of a record whose `Reason` is the zero value returns `ErrInvalidTier` **and** leaves the store unchanged; `Append` of a record with a pre-set non-zero `Seq` returns an error (callers cannot choose a chain position); `Head()` on a fresh ledger returns `false` with no error (Review Focus #1).

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/ledger/ -v`
Expected: FAIL — `undefined: ledger.New`

- [ ] **Step 3: Implement `Append`**

```go
func (l *Ledger) Append(rec record.Record) (record.RecordID, error)
```

Inside one transaction: `Validate()` the record and return `ErrInvalidTier`/`ErrInvalidRecord` **before** opening it; query `SELECT COALESCE(MAX(seq), -1) + 1 FROM records` to get `seq`; assign `rec.Seq`; set `rec.PrevHash` to `GenesisHash` when `seq == 0`, otherwise to the head record's `Hash`; set `rec.RecordedAt = l.now().UTC()` when zero; **set `rec.SignerKeyID = l.signer.KeyID()`**; compute `rec.Hash` via `record.ComputeHash`; sign `rec.Hash[:]`; store. The insert and the `MAX(seq)` read share the transaction, which is why `Seq` cannot race (spec §7).

**Order matters and is easy to get wrong: `SignerKeyID` must be set *before* `ComputeHash`, not after.** `CanonicalBytes` includes `SignerKeyID` (Task 5), so assigning it after hashing would leave the stored record's `SignerKeyID` outside its own digest — and `notary verify` (Task 10) recomputes the hash from the *stored* record, so the recomputation would differ from the stored `Hash` and report tampering on an honest ledger. Setting it first also means the digest commits to the signing identity, which is the stronger property.

Because the `MAX(seq)` read and the insert must not race, they belong to the same transaction. The `Store` interface has no transaction handle, so this task **adds a transactional append primitive to `internal/store`** — e.g. `AppendChained(func(prev record.Record, hasPrev bool) (record.Record, error)) error`, where the callback receives the current head under the write lock and returns the fully-formed record to insert. Reading `Head()` outside a transaction and inserting afterwards reintroduces the race this task exists to prevent.

`Append` must reject a caller-supplied non-zero `Seq` so no caller can influence chain position — this is the property that makes the chain trustworthy.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/ledger/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/ledger/ && git commit -m "feat(ledger): add single write path with in-transaction chain position and signing"
```

---

### Task 10: `notary verify`

**Files:**
- Create: `cmd/notary/verify.go`, `internal/ledger/verify.go`
- Test: `testdata/tamper/README.md`, `internal/ledger/verify_test.go`

**Interfaces:**
- Consumes: `ledger.Ledger` (Task 9), `sign.Verifier` (Task 7), `config.Config.TrustedKeysPath` (Task 7).
- Produces: `ledger.Break` (`{RecordID record.RecordID; Seq uint64; Field string; Detail string}`); `(*Ledger) Verify(v *sign.Verifier) ([]Break, error)`; `sign.LoadTrustedKeys(path string) (map[string]ed25519.PublicKey, error)`; the `notary verify` command with `--checkpoint <file>` (consumed in Task 12) and `--verbose`.

**The trusted-keyring file format, decided here.** `config.TrustedKeysPath` names a file of base64-encoded ed25519 **public** keys, one per line. Blank lines and lines whose first non-space character is `#` are ignored; surrounding whitespace is trimmed. Each key's `KeyID` is computed from the key itself via the existing `keyIDFor` helper — the file never states a key ID. That is deliberate: a format that let the file declare a `keyid` alongside the key would admit a mismatch where the declared identity differs from the key actually used, which is a verification-integrity hole. A malformed line, a key that is not 32 bytes, or a file that is empty after comments is an error, not a silent skip. `LoadTrustedKeys` lives in `internal/sign` (not `cmd/`), so parsing is unit-testable without a CLI.

**`notary verify` with no trusted keys must NOT report success.** If `TrustedKeysPath` is empty or yields an empty keyring, the command must exit non-zero with a plain-language error saying no trusted keys are configured and how to configure them — because "could not verify anything" and "verified everything" must never look the same. Otherwise a broken deployment prints `ok`. `Verify` itself stays pure over the verifier it is given; the emptiness check belongs in the command.

- [ ] **Step 1: Write the failing test** in `internal/ledger/verify_test.go`

Assert: `Verify` on an empty store returns an empty `[]Break` and no error (Review Focus #1); `Verify` on a clean 5-record chain returns no breaks; after `UPDATE records SET event='memory_kept' WHERE seq=3` via raw SQL, `Verify` returns exactly one `Break` with `Seq == 3` and `Field == "hash"` (Review Focus #3); after corrupting `prev_hash` on `seq=4`, the returned break names `seq 4` and `Field == "prev_hash"`; after flipping one signature byte on `seq=2`, the break names `seq 2` and `Field == "signature"`.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/ledger/ -run TestVerify -v`
Expected: FAIL — `undefined: (*ledger.Ledger).Verify`

- [ ] **Step 3: Implement `Verify`**

Walk records ordered by `Seq` ascending. For each, in order: (1) recompute `record.ComputeHash` and compare with the stored `Hash` → `Field: "hash"`; (2) compare `PrevHash` with the predecessor's `Hash`, or with `GenesisHash` at `seq 0` → `Field: "prev_hash"`; (3) `Verifier.Verify` the stored `Hash` under `SignerKeyID` → `Field: "signature"`; (4) detect a `Seq` that is not exactly predecessor+1 → `Field: "seq"`. Record every break rather than stopping at the first, so one run reports all damage. A record that fails to *decode* from the store (a SQLite-level tamper that breaks the `Reason`) surfaces as `Field: "decode"`.

- [ ] **Step 4: Implement `cmd/notary/verify.go`**

`notary verify` loads config, opens the store and the trusted keyring, runs `Verify`, prints one line per break as `record <id> (seq <n>): <field> — <detail>`, prints `ok: <n> records verified` when there are none, and exits non-zero if any break is found. With `--verbose`, print each record's ID and `Seq` as it is checked.

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/ledger/ -v && go run ./cmd/notary verify` (against a fresh DB)
Expected: PASS; `ok: 0 records verified`.

- [ ] **Step 6: Commit**

```bash
git add internal/ledger/ cmd/notary/ && git commit -m "feat(verify): walk the chain, reporting the exact record and field that breaks"
```

---

### Task 11: Truncation detection

**Files:**
- Modify: `internal/ledger/verify.go`, `cmd/notary/verify.go:1`
- Test: `internal/ledger/truncation_test.go`

**Interfaces:**
- Consumes: `sign.Checkpoint` (Task 8), `(*Ledger) Verify` (Task 10).
- Produces: `(*Ledger) Checkpoint(sg *sign.Signer, now time.Time) (sign.Checkpoint, error)`; `(*Ledger) VerifyAgainstCheckpoint(c sign.Checkpoint, v *sign.Verifier) ([]Break, error)`; `ledger.ErrTruncated`.

- [ ] **Step 1: Write the failing test** in `internal/ledger/truncation_test.go`

`TestTruncationIsOnlyCaughtByCheckpoint` (Review Focus #2) — build a 6-record chain; take a checkpoint; then `DELETE FROM records WHERE seq >= 4` via raw SQL. Assert: plain `Verify` returns **no breaks** (the remainder is self-consistent — this is the hazard the feature exists for); `VerifyAgainstCheckpoint` returns a break whose `Field == "truncation"` and whose error wraps `ErrTruncated`; and after deleting *all* records, `VerifyAgainstCheckpoint` still reports truncation rather than "ok: 0 records".

Also assert a checkpoint taken against a longer chain than the store is **not** reported as truncation when the store has simply advanced — appending records after the checkpoint must leave `VerifyAgainstCheckpoint` clean.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/ledger/ -run TestTruncationIsOnlyCaughtByCheckpoint -v`
Expected: FAIL — `undefined: (*ledger.Ledger).VerifyAgainstCheckpoint`

- [ ] **Step 3: Implement checkpoint creation and comparison**

`Checkpoint` reads the head and signs `(Seq, Hash, At)`. `VerifyAgainstCheckpoint` first verifies the checkpoint's own signature (an unverifiable checkpoint is a hard error, not a break), then: if the store is empty and the checkpoint's `Seq >= 0`, return `ErrTruncated`; if the store's head `Seq` is **less than** the checkpoint's `Seq`, return `ErrTruncated`; if the head `Seq` is greater or equal, confirm the record at the checkpoint's `Seq` has the checkpoint's `Hash` — a mismatch there means the chain was rewritten, which is also `ErrTruncated`.

- [ ] **Step 4: Wire `--checkpoint <file>` into `notary verify`**

`notary verify --checkpoint c.json` loads the checkpoint via `sign.UnmarshalCheckpoint` and runs both checks. Add `notary verify --write-checkpoint c.json` to emit a fresh checkpoint, which is how Task 12's flow is exercised by hand.

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/ledger/ -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/ledger/ cmd/notary/ && git commit -m "feat(verify): detect chain truncation via signed head checkpoints"
```

---

### Task 12: Mid-write crash safety

**Files:**
- Test: `internal/ledger/crash_test.go`
- Create: `internal/ledger/testdata/crashwriter/main.go` (a `main` package the test builds and runs)

**Interfaces:**
- Consumes: `ledger.Ledger` (Task 9), `store.Open` (Task 6).
- Produces: test-only.

- [ ] **Step 1: Write the crashing helper**

`testdata/crashwriter/main.go` opens a store at `os.Args[1]`, builds a ledger, appends `n` records, and then — when `os.Args[2] == "crash"` — calls `os.Exit(1)` immediately after the final `Append` returns but before the process can flush anything further. This simulates death between hash computation and commit from the ledger's point of view.

- [ ] **Step 2: Write the failing test** in `internal/ledger/crash_test.go`

`TestCrashMidWriteLeavesChainIntact` — `go build` the helper into `t.TempDir()`, run it with `crash`, then reopen the store and assert: `Verify` reports no breaks; the head is either the last fully-committed record or one short of it; **no** row exists whose `hash`, `prev_hash`, or `signature` is NULL or zero-length; and `VerifyAgainstCheckpoint` against a checkpoint taken before the run reports no truncation.

- [ ] **Step 3: Run it and confirm it passes**

Run: `go test ./internal/ledger/ -run TestCrashMidWriteLeavesChainIntact -v`
Expected: PASS — no partial or corrupt entry exists. If this fails, the transaction boundary in `Append` (Task 9) is wrong; fix it there, not here.

- [ ] **Step 4: Commit**

```bash
git add internal/ledger/ && git commit -m "test(ledger): prove a mid-write crash cannot corrupt the chain"
```

---

### Task 13: The Mem0 REST client

**Files:**
- Create: `internal/mem0/client.go`, `internal/mem0/types.go`
- Test: `internal/mem0/client_test.go`, `internal/mem0/testdata/*.json`

**Interfaces:**
- Produces: `mem0.Client`; `mem0.NewClient(baseURL, apiKey string, hc *http.Client) *Client`; `(*Client) Add(ctx, AddRequest) (AddResponse, error)`; `(*Client) Search(ctx, SearchRequest) (SearchResponse, error)`; `(*Client) GetAll(ctx, GetAllRequest) (GetAllResponse, error)`; `(*Client) History(ctx, memoryID string) (HistoryResponse, error)`; `(*Client) EventStatus(ctx, eventID string) (EventStatusResponse, error)`; and the wire types `mem0.AddRequest`, `mem0.AddResponse`, `mem0.SearchRequest`, `mem0.SearchResponse`, `mem0.GetAllRequest`, `mem0.GetAllResponse`, `mem0.HistoryEvent`, `mem0.HistoryResponse`, `mem0.EventStatusResponse`, `mem0.Memory`, `mem0.SearchResult`.

**The fixtures are already recorded.** `internal/mem0/testdata/` contains seven
real captured responses plus a `FIXTURES.md` documenting their provenance. Do not
re-record and do not hand-write replacements — recording needs a live API key that is
deliberately absent from this repo.

**The recorded shapes are richer and less uniform than the documented shapes assumed, and
they are the contract.** Details that must be honoured are listed in `FIXTURES.md`; the
load-bearing ones are: `search` returns `score_breakdown{semantic,bm25,entity}` alongside
`score`; `get_all` returns `replaced_by`, `synthesized`, and `structured_attributes`;
`event_status` carries `event_type`, a full `payload` echo, and `results[]` with each
result's own `event`; field presence is inconsistent between endpoints (`search` emits
`agent_id`/`app_id`/`run_id` as explicit `null` where `get_all` omits them, and `metadata`
is `{}` in one and `null` in another); and no `hash` field was observed anywhere.

```go
// Memory is the memory object as returned by list/search endpoints. Optional
// fields are pointers or omit-empty-tolerant because the API is inconsistent
// about emitting them as null versus omitting them (see FIXTURES.md).
type Memory struct {
    ID          string         `json:"id"`
    Memory      string         `json:"memory"`
    UserID      string         `json:"user_id"`
    AgentID     *string        `json:"agent_id"`
    AppID       *string        `json:"app_id"`
    RunID       *string        `json:"run_id"`
    Metadata    map[string]any `json:"metadata"`
    Categories  []string       `json:"categories"`
    CreatedAt   string         `json:"created_at"`
    UpdatedAt   string         `json:"updated_at"`
    ExpiresAt   *string        `json:"expiration_date"`
    // Hash is retained for completeness only: NO fixture shows this field, so
    // nothing here proves the platform returns it.
    Hash string `json:"hash"`
}

// SearchResult adds the ranking evidence to a Memory.
type SearchResult struct {
    Memory
    Score          float64        `json:"score"`
    ScoreBreakdown ScoreBreakdown `json:"score_breakdown"`
}

type ScoreBreakdown struct {
    Semantic float64 `json:"semantic"`
    BM25     float64 `json:"bm25"`
    Entity   float64 `json:"entity"`
}
```

- [ ] **Step 1: Write the failing test** in `internal/mem0/client_test.go`

Using `httptest.NewServer` with recorded fixtures from `testdata/`, assert: `Add` POSTs to `/v3/memories/add/` with header `Authorization: Token <key>` and a body whose `messages` and `infer` match the request, and decodes `{status, event_id}`; `Search` POSTs to `/v3/memories/search/` with entity IDs **inside `filters`** (a top-level entity ID is a 400 from Mem0) and decodes `results` with a `score` on each; `GetAll` POSTs to `/v3/memories/` and decodes the `{count, next, previous, results}` envelope, correctly handling `metadata: null` and the absent `agent_id`/`app_id`/`run_id`; `History` GETs `/v1/memories/{id}/history/` and decodes an event list whose entries carry `event`, `old_memory`, and `new_memory`; `EventStatus` GETs `/v1/event/{id}/` and decodes the full shape including `event_type`, `status`, `results[].event`, and `latency`; `Delete` issues `DELETE /v1/memories/{id}/`; a 400 response body is surfaced in the returned error (the fixture's body is `{"error": "<python-repr string>"}`, so parse defensively), and a 5xx is surfaced as an error, not a panic; a request for `/v1/ping/` is not made implicitly.

Assert against the recorded fixtures byte-shape where it matters (e.g. `score_breakdown` present, `event: "ADD"` in `results`), so a future API change shows up as a failing test rather than silent drift.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/mem0/ -v`
Expected: FAIL — `undefined: mem0.NewClient`

- [ ] **Step 3: Implement `types.go` then `client.go`**

Plain `net/http`, no SDK. `NewClient` sets a default `*http.Client` with a 30s timeout when `hc` is nil. The API key is stored in an unexported field and never included in an error or a log. Request bodies are built from small exported structs so the `filters`-nesting rule is expressed in the type, not at each call site.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/mem0/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/mem0/ && git commit -m "feat(mem0): add thin REST client for add/search/get_all/history/event"
```

---

### Task 14: The hash-chained gap log

**Files:**
- Create: `internal/gap/gaplog.go`
- Test: `internal/gap/gaplog_test.go`

**Interfaces:**
- Consumes: `record.EventType`, `record.Scope`, `record.Hash` (Task 4).
- Produces: `gap.Entry`; `gap.Break` (`{Line int; Counter uint64; Field string; Detail string}`); `gap.Log`; `gap.Open(path string) (*Log, error)`; `(*Log) Record(e Entry) error`; `(*Log) Verify() ([]Break, error)`; `(*Log) Close() error`; `gap.Verify(path string) ([]Break, error)`; `gap.WriteChannels(path string, w io.Writer) []io.Writer`.

```go
type Entry struct {
    Counter       uint64
    At            time.Time
    Kind          record.EventType
    Scope         record.Scope
    CorrelationID string
    Detail        string
    PrevHash      record.Hash
    Hash          record.Hash
}
```

- [ ] **Step 1: Write the failing test** in `internal/gap/gaplog_test.go`

Assert: appending three entries produces counters 0, 1, 2 with each `PrevHash` equal to the previous entry's `Hash` and the first equal to the 32 zero bytes; `Verify` on a clean log returns no breaks; **deleting the middle line of the file** makes `Verify` report a break with `Field == "chain"` (a gap cannot be silently erased); editing a `Detail` field makes `Verify` report `Field == "hash"`; reopening an existing log continues the counter rather than restarting at 0; `WriteChannels` returns two distinct writers and the marker is written to both.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/gap/ -v`
Expected: FAIL — `undefined: gap.Open`

- [ ] **Step 3: Implement `gaplog.go`**

The log is newline-delimited JSON, opened `O_APPEND|O_CREATE|O_WRONLY` with `0600`, and flushed after every write. `Open` reads the tail to recover the last counter and hash, so a restart continues the chain. `Hash` is `sha256("notary/gap/v1" ‖ uint64 BE Counter ‖ At RFC3339Nano UTC ‖ Kind ‖ Scope fields ‖ CorrelationID ‖ Detail ‖ PrevHash[:])`.

The log deliberately does **not** use the record store: it exists for the case where the store is unreachable, so sharing a failure domain would defeat it. Document that in the file's package comment.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/gap/ -v`
Expected: PASS

- [ ] **Step 5: Extend `notary verify` to cross-check the gap log**

Add `ledger.Break{Field: "gap"}` entries for gap records whose `(Kind, Scope, CorrelationID)` matches no record in the store. `notary verify` prints them after chain breaks and exits non-zero if any exist. Add a test in `internal/gap/gaplog_test.go` that writes a gap entry with a correlation ID absent from the store and asserts `notary verify` surfaces it. **Note:** the "matched" case cannot be tested until Task 15 gives gaps a corresponding record, so assert only the unmatched case here and revisit in Task 19.

- [ ] **Step 6: Run the tests and confirm they pass**

Run: `go test ./internal/gap/ ./internal/ledger/ -v`
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/gap/ internal/ledger/ cmd/notary/ && git commit -m "feat(gap): add hash-chained gap log and wire it into verify"
```

---

### Task 15: The `Interceptor` interface and its fail mode

**Files:**
- Create: `internal/interceptor/interceptor.go`
- Modify: `config/config.go` (add `FailMode FailMode`)
- Test: `internal/interceptor/interceptor_test.go`

**Interfaces:**
- Consumes: `record.Record` (Task 4), `ledger.Ledger` (Task 9), `gap.Log` (Task 14).
- Produces: `interceptor.FailMode` (`FailOpenLoud`, `FailClosed`); `interceptor.Interceptor`; `interceptor.AuditWriter`; `interceptor.NewAuditWriter(l *ledger.Ledger, g *gap.Log, channels []io.Writer) *AuditWriter`; `(*AuditWriter) Write(rec record.Record) error`; `interceptor.ErrFailClosedUnimplemented`.

`channels` comes from `gap.WriteChannels(gapLogPath, os.Stderr)` (Task 14) — constructing them that way is what guarantees the two channels do not share a failure domain.

```go
type FailMode uint8
const (
    FailOpenLoud FailMode = iota + 1
    FailClosed                        // defined; never implemented in v1
)

type AuditWriter struct { /* unexported: ledger, gaplog, channels */ }

// Write attempts the ledger and, on failure, emits an audit_gap marker.
// It NEVER returns an error for a ledger failure: audit problems must not
// become caller problems.
func (w *AuditWriter) Write(rec record.Record) error
```

- [ ] **Step 1: Write the failing test** in `internal/interceptor/interceptor_test.go`

Assert: `FailMode(0)` is not valid and `Validate()` rejects it; `FailClosed.Validate()` returns `ErrFailClosedUnimplemented` (the value exists in the enum but is never implemented — spec §1 non-goals); `AuditWriter.Write` on a healthy ledger stores exactly one record and returns nil; `AuditWriter.Write` with a ledger whose store is closed returns **nil** (not an error), writes an `audit_gap` entry to the gap log, and writes a marker line to **every** provided channel (Review Focus #4).

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/interceptor/ -v`
Expected: FAIL — `undefined: interceptor.NewAuditWriter`

- [ ] **Step 3: Implement `interceptor.go`**

```go
type Interceptor interface {
    FailMode() FailMode
    Close() error
}
```

`AuditWriter.Write` is the fail-open-loud mechanism in one place: try `ledger.Append`; on error, construct a `record` with `Event: record.EventAuditGap` and a `Reason` built by `record.NewObservedReason(record.ReasonAuditUnavailable, ev)` where `ev` uses `record.SourceNotaryInstrumentation`; write it to the gap log; write a one-line marker to **every** channel; and return nil regardless. If the gap log write *also* fails, write the channel markers anyway and **then** return the gap-log error — the caller is a background reconciliation path at that point, and a doubly-failed write is a real error worth surfacing.

Note that the `audit_gap` record cannot be written to the ledger (that is what failed), so gaps exist only in the gap log. Task 19 reconciles them back when the store recovers.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/interceptor/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/interceptor/ && git commit -m "feat(interceptor): add Interceptor interface, fail modes, and the fail-open-loud writer"
```

---

### Task 16: The library interceptor

**Files:**
- Create: `internal/interceptor/library/mem0.go`
- Test: `internal/interceptor/library/mem0_test.go`

**Interfaces:**
- Consumes: `mem0.Client` (Task 13), `interceptor.AuditWriter` (Task 15), `record.*` (Tasks 1–5).
- Produces: `library.Mem0Interceptor`, which **implements `interceptor.Interceptor`** (`FailMode() interceptor.FailMode` returning `interceptor.FailOpenLoud`, and `Close() error`); `library.New(mc *mem0.Client, aw *interceptor.AuditWriter, scope record.Scope, now func() time.Time) *Mem0Interceptor`; `(*Mem0Interceptor) Add(ctx, correlationID string, messages []string) (mem0.AddResponse, error)`; `(*Mem0Interceptor) Search(ctx, correlationID string, q mem0.SearchRequest) (mem0.SearchResponse, error)`; `library.ErrMissingCorrelationID`; `library.DeriveCorrelationID`.

This task does **not** set `Record.IdempotencyKey` — Phase 3 writes keyless records and the store's uniqueness is a partial index for exactly that reason (Task 6). Task 18 adds key derivation and tightens `Append`.

- [ ] **Step 1: Write the failing test** in `internal/interceptor/library/mem0_test.go`

Assert (all against an `httptest` Mem0, no live API):
- `Add` with a correlation ID makes one Mem0 call and writes exactly one record with `Event == record.EventAddRequested`, `Reason.Tier() == record.Observed`, `Reason.Kind() == record.ReasonAddAcknowledged`, evidence source `record.SourceMem0Response`, and a payload containing the returned `event_id`. The add is *acknowledged* here, not *resolved* — resolution is the reconciler's job (Phase 5).
- `Search` with a correlation ID writes one `EventSearchPerformed` record (`Observed`, kind `record.ReasonSearchPerformed`) whose evidence payload contains the returned result count, plus one `EventMemorySurfaced` record per returned memory, each `Observed` with `ReasonReturnedBySearch`, carrying the memory's `score` and rank in the payload, and `Subject.MemoryID` set.
- `Add` and `Search` with an empty correlation ID return `ErrMissingCorrelationID` **before** any HTTP call is made (assert the test server recorded zero requests) — this is the spec §7 rule that Notary never invents a key from a clock.
- When the Mem0 call returns a 400, `Add` returns that error and writes **no** record.
- When the ledger is broken (closed store), `Search` still returns the Mem0 results successfully and an `audit_gap` lands in the gap log (Review Focus #4).

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/interceptor/library/ -v`
Expected: FAIL — `undefined: library.New`

- [ ] **Step 3: Implement `mem0.go`**

Each method: validate `correlationID` non-empty and return `ErrMissingCorrelationID` immediately; call Mem0; on error return it without writing; on success build the observation record(s) and hand each to `AuditWriter.Write`, ignoring only the ledger-failure path (which `AuditWriter` already absorbs) and returning any gap-log error.

`Subject.ContentHash` is the SHA-256 of the memory text for surfaced results; for the add request it is the SHA-256 of the concatenated messages, so a later `get_all` diff can match by content without re-reading Mem0.

`DeriveCorrelationID(scope record.Scope, seed string) string` is a helper for callers with no natural ID: `sha256("notary/corr/v1" ‖ scope fields ‖ seed)` base64-encoded. It takes an explicit seed and never a timestamp, so it cannot silently become the non-deterministic key the spec forbids.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/interceptor/library/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/interceptor/library/ && git commit -m "feat(library): add Mem0 interceptor writing Observed records inline"
```

---

### Task 17: Idempotency key derivation

**Files:**
- Create: `internal/record/idem.go`
- Test: `internal/record/idem_test.go`

**Interfaces:**
- Consumes: `record.EventType`, `record.Scope`, `record.Hash` (Task 4).
- Produces: `record.DeriveIdemKey(kind EventType, scope Scope, eventID, correlationID string, requestDigest Hash) (IdemKey, error)`; `record.ErrIdemKeyUnavailable`.

- [ ] **Step 1: Write the failing test** in `internal/record/idem_test.go`

Assert: for an add (`EventAddRequested`) with a non-empty `eventID`, the key is stable across calls and differs when `eventID` differs; for a search (`EventSearchPerformed`) with a non-empty `correlationID`, the key is stable and differs when the correlation ID differs; `DeriveIdemKey` with **both** `eventID` and `correlationID` empty returns `ErrIdemKeyUnavailable`; the returned key never contains a timestamp — assert two calls at different wall-clock times produce the same key.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/record/ -run TestDeriveIdemKey -v`
Expected: FAIL — `undefined: record.DeriveIdemKey`

- [ ] **Step 3: Implement `idem.go`**

```go
func DeriveIdemKey(kind EventType, scope Scope, eventID, correlationID string, requestDigest Hash) (IdemKey, error)
```

Prefer `eventID` when non-empty (`sha256("notary/idem/v1" ‖ kind ‖ scope ‖ eventID ‖ requestDigest[:])`); else use `correlationID` with the same construction; else return `ErrIdemKeyUnavailable`. No clock, no randomness, no counter — the same logical event must always produce the same key (spec §7).

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/record/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/record/ && git commit -m "feat(record): add deterministic idempotency key derivation"
```

---

### Task 18: Idempotent append

**Files:**
- Modify: `internal/ledger/ledger.go:1`, `internal/interceptor/library/mem0.go:1`
- Test: `internal/ledger/idempotency_test.go`

**Interfaces:**
- Consumes: `record.DeriveIdemKey` (Task 17), `(*Ledger) Append` (Task 9).
- Produces: `(*Ledger) Append` gains upsert semantics; `ledger.ErrIdemKeyRequired`; `(*AuditWriter) Write` unchanged in signature but now idempotent through the ledger.

- [ ] **Step 1: Write the failing test** in `internal/ledger/idempotency_test.go`

`TestRetriedEventProducesExactlyOneRecord` — append the same `record.Record` (same `IdempotencyKey`) twice; assert the second `Append` returns the **same** `RecordID` as the first, no new row exists, the total count is 1, and `Verify` reports no breaks. Also assert that a record with an empty `IdempotencyKey` returns `ErrIdemKeyRequired` — every write must be dedupable from Phase 4 onward, so an unnamed write is a bug, not a feature.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/ledger/ -run TestRetriedEventProducesExactlyOneRecord -v`
Expected: FAIL — the second `Append` currently returns `store.ErrDuplicateIdemKey`

- [ ] **Step 3: Change `Append` to upsert**

Before assigning a `Seq`, look up `ByIdemKey`. If a record exists, return its `ID` and write nothing — do **not** compare content and do not update anything; the first observation is the record. A *different* event or tier produces a *different* key (Task 17 includes the kind in the digest), so later knowledge appends rather than mutates (spec §7). Keep `store.PutRecord` returning `ErrDuplicateIdemKey`; the ledger is what converts a duplicate into a no-op.

- [ ] **Step 4: Make the interceptor always populate `IdempotencyKey`**

In `library/mem0.go`, call `record.DeriveIdemKey` for every record it writes and return the error if it is unavailable. Update `library/mem0_test.go` to assert every written record has a non-empty `IdempotencyKey`.

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./... -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(ledger): make Append idempotent on the derived key"
```

---

### Task 19: Reconcile gaps back into the ledger

**Files:**
- Modify: `internal/interceptor/interceptor.go:1`, `cmd/notary/verify.go:1`
- Create: `internal/interceptor/replay_gaps.go`
- Test: `internal/interceptor/replay_gaps_test.go`

**Interfaces:**
- Consumes: `gap.Log` (Task 14), `(*Ledger) Append` (Task 18), `(*AuditWriter) Write` (Task 15).
- Produces: `(*AuditWriter) ReplayGaps(ctx context.Context, lookup func(correlationID string) (record.Record, bool)) (int, error)`.

This closes the loop the spec describes in §8: a gap entry carries enough context to re-derive the missing record once the store recovers.

- [ ] **Step 1: Write the failing test** in `internal/interceptor/replay_gaps_test.go`

Assert: with a broken ledger, `Write` a record carrying a correlation ID and an `IdempotencyKey`; the gap log gains one entry and the store has zero records. Repair the store, call `ReplayGaps`, and assert the store now has exactly one record with the original `IdempotencyKey`, the gap log gained a matching "reconciled" entry, and `notary verify` no longer reports a `gap` break for that correlation ID (Review Focus #4, matched case — the assertion deferred from Task 14). Calling `ReplayGaps` twice must add nothing the second time.

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./internal/interceptor/ -run TestReplayGaps -v`
Expected: FAIL — `undefined: (*interceptor.AuditWriter).ReplayGaps`

- [ ] **Step 3: Implement `ReplayGaps`**

For each unreconciled gap entry, re-read the corresponding record from a caller-supplied source. Because a gap entry stores the correlation ID and the failure reason but not the full record, `ReplayGaps` requires the caller to supply a `Lookup func(correlationID string) (record.Record, bool)` mapping. If the lookup misses, leave the entry unreconciled and count it. Reconciliation is idempotent because the replayed record carries its original `IdempotencyKey` and `Append` is a no-op on it (Task 18).

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/interceptor/ ./internal/gap/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/interceptor/ cmd/notary/ && git commit -m "feat(interceptor): reconcile gap-log entries back into the ledger"
```

---

## Verification

After all tasks, run the full suite and the acceptance checks that closed Phases 1–4:

```bash
go build ./... && go test ./... -race -v
go run ./cmd/notary version
go run ./cmd/notary --help
go run ./cmd/notary verify                  # ok: 0 records verified
go run ./cmd/notary verify --write-checkpoint /tmp/c.json
go run ./cmd/notary verify --checkpoint /tmp/c.json
```

Expected: build clean; every package's tests PASS under `-race`; `verify` reports no breaks; the checkpoint round-trips.

**Manual tamper check** (Review Focus #3, done by hand as P2 requires):

```bash
sqlite3 notary.db "UPDATE records SET event='memory_kept' WHERE seq=3;"
go run ./cmd/notary verify    # must name record <id> (seq 3): hash
```
