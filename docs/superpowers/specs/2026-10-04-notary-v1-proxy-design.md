# Notary v1 Phase 9 — proxy mode: Design

**Status:** draft for review. The five decisions in §3 were taken with the operator during brainstorming; this
document records them. Per the brainstorming gate, no implementation starts until this spec is reviewed and a
plan is written.

**Date:** 2026-10-04
**Depends on:** Phases 0–8, complete, pushed and CI-green (`master` at `c5adf33`).
**Phase table:** architecture spec §13, row 9.

---

## 1. What proxy mode is for

Library mode requires the application to be in Go and to call Notary's own API: it constructs a
`Mem0Interceptor`, and every `Add` and `Search` goes through it. That is the right trade for an application
that can be changed, and the wrong one for an application that cannot — a Python or TypeScript service, a
vendor's agent runtime, anything whose Mem0 calls are buried in a dependency.

Proxy mode serves those. The application re-points its Mem0 base URL at Notary; Notary forwards every request
to the real Mem0 and returns the response untouched, recording what it saw on the way through. No code change
in the application, in any language, and no Notary dependency in its build.

The audience is unchanged from §1 of the parent spec — a compliance lead who must explain an agent's memory
decisions without reading code. What changes is that the records no longer depend on the application
cooperating with a library.

**The property that makes this worth building.** A deployment can move from library mode to proxy mode, or run
both against one ledger, and the ledger's *meaning* does not change. That is not an aspiration to be hoped for
at review time: §7 and §9 make it a field-level check with a test. It is also why §4 does the refactor it does.

**The line the proxy draws.** The proxy records **requests**; the reconciler records **outcomes**. An add being
requested and acknowledged, and a search being performed with what it surfaced, are facts that exist nowhere if
the application does not embed Notary — only a proxy can see them. A memory being removed, or an asynchronous
add reaching a terminal state, are facts that follow from the ledger's own contents, and the reconciler already
derives them: it enumerates a scope to classify memories as kept or gone, reads a memory's history to attribute
a removal, and polls an event to resolve an add. Drawing the line here is what keeps proxy mode from becoming a
second, divergent audit path — and it is why §7 records exactly two endpoints and no others.

## 2. What is already fixed, and what is deliberately not

The architecture spec excludes this phase from its own scope, in as many words:

> **Scope:** v1 (library mode). Proxy mode is Phase 9 of the same build but is not the subject of this document.

So this spec is the subject, and it owes what the parent spec did not. What the parent does fix:

- **§4's package tree** reserves `internal/interceptor/proxy/proxy.go` for this phase. §14's delta table lists
  it among the packages the original folder layout did not account for.
- **§4**, on why a deployment mode is not a reconciler: *"A deployment mode wraps live traffic; a reconciler
  reads history. Collapsing them would force proxy mode and reconcile mode to share a lifetime they do not
  have."* This phase therefore adds no reconciliation and reads no history — and §1's line is a sharper
  statement of the same boundary.
- **`.clinerules` §5**: *"In proxy mode, a failure in the audit-write path must never block, delay, or fail the
  underlying Mem0 request/response."* §3's third decision is a ruling on what that sentence means; §6 is the
  architecture that satisfies it.
- **§5's vocabulary is closed and gate-tested.** This phase **adds no member to it**, which is a deliberate
  outcome of the first planning decision and the reason the phase reaches into no foundational package except
  one it is explicitly authorised to touch (§4).

**`.clinerules` calls the type `ProxyInterceptor`, in a folder tree whose phase numbering predates the
renumbering** (§13 moved proxy from 7 to 9). The name is kept: `internal/interceptor/proxy` exports a type that
satisfies `interceptor.Interceptor` (it reports a fail mode and it closes), which is what the name promises.
This spec adds one correction to `.clinerules`: its folder tree still says *"(Phase 7)"*.

## 3. The decisions taken in brainstorming

Each was put to the operator with its cost; each cost is recorded here rather than in a footnote.

First, the decision that shaped the rest, because it was arrived at the hard way: an earlier draft recorded four
endpoints and added a reason kind to `internal/record` to do it. Checking the second of those four against the
machinery is what killed the plan — a proxy's `add_resolved` would have carried the same record ID as the
reconciler's with a different idempotency key, and the `id` column's UNIQUE constraint would have turned a
legitimate observation into a fail-open-loud gap entry under a name that was not even the same claim. The
endpoints that need reconstructing another writer's facts are the endpoints this phase does not record (§1's
line, §7).

