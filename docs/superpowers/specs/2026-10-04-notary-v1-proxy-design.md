# Notary v1 Phase 9 — proxy mode: Design

**Status:** draft for review. The six decisions in §3 were taken with the operator during brainstorming; this
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

Proxy mode serves those. The application re-points its Mem0 base URL at Notary; Notary forwards every
request to the real Mem0 and returns the response untouched, recording what it saw on the way through. No
code change in the application, in any language, and no Notary dependency in its build.

The audience is unchanged from §1 of the parent spec — a compliance lead who must explain an agent's memory
decisions without reading code. What changes is that the records no longer depend on the application
cooperating with a library.

**The property that makes this worth building.** A deployment can move from library mode to proxy mode,
or run both against one ledger, and the ledger's *meaning* does not change. That is not an aspiration to be
hoped for at review time: §7 and §9 make it a field-level check with a test. It is also why §4 does the
refactor it does.

## 2. What is already fixed, and what is deliberately not

The architecture spec excludes this phase from its own scope, in as many words:

> **Scope:** v1 (library mode). Proxy mode is Phase 9 of the same build but is not the subject of this document.

So this spec is the subject, and it owes what the parent spec did not. What the parent does fix:

- **§4's package tree** reserves `internal/interceptor/proxy/proxy.go` for this phase. §14's delta table
  lists it among the packages the original folder layout did not account for.
- **§4**, on why a deployment mode is not a reconciler: *"A deployment mode wraps live traffic; a reconciler
  reads history. Collapsing them would force proxy mode and reconcile mode to share a lifetime they do not
  have."* This phase therefore adds no reconciliation and reads no history.
- **`.clinerules` §5**: *"In proxy mode, a failure in the audit-write path must never block, delay, or fail
  the underlying Mem0 request/response."* §3's second decision is a ruling on what that sentence means;
  §6 is the architecture that satisfies it.
- **§5's vocabulary is closed and gate-tested**, so §7's single new reason kind is a change to a foundational
  package and is treated as one.

**`.clinerules` calls the type `ProxyInterceptor`, in a folder tree whose phase numbering predates the
renumbering** (§13 moved proxy from 7 to 9). The name is kept: `internal/interceptor/proxy` exports a type
that satisfies `interceptor.Interceptor` (it reports a fail mode and it closes), which is what the name
promises. This spec adds one correction to `.clinerules`: its folder tree still says *"(Phase 7)"*.

## 3. The decisions taken in brainstorming

Each was put to the operator with its cost; each cost is recorded here rather than in a footnote.

1. **A record's identity comes from a request header, with a derived fallback.**
   `X-Notary-Correlation-Id` when the caller sets it — that is the same correlation ID library mode's caller
   passes explicitly, so a caller who wants identical semantics across both modes gets them, including exact
   retry deduplication. When it is absent the proxy derives one:
   `DeriveCorrelationID(scope, hex(record.ContentHash(method, path, body)))`
   (`internal/interceptor/library/mem0.go:557`, `internal/record/content.go:44` — the digest is
   length-prefixed per part, so the three fields cannot be re-split ambiguously).
   **Cost:** a caller that sets no header gets a key that is stable across an exact retry and *also* collapses
   two genuinely distinct identical requests into one record. That is the silent-loss class this project
   already fought once (the `Task 18` ruling recorded in `internal/interceptor/library/mem0.go:424-441`), and
   the fallback reintroduces it — deliberately, for callers who chose not to identify themselves.

2. **The response path never touches the ledger; the write path is decoupled behind a bounded queue.**
   This is the broad reading of the §5 guardrail: a *slow* audit write — lock contention, a disk stall — must
   not become a Mem0 outage either, not merely a *failing* one.
   **Cost:** the response can be returned before the record exists, so a process crash loses whatever was
   queued. A synchronous design would lose only what it had not yet written. §6 bounds this with a drain on
   `Close` and the fact that a lost queue is a lost *record*, never a lost gap — the ledger stays honest about
   what it does and does not contain.

