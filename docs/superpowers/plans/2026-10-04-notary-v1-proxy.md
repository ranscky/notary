# Notary v1 Phase 9 — proxy mode: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `notary proxy`, a reverse proxy in front of Mem0 that records the same `add` and `search` records library mode records, without sitting in the response path.

**Architecture:** Extract the record-building wiring out of `internal/interceptor/library` into a shared `Observer` in `internal/interceptor`, with a one-method `Sink` seam, and refactor `library` to delegate to it. The proxy decodes Mem0's HTTP request and response into the observer's inputs, forwards through `httputil.ReverseProxy`, and hands the resulting records to a bounded queue drained by one writer goroutine. A drop on a full queue becomes a durable gap entry.

**Tech Stack:** Go 1.25, Cobra, stdlib `net/http/httputil`, `modernc.org/sqlite` (pure Go), testify.

**Spec:** `docs/superpowers/specs/2026-10-04-notary-v1-proxy-design.md`

## Global Constraints

- Go 1.25; no new dependencies and **no change to `go.mod`**.
- Stdlib `crypto/ed25519` only; `modernc.org/sqlite` stays pure Go.
- Never panic in library code; wrap errors `fmt.Errorf("...: %w", err)`.
- No global mutable state. No hardcoded secrets. Never log key material.
- The record is the source of truth; a render is a view and never touches a hash.
- Idempotent writes; fail loud, never fail silent.
- **No change to `internal/record`, `internal/export`, or the closed vocabulary** — no new event, reason kind or tier (spec §7). A later change that adds one is a plan violation, and Task 2's test suite asserts the gates still pass.
- `internal/interceptor/library`'s exported surface (`New`, `Add`, `Search`, `FailMode`, `Close`, `Sensitive`, `WithSensitivityRules`, `DeriveCorrelationID`, `ErrMissingCorrelationID`) and every record it emits must be unchanged. This refactor is the one `.clinerules` §5 authorises in writing.
- In proxy mode a failure in the audit-write path must never block, delay, or fail the underlying Mem0 request/response.
- The whole suite runs `-p 1` (`go test -p 1 ./... -count=1`); `internal/ledger` fails spuriously in parallel.
- `gofmt -l .` empty and `go vet ./...` clean before each commit.

## Review Focus

Inputs and conditions the spec implies but no task's own tests necessarily cover. Each gets its test in the task named.

1. **The upstream is unreachable or slow.** The caller must get a real HTTP error promptly, not a hang, and nothing may be recorded or gapped; and neither the error nor a log may carry the `Authorization` header or the query string (spec §8). (Task 3)
2. **A request or response body over `--max-body`.** Forwarded intact to the caller, with no record *and* no gap entry — the asymmetry the spec records at §12 and questions at §13.4. (Task 3)
3. **`Close` racing with in-flight enqueues.** No send on a closed channel, no panic, and no record accepted before `Close` is missing from the sink afterwards. (Task 2, Task 4)
4. **Two identical requests with no correlation header.** One record, not two — decision 2's documented collapse, asserted so it is a known behaviour rather than a surprise. (Task 3)
5. **A non-observed endpoint carrying a malformed body.** Passed through byte-for-byte and never parsed: the proxy must not fail a request it is not recording. (Task 3)

---

### Task 1: The shared `Observer`, and the `library` refactor

**Files:**
- Create: `internal/interceptor/observer.go`
- Test: `internal/interceptor/observer_test.go`
- Modify: `internal/interceptor/library/mem0.go`
- Test: `internal/interceptor/library/mem0_test.go` (tighten the existing assertions)