1. **Coverage is exactly what library mode records: `add` and `search`.** No delete, no event-status, no
   `get_all`, no `history`, and therefore no new event, no new reason kind, and no new phrasing sentence.
   **Cost:** a memory deleted through the proxy is not recorded *by the proxy*; the reconciler records it
   eventually, as `memory_dropped` + `removed_by_mem0`, which states that Mem0 no longer holds it but does not
   name the caller who asked. An asynchronous add resolved during a caller's own poll is likewise the
   reconciler's to resolve. This narrows what the proxy can answer, and it is the price of the proxy's records
   being library mode's records.

2. **A record's identity comes from a request header, with a derived fallback.**
   `X-Notary-Correlation-Id` when the caller sets it — the same correlation ID library mode's caller passes
   explicitly, so a caller who wants identical semantics across both modes gets them, including exact retry
   deduplication. When it is absent the proxy derives one:
   `DeriveCorrelationID(scope, hex(record.ContentHash(method, path, body)))`
   (`internal/interceptor/library/mem0.go:557`, `internal/record/content.go:44` — the digest is length-prefixed
   per part, so the three fields cannot be re-split ambiguously).
   **Cost:** a caller that sets no header gets a key that is stable across an exact retry and *also* collapses
   two genuinely distinct identical requests into one record. That is the silent-loss class this project already
   fought once (the `Task 18` ruling recorded at `internal/interceptor/library/mem0.go:424-441`), and the
   fallback reintroduces it — deliberately, for callers who chose not to identify themselves.

3. **The response path never touches the ledger; the write path is decoupled behind a bounded queue.**
   This is the broad reading of the §5 guardrail: a *slow* audit write — lock contention, a disk stall — must
   not become a Mem0 outage either, not merely a *failing* one.
   **Cost:** the response can be returned before the record exists, so a process crash loses whatever was
   queued. A synchronous design would lose only what it had not yet written. §6 bounds this with a drain on
   `Close` and the fact that a lost queue is a lost *record*, never a lost gap — the ledger stays honest about
   what it does and does not contain. The reading is argued in §6, because a skeptic would call this machinery
   YAGNI and the argument deserves to be in the document rather than in a conversation.

4. **The phase ships a package plus a `notary proxy` command.**
   The house pattern: phases 5–8 each shipped a package and the command that drives it, and the handler is
   testable with `httptest` while the command is what deploys.
   **Cost:** the command must take a new `--addr` surface and must join the config story, including the
   sensitivity-rules problem §8 records.

5. **A dropped record is durable and owed, not merely announced.**
   The background writer drains drops into gap entries, which is what the gap log exists for
   (`internal/gap/gaplog.go:206` — a plain append, no signature, so it is cheap enough to do off the response
   path). A drop therefore appears in the ledger's own accounting and is reconcilable, exactly as a
   ledger-write failure is today.
   **Cost:** a gap entry per dropped record means a sustained overload writes many entries. §6 bounds the tally
   and states what happens to drops beyond the bound instead of pretending the bound does not exist.

## 4. The shape, and the refactor that is an explicit ask

**The problem.** For add and search the proxy must build the records library mode already builds: the same
events, the same reason kinds, the same frozen evidence payloads, the same `<id>#<rank>` sibling IDs, the same
idempotency-key derivation. Today that lives inside `internal/interceptor/library` as unexported methods on
`Mem0Interceptor` — `observeAdd` (`:298`), `observeSearch` (`:341`), `writeSearchPerformed` (`:356`),
`writeSurfaced` (`:398`) — driven by a construction-time scope, a rule set, a clock and an `AuditWriter`.

**What is already shared, and what is not.** The byte-sensitive part is shared already:
`mem0.AddPayload`, `mem0.SearchPerformedPayload` and `mem0.MemorySurfacedPayload` live in
`internal/mem0/evidence.go:34,41,53`, because the reconciler reads the same payloads back and must not import
its sibling interceptor. What is *not* shared is the wiring around them — the event, the reason kind, the
subject, the content, and above all the idempotency key, whose derivation is subtle enough that this project has
already shipped one silent-loss bug in it (the `Task 18` ruling at `internal/interceptor/library/mem0.go:424-441`
exists because keying on the wrong identifier collapsed two distinct records into one).

**The shape.** Extract that wiring into a new `Observer` in `internal/interceptor`, holding the scope, the rule
set, the clock and a write sink; `library.Mem0Interceptor` keeps its public API and its emitted records and
delegates to it. The proxy's job is then only to turn Mem0's HTTP request and response into the observer's
inputs.