3. **Coverage: add, search, delete and async-add status. `get_all` and `history` pass through unrecorded.**
   Chosen after checking the vocabulary: those four have an honest landing place and those two do not (§7).
   **Cost:** `get_all` and `history` are invisible. Named as a boundary in §10 rather than left to be
   discovered, because the reconciler already records their consequences (`memory_kept`, `memory_dropped`),
   which is the part the compliance lead's question needs. The async-add status is also the narrowest of the
   four: it is recorded only for a poll this proxy can attribute to an add it observed (§7), so a deployment
   that polls from a different process than the one that proxied the add gets no record for that poll. This
   option was revised during the spec self-review, after the first draft's claim that the record "fits
   exactly" turned out to hold only for an attributed poll.

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
   **Cost:** a gap entry per dropped record means a sustained overload writes many entries. §6 bounds the
   tally and states what happens to drops beyond the bound instead of pretending the bound does not exist.

6. **A scopeless endpoint gets an empty scope, documented rather than papered over.**
   `DELETE /v1/memories/{id}/` carries no entity dimensions anywhere in the request, so that record's
   `Subject.Scope` is empty (`internal/store/sqlite.go:35-39` — the columns' `DEFAULT ''`). `GET
   /v1/event/{id}/` has the same gap, and §7's attribution rule is what closes it: that record takes the scope
   of the add it resolves, read back from the ledger, rather than an empty one.
   **Cost:** a delete record is attributable by memory id, so `explain --memory` finds it, but a scope-filtered
   `export` or reconcile pass will not. §12 records it as a limitation. A caller-supplied scope header was
   considered and rejected as surface nobody would send.

## 4. The shape, and the refactor that is an explicit ask

**The problem.** For add and search the proxy must build the records library mode already builds: the same
events, the same reason kinds, the same frozen evidence payloads (`mem0.AddPayload`,
`mem0.SearchPerformedPayload`, `mem0.MemorySurfacedPayload`), the same `<id>#<rank>` sibling IDs, the same
idempotency-key derivation. Today that lives inside `internal/interceptor/library` as unexported methods on
`Mem0Interceptor` — `observeAdd` (`:298`), `observeSearch` (`:341`), `writeSearchPerformed` (`:356`),
`writeSurfaced` (`:398`) — driven by a construction-time scope, a rule set, a clock and an `AuditWriter`.

**The shape.** Extract that building into a new `Observer` in `internal/interceptor`, holding the scope, the
rule set, the clock and a write sink; `library.Mem0Interceptor` keeps its public API and its emitted records
and delegates to it. The proxy's job is then only to turn Mem0's HTTP request and response into the
observer's inputs.

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

**Packages.**

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
   request is still forwarded exactly as it arrived, and steps 5 and 6 are skipped, because a body that
   cannot be read whole cannot be attested to.
2. **Decide whether this is observed traffic**, by method *and* path, so that paths which share a prefix
   cannot be confused: `POST /v3/memories/add/`, `POST /v3/memories/search/`,
   `DELETE /v1/memories/{id}/`, `GET /v1/event/{id}/` (`internal/mem0/client.go:280,291,344,354`). Note
   `GET /v1/memories/{id}/history/` (`:333`) shares a prefix with the delete path and is *not* observed;
   the method separates them besides.
3. **Capture the arrival instant** and the caller's `X-Notary-Correlation-Id`, then resolve the scope from
   the body's entity fields where the endpoint carries them (§12 records where it does not).
4. **Forward.** The response is streamed back to the caller exactly as Mem0 sent it, and the response status,
   headers and body are untouched — `httputil.ReverseProxy` handles the hop-by-hop rules that a hand-rolled
   forwarder gets wrong.
5. **Tee the response body** under the same cap, for the observation only. A response larger than the cap is
   forwarded to the caller in full but not observed, for the same reason as step 1 and with the same
   consequence: no record.
6. **After the response is complete**, hand the request/response pair to the `Observer`, which builds 0..n
   records and hands each to the sink. Nothing in steps 1–6 has touched the ledger, with one exception that
   is described next.

**The one observation that needs something the request cannot supply.** An event-status poll resolves an add,
and §7 shows that the record for it must carry *that add's* scope, `At` and content hash. None of the three is
in the poll. So for this endpoint, and only this one, the hook reads back the record this proxy already wrote
— `GetRecord`, keyed on the caller's `X-Notary-Correlation-Id`, which is the identity the add was written
under. The response has already been sent by then, so this is not on the response path.

That lookup is best-effort, and its failure mode is the same answer §7 already gives for an unattributable
poll: **the observation is skipped.** If the caller sent no header, or the record is not there, or the read
fails, the poll is forwarded and recorded by nobody. No timeout machinery is needed, because a slow read and
a missing read have one outcome — and it is an outcome the design already owes an answer for.

Step 6 being *after* the response is what makes decision 2 possible at all, and it is not a compromise: an
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

- **A bounded channel** (default 1024 records, `--queue-depth`) feeds **one writer goroutine**, which owns the
  `AuditWriter` and performs every ledger append. One writer means `Seq` follows enqueue order, and it means
  the ledger's own lock is never contended by request goroutines.
- **Enqueue is non-blocking.** On a full queue the producer does two cheap things and returns: it increments
  an in-memory drop tally, and it writes a one-line marker to the process's loud channels. It never waits. The
  response has already been sent by then, so this costs a caller nothing — its only purpose is that the drop
  is not silent.
- **Drops become gap entries.** The writer, between appends, drains the tally into `gap.Log.Record` calls —
  one entry per dropped record, carrying the dropped record's `Event`, `Scope` and `ID` as `Kind`, `Scope` and
  `CorrelationID`, which is the shape `ledger.GapBreaks` accounts for and therefore what makes the gap
  reconcilable rather than merely recorded (`internal/interceptor/interceptor.go:186-206` is the same
  reasoning for the ledger-failure path, and this reuses it).
- **The tally is bounded, and the bound is stated rather than hidden.** The tally holds the identities of up
  to `--queue-depth` drops and a running count of the rest. The writer emits one gap entry per held identity
  and, when the tally overflowed, one final entry recording how many further drops went unnamed. Under
  sustained overload the ledger therefore shows a bounded number of named gaps plus an honest count, and never
  an unbounded queue of pending work.
- **`Close` stops accepting, drains the queue, drains the tally, then closes the gap log.** Its error is the
  doubly-failed case only, exactly as `AuditWriter.Close` is.
- **The doubly-failed case is loud, not swallowed.** If the gap log itself cannot be written, the markers were
  already emitted (the `AuditWriter` writes them before returning its error) and the writer reports it on
  stderr. v1 has no enforcement function, so the proxy keeps serving: an audit outage must not take down
  traffic, which is the whole point of fail-open-loud.

## 7. Record coverage, and the one vocabulary change

**Recorded, with the evidence each can honestly cite:**

| Endpoint | Records | Evidence source |
|---|---|---|
| `POST /v3/memories/add/` | `add_requested` + `add_acknowledged` | Mem0's response body |
| `POST /v3/memories/search/` | `search_performed`, then one `memory_surfaced` per result | Mem0's response body |
| `GET /v1/event/{id}/` | `add_resolved` + `stored_by_mem0` / `add_failed`, **only for a poll this proxy can attribute to an add it observed** | Mem0's status response body |
| `DELETE /v1/memories/{id}/` | `memory_dropped` + **`removed_by_request`** (new) | the status line only (§12) |

`GET /v1/event/{id}/` fits the existing vocabulary — but only for a poll the proxy can attribute, and getting
that right is subtler than the record's *name* suggests. The reconciler builds this record out of the **add**
it already holds in the ledger: its ID is `add_resolved:<kind>:<eventID>`
(`internal/reconcile/adds.go:252-254`), its `At` and `ContentHash` come from that add (`:126-141`), and its
idempotency key hashes the add's scope (`:178-184`; see `record/idem.go:81-85`). A poll carries none of the
three. A proxy recording one from the request alone would therefore form the *same ID* with a *different key*,
and the `id` column's UNIQUE constraint (`internal/store/sqlite_test.go:792`) would reject the append — turning
a legitimate observation into a fail-open-loud gap entry, under a name that is not even the same claim.

So the proxy reads back the add it wrote (§5) and rebuilds the record from the same three inputs the reconciler
uses. The identifier, the key and the claim are then the reconciler's, and the two observations genuinely
collapse to one fact: whichever writes first takes the unique id and the other dedupes silently, which is
`Append`'s documented behaviour for a duplicate key
(`internal/interceptor/library/mem0.go:424-441` relies on exactly that). **A poll the proxy cannot attribute is
not recorded at all** — the honest answer, since the alternative asserts something about an add nobody looked up.

The proxy records only the two outcomes that are Observed from Mem0's response. `no_facts_extracted` is a
`Reconstructed` claim resting on a registered rule (`internal/reconcile/adds.go:209-217`), and it is not the
proxy's to make: the proxy did not do the inferring, and a rule's version must belong to the run that applied it.

**The one new reason kind: `removed_by_request`.** Named to sit beside `removed_by_mem0`, and it says the
thing no existing kind says — who caused the removal. The existing kinds each name someone else:
`removed_by_mem0` says *Mem0* removed it, `absent_from_search` says a search missed it.

Three obligations come with it, and they are why this is a change to a foundational package:

1. It joins the closed vocabulary and its tier map — a constant, `ReasonKinds()`
   (`internal/record/reason.go:72`) and `AllowedTier` (`:79`), where it is `Observed`.
2. `export.Phrase` must word it, because `TestPhraseIsTotalOverTheVocabulary`
   (`internal/export/phrase_test.go:72`) fails the build otherwise. The pair gets a specific sentence beside
   the eleven already specified — *"the memory was dropped: the removal was requested"* — and the eleven must
   not change, which their own test guards.
3. **`EventMemoryDropped`'s doc sentence loosens.** It reads *"a surfaced memory was dropped"*
   (`internal/record/event.go:24-25`), which is true of the reconciler's two cases and false here: the proxy
   never saw this memory surface. The event is the honest one; the comment is what is wrong. Changing a
   comment in `internal/record` is not a vocabulary change, and saying so plainly is better than straining a
   record onto `memory_surfaced` so that a stale comment stays true.

**Passed through unrecorded, and why:** `POST /v3/memories/` (`get_all`) and
`GET /v1/memories/{id}/history/`. The first is an enumeration of everything in a scope and the second a read
of one memory's change log. Neither is a decision about a memory, and neither has an honest landing place:
recording an enumeration as `search_performed` would be a lie, and the record is the source of truth. The
reconciler already records their consequences — `memory_kept` and `memory_dropped` are exactly what it derives
from enumerating and reading history — so the compliance lead's question still gets answered.

## 8. The command and its configuration

```
notary proxy [--addr 127.0.0.1:8080] [--queue-depth 1024] [--max-body 8MiB]
```

Registered in `cmd/notary/root.go` beside the other six. The view of the world is reversed from every other
command: the others read the ledger and print; this one serves traffic and writes. So its stdout is
**unused** — the operator's channel is stderr, where the listen banner and every drop marker go — and it
writes to no file but the ledger and the gap log.

**It needs no Mem0 API key.** It forwards the caller's own `Authorization` header upstream, so it is
credential-neutral and holds no credential of its own. That makes credential hygiene a hard requirement
rather than a nicety: no header value, and no query string, may reach a log, an error, or a record.
`internal/mem0/client.go`'s redaction scheme (`credentialComponents`, `:99-144`) is the model, and §9's
canary is the guard.

**What it does need:** `NOTARY_DB_PATH`, `NOTARY_GAP_LOG_PATH`, `NOTARY_SIGNING_KEY` (ledger appends are
signed, `internal/ledger/ledger.go:100`, so this is required exactly as `reconcile` and `export` require it),
the upstream base URL from `NOTARY_MEM0_BASE_URL`, and a signing key for nothing else.

**The sensitivity-rules problem, and its resolution.** `config/config.go:36-42` documents that sensitivity
rules are reachable only through `NOTARY_SENSITIVITY_RULES`, with no `Config` field and no CLI flag, *because*
*"rules mark content at WRITE time and no notary command writes records, so a flag would have no command to
live on."* `notary proxy` is the first command that writes records, so that sentence becomes false. The
resolution keeps the flag absent and makes the comment true: the proxy reads the rules path from
`NOTARY_SENSITIVITY_RULES`, through the same path an application does, and there is still no flag — on the
grounds that the variable already exists, is already the documented way, and a second way to say the same
thing is how configuration drifts. The comment is corrected, not the decision.

## 9. Testing

**The phase's falsifier: parity between the two modes.** For one operation — an add, and a search with
results — driven through `library.Mem0Interceptor` and through the proxy against the same fixture, the two
sets of records must agree on every field the claim asserts: `ID`, `Event`, `Reason` kind, the evidence
payload bytes, `Subject` (scope, memory id, content hash), `Content` and `IdempotencyKey`.

Note precisely what this is not, because an earlier statement of it was wrong: it is **not** byte-identity.
The canonical hash covers `At` and `RecordedAt` (`internal/record/chain.go:59,97`), so the two modes' records
have different hashes and different signatures *by construction* — they were observed at different instants
on different clocks. A byte-identity claim would be untestable; this one is the strongest property that is
actually true, and it is what "switching modes changes nothing in the ledger" means once the timestamps are
excluded.

Around it:

- **The response path never blocks.** A sink that blocks forever must not delay a response: issue a request
  against a proxy whose write path is deliberately stalled and assert the response arrives.
- **A drop leaves a durable gap entry.** Fill the queue, then assert the gap log holds an entry whose
  correlation ID is the dropped record's, in the shape `ledger.GapBreaks` accounts for — and that the caller's
  response was never delayed by the fill.
- **An event-status poll is attributed or it is not recorded.** With the header set and the add present, the
  record must match the reconciler's on record ID *and* idempotency key — and writing both must leave exactly
  one record in the ledger, which is the dedupe claim turned into an assertion rather than a hope
  (`Append`'s silent no-op on a duplicate key is the mechanism). With no header, with an unknown id, and with
  an add this proxy never wrote, the poll must be forwarded and produce no record at all.
- **Unrecorded endpoints pass through untouched:** `get_all` and `history` arrive at the upstream byte-for-byte
  and produce no record.
- **Forwarding fidelity:** status, headers and body cross the proxy unchanged, against an `httptest` upstream.
- **The refactor changed nothing.** `library`'s emitted records for a fixed operation are the same before and
  after, asserted rather than assumed from the ledger fixtures happening to still pass.
- **Credential canary:** an `Authorization` header and a query string bearing a sentinel value must not appear
  in any record, marker, or error the proxy produces.
- **`Close` drains:** records enqueued before `Close` are in the ledger after it.
- **The vocabulary's own gates:** `ReasonKinds` completeness, `AllowedTier`, `TestPhraseIsTotalOverTheVocabulary`,
  and the test that pins the eleven specified sentences to their exact wording.
- **Config:** the proxy requires a signing key, does not require a Mem0 API key, and honours
  `NOTARY_SENSITIVITY_RULES`.

## 10. Boundaries (out of scope)

- **No `get_all` or `history` recording.** §7 states why, and records it as a boundary rather than a bug.
- **No caller-supplied scope header.** Decision 6.
- **No in-process middleware surface.** Decision 4 ships a command and the package it drives; a Go
  application that wants the handler can mount it, but nothing is designed for that.
- **No sensitivity-rules flag.** §8.
- **No reconciliation, and no ledger scanning.** The parent spec keeps a deployment mode and a reconciler
  apart because they have different lifetimes (§2), and this phase honours that: it never enumerates, never
  diffs, and never reads history. The single exception is the by-id `GetRecord` an event-status poll needs in
  order to attribute itself (§5, §7) — a lookup rather than reconciliation, and one whose failure is already
  the designed outcome.
- **No TLS termination, no authentication of its own, no rate limiting, no caching.** The proxy forwards; the
  deployment owns the edge.
- **No new dependencies.** `net/http/httputil` is stdlib, and `go.mod` does not change.
- **No index.** Nothing here reads the ledger by a new column.

## 11. Sequencing, and why the order is forced

1. **The vocabulary change** (§7): the reason kind, its tier, its phrasing sentence, and the loosening of
   `EventMemoryDropped`'s comment. First, because the delete hook lands on it and its gates are cheap to close
   while nothing else is moving.
2. **The `Observer` extraction and the `library` refactor** (§4), with the parity guard from §9. Before
   anything builds on the observer, so the refactor is reviewed against a suite that has not also changed.
3. **The pipeline** (§6): the queue, the writer goroutine, the drop tally, the gap drain, and `Close`.
4. **The forwarding handler and the observation hooks** (§5), including the event-status attribution write-back.
5. **`notary proxy`** (§8): flags, config, registration, and the credential-hygiene guards.
6. **Docs**: the architecture spec's phase table row 9 and its read-path section, the README's Status section,
   and the stale *"(Phase 7)"* in `.clinerules`' folder tree.

The order is forced at three points: 1 before 4, because the delete hook needs the kind to exist; 2 before 3
and 4, because the hooks call the observer and the pipeline's sink is what it writes through; and 3 before 5,
because the command has to construct the pipeline.

## 12. Limitations recorded, not hidden

- **`DELETE`'s evidence is thin.** A delete's response carries no body, so there are no Mem0 bytes to record
  verbatim the way an event-status response's are. The record attests to the status Mem0 returned — Notary's
  framing of Mem0's answer, not Mem0's own bytes — and the reason kind says so. This is a real limit on what
  the record proves and it is stated rather than dressed up. A `DELETE`'s content hash is
  `record.ContentHash(memoryID)`, the id the operation was about, so the `Subject` is never the zero hash.
- **An event-status record is a reconstruction from the proxy's own add record, not from the poll.** Its `At`,
  its content hash and its scope are the *add's*, because that is what makes it the reconciler's record rather
  than a near-namesake (§7). One consequence is worth stating: the record's `At` is when the add happened, not
  when it was resolved — which is exactly what the reconciler does, and the resolving instant is the poll's
  `RecordedAt` for a reader who wants it.
- **An empty scope on a delete record.** Decision 6. An event-status record does not share this — §7 gives it
  the scope of the add it resolves.
- **A derived correlation ID collapses identical requests.** Decision 1.
- **A crash loses the queue.** Decision 2; `Close` drains, and nothing else does.
- **The ledger's `Seq` is enqueue order, not arrival order.** One writer appends in the order it is handed
  records, and two requests arriving a millisecond apart may be enqueued in either order. `At` carries the
  arrival instant, which is the fact a reader wants; `Seq` is the fact the chain needs.

## 13. Open questions for the reviewer

1. **The extracted `Observer`'s name and boundary.** Is `interceptor.Observer` with a `Sink` the right
   decomposition, or does the shape want a different seam — for instance the `Sink` being an option on
   library's `New` rather than the observer's own constructor?
2. **The drop marker's channel.** The `AuditWriter` takes `channels []io.Writer` and the proxy's drops need
   the same loud sink. Should the proxy build its own, or should the pipeline take the writer and let the
   writer's channels serve both?
3. **Whether `At` should be arrival time or observation time.** §5 chooses arrival, on the grounds that it is
   the moment the operation happened. Library mode's conflation is not an argument for copying it, but a
   reader comparing the two modes' records will see `At` mean slightly different things, and that is worth a
   second opinion.
