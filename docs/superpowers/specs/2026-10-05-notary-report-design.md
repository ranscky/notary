# Notary — `report` and `doctor`: Design (the first post-v1 phase)

**Status:** draft for review. The decisions in §3 were taken with the operator during brainstorming: five
answered during the dialogue (four by multiple choice, one chosen from three proposed shapes) and two proposed
in the design sections and approved as presented. The three questions §12 records were put to the operator,
who delegated them ("choose what works best") and they were decided on merit, with the reasoning kept there.
Per the brainstorming gate, no implementation starts until this spec is reviewed and a plan is written.

**Date:** 2026-10-05
**Depends on:** v1 complete — phases 0–10 of the architecture spec's table, merged and CI-green (`master` at
`6e52a73`, which also carries the licence).
**Phase table:** this is **Phase 11**, beyond the v1 table's ten rows. Adding it means a delta to that table,
not a reopening of v1: nothing here changes an existing command's contract.

---

## 1. What this is for, and what the artefact is

The architecture spec's §1 names the audience in one line: *"a compliance lead who must explain an agent's
memory decisions without reading code or pulling in an engineer."* Today that person's only route to the
ledger is a terminal — `notary explain` needs a record id they have to already know, and `notary export
--from --to | jq` is precisely the "pull in an engineer" the sentence rules out. The proxy phase solved
*recording* for applications that can't embed Notary; nothing yet solves *reading* for the human the whole
design is written for.

This phase adds two commands:

- **`notary report`** — renders the ledger as a small set of **static, self-contained HTML files**:
  a slice's index, one page per memory, one per record, plus the chain's state.
- **`notary doctor`** — validates the environment and says what to fix, and can generate the signing key
  that nothing in the repository can currently generate.

**The artefact is evidence, not an application.** It is a folder of files you open from disk, attach to a
ticket, or hand to an auditor; it needs no server, no network and no framework, and nothing in it executes
beyond the one small filter script. That shape is a deliberate choice over a localhost UI, and it removes a
class of risk rather than mitigating it: a `file://` page cannot be reached by another origin, so there is no
CSRF, no DNS-rebinding surface, and no browser-reachable endpoint. The process that writes the files exits,
so nothing that persists holds a credential.

## 2. What is already fixed, and what this must not disturb

- **§9's read-path table** fixes what each command is: *"`explain` is the single-subject view; `export` is
  the range view."* This phase adds a third row rather than a mode on either — forking `export` or `explain`
  into a second rendering with its own scope rules and assets would blur contracts the spec pins.
- **Redaction is presentation, never storage** (§9 of the architecture spec, and `internal/export`'s
  `redactedSensitive` at `line.go:91`). A generated report is a **durable second copy of the memories it
  describes**, in a directory its owner will email. It inherits the existing rule: sensitive text is
  withheld, the page says it was withheld, and `--include-sensitive` is an explicit opt-in.
- **D10** keeps `internal/phrase` imported **only** by `internal/export`, so the report can reach no language
  model. There is no `--phrase` here, and no paraphrase on any page.
- **The record is the source of truth; a render is a view and never touches a hash.** The report reads
  through `ledger` (`ledger.go:203,209,217`) and writes nothing.
- **`.clinerules`** forbids a new dependency without asking and forbids key material in the repository. Both
  bind §7 and §4 below.
- **Nothing in the repository generates a signing key.** `sign.NewSigner` only loads one (`sign.go:129`), and
  the design says why: *"a key is never generated silently."* §4's `--generate-key` has to earn that.

## 3. The decisions, with their costs

**Answered during brainstorming:**

1. **Static HTML files, no framework — not a localhost GUI.** *Cost:* no live querying, and the console shape
   (a server over a live ledger) is unavailable; adding it later is a new phase, not a flag.
2. **A multi-page site in a directory, not one self-contained document.** §6's pages, linked relatively, with
   `index.html` as the entry point. *Cost:* the folder has to travel as a whole — a single page copied out
   renders unstyled and its links dangle — and the one-file-to-attach form is deferred (§9).
3. **A little vanilla JavaScript, no framework.** Enough for a filter box; nothing else. *Cost:* the pages
   execute code when opened, so the artefact cannot honestly be described as inert — §7 says what it *can* be
   described as.