The seam that makes decoupling work is the sink. `Observer` writes through a one-method interface:

```go
// Sink is everything the Observer needs to dispose of a record. The library
// hands it the AuditWriter (a synchronous append, absorbed by fail-open-loud);
// the proxy hands it its queue (a non-blocking enqueue). Neither knows which.
type Sink interface {
	Write(rec record.Record) error
}
```

`*interceptor.AuditWriter` already satisfies it (`internal/interceptor/interceptor.go:127`), so library mode
passes the writer it already has and its behaviour is unchanged.

**This is the explicit ask.** `.clinerules` §5 says *"Do NOT refactor working code from a prior phase unless
explicitly asked."* Phase 3's `library` package is working, reviewed code, and the operator authorised this
refactor in brainstorming. The authorisation is the reason this section exists as its own section.

**What the refactor must not change.** `library`'s exported surface — `New`, `Add`, `Search`, `FailMode`,
`Close`, `Sensitive`, `WithSensitivityRules`, `DeriveCorrelationID`, `ErrMissingCorrelationID` — and every
record it emits must be identical. The ledger's real-record fixtures guard those bytes end to end today; §9
adds a guard that fails on any drift rather than relying on a fixture happening to notice.

**Packages.** Three files are new, one is refactored, one gains a line, and no package that shipped before this
phase is edited except the one this section is authorised to change.

| Path | Change |
|---|---|
| `internal/interceptor/observer.go` | New. The extracted `Observer` and its `Sink`. |
| `internal/interceptor/library/mem0.go` | Refactored to delegate. Public API and emitted records unchanged. |
| `internal/interceptor/proxy/proxy.go` | New. The forwarding handler, observation hooks and the pipeline. |
| `cmd/notary/proxy.go` | New. The command. |
| `cmd/notary/root.go` | One line: registration. |

## 5. One request, in order

1. **Read the request.** Buffer the body up to `--max-body` (default 8 MiB, the same bound
   `internal/mem0/client.go:32` puts on a response). If the body is larger than that, stop buffering — the
   request is still forwarded exactly as it arrived, and steps 5 and 6 are skipped, because a body that cannot
   be read whole cannot be attested to.
2. **Decide whether this is observed traffic**, by method *and* path, so that paths which share a prefix cannot
   be confused: `POST /v3/memories/add/` and `POST /v3/memories/search/`
   (`internal/mem0/client.go:280,291`). Everything else — `get_all`, `history`, `event` status, `delete` — is
   forwarded and not recorded, which §7 explains as a boundary rather than a gap.
3. **Capture the arrival instant**, the caller's `X-Notary-Correlation-Id`, and the scope from the body. Both
   endpoints this phase observes carry scope in their body, so no record needs an empty one — though not at the
   same depth: an add carries `user_id`/`agent_id`/`app_id`/`run_id` at the top level of its request
   (`internal/mem0/types.go:99-110`), while a search carries them inside `filters` and nowhere else
   (`:127-133`, and `:140-141` states the rule beside `GetAllRequest`).
4. **Forward.** The response is streamed back to the caller exactly as Mem0 sent it, and the response status,
   headers and body are untouched — `httputil.ReverseProxy` handles the hop-by-hop rules that a hand-rolled
   forwarder gets wrong.
5. **Tee the response body** under the same cap, for the observation only. A response larger than the cap is
   forwarded to the caller in full but not observed, for the same reason as step 1 and with the same
   consequence: no record.
6. **After the response is complete**, hand the request/response pair to the `Observer`, which builds 0..n
   records and hands each to the sink. Nothing in steps 1–6 touches the ledger.

Step 6 being *after* the response is what makes decision 3 possible at all, and it is not a compromise: an
add's record is `add_requested` + `add_acknowledged`, whose whole content is Mem0's acknowledgement, so the
record cannot be built before the response exists. The same holds for a search's `memory_surfaced` records,
which carry Mem0's results.

**A Mem0 failure records nothing**, and this is parity rather than a hole: `library.Add` and `library.Search`
return Mem0's error and write no record — *"a Mem0 failure is a real error, not an audit gap"*
(`internal/interceptor/library/mem0.go:224-227`). The proxy behaves identically.

