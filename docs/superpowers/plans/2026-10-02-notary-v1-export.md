# Notary v1 Phase 6 — Export, Redaction and Phrasing: Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the ledger readable — a `notary export` that renders a time range as JSONL, hides content it was told is sensitive without touching a single hash, explains each record's tier in words, and can optionally add a clearly-labelled non-authoritative paraphrase.

**Architecture:** Two new packages. `internal/export` is a read path over `internal/ledger`: it renders each record to one stable JSON line, applying redaction at render time, and can emit a signed head checkpoint so a later `verify --checkpoint` can detect a truncated tail. `internal/phrase` is a small provider-agnostic client over the OpenAI chat-completions schema, importable only by `internal/export`, whose `Paraphrase` output is display-only and degrades to the structured record on any failure. Sensitivity gains two write-time input paths — a per-call option and declarative YAML rules — so redaction has something real to act on.

**Tech Stack:** Go 1.25, `github.com/spf13/cobra` (CLI), `modernc.org/sqlite` (store, unchanged), `github.com/stretchr/testify` (tests), `go.yaml.in/yaml/v3` for the rules file (already in the module graph as an indirect dependency — promoting it to direct adds no download). **No LLM SDK is added.**

**Spec:** `docs/superpowers/specs/2026-10-02-notary-v1-export-design.md` (parent: `docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md`)

## Global Constraints

- Go 1.25; errors wrapped with `%w`; no global mutable state; no `panic` in library code.
- **No dependency additions.** `go.mod` gains exactly one line: `go.yaml.in/yaml/v3` moving from the indirect block to the direct block.
- Secrets come from the environment only. Never a file, never a default, never logged. Key material is redacted by its existing type.
- Never implement on `master`. Work on `feat/export`.
- TDD: every behavioural change starts with a test that is **run and watched failing** before implementation.
- `gofmt -l .` empty and `go vet ./...` clean before every commit.
- The full suite runs `-p 1` (`go test -p 1 ./... -count=1`); `internal/ledger` is load-sensitive and fails spuriously when packages run in parallel.
- A doc comment that contradicts the code is a defect to fix, not leave.

## Review Focus

Five input classes the spec implies but no task's tests would otherwise pin. Each gets its test in the task that owns the code; they are listed here so no task loses them.

1. **A record with no content at all.** `Content` is absent on derived records and on `audit_gap`. Export must not render `"content":""` as though content existed, and redaction must never claim to have hidden something that was never there. A consumer cannot distinguish "nothing was recorded" from "you are not cleared for it" if both look identical.
2. **A range containing zero records.** Must exit 0 and say so on stderr. "No records" and "broken invocation" are different outcomes and must not look the same.
3. **Records written before this phase**, where `Sensitive` was never settable and is therefore false on every row. The whole range must still render, and nothing may treat a false flag as missing data.
4. **A range whose window predates a `Reconstructed` record that describes it.** The reconciler writes `At` = the event time and `RecordedAt` = when Notary worked it out, so a `memory_kept` reconstructed in October about a January add falls inside a January window. This is intended (export bounds on `At`, matching `reconcile --since`) and must be pinned by a test, because the alternative reading — that the same range always yields the same bytes — is what a reasonable person would assume.
5. **A provider response that is not what was asked for**: an empty `choices` array, a non-JSON body, a 200 carrying an error object. Each must degrade to the structured record with a note, never to a failed export and never to an empty paraphrase rendered as one.

---

## Task 1: The per-call sensitivity option

**Files:**
- Modify: `internal/interceptor/library/mem0.go` (the `New`/`Add`/`Search` signatures and the write path; the doc comment at :124 that claims sensitivity is impossible)
- Modify: `internal/interceptor/library/mem0_test.go`
- Test: `internal/interceptor/library/sensitivity_test.go`

**Interfaces:**
- Consumes: `interceptor.AuditWriter`, `record.NewObservedReason`, `record.ObservedEvidence` (existing).
- Produces: `library.CallOption`, `library.Sensitive() CallOption`; `Add(ctx context.Context, correlationID string, messages []string, opts ...CallOption) (mem0.AddResponse, error)`; `Search(ctx context.Context, correlationID string, q mem0.SearchRequest, opts ...CallOption) (mem0.SearchResponse, error)`. Variadic, so every existing call site compiles unchanged.