**Interfaces:**
- Produces:
  - `type Sink interface { Write(rec record.Record) error }`
  - `type Observer struct{ ... }`, `func NewObserver(sink Sink, opts ...ObserverOption) *Observer`
  - `type ObserverOption func(*Observer)`, `func WithRules(rs *RuleSet) ObserverOption`
  - `type AddObservation struct { Scope record.Scope; CorrelationID string; Messages []string; Metadata map[string]any; Response mem0.AddResponse; At, RecordedAt time.Time; Sensitive bool }`
  - `type SearchObservation struct { Scope record.Scope; CorrelationID string; Request mem0.SearchRequest; Response mem0.SearchResponse; At, RecordedAt time.Time; Sensitive bool }`
  - `func (o *Observer) Add(obs AddObservation)`, `func (o *Observer) Search(obs SearchObservation)`
  - `func DeriveCorrelationID(scope record.Scope, seed string) string` — moved here from `library`, which keeps an exporting one-line delegator.
- Consumes: `record.Record`, `record.ContentHash`, `record.DeriveIdemKey`, `mem0.AddResponse`, `mem0.SearchResponse`, `RuleSet`.

**Notes the implementer needs:** the bodies move from `internal/interceptor/library/mem0.go` — `observeAdd` (:298), `observeSearch` (:341), `writeSearchPerformed` (:356), `writeSurfaced` (:398), `classify`/`marking` (:156-174), `idemKey` (:492), `observedReason` (:504), `derivedRecordID` (:520), `DeriveCorrelationID` (:557). Take the scope per call rather than per `Observer`, because the proxy's scope comes from each request's body. The `Observer` needs no clock: both instants arrive as arguments, and `library` passes its own reading for both so its emitted bytes do not move.

- [ ] **Step 1: Tighten the guard in `internal/interceptor/library/mem0_test.go`**

This task is a refactor, so its guard is a **characterization test**: it pins today's behaviour and must stay green *through* the change. It is not a red-then-green test, and Step 2 will pass immediately — that is the point.

In `TestAddWritesOneObservedRecord` (:267), replace the `assert.NotEmpty(t, rec.IdempotencyKey, ...)` assertion with the exact derivation, and add the content hash:

```go
wantHash := record.ContentHash("hello", "world")
assert.Equal(t, wantHash, rec.Subject.ContentHash)
wantKey, err := record.DeriveIdemKey(record.EventAddRequested, record.ReasonAddAcknowledged, scope, "", "corr-add-1", wantHash)
require.NoError(t, err)
assert.Equal(t, wantKey, rec.IdempotencyKey)
```

Do the same in `TestSearchWritesPerformedAndSurfacedRecords` (:342) for both the `search_performed` record and each `memory_surfaced` record, using that test's own correlation ID, rank and query text. The point of asserting the *derivation* rather than a literal digest is that this is what the refactor could silently move — `idemKey`'s identifier argument in particular, which carries the ruling at `mem0.go:424-441`.

- [ ] **Step 2: Run it to verify it passes on the unchanged code**

Run: `go test ./internal/interceptor/library/ -count=1`
Expected: PASS. A failure here is a wrong expectation in Step 1, not a bug — fix the expectation before continuing.

- [ ] **Step 3: Create `internal/interceptor/observer.go`**

The interfaces above, with the moved bodies. Two things the signatures do not determine:

- A nil `sink`, and a nil rule set, must both be safe (no panic, nothing written), matching `library`'s current tolerance. `RuleSet.Match` is already nil-safe.
- `Add` and `Search` must be safe for concurrent use: they write no shared state, and the only field they read is the immutable rule set.

- [ ] **Step 4: Add `DeriveCorrelationID` to `internal/interceptor` and make `library`'s delegator**

Move the implementation (its `correlationDomain` const and its body) to `observer.go`. Then in `library/mem0.go` keep the exported name with the same signature as a one-line delegation, so the surface the spec pins is intact. A package cannot import its own parent's sibling the other way round, which is why the definition moves rather than being shared.

- [ ] **Step 5: Refactor `internal/interceptor/library/mem0.go` to delegate**

`Mem0Interceptor` holds an `*interceptor.Observer` built in `New` from `m.aw` and `WithRules(m.rules)`, and `Add`/`Search` call `o.Add`/`o.Search` with the same inputs the extracted builders used. `Add` and `Search` keep their Mem0 call, their `ErrMissingCorrelationID` check, their nil-client check, and their `resolveCallOptions` handling.