**Timestamps.** `At` is the arrival instant captured in step 3 — the moment the caller's operation happened —
and `RecordedAt` is stamped by the writer goroutine when it appends. Library mode conflates the two (it calls
the clock once, after the response); the proxy does not, so its records say two true things where library
mode's say one.

## 6. The write pipeline

**Why this machinery exists**, since a reader comparing it to library mode's synchronous append will ask. In
library mode the application *chose* Notary and accepted its cost. A proxy sits in front of everything,
including traffic from callers who never opted in and cannot be told about it, so "an audit problem must never
become a caller's problem" has more force here than it does inside a library. The failure that matters is not
only a *failing* ledger write, which `AuditWriter` already absorbs, but a *slow* one: with synchronous appends
from every request goroutine, a locked or stalled store turns audit throughput into a latency ceiling on a
customer's memory layer. The cost is paid on the other side — a crash loses the queue — and it is paid in audit
completeness rather than in availability, which is the direction fail-open-loud already points.

- **A bounded channel** (default 1024 records, `--queue-depth`) feeds **one writer goroutine**, which owns the
  `AuditWriter` and performs every ledger append. One writer means `Seq` follows enqueue order, and it means the
  ledger's own lock is never contended by request goroutines.
- **Enqueue is non-blocking.** On a full queue the producer does two cheap things and returns: it increments an
  in-memory drop tally, and it writes a one-line marker to the process's loud channels. It never waits. The
  response has already been sent by then, so this costs a caller nothing — its only purpose is that the drop is
  not silent.
- **Drops become gap entries.** The writer, between appends, drains the tally into `gap.Log.Record` calls — one
  entry per dropped record, carrying the dropped record's `Event`, `Scope` and `ID` as `Kind`, `Scope` and
  `CorrelationID`, which is the shape `ledger.GapBreaks` accounts for and therefore what makes the gap
  reconcilable rather than merely recorded (`internal/interceptor/interceptor.go:186-206` is the same reasoning
  for the ledger-failure path, and this reuses it).
- **The tally is bounded, and the bound is stated rather than hidden.** The tally holds the identities of up to
  `--queue-depth` drops and a running count of the rest. The writer emits one gap entry per held identity and,
  when the tally overflowed, one final entry recording how many further drops went unnamed. Under sustained
  overload the ledger therefore shows a bounded number of named gaps plus an honest count, and never an
  unbounded queue of pending work.
- **`Close` stops accepting, drains the queue, drains the tally, then closes the gap log.** Its error is the
  doubly-failed case only, exactly as `AuditWriter.Close` is.
- **The doubly-failed case is loud, not swallowed.** If the gap log itself cannot be written, the markers were
  already emitted (the `AuditWriter` writes them before returning its error) and the writer reports it on
  stderr. v1 has no enforcement function, so the proxy keeps serving: an audit outage must not take down
  traffic, which is the whole point of fail-open-loud.

## 7. Record coverage, and the boundary

**Recorded — exactly what library mode records, with the evidence Mem0's own response supplies:**

| Endpoint | Records | Evidence source |
|---|---|---|
| `POST /v3/memories/add/` | `add_requested` + `add_acknowledged` | Mem0's response body (`SourceMem0Response`) |
| `POST /v3/memories/search/` | `search_performed`, then one `memory_surfaced` per result | Mem0's response body (`SourceMem0Response`) |

**This phase adds nothing to `internal/record`.** No new event, no new reason kind, no new tier, and therefore
no new sentence in `export.Phrase` — whose totality gate (`internal/export/phrase_test.go:72`) and
specified-sentence test (`:98`) both pass untouched, as does the vocabulary completeness test
(`internal/record/reason.go:72,79`). The architecture spec's §5 vocabulary table is not amended. That is the
point of the coverage decision, not a happy side effect of it.

**Forwarded and not recorded, and why each is not a gap:**

- **`POST /v3/memories/` (`get_all`) and `GET /v1/memories/{id}/history/`.** Reads of everything in a scope and
  of one memory's change log. Neither is a request whose fact is lost by not recording it here, because the
  reconciler derives what follows from them: `memory_kept` and `memory_dropped` are precisely what it
  concludes by enumerating and reading history.
- **`GET /v1/event/{id}/`.** Polling an asynchronous add's status. This is how the reconciler resolves an add
  (`add_resolved` + `stored_by_mem0` / `add_failed` / `no_facts_extracted`, `internal/reconcile/adds.go:73`),
  and building that record is not the proxy's to do: its `At`, its content hash and its scope come from the add
  record in the ledger (`:126-141`), and its idempotency key hashes that scope (`:178-184`). A proxy would have
  to reconstruct another writer's facts to form it — which an earlier draft of this spec assumed was trivial and
  was wrong about.
