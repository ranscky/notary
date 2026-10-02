# Notary v1 Phase 6 — Export, Redaction and Phrasing: Design

**Date:** 2026-10-02
**Branch:** `feat/export`
**Parent spec:** [`2026-09-28-notary-v1-architecture-design.md`](./2026-09-28-notary-v1-architecture-design.md) §9, §10, §11, §12
**Status:** design approved, implementation plan not yet written

---

## 1. What this phase is for

Phases 0–4 built a ledger that records what Notary witnessed. Phase 5 built the reconciler, so the
ledger now holds claims at **all three tiers** — `Observed`, `Reconstructed`, and `Internal` — and is
the first time the store contains something a human would want to read.

Phase 6 is the first phase that **shows** it to anyone. That makes it two things at once, and both
are riskier than they look:

- It is where the privacy promise is kept or broken. The store is a durable second copy of the
  memories it describes, so this is the phase that decides whether that copy leaks.
- It is where a non-authoritative language model is allowed anywhere near a compliance artifact.
  That is the most dangerous thing this project will ever do, and the parent spec's answer
  (paraphrases are display-only, opt-in, and degrade) is the right one — but only if the code makes
  it structurally true rather than a convention someone remembers.

There is no new evidence in this phase. Everything it prints was already decided, signed and
chained. Its whole job is to report that faithfully, including the parts it cannot show.

---

## 2. The organising principle

**Presentation may never change the evidence.**

Every decision below falls out of that one rule:

- Redaction removes text from what you see and changes no hash, because the hash covers the real
  content and the real content is still there.
- Tier phrasing restates a record; it is never the record's source of truth, and it is always
  printed beside the structured fields it restates.
- A paraphrase is never an input to any decision function, which is enforced by which package can
  import the type at all.
- Every loss is **stated**: a redacted entry says it was redacted, a degraded paraphrase says it
  failed, and a truncated export is detectable because a checkpoint travelled with it.

The rule's corollary matters as much: when the tool cannot show something, it says so rather than
showing something that looks complete.

---

## 3. Decisions taken during design

| # | Decision | Note |
|---|---|---|
| D1 | **Supersedes parent D10.** Providers are pluggable through the OpenAI-compatible chat-completions schema. DeepSeek is the provider actually in use. **No provider SDK is added.** | Parent D10 named the Anthropic dependency as approved. The operator's requirement is that all major providers be supported and that DeepSeek work; `internal/phrase` remains the only package that reaches an LLM, so D10's load-bearing half — compiler-enforced isolation — survives. Recorded as a delta in §11. |
| D2 | The client sends **only** `model` and `messages**, and posts to **`/chat/completions`**. | Verified during design, not assumed. Anthropic's compatible layer **silently ignores** unknown fields, so a rich request would appear to work while quietly dropping intent; Gemini's compatible layer **404s `/responses`** outright. A minimal request is the only one every provider honours as written. |
| D3 | Sensitivity has **two** input paths, both marking content at **write** time: a caller option on `Add`/`Search`, and declarative `Rule` configuration. | Parent D4 says `Sensitive` is caller/config-supplied. Until this phase nothing could supply it: `library/mem0.go` documents `Content.Sensitive` as *always false*, with no way to express the opposite. Marking must happen before hashing, which is the only point where it means anything. |
| D4 | Redaction happens **at render only**; `--include-sensitive` cannot change any hash. | Upholds parent D4. Structurally true rather than promised: the hash is computed over the stored content, and rendering never writes. |
| D5 | Every redacted entry **states that it was redacted**, and redaction is opt-out only via `--include-sensitive`. | The default is the safe one. A reader who sees nothing must be able to tell "nothing was there" from "I am not being shown it". |
| D6 | Deterministic **tier phrasing** lives in `internal/export` for now. | It is needed by `explain` in Phase 8, which is when it should be extracted. Extracting it now would add a package the parent spec's tree does not have, for a caller that does not exist. |
| D7 | `Paraphrase` lives in `internal/phrase`, and **no decision package can import it**. | Enforced by the import graph and checked by a test, so "generated text is never an input" is a compile-time property rather than a review item. |
| D8 | `export` emits **JSONL to stdout** and writes the checkpoint to a file given by `--checkpoint-out`, **in the format `verify --checkpoint` already reads**. | Keeps a bulk export streamable and greppable, and makes the exported range immediately verifiable with a command that already exists. |
| D9 | The LLM piece lands **last** and is independently revertable. | It is the only piece with an external service, a cost, and a failure mode that does not exist anywhere else in the project. |
| D10 | No secrets in the repository. The phrasing provider is configured by `NOTARY_PHRASE_API_KEY`, `NOTARY_PHRASE_MODEL`, `NOTARY_PHRASE_BASE_URL`, env only. | `.clinerules` apply unchanged. There is deliberately no config-file path for the key. |

---

## 4. Surfaces

### 4.1 Packages and the import rule

```
cmd/notary ─ notify export · verify · reconcile · gaps · version
      │
      ├─ internal/export ──┬─ internal/ledger   (read path)
      │                    ├─ internal/phrase   (LLM, opt-in)
      │                    └─ internal/sign     (checkpoint)
      ├─ internal/phrase   (OpenAI-compatible client; imports no Notary decision package)
      └─ internal/reconcile · internal/ledger · internal/interceptor · internal/record · internal/mem0 · internal/sign · internal/store · internal/gap