- [ ] **Step 1: Write the failing test** in `sensitivity_test.go`: `TestSensitiveOptionMarksObservedContent`. Build an interceptor against the existing httptest stub; call `Add(..., library.Sensitive())`; read the ledger; assert the `add_requested` record's `Content.Sensitive` is true. Second test `TestDefaultIsNotSensitive`: the same call with no option leaves it false.
- [ ] **Step 2: Run and watch it fail.** `go test -run TestSensitiveOptionMarksObservedContent ./internal/interceptor/library/ -count=1 -v` → FAIL: undefined `library.Sensitive`.
- [ ] **Step 3: Implement.** `type CallOption func(*callConfig)`; `Sensitive()` sets a bool; the write path consults it when building each `record.Content`. Note: the flag is already covered by the record hash (`record/chain.go` mixes `Content.Sensitive`), so this needs no hashing change — assert that in Step 5 rather than assuming it.
- [ ] **Step 4: Run and watch it pass.**
- [ ] **Step 5: Add `TestSensitiveChangesTheHash`**: build the same record twice, once sensitive and once not, and assert the hashes differ. This pins that the flag is tamper-evident.
- [ ] **Step 6: Correct the doc comment** at `library/mem0.go:124`, which states `Content.Sensitive` is always false and cannot be expressed. It is now expressible, and §11 of the spec records why leaving it would be a defect in its own right.
- [ ] **Step 7: Commit** — `feat(interceptor): mark content sensitive per call`.

---

## Task 2: Declarative sensitivity rules

**Files:**
- Create: `internal/interceptor/rules.go`
- Test: `internal/interceptor/rules_test.go`
- Modify: `config/config.go` (add `EnvSensitivityRules`), `config/config_test.go`
- Create: `config/sensitivity.go`
- Test: `config/sensitivity_test.go`

**Interfaces:**
- Produces: `interceptor.Rule{Name string; Scope record.Scope; MetadataKey, MetadataValue string}`; `interceptor.NewRuleSet(rules []Rule) *RuleSet`; `(*RuleSet).Match(scope record.Scope, metadata map[string]any) (name string, matched bool)`; `config.EnvSensitivityRules = "NOTARY_SENSITIVITY_RULES"`; `config.LoadSensitivityRules(path string) ([]interceptor.Rule, error)`.

The layering is deliberate: the rule type and its matching live in `internal/interceptor`, and `config` maps YAML onto them. `config` already imports `internal/reconcile`; nothing imports `config` except `cmd`, so this creates no cycle. The name is `SensitivityRule`-shaped in YAML (`rules:`) but the Go type is `interceptor.Rule` — distinct from the reconciler's claim registry, which is a different concept that happens to share a word.

The loader is the **application-facing** path, reached by `NOTARY_SENSITIVITY_RULES` at the point the interceptor is constructed. It is not wired into any CLI command, because no command writes records; Task 3's test pairs the loader with `NewRuleSet` so the two halves are exercised together, and Task 11 documents the path for operators.

- [ ] **Step 1: Write the failing tests.** `config/sensitivity_test.go`: `TestLoadSensitivityRules` parses a YAML document with two rules; `TestRuleWithNoClausesIsRejected` asserts a rule specifying neither a scope nor a metadata key fails to load with an error naming the rule, because it would mark an entire ledger sensitive; `TestMalformedYAMLNamesTheFile`.
- [ ] **Step 2: Run and watch them fail.**
- [ ] **Step 3: Implement `config.LoadSensitivityRules`** over `go.yaml.in/yaml/v3`. Promote the module to direct with `go mod tidy` and confirm it is the only `go.mod` change.
- [ ] **Step 4: Run and watch them pass.**
- [ ] **Step 5: Write the failing matcher tests** in `internal/interceptor/rules_test.go`: a scope-only rule matches any metadata; a metadata-only rule matches any scope; a rule with both requires both; **first match wins in slice order**; no rule matches → `("", false)`; a metadata value of a non-string type does not match a string rule (Mem0 metadata is `map[string]any`).
- [ ] **Step 6: Run and watch them fail**, then implement `RuleSet.Match` and run again to pass.
- [ ] **Step 7: Commit** — `feat(config): declarative sensitivity rules`.

---

## Task 3: The interceptor applies rules at write time