**Preserve the nil-`aw` guard.** Today `write` (:464) returns early when `m.aw == nil`, and that guard must survive *at the call site*: a nil `*interceptor.AuditWriter` stored in the `Sink` interface is not a nil interface, so handing it to the Observer would call `Write` on a nil pointer and panic. Keep the check before the Observer call, and add a test for it if `TestFailModeAndClose` (:637) does not already cover writing through a nil writer.

- [ ] **Step 6: Write the `Observer`'s own tests**

In `internal/interceptor/observer_test.go`, against a recording fake `Sink`:

- `TestObserverAddSinksOnePhrasedRecord` — one `add_requested` record with the expected ID, event, reason kind, scope and evidence source.
- `TestObserverSearchSinksPerformedThenSurfacedInRankOrder` — the `search_performed` record first, then one per result in rank order, IDs suffixed `#1`, `#2`.
- `TestObserverMarksContentSensitiveFromACallFlagAndFromARule` — both mark it, and the two are an OR.
- `TestObserverWithANilSinkWritesNothing` — no panic.

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go test ./internal/interceptor/... -count=1`
Expected: PASS, including every pre-existing `library` test unchanged.

- [ ] **Step 8: Commit**

```bash
git add internal/interceptor/observer.go internal/interceptor/observer_test.go internal/interceptor/library/
git commit -m "refactor(interceptor): extract the record observer the proxy will share"
```

### Task 2: The write pipeline

**Files:**
- Create: `internal/interceptor/proxy/pipeline.go`
- Test: `internal/interceptor/proxy/pipeline_test.go`

**Interfaces:**
- Consumes: `interceptor.Sink`, `gap.Log`, `gap.Entry`, `record.Record`.
- Produces:
  - `type RecordSink interface { interceptor.Sink; Close() error }` — `*interceptor.AuditWriter` satisfies it.
  - `func NewPipeline(sink RecordSink, gaps *gap.Log, channels []io.Writer, depth int) *Pipeline`
  - `func (p *Pipeline) Write(rec record.Record) error` — satisfies `interceptor.Sink`; never blocks.
  - `func (p *Pipeline) Close() error`, `func (p *Pipeline) Drops() uint64`

**Notes the implementer needs:** `Pipeline` borrows `gaps`; it does not close it. `Close` drains the tally (which needs the gap log) **before** calling `sink.Close()`, because `AuditWriter.Close` is what closes the gap log. A `depth` below 1 is clamped to 1 rather than accepted, since a zero-capacity channel would make every write a drop. The drop tally holds up to `depth` identities plus a count of the rest (spec §6).

**This task owns the package's shared test fixtures.** Task 3's tests live in the same package and need the same things — a stalling `RecordSink` fake, and a temp-dir ledger plus gap log harness — so Task 3 reuses what this task defines rather than declaring its own, which in one package would be a duplicate symbol. Define them here with the names Task 3's Interfaces block cites.

- [ ] **Step 1: Write the failing tests**

Against a fake `RecordSink` the test can stall, and a real `gap.Log` over `t.TempDir()`:

```go
// The contract: the caller's goroutine never waits on the sink.
func TestPipelineWriteDoesNotBlockWhenTheSinkStalls(t *testing.T) {
	// depth 1, and a fake sink whose Write blocks on a channel the test owns.
	started, release := make(chan struct{}), make(chan struct{})
	// (fake sink: close(started) on the first Write, then <-release)
	// First Write fills the queue-or-the-writer; second must return promptly.
	done := make(chan struct{})
	go func() { _ = p.Write(rec1); _ = p.Write(rec2); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Write blocked on a stalled sink; the caller's goroutine must never wait")
	}
	assert.Equal(t, uint64(1), p.Drops(), "the record that could not be queued is a drop")
	close(release)
	<-started
}
func TestPipelineDrainsAGapEntryForEachDrop(t *testing.T) {
	// after the drop above, gap.Read(path) holds exactly one entry with
	// Kind == rec2.Event, Scope == rec2.Subject.Scope, CorrelationID == rec2.ID.
}
func TestPipelineCloseDrainsQueuedRecords(t *testing.T)
func TestPipelineCloseDrainsTheTallyBeforeClosingTheSink(t *testing.T)
// the fake sink records whether the gap held the entry at the moment Close ran.
func TestPipelineBoundedTallyCountsTheDropsItCannotName(t *testing.T)
// depth 1, more drops than the tally holds: held identities get entries, plus
// one final entry whose Detail carries the remaining count.
func TestPipelineClampsADepthBelowOne(t *testing.T)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/interceptor/proxy/ -count=1`
Expected: FAIL — no such package.

- [ ] **Step 3: Implement `internal/interceptor/proxy/pipeline.go`**

A buffered channel of `record.Record` of capacity `depth`, one writer goroutine started by `NewPipeline` that ranges over it calling `sink.Write`, and a `sync.Once`-guarded `Close` that stops accepting, closes the channel, waits for the goroutine (a `sync.WaitGroup` or a done channel), then drains the tally into `gaps.Record`, then calls `sink.Close`. `Write` uses a non-blocking `select` with a `default` branch that records the drop. The drop marker line goes to every element of `channels`, best-effort, never a nil-channel panic.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/interceptor/proxy/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/interceptor/proxy/
git commit -m "feat(proxy): a write pipeline whose caller never waits on the ledger"
```