```

`internal/phrase` is imported by `internal/export` and by nothing else. It imports `internal/record`
for scope and evidence types only — never `internal/reconcile`, `internal/ledger`, or `internal/store`.
A test asserts this with a package-graph check, so D7 cannot rot.

### 4.2 The command

```
notary export --from <RFC3339> [--to <RFC3339>] [--include-sensitive]
              [--checkpoint-out <path>] [--phrase] [--max-span <duration>]
              [--user-id <id>] [--agent-id <id>] [--app-id <id>] [--run-id <id>]
```

- `--to` defaults to now. `--from` is required: an unbounded export of a large ledger is a footgun,
  and requiring the start of the range makes the cost explicit.
- Scope flags mirror `reconcile`'s four, so "everything between these dates" and "everything for this
  user" compose the same way in both commands.
- The four scope flags match on the record's subject scope, exactly as `reconcile`'s `Window` does.

### 4.3 The sensitivity rules file

Declarative YAML, referenced by path through `NOTARY_SENSITIVITY_RULES` only — **no CLI flag**. Rules
mark content at write time, and no `notary` command writes records, so a flag would have nothing to
attach to: the rules belong to whatever application constructs the interceptor. Rules are configuration,
never code, per parent §10.

```yaml
# Any rule that matches marks the record's content Sensitive at write time.
rules:
  - name: health-data
    match:
      scope:
        user_id: patient-42      # optional; omitted means any scope
      metadata:
        category: health         # optional; matches Mem0 metadata key=value
```

A rule matches when **every** clause it specifies matches. A rule specifying neither clause is
rejected at load time: a rule that matches everything would mark an entire ledger sensitive by
accident, and that failure should be loud at startup rather than discovered in an export.

---

## 5. The sensitivity input paths

Two ways in, one meaning.

**Caller-supplied.** `library.Mem0Interceptor.Add` and `.Search` gain variadic options, so every
existing call site compiles unchanged:

```go
resp, err := it.Add(ctx, correlationID, messages, library.Sensitive())
```

The option sets `Content.Sensitive` on the `Observed` records the call writes, before they are hashed.

**Rule-supplied.** The interceptor is constructed with the loaded rule set. For each record it is
about to write, the first matching rule marks the content sensitive. Rules are evaluated in file
order and the first match wins, so the outcome does not depend on map iteration.

**Precedence.** Caller-supplied and rule-supplied are an **OR**: either marks it. There is no way to
mark content non-sensitive against a matching rule, because the safe direction is the only one worth
having here.

The doc line in `library/mem0.go` that says `Content.Sensitive` is always false becomes false itself,
and is rewritten as part of this phase.

---

## 6. Redaction

Redaction removes `Content.Text`. It does not remove the hash, the `Sensitive` flag, or the fact that
content existed.

A redacted line carries the subject's `content_hash` and a `redacted` string naming **why** — the flag,
as `sensitive`. It carries no text, and the field is absent entirely when content was shown.

**Corrected after Task 3.** This first promised to name the *rule* that matched, as `rule:<name>`. That
is not achievable without a hash change: `record.Content` is `{Text, Sensitive}`, both mixed into the
record hash, so persisting which rule fired would change the digest of every record ever written. The
flag is the evidentiary fact; which rule produced it is declarative configuration, reproducible from
the rules file, so it is not recorded. Relatedly, a **metadata** clause in a rule can only match on the
search-surfaced path, because `Add` carries no Mem0 metadata — a scope clause is what marks an add.

**Every redacted entry states that it was redacted.** A JSONL consumer must never have to guess whether
an absent `content` field means "there was no content" or "you are not cleared for it".

**The hash is invariant.** `--include-sensitive` changes what is printed and nothing else. This is not
a convention: rendering is a read path with no writer, and the ledger's own `Append` is the only code
that computes a hash. The round-trip test (§10) proves it by exporting the same range twice, with and
without the flag, and asserting every hash in both outputs is identical to the stored record's.

---

## 7. Tier phrasing

One sentence per record, derived mechanically from fields that are already there — `Event`, `Tier`,
`Reason.Kind()` — plus the scope's memory id when the record has one.

The vocabulary is the seven events crossed with the ten reason kinds, restricted to the combinations
`record` can actually construct:

| Event | Kinds it carries | Example phrasing |
|---|---|---|
| `add_requested` | `add_acknowledged` | "an add was requested and Mem0 acknowledged it" |
| `add_resolved` | `stored_by_mem0`, `no_facts_extracted`, `add_failed` | "the add resolved: Mem0 stored memory \<id\>" |
| `memory_kept` | `stored_by_mem0`, `kept_by_content_match` | "the memory was kept; it is still present in the scope" |
| `memory_dropped` | `removed_by_mem0`, `absent_from_search` | "the memory was dropped: Mem0 no longer holds it" |
| `search_performed` | `search_performed` | "a search ran in this scope" |
| `memory_surfaced` | `returned_by_search` | "the memory was returned by a search at rank N" |
| `audit_gap` | `audit_unavailable` | "an operation happened that Notary failed to record" |

Two properties matter more than the wording:

- **Totality.** Phrasing is a total function over the combinations `record` can construct, and a test
  enumerates them and fails on an unphrased one. A new claim kind must not be able to ship with no
  wording, because an unphrased claim in a compliance export reads as a missing record.
- **Subordination.** The sentence is printed **beside** the structured fields, never instead of them,
  and a phrasing test asserts the structured fields are always present. A reader can ignore the prose;
  no consumer can depend on it.

---

## 8. `internal/phrase`

A small HTTP client over the OpenAI-compatible schema, built the way `internal/mem0` is: an
`http.Client`, recorded fixtures, and no SDK (D1).

```go
type Client struct{ /* baseURL, apiKey, model, httpClient */ }