- **`DELETE /v1/memories/{id}/`.** A memory removed through the proxy is recorded by the reconciler, as
  `memory_dropped` + `removed_by_mem0`: that reason states Mem0 no longer holds it, which is true and durable,
  but it does not name the caller who asked. Recording the caller's request would need a new reason kind — the
  only vocabulary change this phase would have needed — and a `DELETE`'s response carries no body to attest to,
  so the record could only cite Notary's framing of Mem0's status line. Both costs were judged higher than the
  gap being closed, because the fact of the removal is not lost, only its attribution.

## 8. The command and its configuration

```
notary proxy [--addr 127.0.0.1:8080] [--queue-depth 1024] [--max-body 8MiB]
```

Registered in `cmd/notary/root.go` beside the other six. The view of the world is reversed from every other
command: the others read the ledger and print; this one serves traffic and writes. So its stdout is **unused**
— the operator's channel is stderr, where the listen banner and every drop marker go — and it writes to no file
but the ledger and the gap log.

**It needs no Mem0 API key.** It forwards the caller's own `Authorization` header upstream, so it is
credential-neutral and holds no credential of its own. That makes credential hygiene a hard requirement rather
than a nicety: no header value, and no query string, may reach a log, an error, or a record.
`internal/mem0/client.go`'s redaction scheme (`credentialComponents`, `:99-144`) is the model, and §9's canary
is the guard.

**What it does need:** `NOTARY_DB_PATH`, `NOTARY_GAP_LOG_PATH`, `NOTARY_SIGNING_KEY` (ledger appends are
signed, `internal/ledger/ledger.go:100`, so this is required exactly as `reconcile` and `export` require it),
and the upstream base URL from `NOTARY_MEM0_BASE_URL`.

**The sensitivity-rules problem, and its resolution.** `config/config.go:36-42` documents that sensitivity
rules are reachable only through `NOTARY_SENSITIVITY_RULES`, with no `Config` field and no CLI flag, *because*
*"rules mark content at WRITE time and no notary command writes records, so a flag would have no command to
live on."* `notary proxy` is the first command that writes records, so that sentence becomes false. The
resolution keeps the flag absent and makes the comment true: the proxy reads the rules path from
`NOTARY_SENSITIVITY_RULES`, through the same path an application does, and there is still no flag — on the
grounds that the variable already exists, is already the documented way, and a second way to say the same thing
is how configuration drifts. The comment is corrected, not the decision.

## 9. Testing

**The phase's falsifier: parity between the two modes.** For one operation — an add, and a search with results
— driven through `library.Mem0Interceptor` and through the proxy against the same fixture, the two sets of
records must agree on every field the claim asserts: `ID`, `Event`, `Reason` kind, the evidence payload bytes,
`Subject` (scope, memory id, content hash), `Content` and `IdempotencyKey`.

Note precisely what this is not, because an earlier statement of it was wrong: it is **not** byte-identity. The
canonical hash covers `At` and `RecordedAt` (`internal/record/chain.go:59,97`), so the two modes' records have
different hashes and different signatures *by construction* — they were observed at different instants on
different clocks. A byte-identity claim would be untestable; this one is the strongest property that is
actually true, and it is what "switching modes changes nothing in the ledger" means once the timestamps are
excluded. With coverage at add and search only (§7), this test also has no awkward cases: every record the
proxy writes has a library-mode counterpart to compare against.

Around it:

- **The response path never blocks.** A sink that blocks forever must not delay a response: issue a request
  against a proxy whose write path is deliberately stalled and assert the response arrives.
- **A drop leaves a durable gap entry.** Fill the queue, then assert the gap log holds an entry whose
  correlation ID is the dropped record's, in the shape `ledger.GapBreaks` accounts for — and that the caller's
  response was never delayed by the fill.
- **Unrecorded endpoints pass through untouched:** `get_all`, `history`, event status and delete arrive at the
  upstream byte-for-byte and produce no record.
- **Forwarding fidelity:** status, headers and body cross the proxy unchanged, against an `httptest` upstream.
- **The refactor changed nothing.** `library`'s emitted records for a fixed operation are the same before and
  after, asserted rather than assumed from the ledger fixtures happening to still pass.
- **Credential canary:** an `Authorization` header and a query string bearing a sentinel value must not appear
  in any record, marker, or error the proxy produces.