### Task 3: The forwarding handler and the observation hooks

**Files:**
- Create: `internal/interceptor/proxy/proxy.go`
- Test: `internal/interceptor/proxy/proxy_test.go`, `internal/interceptor/proxy/parity_test.go`

**Interfaces:**
- Consumes: `interceptor.Observer`, `interceptor.AddObservation`, `interceptor.SearchObservation`, `interceptor.DeriveCorrelationID`, `RecordSink`, `mem0.AddRequest`/`AddResponse`/`SearchRequest`/`SearchResponse`.
- Produces: `func New(upstream *url.URL, obs *interceptor.Observer, sink RecordSink, out io.Writer, maxBody int64) *ProxyInterceptor`, plus `(*ProxyInterceptor).ServeHTTP`, `.FailMode() interceptor.FailMode`, `.Close() error`.

**Notes the implementer needs:** `ServeHTTP` is a `httputil.ReverseProxy` whose `ModifyResponse` records: read the response body up to `maxBody`, hand the *same bytes back* to the caller by replacing `resp.Body` with a fresh reader, and only then observe. `out` receives drop and error markers only; the response body is never written there. The correlation ID comes from the `X-Notary-Correlation-Id` request header, else `interceptor.DeriveCorrelationID(scope, hex(record.ContentHash(method, path, body)))` (spec §3 decision 2). Decode with the `mem0` types, which carry the right JSON tags — `AddRequest` has its entity ids at the top level, `SearchRequest` carries them in `Filters` (spec §5 step 3).

**The two instants, which the spec pins and the signature does not show.** Set `At` to the instant the request arrived, captured before the forward, and leave `RecordedAt` as the zero `time.Time`: `ledger.Append` stamps a zero `RecordedAt` with the writer's clock, which is what makes "when it happened" and "when it was written" two different true facts and exposes queueing delay to an operator (spec §5, §13.3). Do not set `RecordedAt` to the arrival instant — that is library mode's conflation, and the parity test would then be comparing a field it deliberately excludes.

- [ ] **Step 1: Write the failing tests**

`proxy_test.go`, against a `httptest` Mem0 upstream and a recording `RecordSink`:

- `TestProxyForwardsStatusHeadersAndBodyUnchanged` — Review Focus 5's vehicle too: a non-observed path (`DELETE`, `GET /v1/memories/{id}/history/`) with a deliberately malformed JSON body reaches the upstream byte-for-byte and records nothing.
- `TestProxyRecordsAnAddFromTheRequestBodyAndResponse` — one `add_requested` record whose scope came from the body's entity ids and whose evidence is the response.
- `TestProxyRecordsASearchPerformedAndEachSurfacedMemoryInRankOrder`.
- `TestProxyUsesTheCallerCorrelationHeader` — the record's ID is the header's value.
- `TestProxyDerivesACorrelationIDWhenTheHeaderIsAbsent`, and `TestProxyCollapsesTwoIdenticalHeaderlessRequests` — Review Focus 4: the second add produces no second record.
- `TestProxyForwardsAnOverCapBodyWithoutRecordingIt` — Review Focus 2: the upstream sees the whole body, the sink sees nothing, and the gap log stays empty.
- `TestProxyReturnsAnErrorWhenTheUpstreamIsUnreachable` — Review Focus 1: a non-2xx reaches the caller, nothing is recorded, and neither the error nor `out` contains the API key sent in the `Authorization` header or a query-string sentinel.
- `TestProxyRecordsNothingWhenMem0ReturnsAnError` — parity with `library.Add`: a 500 from the upstream writes no record.

`parity_test.go` — the phase's falsifier (spec §9). Both modes drive the **same** `httptest` Mem0 server:

```go
// One add and one search, driven once through library.Mem0Interceptor (with
// mem0.NewClient pointed at the test server) and once through the proxy (with
// the same server as upstream), each writing to its own real ledger over a temp
// SQLite file. Then compare the two record sets on every field the claim
// asserts: ID, Event, Reason kind and tier, the evidence payload bytes, Subject
// (Scope, MemoryID, ContentHash), Content, and IdempotencyKey.
//
// At, RecordedAt, Hash and Signature are EXCLUDED, and the test says why: the
// canonical hash covers the instants (record/chain.go:59,97), so byte-identity
// is impossible by construction and asserting it would be untestable.
func TestProxyAndLibraryProduceTheSameRecords(t *testing.T)
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/interceptor/proxy/ -count=1`
Expected: FAIL — `New`, `ProxyInterceptor` undefined.

- [ ] **Step 3: Implement `internal/interceptor/proxy/proxy.go`**

Build the handler from `httputil.NewSingleHostReverseProxy(upstream)` with `ModifyResponse` set to the observing callback, wrap it, and expose `FailMode`/`Close` so the type satisfies `interceptor.Interceptor`. Match the observed paths on method *and* path. Every failure inside the observer — a decode error, an over-cap body — must degrade to "no record", never to a failed request.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/interceptor/proxy/ -count=1`
Expected: PASS, including the parity test.

- [ ] **Step 5: Commit**

```bash
git add internal/interceptor/proxy/
git commit -m "feat(proxy): forward Mem0 traffic and observe what library mode would"
```

### Task 4: `notary proxy`

**Files:**
- Create: `cmd/notary/proxy.go`
- Test: `cmd/notary/proxy_test.go`
- Modify: `cmd/notary/root.go` (one line: `cmd.AddCommand(newProxyCmd())`)
- Modify: `config/config.go` (the sensitivity-rules comment at :36-42)

**Interfaces:**
- Consumes: `proxy.New`, `proxy.NewPipeline`, `interceptor.NewObserver`, `interceptor.NewAuditWriter`, `config.Config`, `store.Open`, `ledger.New`, `gap.Open`, `sign.NewSigner`.

**Flags:** `--addr` (default `127.0.0.1:8080`), `--queue-depth` (default `1024`), `--max-body` (default `8MiB`, parsed by a stdlib `pflag.Value` — pflag has `BytesHex`/`BytesBase64` but no human-readable byte-size type, so the flag type is ours). Follow `cmd/notary/reconcile.go`'s shape for opening the store, ledger, signer and gap log.

**Notes the implementer needs:** this command needs a signing key and **must not** require a Mem0 API key (spec §8) — it forwards the caller's credentials. Its stdout is unused; the listen banner and every marker go to stderr. It must install a signal-aware context (`SIGINT`, `SIGTERM`) and call `Close` on the pipeline before returning, because the queue is only ever drained by `Close`, and without this every graceful stop would lose it.

- [ ] **Step 1: Write the failing tests**

Following `newTestReconcileCmd`:

- `TestProxyCmdRefusesToStartWithoutASigningKey` — the error names `NOTARY_SIGNING_KEY`.
- `TestProxyCmdStartsWithoutAMem0APIKey` — with `cfg.Mem0APIKey` empty and a signing key set, start-up succeeds and the listener answers; the point is that the key is not required.
- `TestProxyCmdRegistersTheThreeFlagsAndNoOthers` — `--addr`, `--queue-depth`, `--max-body` exist; `--phrase`, `--checkpoint-out`, `--from`, `--to` and the four scope flags do not.
- `TestProxyCmdShutdownDrainsThePipeline` — Review Focus 3 via the command: with a record accepted before the shutdown signal, the ledger holds it after `runProxy` returns.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cmd/notary/ -run TestProxy -count=1`
Expected: FAIL — `newProxyCmd` undefined.