4. **Bounded by construction: a subject, a scope or a range — no `--all`.** *Cost:* producing "everything"
   takes an explicit wide range. That is the point: a full render of a production ledger would write
   thousands of pages holding memory text.
5. **Both halves of setup: a `doctor` command and a setup page.** *Cost:* two artefacts to keep in step; the
   page and the command render the same checks, which live in one place (§4) so there is one source.

**Proposed in the design sections and approved:**

6. **The setup page is not part of the evidence report.** `doctor --out DIR` writes `setup.html`; `report`
   never does. *Cost:* a reader who wants both runs two commands, in exchange for a shared report never
   carrying the operator's paths, config state and key status alongside the audit trail.
7. **A verification section, on by default, with `--no-verify`.** `verify` reads the ledger and the trusted
   public keys and needs no private key (`ledger.Verify`, `verify.go:74`), so the generator can check the
   chain and report what it found. *Cost:* this is the piece to cut first if the phase needs to shrink — and
   it carries the honest wrinkle in §9, that a chain's integrity is a whole-ledger property, so a report of
   one Tuesday states the whole chain's state.

**Found while writing the plan, and decided on merit:**

8. **The report's chain state is `verify`'s answer, which means extracting verify's break collection into
   one shared function.** `notary verify` reports more than the chain walk: it cross-checks the gap log
   (`gap.Read` → `st.SeqEntries` → `ledger.GapBreaks`) and checks the gap log's own integrity (`gap.Verify` →
   `ledger.GapIntegrityBreaks`), and that orchestration lives inline in `cmd/notary/verify.go`. A report built
   on `ledger.Verify` alone would therefore say "clean" for a ledger `verify` exits non-zero on — two answers,
   in one project, to *"is this intact?"*, and the report is the artefact a reader cannot cross-check. So the
   break collection moves into one function both callers use. **This refactors Phase 2's
   `cmd/notary/verify.go`, which `.clinerules` §5 says not to do "unless explicitly asked" — this decision
   records that explicit ask**, on the same terms as the operator's "Approach A" answer that authorised
   Phase 9's `library` refactor. *Cost:* one prior-phase file changes, and the extraction carries a
   differential test (the report's state must equal what `verify` reports for the same ledger) rather than
   trust. The alternative — narrowing the page to "the chain walk reported no breaks" — was rejected because
   it institutionalises the divergence instead of preventing it.

**Decided after the task review, on the operator's answer:**

9. **`doctor --trusted-keys-out PATH`, the flag that finishes the setup.** The task's own review found that
   `--generate-key` produced the private half while `verify`, `replay` and `report` all need the base64
   **public** half, so first-run setup still ended with "now find another tool" — the phase's stated purpose
   half met. §4 carries the shape and the reasoning; the load-bearing points are that it **requires
   `--generate-key`** (the public half cannot be derived from a loaded key, because `Signer` exposes only
   `KeyID()` and `Sign`), that it is `0644` because public material is not a credential, and that it — unlike
   `--key-out` — is **allowed** inside a checkout, because the guardrail covers secrets and signing keys and a
   public half is neither. *Cost:* one more flag, and an asymmetry between the two `-out` flags whose reason
   (`secret` vs `public`) has to be legible from the help text, since the codes differ for a reason rather
   than by accident.

## 4. `notary doctor`

```
notary doctor [--out DIR] [--generate-key] [--key-out PATH] [--trusted-keys-out PATH]
```

(`--verbose` is the root command's persistent flag, not this command's.)

Findings are computed once, in one place, and rendered by both the command's text output and `setup.html`.
Each finding carries a severity, one plain sentence saying what is wrong, and **the exact command that fixes
it** — a diagnosis without the fix is half a tool.

The checks, each of which is a fact the code can establish:

- `NOTARY_DB_PATH` resolves and the store opens (`store.Open`).
- `NOTARY_GAP_LOG_PATH` resolves and the gap log is **writable** — not merely present, since a gap log on a
  read-only volume fails exactly when it matters.
- `NOTARY_TRUSTED_KEYS_PATH` resolves and parses into a keyring (`sign.LoadTrustedKeys`).
- `NOTARY_SIGNING_KEY` is set and loads as a signer (`sign.NewSigner`), *when the command being diagnosed
  needs one* — reported as "not needed for the read paths" rather than as a failure when it is absent.
- `NOTARY_MEM0_BASE_URL` parses as an absolute `http`/`https` URL with a host — the same validation the proxy
  learned to do.
- `NOTARY_MEM0_API_KEY` presence, reported as required only for the commands that make a Mem0 call.
- `NOTARY_SENSITIVITY_RULES`, when set, parses (`config.LoadSensitivityRules`).
- The ledger's chain state — the same shared break collection `notary verify` uses (§3 decision 8), so a
  doctor run answers "is this intact?" with the same answer `verify` gives, never a narrower one.

`doctor` exits non-zero when any finding is an error, so it can gate a pipeline; warnings alone do not.

**`--generate-key`** writes a fresh ed25519 seed. It is explicit because silence is the safety property, and
it is the only thing in the repository that creates key material. Default behaviour prints the
`export NOTARY_SIGNING_KEY=…` line to **stdout** and writes no file — a caller can capture it however it
likes. `--key-out PATH` writes the seed to a file instead, `0600`, and **refuses a path inside the repository**
(the guardrail's rule, enforced rather than documented). The generated material is never printed as part of a
finding, never echoed to a log, and never appears in `setup.html`.

**`--trusted-keys-out PATH` completes the setup, and it is why this flag exists.** `verify`, `replay` and
`report` all read a trusted-keys file — the base64 **public** half — and nothing in the repository could
produce one: `sign.Signer` exposes `KeyID()` (a `base64(SHA-256(public))` fingerprint, `sign.go:264`) and
`Sign`, but no accessor for the public key itself, so the public half cannot be recovered from a *loaded* key
without either changing `sign` or re-implementing its parser. `--generate-key` is holding the seed, though, so
`ed25519.NewKeyFromSeed(seed).Public()` gives it directly. The flag therefore **requires `--generate-key`**,
and says so when it is missing — the tool will not pretend to derive what it cannot read. The file is written
as one base64 line, the format `sign.LoadTrustedKeys` reads (`internal/sign/keys.go:36-45`), at mode `0644`:
unlike the private half it is **not** a credential, it is meant to be readable by whatever verifies, and the
guardrail's "no secrets or signing keys in the repository" does not reach it. It may therefore live in a
checkout if the deployment wants configuration as code, though the help text says it is normally deployment
config and belongs outside one.

## 5. `notary report`

```
notary report --out DIR (--memory <mem0-id> | --from <RFC3339> [--to <RFC3339>])
              [--user-id|--agent-id|--app-id|--run-id] [--include-sensitive] [--no-verify] [--force]
```

- **Exactly one subject**, mirroring `explain`'s discipline (`internal/explain`'s both-empty rejection, and
  the CLI's explicit mutual exclusion): either `--memory <id>` or `--from` with `--to` defaulting to now. Both,
  or neither, is a refusal that names what was given and what to do.
- **The four scope flags narrow a range** exactly as they do for `export` and `replay`, through the one
  matcher (`export.ScopeMatches`), so the three range views compose identically. They are refused with
  `--memory`, where a single subject already identifies its scope.
- **`--out DIR` is required**, and it **refuses a non-empty directory** unless `--force` is given. A report
  generator that writes into whatever directory it is handed is a footgun; the refusal is the difference
  between "you asked for this folder" and "it merged into my documents folder".
- **`--include-sensitive`** matches `export` and `explain`: off by default, and it changes only what is
  printed, never a hash.
- **`--no-verify`** skips §6's chain-state page. It exists for a ledger large enough that a full walk is slow.
- **`report`'s help text names where the setup page lives** (`notary doctor --out`), which is the whole
  discoverability cost of §3 decision 6 and cheaper than giving `report` a second, non-evidence product.

## 6. The pages, and what each one claims

| File | Carries |
|---|---|
| `index.html` | The slice: the exact command that produced it, the instant, the memory and record counts, the chain state (below), a filter box, and a table of memories with their record counts **and a per-tier count breakdown** — each row linking to its page |
| `memory/<n>.html` | One memory's lifecycle: the same timeline the CLI's `explain --memory` renders — event, reason kind, tier, both instants, and content — with each line linking to its record |
| `record/<n>.html` | One record's story: the phrased sentence from `export.Phrase`, the event, the reason kind, the tier, the evidence payload, the subject (memory id, scope, content hash), and the chain fields — `prev_hash`, `hash`, `signature`, `signer_key_id` — with the long hex in a collapsed `<details>` |
| `verify.html` | The chain state in full: clean, or every break with the record id and the field that failed |
| `assets/report.css`, `assets/report.js` | §7 |
| `setup.html` | Only from `doctor --out`, never from `report`: §4's findings (§3 decision 6) |

**Page filenames are derived, never taken from an id.** A record id can carry `#`, `:`, `/` and non-ASCII —
`corr-search-1#1` and `add_resolved:stored_by_mem0:<event-id>` both exist in this codebase — so a filename
built by pasting an id is both non-portable (a colon is illegal on Windows) and a path-traversal risk if an id
ever contains `..`. Pages are therefore named from **a sequence number in `Seq` order — memories numbered by
their first record's `Seq`, records by their own — plus the first eight hex characters of
`record.ContentHash(id)`**, reusing the project's existing length-prefixed digest rather than inventing a
naming hash. The id itself is displayed on the page. A test feeds a hostile id (`..`, `/`, a control
character) and asserts nothing is written outside `--out`.

**Two claims about verification, and the line between them.**

- **Every page that shows a hash says the file verifies nothing by itself.** The reader gets the
  `notary verify` command to re-run. A generated file asserting *"verified"* would be the unbacked claim this
  project exists to refuse.
- **The chain state is `verify`'s answer, through the one shared break collection (§3 decision 8)**, so the
  page and `notary verify` cannot disagree. It is **attributed to the run that produced the file** — "as of
  <instant>, this tool's verification reported …" — with the re-run command beside it. It is the generator's
  finding, not a property of the file.

## 7. Assets, and the no-dependency rule

- `assets/report.css` and `assets/report.js` are embedded in the binary with **`go:embed`** (standard
  library) and written out beside the pages, linked relatively. **No new dependency**, per `.clinerules`.
- **No CDN, no fetched fonts, no remote anything.** The pages must be readable on a machine with no network,
  and a report that phoned a CDN when opened would leak the fact that it was opened, to whom. A test asserts
  no `http://` or `https://` URL appears in any generated file. Because one script does ship, the artefact
  cannot honestly be described as *inert*; what it can be described as is offline, self-contained and free of
  any request — which is the property the test pins.
- **The filter is progressive enhancement.** `assets/report.js` filters the index table in the DOM; with
  JavaScript disabled the table is complete and every link still works. Nothing else on any page depends on
  script — the drill-down is ordinary links.
- **All stored text is escaped by `html/template`, never by string concatenation.** Memory text is arbitrary
  content from an agent's interactions; a report is a document a browser renders. The template engine's
  auto-escaping is the control, and a test puts `<script>` and `</textarea>` in a memory's text and asserts
  they render as text. (This is the same failure class as the prose-injection bug the proxy phase fixed, in a
  medium where it matters more.)

## 8. What it must not do

- **No server, no live query.** That was the rejected console shape.
- **No `--all`.** The slice is the bound.
- **No paraphrase, and no import of `internal/phrase`** (D10).
- **No key.** The report reads and renders; `--no-verify` makes it need no configuration at all beyond a
  readable ledger. Verification needs the *public* keyring only, and the private key must never load here —
  the generator does not sign anything, so it has no reason to hold one.
- **No writes to the ledger or the gap log.** Both are opened read-only.
- **No claim it cannot back** — §6.

## 9. Limitations recorded, not hidden

- **The chain state is whole-ledger, not slice-scoped.** `ledger.Verify` walks the chain
  (`internal/ledger/verify.go:74`), because that is the only thing a hash chain admits: an intact prefix of a
  tampered chain is still tampered. A report of one memory therefore states the whole ledger's integrity, and
  the page says so rather than implying the slice was checked.
- **The pages need their siblings.** `assets/` and the relative links are why the folder travels as a whole;
  a single page copied out on its own renders unstyled and its links dangle. That is the cost of choosing
  static files over one self-contained document (§3 decision 2's deferred alternative).
- **The index is not paginated.** It is bounded by the slice, which is a deliberate choice (§3 decision 4);
  a range wide enough to produce a very long index is a range the reader asked for.
- **Rendering is not streaming.** A large slice builds in memory before it is written. The bound is what keeps
  that acceptable, and it is the bound that would have to change first if it stopped being true.

## 10. Testing

- **The report renders a fixture ledger**: the pages exist, the index links resolve to the pages they name,
  and the memory and record pages carry the same phrasing `export.Phrase` produces for the same records.
- **Redaction by default**: a sensitive record's text is absent from every generated file; present with
  `--include-sensitive`; and no page's hashes differ between the two runs.
- **A hostile id cannot escape `--out`** (§6).
- **Stored text cannot become markup** (§7) — `<script>` in a memory's text renders as text.
- **The output is offline**: no absolute URL to any host in any file (§7).
- **No key material anywhere in the output**, with the same canary discipline `export` and `replay` use.
- **The chain state reports both ways**: clean on an intact fixture, and naming the exact record and field on
  a tampered one — the tampered fixture `testdata/tamper/` already exists.
- **The chain state equals what `notary verify` reports for the same ledger** — a differential test over three
  fixtures: an intact one, one with an edited record, and one whose *gap log* is broken or unmatched. The
  third is the case `ledger.Verify` alone cannot see (§3 decision 8), so it is the one that proves the
  extraction was necessary rather than decorative.
- **`--out` refuses a non-empty directory** without `--force`, and writes nothing when it refuses.
- **The subject selector refuses both, and neither**, naming what was given.
- **`doctor`'s checks** are table-driven: one case per check, each asserting the finding's severity and that
  its message names the fix command.
- **`--generate-key` never writes into the repository** and never prints key material in a finding.

## 11. Sequencing

1. **The shared break collection** (§3 decision 8) — extract `cmd/notary/verify.go`'s orchestration (the chain
   walk, the gap cross-check, and the gap log's integrity) into one function, with `verify` refactored to call
   it and a differential test proving the answers are the same. **First**, because `doctor`'s chain-state check
   and the report's `verify.html` both need it, and because `verify` is a shipped command whose behaviour must
   be provably unchanged before anything depends on the extraction.
2. **`doctor`** — the checks and the command, plus `--generate-key` and its `setup.html`. It depends on task 1
   for the chain state and on nothing else, which keeps it a real deliverable if the phase stops early.
3. **`internal/report`** — the page renderer, the derived filenames, the assets, and the reading through
   `ledger`. Built against a fixture ledger so it needs no command.
4. **`notary report`** — the command, its flags, and the registration in `root.go`.
5. **Docs** — the architecture spec's §9 read-path table gains a row, §13's table gains row 11, §14's delta
   table gains `internal/report`; the README gains both commands; `.clinerules`' folder tree gains the package.

## 12. Questions that were open, and how they resolved

1. **Whether `doctor --out` should exist at all**, or whether the setup page belongs to `report --setup`.
   **Resolved: it stays on `doctor`.** The page is a rendering of `doctor`'s findings, so the command that
   produces the findings produces its rendering — one concept, one owner. `report`'s identity is "a shareable
   evidence artefact", and a config page is neither evidence nor shareable, so a `--setup` mode would muddy
   the one thing `report` has to be crisp about. The discoverability cost is paid by §5's help-text pointer.
2. **Whether the record page should carry the signature bytes.** **Resolved: yes, collapsed beside the
   hashes.** The report may be the only artefact that survives a deployment, and a record representation that
   omits the one field a verifier actually checks is a partial one. It is public data, it costs a collapsed
   block, and §6's "this file verifies nothing by itself" note already stops a reader from thinking they can
   check it by eye.
3. **Whether the index's "highest tier" column was the right summary.** **Resolved: the column was a
   category error and is replaced by a per-tier count breakdown.** The architecture spec's §3 is explicit that
   a tier *"is not a confidence score and not a severity"* — so ranking them into a "highest" was wrong on the
   project's own terms, and whichever end one picked would bias the view. Counts per tier assert nothing the
   record does not: a memory with three `Observed` claims and one `Reconstructed` one says exactly that.
4. **Whether the record page could carry the evidence payload at all** — found while implementing the pages,
   and it is a contradiction inside this document rather than a new question. §7's record row requires "the
   evidence payload"; §9 and the plan require every page to render from `export.Line`, "never from a second
   reading of the record". `Line` carries `ReasonKind` and nothing else of the reason
   (`internal/export/line.go:35`), so the two clauses cannot both hold — the payload is reachable only through
   `record.Reason`'s accessors (`Reason.Observed()`, `Reason.Reconstructed()`, `Reason.InternalNote()`). Those
   payloads are the typed Mem0 protocol structures (`mem0.AddPayload`, `mem0.EventStatusResponse`,
   `mem0.SearchPerformedPayload`) — but this item's first draft claimed those are "event ids, statuses and
   search parameters, **not memory text**, so `--include-sensitive` is not implicated", and **that claim is
   false.** The implementation's review traced the reconcile paths and demonstrated it: `buildKeptObserved`
   marshals the *whole* `mem0.Memory` listing entry (`internal/reconcile/kept.go:182-184`), whose `Memory
   string` field carries the memory text (`internal/mem0/types.go:47`), and `addResolvedReason` marshals the
   whole `mem0.EventStatusResponse`, whose payload and results are upstream `map[string]any`
   (`internal/reconcile/adds.go:196-205`). A probe over one default report (`IncludeSensitive: false`) holding
   a sensitive interceptor record and a reconcile-shaped one showed the add page withholding the text, the
   index promising that "a record marked sensitive prints no text", and **the reconcile record's page printing
   that same text verbatim inside its evidence envelope.** Memory text can reach the artefact with the
   sensitivity flag off, on the ordinary path for reconciled records rather than a corner case. **Resolved: §7 wins, and the rule it appeared to break was never
   about this field.** The "render from `Line`" rule exists so that "the report and an exported line cannot
   describe a record differently"; for a field the line does not describe, that divergence is impossible, and
   the rule's purpose is served by reading the payload from the one place it exists. Refusing it would leave
   the evidence artefact without the *why* — the same partial-representation defect §12.2 rejected for the
   signature. **What renders is the reason's own canonical encoding — `Reason.Encode()`, verbatim, collapsed,
   and absent rather than empty when there is no evidence — and implementation refined this in a way worth
   recording.** The payload first reached the page through `ObservedEvidence.Payload()`, and it lands in
   `Encode()`'s envelope as raw JSON (`observedEnvelope.Payload` is a `json.RawMessage`,
   `internal/record/reason.go:454`), so the bytes the hash covers are the bytes on the page, with no
   pretty-printing step to falsify them. It is also **complete where `Payload()` was not**: `Encode()` is the
   whole self-describing reason — version tag, kind, tier, and the payload for that tier — so an `Observed`
   reason shows its source *and* its payload, a `Reconstructed` one shows its basis, rule, rule version and
   confidence, and an `Internal` one shows its note. `Payload()` is the *only* exported accessor on any of the
   three evidence types — there is no `Source()`, no `Basis()`, no `Note()` — so the narrower rendering would
   have shown nothing at all for two of the three kinds, which is the same partial-representation defect
   §12.2 rejected. Reading the canonical encoding needs no new API and no change to `internal/record`: the
   package exposed `Payload()` for `reconcile`'s single consumer (`internal/reconcile/worklist.go:291`), and
   that is not a licence to widen it here. **Cost:** the envelope is a storage-format surface, so the artefact
   shows a reader the wire shape (`notary/reason/v1`). That is the same trade §12.2 makes by publishing
   `signature`, and it is reversible — a later phase wanting decoded fields should add accessors to
   `internal/record` and record the ask.
5. **What to do about memory text arriving through the evidence block.** Put to the operator with the probe
   above, and **resolved: disclose it in the artefact, and fix it at the source in its own phase.** The
   evidence block's `<summary>` states plainly that it is the stored reason verbatim and is **not** filtered by
   `--include-sensitive`; the index's promise stops implying that the whole artefact is filtered and says what
   is actually true — a record marked sensitive prints no *content*, while a record's evidence may carry text
   the sensitivity rules never marked, because reconcile's observed payloads are upstream objects rather than
   curated fields. *Why not gate the block behind the flag:* that would withhold the *why* for **every**
   record to conceal it for some, which is the completeness §12.4 was built on — a report that withholds its
   own evidence is less useful to the reader it exists for, and the flag's name would then be doing work its
   mechanism cannot back. *Why not fix the payloads now:* the real repair is to narrow what reconcile records —
   ids, scores and hashes rather than the whole `mem0.Memory` — and that changes what records **hash**, so it
   bears on the verification of already-written ledgers and belongs to its own phase with its own spec. It is
   recorded here and in the phase ledger as a named open item, not as a thing quietly accepted: **a future
   phase should narrow reconcile's observed payloads, and this artefact's disclosure is what makes waiting
   honest rather than silent.**