func NewClient(baseURL, apiKey, model string, hc *http.Client) *Client

// Paraphrase restates the claims in records. Each record carries its own tier,
// so no tier is passed separately.
func (c *Client) Paraphrase(ctx context.Context, records []record.Record) (Paraphrase, error)
```

**Request.** `POST {baseURL}/chat/completions` with `{"model": ..., "messages": [...]}` and nothing
else (D2). The prompt carries the structured fields — event, tier, reason kind, scope, memory id,
content hash — and never the content text, so the phrasing pass cannot leak the very thing redaction
exists to protect. That is a deliberate consequence of the organising principle: a paraphrase is
derived from the audit metadata, not from the sensitive payload.

**Response.** `choices[0].message.content`. A response with no choices, or an empty content string, is
an error, not an empty paraphrase.

**`Paraphrase`.** A distinct type carrying the text, the model name, and the time it was generated. It
is display-only: it appears in the JSONL under a `paraphrase` object, labelled as a paraphrase, next
to — never inside — the record's own fields. Because the type lives in `internal/phrase`, no decision
package can even name it (D7).

**Failure degrades.** Any error — network, status, malformed body, timeout — leaves the structured
record untouched and adds a note saying the paraphrase failed and why. The export still succeeds: a
phrasing outage must never cost an operator their audit output, and a failed paraphrase is reported,
never silently omitted.

**Opt-in.** `--phrase` is required. There is no configuration that turns it on permanently, so a
provider is only ever billed for an export someone asked for.

---

## 9. `notary export`

**Flow.** Validate flags → open the ledger read-only → list records in the range through
`ledger.ListRecords` → for each, render one JSONL object (with redaction applied) → optionally
paraphrase → write the checkpoint if `--checkpoint-out` was given.

**Validation** (parent §9): `--from` and `--to` parsed as RFC3339 with plain-language errors naming
the expected format; `--to` before `--from` rejected rather than silently returning nothing; an empty
result is a success with zero lines and says so on stderr, so "no records" and "broken invocation" are
distinguishable.

**Bounding.** The span `--from` to `--to` is capped at a documented default of 366 days, overridable
with `--max-span <duration>`, because the cost of an export scales with the range and the failure mode
of a compliance export should be an error rather than an out-of-memory kill. Long ranges stream: each
record is rendered and written as it is read, so memory does not scale with the export.

**Exit codes.** Zero on success, including a zero-record range. Non-zero on validation failure, on a
read failure, and on a checkpoint write failure. A phrasing failure does **not** change the exit code
(§8's degradation rule) but is reported on stderr and noted in the output.

**The JSONL object.**

```json
{"seq":2,"id":"memory_kept:stored_by_mem0:eccd10b3-...","at":"...","recorded_at":"...",
 "event":"memory_kept","tier":"Reconstructed","reason_kind":"stored_by_mem0",
 "memory_id":"eccd10b3-...","scope":{"user_id":"u1"},
 "content_hash":"...","redacted":"sensitive","phrasing":"the memory was kept; ...",
 "prev_hash":"...","hash":"...","signature":"...","signer_key_id":"..."}