- [ ] **Step 3: Implement `cmd/notary/proxy.go` and register it**

`runProxy(cmd, cfg)` opens the signer, store, ledger, gap log, observer and pipeline in `reconcile.go`'s order, builds the upstream URL from `cfg.Mem0BaseURL`, serves with `http.Server`, and on the signal-aware context calls `pipeline.Close()` before returning. Print the listen banner to stderr, not stdout. Refuse to start when `cfg.SigningKeyEnv` is empty, naming `NOTARY_SIGNING_KEY`, and do **not** refuse when `cfg.Mem0APIKey` is empty.

- [ ] **Step 4: Correct the `config/config.go` comment**

`:36-42` says rules have no flag because *"no notary command writes records"*. `notary proxy` writes them, so the premise is now false. Rewrite the sentence to say what is true: the rules path comes from `NOTARY_SENSITIVITY_RULES` for the write paths that exist, and there is deliberately still no flag, because a second way to say the same thing is how configuration drifts. Do not add a flag. Also add `NOTARY_MEM0_BASE_URL`-aware wording only if you find the existing comment about the proxy's upstream is actually wrong — check it, do not assume.

- [ ] **Step 5: Run the tests, then the whole suite, serially**

Run: `go test ./cmd/notary/ -count=1`, then `go test -p 1 ./... -count=1` (allow several minutes; `internal/ledger` alone is ~60s, and a cold build may exceed a two-minute shell cap, in which case re-run it).
Expected: PASS everywhere — **including the vocabulary and phrasing gates** (`internal/record`'s completeness test, `internal/export`'s `TestPhraseIsTotalOverTheVocabulary` and `TestPhraseKeepsTheSpecifiedSentences`), which must pass with no edit to `internal/record` or `internal/export`. That green is the assertion for spec §7's "this phase adds nothing to the vocabulary"; if you had to change either package, the coverage decision has been violated and the task is not done.

- [ ] **Step 6: Commit**

```bash
git add cmd/notary/ config/config.go
git commit -m "feat(cmd): notary proxy"
```

### Task 5: The documented surface