- **`Close` drains:** records enqueued before `Close` are in the ledger after it.
- **Nothing foundational moved:** `ReasonKinds`/`AllowedTier` completeness, `TestPhraseIsTotalOverTheVocabulary`
  and the specified-sentences test all pass with no edit to `internal/record` or `internal/export` — which is a
  test of the coverage decision, and fails loudly if a later change quietly adds vocabulary.
- **Config:** the proxy requires a signing key, does not require a Mem0 API key, and honours
  `NOTARY_SENSITIVITY_RULES`.

## 10. Boundaries (out of scope)

- **No delete, event-status, `get_all` or `history` recording.** §7, and §1's line is the principle behind it.
- **No vocabulary change at all**, and therefore no architecture-spec §5 delta.
- **No in-process middleware surface.** Decision 4 ships a command and the package it drives; a Go application
  that wants the handler can mount it, but nothing is designed for that.
- **No sensitivity-rules flag.** §8.
- **No reconciliation, and no ledger reads of any kind.** §2 and §1: the proxy never enumerates, never diffs,
  never reads history, and never looks anything up. It only appends.
- **No caller-supplied scope header.** Both recorded endpoints carry scope in their body; there is no
  scopeless record to attribute.
- **No TLS termination, no authentication of its own, no rate limiting, no caching.** The proxy forwards; the
  deployment owns the edge.
- **No new dependencies.** `net/http/httputil` is stdlib, and `go.mod` does not change.
- **No index.** Nothing here reads the ledger by a new column.

## 11. Sequencing, and why the order is forced

1. **The `Observer` extraction and the `library` refactor** (§4), with the parity guard from §9. First, because
   everything else calls the observer, and because the refactor is reviewed against a suite that has not also
   changed — if it lands alongside the proxy, a parity failure could belong to either.
2. **The pipeline** (§6): the queue, the writer goroutine, the drop tally, the gap drain, and `Close`.
3. **The forwarding handler and the observation hooks** (§5).
4. **`notary proxy`** (§8): flags, config, registration, and the credential-hygiene guards.
5. **Docs**: the architecture spec's phase table row 9, the README's Status section, and the stale
   *"(Phase 7)"* in `.clinerules`' folder tree.

The order is forced twice: 1 before 3, because the hooks call the observer; and 2 before 4, because the command
has to construct the pipeline. 1 and 2 are independent of each other, which is worth knowing when the plan is
written — either could be swapped or even run in parallel behind the `Sink` seam.

## 12. Limitations recorded, not hidden

- **A crash loses the queue.** Decision 3; `Close` drains, and nothing else does.
- **A derived correlation ID collapses identical requests.** Decision 2.
- **A delete through the proxy is attributed by the reconciler, not by the proxy.** Decision 1: the removal is
  recorded, the caller who asked for it is not.
- **The ledger's `Seq` is enqueue order, not arrival order.** One writer appends in the order it is handed
  records, and two requests arriving a millisecond apart may be enqueued in either order. `At` carries the
  arrival instant, which is the fact a reader wants; `Seq` is the fact the chain needs.
- **Nothing is recorded for traffic over the body cap.** §5 steps 1 and 5: such a request is forwarded
  untouched and produces no record, and unlike a queue drop it does not even produce a gap entry, because the
  bytes needed to identify it were never read. This is the smallest hole in the phase and it is stated rather
  than left to be found.

## 13. Open questions for the reviewer

1. **The extracted `Observer`'s name and boundary.** Is `interceptor.Observer` with a `Sink` the right
   decomposition, or does the shape want a different seam — for instance the `Sink` being an option on
   library's `New` rather than the observer's own constructor?
2. **The drop marker's channel.** The `AuditWriter` takes `channels []io.Writer` and the proxy's drops need the
   same loud sink. Should the proxy build its own, or should the pipeline take the writer and let the writer's
   channels serve both?
3. **Whether `At` should be arrival time or observation time.** §5 chooses arrival, on the grounds that it is
   the moment the operation happened. Library mode's conflation is not an argument for copying it, but a reader
   comparing the two modes' records will see `At` mean slightly different things, and that is worth a second
   opinion.
4. **Whether the body cap should produce a gap.** §12's last bullet: an over-cap request is forwarded and
   silently unrecorded, while a queue drop is durably owed. Is that asymmetry defensible — the cap being a
   deliberate configuration limit rather than an overload — or should an over-cap request also be tallied?