```

`redacted` is absent when content was shown. `phrasing` is always present; `paraphrase` appears only
under `--phrase` and never replaces the fields beside it. Order is stable and the field set is fixed,
so two exports of the same range differ only where the ledger does — which is what makes them
diffable in a review.

**The checkpoint attests the ledger head at export time**, not the last record in the range. That is
the point of it, per parent §7: the truncation a hash chain cannot see is a shortened or rewritten
**tail**, so a checkpoint taken at export and verified later is what makes that detection possible.

---

## 10. Testing strategy

- **Redaction round-trip** (parent §12 item 8). Export a range twice, with and without
  `--include-sensitive`, and assert: every hash is identical across both outputs and equal to the
  stored record's; the sensitive record's text appears in exactly one of them; and the redacted line
  states that it was redacted.
- **Sensitivity input paths.** A caller-marked add and a rule-marked add both produce a stored record
  with `Sensitive` true, and the flag is covered by the record's hash (mutating it changes the hash).
- **Rules.** Matching, first-match-wins ordering, an empty-clause rule rejected at load, and a rule
  that matches nothing being harmless.
- **Phrasing totality and subordination.** Every constructible (event, kind) pair has phrasing; every
  rendered line carries the structured fields alongside it.
- **`internal/phrase` fixtures.** Recorded responses for the compatible schema, captured from the
  provider in use with provenance in `FIXTURES.md`, exactly as `internal/mem0` documents its own. Plus
  synthetic fixtures for the two quirks the design research established: a single-element `choices`
  array, and a response carrying unknown fields that must be ignored rather than refused.
- **Degradation.** Network error, non-2xx, empty choices, and a body that is not JSON each leave the
  record intact, add a note, and keep the exit code zero.
- **Import graph.** A test asserts no decision package imports `internal/phrase` (D7).
- **Checkpoint integration.** An export with `--checkpoint-out` is accepted by `verify --checkpoint`,
  and a truncated export is rejected by it.
- **Key-material canary.** No export output, in any mode, contains signing key material.

The live provider check is a separate, opt-in piece of work using the same discipline as
`internal/reconcile/live_mem0_test.go`: a build tag, a key from the environment, no gate. It needs a
provider key that does not exist in this repository yet, and will be requested when it is needed.

---

## 11. Deltas to the parent spec

1. **D10 is superseded.** The parent records the Anthropic dependency as approved and as
   `internal/phrase`'s reason to exist. The operator requires multi-provider support and uses DeepSeek,
   so the phase builds a provider-agnostic OpenAI-compatible client and adds **no** SDK. The isolation
   property D10 was protecting — that no core package can import an LLM — is preserved and now
   enforced by a test (§4.1).
2. **`internal/phrase` is not "the Anthropic adapter".** Parent §11's phrasing is amended: it is the
   only package that talks to an LLM, and the only importer of an LLM client, whatever provider that
   client is pointed at.
3. **`Config.SensitivityRules` is implemented.** Parent §10 already specifies it; this phase is where
   it becomes real, together with the `Rule` shape and the file that carries it.
4. **The interceptor's doc comment is corrected.** `library/mem0.go` states that `Content.Sensitive`
   is always false and cannot be expressed. After this phase that sentence is no longer true, and
   leaving it would be another documented-but-unimplemented claim in reverse.

---

## 12. Out of scope

- **`explain`, `replay`, proxy mode** — phases 8, 7 and 9. Tier phrasing is written so that extracting
  it for `explain` is mechanical, but the extraction is not done here (D6).
- **Publishing checkpoints to an external transparency log** — parent §7 documents this as future work
  and the only complete defence against truncation. This phase produces the checkpoint; it does not
  distribute it.
- **`FailClosed`** — parent §10 reserves it, and nothing implements it in v1.
- **Retries and backoff for the phrasing provider.** One attempt; a failure degrades with a note. A
  bounded retry is defensible and is deliberately not added, because a retry multiplies a bill the
  operator cannot see and degradation is already the documented behaviour.
- **Streaming the paraphrase, embeddings, and provider-specific parameters** — the client sends the
  minimum field set on purpose (D2).
- **A `--format` option.** JSONL is the only export format; adding a second before a consumer asks for
  one would be speculative.
- **Config-file support for the provider key.** Env only (D10).