**Files:**
- Modify: `docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md` (phase table row 9, and §2's Scope line if it now reads as a claim about what exists)
- Modify: `README.md` (Status: the phase count, the scope badge, and moving proxy mode out of "Not built yet")
- Modify: `.clinerules` (the folder tree's stale `# ProxyInterceptor (Phase 7)`)

**Interfaces:** none — documentation only.

- [ ] **Step 1: Mark the phase table's row 9 complete**, matching row 8's form rather than assuming it. Verify what row 8 says first.

- [ ] **Step 2: Check §2's Scope line** — *"v1 (library mode). Proxy mode is Phase 9 of the same build but is not the subject of this document."* Decide whether shipping this phase makes it false or merely dated, and correct it only if it now misleads.

- [ ] **Step 3: Update the README's Status section** — the phase count (1–8 → 1–9), the `scope-...` badge, and a `notary proxy` bullet beside the others stating what it does, that it needs a signing key, that it needs no Mem0 API key, and that unrecorded endpoints pass through. Derive every claim from `cmd/notary/proxy.go` and the spec, not from memory.

- [ ] **Step 4: Correct `.clinerules`' folder tree** — `# ProxyInterceptor (Phase 7)` → `(Phase 9)`, and check whether the tree needs `pipeline.go` beside `proxy.go` to match what shipped.

- [ ] **Step 5: Correct anything this phase made false**, checking each sentence against the code before changing it. The `.clinerules` guardrail about proxy mode should now be verifiably true; say so rather than restating it.

- [ ] **Step 6: Commit**

```bash
git add docs/ README.md .clinerules
git commit -m "docs: record Phase 9"
```

---

## Risk, and where it is concentrated

- **Task 1's characterization test is the refactor's only guard.** The refactor's whole obligation is that
  nothing moves, and the existing `library` tests stop short of the idempotency key and content hash — the two
  fields a wiring change can move silently. Tightening them *before* the refactor (Step 1) is what makes the
  later steps safe to review.
- **Task 3's parity test is the phase's falsifier.** If the proxy's records diverge from library mode's, the
  phase's central claim is false, and the divergence will be in exactly the fields Step 1 pinned.
- **The typed-nil `Sink`** (Task 1 Step 5) is the one trap that turns a tolerance into a panic in production.
- **Least certain: whether the pipeline's drop tally bound is right.** It holds `--queue-depth` identities; a
  sustained overload past that writes one summary entry instead of names. The spec records this as a
  deliberate bound, but the number is the first thing to revisit if the gap log ever proves noisy.

## Sequencing, and why the order is forced

1 → 3 (the hooks call the observer), 2 → 4 (the command constructs the pipeline). Tasks 1 and 2 are
**independent of each other** behind the `Sink` seam, so they may be executed in either order or in parallel;
3 needs 1 and consumes 2's `RecordSink`; 4 needs 2 and 3; 5 records what exists.

## Spec coverage

| Spec section | Task |
| --- | --- |
| §3 decision 1 — coverage is add and search, no vocabulary change | 3 (and asserted by 2's gates) |
| §3 decision 2 — the header, with a derived fallback | 3 |
| §3 decision 3 — decoupled, bounded queue | 2 |
| §3 decision 4 — a package plus a command | 3, 4 |
| §3 decision 5 — drops become durable gap entries | 2 |
| §4 — the `Observer` extraction and the `Sink` seam | 1 |
| §5 — one request, in order; timestamps | 3 |
| §6 — the pipeline, the tally bound, `Close` | 2 |
| §7 — what is recorded, what passes through, why | 3 |
| §8 — flags, config, credentials, the rules comment | 4 |
| §9 — parity, drops, never-blocks, untouched pass-through | 2, 3 |
| §10 — boundaries | 4 (`--max-body` behaviour), 3 |
| §12 — limitations | 2 (over-cap vs drop), 3 (collapse) |

## Type consistency

- `record.Record`, `record.Scope`, `record.ContentHash`, `record.DeriveIdemKey` — existing, unchanged.
- `mem0.AddRequest`, `mem0.AddResponse`, `mem0.SearchRequest`, `mem0.SearchResponse`, `mem0.Filters` — existing, unchanged, and reused for BOTH the client call in `library` and the HTTP decoding in `proxy`.
- `interceptor.Sink` → `proxy.RecordSink` (which embeds it) → satisfied in production by `*interceptor.AuditWriter` and in tests by a fake.
- `interceptor.Observer.Add(AddObservation)` / `.Search(SearchObservation)` — one name each, called by Task 1's `library` and Task 3's `proxy` with the same two struct types.
- `interceptor.DeriveCorrelationID` — the definition; `library.DeriveCorrelationID` delegates to it with an identical signature.