**Files:**
- Modify: `internal/interceptor/library/mem0.go` (`New` gains `opts ...Option`; the write path applies the rule set)
- Test: `internal/interceptor/library/sensitivity_test.go`
- Create: `internal/interceptor/library/options.go`

**Interfaces:**
- Consumes: `interceptor.RuleSet` (Task 2), `library.Sensitive()` (Task 1).
- Produces: `library.Option`, `library.WithSensitivityRules(rs *interceptor.RuleSet) Option`; `New(mc *mem0.Client, aw *interceptor.AuditWriter, scope record.Scope, now func() time.Time, opts ...Option) *Mem0Interceptor`.

Two option families, on purpose: `Option` configures the interceptor at construction, `CallOption` configures one call. Marking is an **OR** between them — either marks content sensitive — and there is deliberately no way to un-mark against a matching rule, because the safe direction is the only one worth having.

- [ ] **Step 1: Write the failing tests.** `TestRuleMarksContentSensitive`: a rule matching the interceptor's scope marks an ordinary `Add`. `TestCallOptionAlsoMarksWhenNoRuleMatches`. `TestRuleAndOptionBothMark`. `TestNoRulesMeansNothingIsMarked`.
- [ ] **Step 2: Run and watch them fail.**
- [ ] **Step 3: Implement.** The rule set is consulted where each `record.Content` is built, before the record is written. Rules need the record's scope and the Mem0 metadata available at that point; for `Add` the metadata is what the caller sent, and for `Search` the per-result metadata is what Mem0 returned.
- [ ] **Step 4: Run and watch them pass.**
- [ ] **Step 5: Run the whole package** — `go test ./internal/interceptor/... -count=1` — because this changes a constructor every existing test calls.
- [ ] **Step 6: Commit** — `feat(interceptor): apply sensitivity rules when writing records`.

---

## Task 4: `internal/export` — the read path and the JSONL line

**Files:**
- Create: `internal/export/export.go`, `internal/export/line.go`
- Test: `internal/export/export_test.go`, `internal/export/line_test.go`

**Interfaces:**
- Consumes: `ledger.ListRecords(from, to time.Time) ([]record.Record, error)`, `store.Head()`, `store.SeqEntries()` (existing).
- Produces: `export.Reader` (the one-method interface `ListRecords`); `export.Request{From, To time.Time; Scope record.Scope; IncludeSensitive bool; CheckpointOut string; MaxSpan time.Duration; Phrase bool}`; `export.Line` (the JSONL shape); `export.Render(rec record.Record, includeSensitive bool) (Line, error)`; `export.Exporter` with `Export(ctx context.Context, req Request, out io.Writer) (Result, error)`; `export.Result{Records, Redacted int; Checkpoint *sign.Checkpoint}`.

`Reader` is an interface so the exporter can be tested against a fake ledger without SQLite, exactly as `reconcile.Reader` is.

**Export bounds on `At` — the event time — not `RecordedAt.** This matches `reconcile --since`, and it means a record reconstructed later appears in the window its events belong to. State it in `--help`; Task 8 tests it.

- [ ] **Step 1: Write the failing tests** in `line_test.go`: `TestLineCarriesEveryStructuredField` asserts the rendered line has the sequence, ids, `at`, `recorded_at`, event, tier, reason kind, scope, memory id, content hash and the chain fields, with `redacted` absent when content was shown and no `paraphrase` object at all. The `phrasing` field arrives in Task 5 and the `paraphrase` object in Task 10; assert those there, not here. `TestLineOrderIsStable`: rendering the same record twice is byte-identical.
- [ ] **Step 2: Run and watch them fail.**
- [ ] **Step 3: Implement `Render` and `Line`.** Use a fixed struct with `json` tags rather than a `map`, so field order is a property of the type and cannot drift.
- [ ] **Step 4: Run and watch them pass.**
- [ ] **Step 5: Write the failing tests** in `export_test.go`: `TestExportStreamsInRange` (only records whose `At` falls in range appear), `TestEmptyRangeIsSuccess` (zero records, nil error, `Result.Records == 0`), `TestScopeFlagsNarrowTheRange`, `TestMaxSpanIsEnforced` (a span over the cap returns an error naming `--max-span`).
- [ ] **Step 6: Run and watch them fail**, then implement `Exporter.Export` and run again to pass.
- [ ] **Step 7: Add `TestRecordsWithoutContentRenderWithoutIt`** (Review Focus 1): a record with no content produces a line with no `content` field and no `redacted` field — not an empty string, and not a redaction claim.
- [ ] **Step 8: Add `TestUnsensitiveLegacyRecordsRender`** (Review Focus 3): a record whose `Sensitive` is false renders its text.
- [ ] **Step 9: Commit** — `feat(export): read a range and render JSONL`.

---

## Task 5: Tier phrasing, total over the claim vocabulary

**Files:**
- Create: `internal/export/phrase.go`
- Test: `internal/export/phrase_test.go`

**Interfaces:**
- Produces: `export.Phrase(rec record.Record) (string, bool)` — the sentence and whether one exists.

Deliberately in `internal/export` for now (spec D6). `explain` in Phase 8 needs the same text; the extraction is mechanical when a second caller exists, and adding a package the parent spec's tree does not have, for a caller that does not exist, is not.

- [ ] **Step 1: Write the failing test** `TestPhraseIsTotalOverConstructibleRecords`: enumerate every (event, reason kind) pair the `record` package can construct — including the pairs the reconciler's `Rules()` registry yields — and assert `Phrase` returns `("", false)` for none of them. The table is the spec's §7 table; the enumeration is what stops a new claim kind shipping with no wording.
- [ ] **Step 2: Run and watch it fail** with the unphrased pairs named in the failure message.
- [ ] **Step 3: Implement `Phrase`** as a switch over (event, kind) returning the spec's §7 sentences.
- [ ] **Step 4: Run and watch it pass.**
- [ ] **Step 5: Add `TestRenderedLineCarriesTheStructuredFieldsBesideThePhrasing`** (subordination, spec §7): the line has both, and the phrasing is never the only representation of the claim.
- [ ] **Step 6: Commit** — `feat(export): phrase every claim in words`.

---

## Task 6: Redaction at render, and the round trip

**Files:**
- Modify: `internal/export/line.go`, `internal/export/export.go`
- Test: `internal/export/redaction_test.go`

**Interfaces:**
- Consumes: `export.Line`, `export.Render`, `export.Export` (Tasks 4–5).
- Produces: `redacted` populated as `"sensitive"` when the stored flag hid the text; `Result.Redacted` counting them.

**Corrected after Task 3, before this task ran.** This first said `"rule:<name>"` when a rule matched. That is not achievable: `record.Content` is `{Text, Sensitive}` and both are mixed into the record hash, so persisting which rule fired would change the digest of every record ever written. Task 3 threaded the matched name through its own internal marking struct and nothing persists it — **do not try to persist it**. The flag is the evidentiary fact; which rule produced it is declarative configuration, reproducible from the rules file.

- [ ] **Step 1: Write the failing test** `TestRedactionRoundTripIsHashInvariant` — spec §10's required test. Build a ledger with one sensitive and one not. Export the range twice, with and without `IncludeSensitive`. Assert: every `hash`, `prev_hash` and `signature` is byte-identical across both outputs and equal to the stored record's; the sensitive record's text appears in exactly one of them; the redacted line's `redacted` field is present and non-empty.
- [ ] **Step 2: Run and watch it fail** (text still present under the default).
- [ ] **Step 3: Implement.** Rendering takes the stored `Sensitive` flag and clears the text; it never touches the hash. The `redacted` string is `"sensitive"` — not `"rule:<name>"`, which Task 3 established is unachievable without a hash change (see this task's Interfaces note).
- [ ] **Step 4: Run and watch it pass.**
- [ ] **Step 5: Add `TestRedactedLineNeverClaimsToHideAbsentContent`** (Review Focus 1 from the redaction side): a record with no content and a true flag renders no `redacted` claim.
- [ ] **Step 6: Commit** — `feat(export): redact content at render without touching hashes`.

---

## Task 7: The checkpoint travels with the export

**Files:**
- Modify: `internal/export/export.go`
- Test: `internal/export/checkpoint_test.go`

**Interfaces:**
- Consumes: `ledger.Checkpoint(sg *sign.Signer, now time.Time) (sign.Checkpoint, error)` (existing), and whatever `verify --write-checkpoint` already uses to serialise a checkpoint to a file.
- Produces: `Request.CheckpointOut` honoured; `Result.Checkpoint` set.

The checkpoint attests the **ledger head**, not the last record in the range (spec §9). That is what makes it useful: the truncation a hash chain cannot see is a shortened tail.

- [ ] **Step 1: Read how `verify --write-checkpoint` serialises a checkpoint**, and reuse that exact format and helper. Do not invent a second shape for the same artifact.
- [ ] **Step 2: Write the failing test** `TestExportedCheckpointIsAcceptedByVerify` — export with `CheckpointOut`, then run the exported file through `verify --checkpoint` and assert success. `TestCheckpointDetectsATruncatedTail` — truncate the ledger, then verify against the same checkpoint and assert failure. Use the `cmd/notary` test harness that already drives `verify`.
- [ ] **Step 3: Run and watch them fail.**
- [ ] **Step 4: Implement.**
- [ ] **Step 5: Run and watch them pass.**
- [ ] **Step 6: Commit** — `feat(export): emit a verifiable head checkpoint`.

---

## Task 8: `notary export`

**Files:**
- Create: `cmd/notary/export.go`, `cmd/notary/export_test.go`
- Modify: `cmd/notary/root.go` (register the command)

**Interfaces:**
- Consumes: `export.Exporter`, `export.Request`, `config` (Task 2), `ledger.New`, `store.Open`, `sign.NewSigner`.
**Produces:** the `export` subcommand with `--from`, `--to`, `--include-sensitive`, `--checkpoint-out`, `--max-span`, `--user-id`, `--agent-id`, `--app-id`, `--run-id`.

**`--phrase` is deliberately not here** — Task 10 adds it, which is what makes Tasks 9–10 revertable as a unit (see Sequencing). There is also **no `--sensitivity-rules` flag**: rules mark content at *write* time, no CLI command writes records, so the rules are an application-facing path configured by `NOTARY_SENSITIVITY_RULES` (Task 2). Spec §4.3's "or `--sensitivity-rules`" has no command to live on and is dropped, recorded in Task 11's documents.

- [ ] **Step 1: Write the failing tests**, mirroring `cmd/notary/reconcile_test.go`'s harness: `TestExportRequiresFrom`; `TestExportRejectsMalformedFromNamingRFC3339`; `TestExportRejectsToBeforeFrom`; `TestExportEmptyRangeSucceedsAndSaysSo` (exit 0, stderr says no records — Review Focus 2); `TestExportWritesJSONLToStdout`; `TestExportScopeFlagsNarrowTheRange`; `TestExportHelpListsItsFlags`.
- [ ] **Step 2: Run and watch them fail.**
- [ ] **Step 3: Implement the command.** `--from` required; `--to` defaults to now; `--max-span` defaults to 366 days; records stream to `out` as they are read rather than being collected first.
- [ ] **Step 4: Run and watch them pass.**
- [ ] **Step 5: Add `TestExportBoundsOnAtNotRecordedAt`** (Review Focus 4): append an `add_requested` with `At` in January and `RecordedAt` in October, export a January window, and assert the record appears. This is the test that fixes the semantics, and the one a future reader will look for.
- [ ] **Step 6: Commit** — `feat(cli): add the export command`.

---

## Task 9: `internal/phrase` — the provider-agnostic client

**Files:**
- Create: `internal/phrase/client.go`, `internal/phrase/paraphrase.go`, `internal/phrase/testdata/`, `internal/phrase/FIXTURES.md`
- Test: `internal/phrase/client_test.go`
- Modify: `config/config.go` (`EnvPhraseAPIKey`, `EnvPhraseModel`, `EnvPhraseBaseURL`), `config/config_test.go`, `docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md` (D10 and §11, per spec §11)

**Interfaces:**
- Produces: `phrase.NewClient(baseURL, apiKey, model string, hc *http.Client) *Client`; `(*Client).Paraphrase(ctx context.Context, records []record.Record) (Paraphrase, error)`; `phrase.Paraphrase{Text, Model string; At time.Time}`; `config.EnvPhraseAPIKey = "NOTARY_PHRASE_API_KEY"`, `config.EnvPhraseModel = "NOTARY_PHRASE_MODEL"`, `config.EnvPhraseBaseURL = "NOTARY_PHRASE_BASE_URL"`.

The request is `POST {baseURL}/chat/completions` carrying **only** `model` and `messages` (spec D2). Not a stylistic choice: Anthropic's compatible layer silently ignores unknown fields, so a richer request would appear to work while dropping intent, and Gemini's 404s `/responses` outright. The prompt carries event, tier, reason kind, scope, memory id and content hash — **never content text** (spec §8), so the pass cannot leak what redaction exists to hide.

- [ ] **Step 1: Write the failing tests** against an httptest stub, no network: `TestRequestPathIsChatCompletions`; `TestRequestBodyCarriesOnlyModelAndMessages` (decode the body and assert the top-level key set is exactly those two — this is the test that keeps D2 true); `TestPromptNeverContainsContentText` (seed a record with distinctive text and assert it appears nowhere in the request); `TestSingleElementChoicesIsAccepted`; `TestUnknownResponseFieldsAreIgnored`; `TestEmptyChoicesIsAnError`; `TestNonJSONBodyIsAnError`; `TestNon2xxIsAnErrorNamingTheStatus`.
- [ ] **Step 2: Run and watch them fail.**
- [ ] **Step 3: Implement** `Client` and `Paraphrase`, following `internal/mem0/client.go`'s shape.
- [ ] **Step 4: Run and watch them pass.**
- [ ] **Step 5: Record fixtures.** Capture a real response from the provider in use into `internal/phrase/testdata/` and document its provenance in `FIXTURES.md` exactly as `internal/mem0` does. **This needs a provider API key that does not exist in the repository yet** — stop and ask the operator for `NOTARY_PHRASE_API_KEY` before this step, and show them where it goes.
- [ ] **Step 6: Amend the parent spec** — D10 and §11 — per spec §11 deltas 1 and 2, so the parent no longer claims Anthropic is the approved dependency when the code has no SDK at all.
- [ ] **Step 7: Commit** — `feat(phrase): a provider-agnostic compatible client`.

---

## Task 10: `--phrase`, and the isolation that keeps it honest

**Files:**
- Modify: `internal/export/export.go`, `internal/export/line.go`, `cmd/notary/export.go`
- Test: `internal/export/paraphrase_test.go`, `internal/export/imports_test.go`, `cmd/notary/export_test.go`

**Interfaces:**
- Consumes: `phrase.Client` (Task 9).
- Produces: `export.Paraphraser` — the one-method interface `Paraphrase(ctx, records []record.Record) (phrase.Paraphrase, error)` — so `internal/export` can be tested with a fake and the CLI supplies the real client only under `--phrase`; `Line.Paraphrase *phrase.Paraphrase`; `Result.ParaphraseFailed bool`.

- [ ] **Step 1: Write the failing tests.** `TestPhraseIsOffByDefault` (no client, no request made — assert the fake was never called). `TestParaphraseAppearsBesideTheRecordNotInsteadOfIt`. `TestParaphraseFailureDegradesAndDoesNotFailTheExport`: for each of a network error, a non-2xx, an empty `choices`, and a non-JSON body, the record is intact, a note is present, and `Export` returns no error (Review Focus 5). `TestParaphraseFailureStillExitsZero` at the CLI level.
- [ ] **Step 2: Run and watch them fail.**
- [ ] **Step 3: Implement** the wiring, the `paraphrase` object and the failure note.
- [ ] **Step 4: Run and watch them pass.**
- [ ] **Step 5: Write `TestNoDecisionPackageImportsPhrase`** in `imports_test.go`: walk the module's import graph with `go list -deps` (or parse the files) and assert only `internal/export` and `cmd/notary` reach `internal/phrase`. Spec D7 claims the compiler enforces "generated text is never an input"; this test is what makes that claim checkable.
- [ ] **Step 6: Run it and watch it pass**, then confirm it fails if the import is added artificially.
- [ ] **Step 7: Commit** — `feat(export): opt-in paraphrase that degrades rather than failing`.

---

## Task 11: The documented surface

**Files:**
- Modify: `README.md`, `docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md` (the phase table), `docs/superpowers/specs/2026-10-02-notary-v1-export-design.md` (status line)

**Interfaces:** none — documentation only.

- [ ] **Step 1: Update the phase table** in the parent spec: Phase 6 `⬜ not started` → `✅ complete`.
- [ ] **Step 2: Document `export` in the README** beside `verify`, including the redaction default, the `--include-sensitive` escape hatch, the checkpoint integration, and that `--phrase` needs a provider key from the environment.
- [ ] **Step 3: Set the Phase 6 spec's status** to reflect that it is implemented.
- [ ] **Step 4: Run the full suite and the formatter** — `gofmt -l .`, `go vet ./...`, `go test -p 1 ./... -count=1` — and commit.
- [ ] **Step 5: Commit** — `docs(export): record Phase 6 as shipped`.

---

## Risk, and where it is concentrated

- **Tasks 6 and 8 carry the most risk.** Task 6 is where the phase's central promise is kept or broken: if redaction ever touches a hash, or if the round-trip test is written loosely enough to pass while a hash changes, the export silently becomes evidence-destroying rather than evidence-hiding. Task 8 fixes semantics — which timestamp a range bounds on — that no later task can correct without changing behaviour.
- **Task 9 is the only task with an external service, a cost, and a failure mode that exists nowhere else in the project.** It is also the only task that cannot be finished without an operator-supplied secret.
- **Task 3 touches a constructor every existing interceptor test calls.** It is expected to be a tidy change; if it is not, the option design in Tasks 1–3 is wrong rather than the tests.

## Sequencing, and why the LLM piece reverts cleanly

Tasks 1–8 deliver a complete, useful `export` with no LLM anywhere. Tasks 9–10 add the paraphrase and are **self-contained**: Task 9 creates two new files and three config constants, and Task 10 changes three existing files at exactly one seam — the `export.Paraphraser` interface. The interface is unscoped, with no-op forever; nothing depends on a paraphrase existing.

So reverting Tasks 9–10 is `git revert` of two commits: the `--phrase` flag disappears, no `paraphrase` object appears, no `internal/phrase` package remains, and every export still round-trips its hashes. That is the property worth designing for, because this is the piece most likely to be rejected after the fact — by a security review, by a provider outage, or by the operator deciding an LLM near a compliance artifact is not worth it after all.

## Spec coverage

| Spec section | Task |
|---|---|
| §3 D1 (no SDK, providers pluggable) | 9 |
| §3 D2 (minimal request, `/chat/completions`) | 9 (Steps 1, 6) |
| §3 D3 (two sensitivity input paths, write time) | 1, 2, 3 |
| §3 D4, D5 (redaction at render, hash invariant, stated) | 6 |
| §3 D6 (phrasing stays in export) | 5 |
| §3 D7 (`Paraphrase` unimportable by decision packages) | 10 |
| §3 D8 (JSONL + checkpoint in `verify`'s format) | 4, 7 |
| §3 D9 (LLM lands last, revertable) | 9, 10, and the sequencing note |
| §3 D10 (env-only provider config) | 9 |
| §4.1 (import graph) | 10 |
| §4.2 (the command and its flags) | 8 |
| §4.3 (the rules file) | 2 |
| §5 (both input paths, OR semantics) | 1, 2, 3 |
| §6 (redaction, stated, hash invariant) | 6 |
| §7 (phrasing totality and subordination) | 5 |
| §8 (`internal/phrase`, prompt, degradation, opt-in) | 9, 10 |
| §9 (flow, validation, bounding, exit codes, line shape, checkpoint) | 4, 7, 8 |
| §10 (testing strategy, all nine bullets) | 1–10 |
| §11 (deltas to the parent spec) | 9 (Step 6), 11 |
| §12 (out of scope) | nothing — deliberate |

## Type consistency

`interceptor.Rule` / `interceptor.RuleSet.Match` are defined in Task 2 and consumed by Task 3 only. `library.Option` / `library.CallOption` are distinct families defined in Tasks 1 and 3. `export.Reader`, `export.Request`, `export.Line`, `export.Render`, `export.Exporter`, `export.Result`, `export.Phrase` are defined across Tasks 4 and 5 and consumed by Tasks 6, 7, 8 and 10. `export.Paraphraser` is defined in Task 10 and implemented by both `phrase.Client` and the tests' fake. `phrase.NewClient` / `Paraphrase` are defined in Task 9 and consumed in Task 10. Config constants are named once, in the task that first needs them: `EnvSensitivityRules` (2), `EnvPhraseAPIKey`/`EnvPhraseModel`/`EnvPhraseBaseURL` (9).
