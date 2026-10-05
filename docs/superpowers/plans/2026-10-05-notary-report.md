# Notary — `report` and `doctor` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `notary report`, which renders a slice of the ledger as a set of static, self-contained HTML files for the compliance lead the design names, and `notary doctor`, which validates the environment and can generate the signing key nothing in the repository can currently generate.

**Architecture:** One new renderer package (`internal/report`) reading through `ledger` and rendering from `export.Line`, so a report page and an exported line always describe a record the same way; one new check engine (`internal/doctor`) whose findings both the command and the setup page render; and one extraction, ahead of everything, so that "is this ledger intact?" has a single definition instead of the report and `verify` answering it differently.

**Tech Stack:** Go 1.25, Cobra, `html/template` and `go:embed` (standard library), `modernc.org/sqlite`, testify.

**Spec:** `docs/superpowers/specs/2026-10-05-notary-report-design.md`

## Global Constraints

- Go 1.25; **no new dependencies and no change to `go.mod`**. `html/template`, `go:embed` and `crypto/ed25519` are stdlib.
- Never panic in library code; wrap errors `fmt.Errorf("...: %w", err)`; exported names get doc comments; no global mutable state.
- The record is the source of truth; a render is a view and **never touches a hash**.
- **No key material in the repository, and none in any output.** Never log or render key material; the report and `setup.html` must contain none.
- **Redaction is presentation** (`internal/export`'s `redactedSensitive`, `line.go:91`): a sensitive record's text is withheld by default, the page says it was withheld, `--include-sensitive` is the opt-in, and **no hash differs between the two runs**.
- **No import of `internal/phrase`** (D10). No paraphrase anywhere in this phase.
- **No writes** to the ledger or the gap log. Every read path here is read-only.
- **`notary verify`'s observable behaviour must not change** in Task 1 — its existing tests are the guard, and its printed output is what a differential test compares against.
- `gofmt -l .` empty and `go vet ./...` clean before each commit; the whole suite runs `-p 1` (`go test -p 1 ./... -count=1`); `internal/ledger` fails spuriously in parallel.
- **`cmd/notary/root_test.go` pins the subcommand list** (`rootSubcommands`, both directions). Any task that registers a command must extend that list, and the test failing first is the expected red.

## Review Focus

Inputs and conditions the spec implies but no task's own tests necessarily cover. Each gets its test in the task named.

1. **A gap-log-only failure.** A ledger whose *records* are all intact but whose gap log is corrupt, or holds an entry matching no record, is the one case `ledger.Verify` alone cannot see — and the case that makes Task 1's extraction necessary rather than decorative. The report and `verify` must agree on it. (Task 1, Task 4)
2. **A hostile id must not escape `--out`.** Record ids in this codebase carry `#`, `:` and `/` (`corr-search-1#1`, `add_resolved:stored_by_mem0:<event-id>`), so a filename built from an id is a path-traversal risk. (Task 3)
3. **Stored text must not become markup.** Memory text is arbitrary agent-supplied content rendered into a browser document. `html/template`'s auto-escaping is the control; a page built by concatenation would not be. (Task 3)
4. **The output must be offline.** No absolute URL to any host in any generated file, so a report cannot phone home when opened. (Task 4)
5. **`--no-verify` must not render as "clean".** A skipped verification and a passing one are different facts, and a zero-length break list cannot distinguish them. (Task 5)

---

### Task 1: The shared break collection

**Files:**
- Modify: `internal/ledger/gap.go` (or a new `internal/ledger/breaks.go` beside it)
- Test: `internal/ledger/breaks_test.go`
- Modify: `cmd/notary/verify.go`
- Test: `cmd/notary/verify_test.go` (add the differential test)

**Interfaces:**
- Produces: `func CollectBreaks(l *Ledger, st store.Store, gapPath string, v *sign.Verifier) ([]Break, error)`
- The README of the design's §3 decision 8: **this is the explicit ask** that `.clinerules` §5 requires for refactoring prior-phase code. It is recorded in the spec; treat the spec as the authorisation.

**Notes the implementer needs:** the orchestration to move is `runVerify`'s, `cmd/notary/verify.go:140-180`: the chain walk (`l.Verify(v)`), the gap cross-check (`gap.Read` → `st.SeqEntries` → `ledger.GapBreaks`), and the gap log's own integrity (`gap.Verify` → `ledger.GapIntegrityBreaks`) — **in that order**, because `verify` prints them in it. `Ledger` deliberately does not expose its store, which is why the function takes both; say so in its doc comment. `CollectBreaks` must not print anything and must not decide policy: it returns breaks, and each caller decides what to do with them.

- [ ] **Step 1: Write the failing test**

In `internal/ledger/breaks_test.go`, over three fixtures built the way that package's own tests build them:

- `TestCollectBreaksIsCleanOnAnIntactLedger` — no breaks, no error.
- `TestCollectBreaksReportsAnEditedRecord` — one break whose `RecordID` and `Field` name the edited record.
- **`TestCollectBreaksReportsAGapEntryMatchingNoRecord`** — **the load-bearing one (Review Focus 1)**: a ledger whose records are all intact and whose gap log holds an entry naming a correlation id no record accounts for. It must return a break, and `l.Verify(v)` alone must return none — assert both, so the test fails if the extraction ever narrows to the chain walk.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/ledger/ -run TestCollectBreaks -count=1`
Expected: FAIL — `undefined: CollectBreaks`.

- [ ] **Step 3: Implement `CollectBreaks`**

Move the three stages out of `runVerify`, preserving their order, their error wrapping, and the comments that explain each (`verify_test.go` documents why the gap checks exist; keep that reasoning with the code that does them).

- [ ] **Step 4: Refactor `runVerify` to call it**

`runVerify` keeps everything else — the flags, the checkpoint handling, the printing, the exit code — and loses only the three stages. Its printed output must be byte-identical; the existing `cmd/notary` tests assert it.

- [ ] **Step 5: Add the differential test**

In `cmd/notary/verify_test.go`: for each of the three fixtures above, **`CollectBreaks`'s answer and the command's printed output must agree** — the break count, the record ids and the fields, compared as *identities* rather than as formatted text, so a cosmetic change to `verify`'s wording does not break it. That is what proves the extraction changed nothing; the report's own rendering of the same state is asserted in Task 4, and together they are Review Focus 1's guard.

*(The controller corrected this step during preflight: it originally said "the report's chain state", which cannot exist until Task 3. The differential is between the extracted function and the command it came from.)*

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/ledger/ ./cmd/notary/ -count=1`
Expected: PASS, with every pre-existing `verify` test unchanged.

- [ ] **Step 7: Commit**

```bash
git add internal/ledger/ cmd/notary/
git commit -m "refactor(ledger): one definition of whether the ledger is intact"
```

### Task 2: `notary doctor`

**Files:**
- Create: `internal/doctor/doctor.go`, `internal/doctor/setup.html` (the page template, `go:embed`-ed)
- Test: `internal/doctor/doctor_test.go`
- Create: `cmd/notary/doctor.go`
- Test: `cmd/notary/doctor_test.go`
- Modify: `cmd/notary/root.go` (one line), `cmd/notary/root_test.go` (the list)

**Interfaces:**
- Produces:
  - `type Severity int` with `OK`, `Warn`, `Err`; `type Finding struct { Check string; Severity Severity; Message string; Fix string }`
  - `func Diagnose(cfg *config.Config, env map[string]string, now time.Time) []Finding` — takes an environment map rather than reading `os.Environ`, so it is testable the way `config.LoadFrom` already is.
  - `func RenderSetup(findings []Finding, w io.Writer) error`
- Consumes: `config.Config`, `store.Open`, `gap.Open`, `sign.NewSigner`, `sign.LoadTrustedKeys`, `config.LoadSensitivityRules`, `ledger.CollectBreaks` (Task 1).

**Flags:** `--out DIR` (writes `setup.html`), `--generate-key`, `--key-out PATH`, `--trusted-keys-out PATH`.

**Notes the implementer needs:** the checks are §4's list, each a fact the code can establish — the paths resolve, the gap log is *writable* (not merely present; open it for append), the trusted keys parse, the signing key loads **when the command being diagnosed needs one** (report "not needed for the read paths" rather than a failure when absent), the Mem0 base URL is an absolute `http`/`https` URL with a host (`cmd/notary/proxy.go`'s `parseUpstream` is the precedent — reuse its validation, do not write a second one), the Mem0 key's presence, the rules file parses when set, and the chain state via `CollectBreaks`. `doctor` exits non-zero when any finding is `Err`; warnings alone do not. **Nothing generates a key today and that is the first-run blocker**, so `--generate-key` is the whole point of the command's second half: it prints `export NOTARY_SIGNING_KEY=…` to **stdout** and writes no file by default, `--key-out PATH` writes `0600` and **refuses a path inside the repository**. Key material never appears in a finding and never in `setup.html`. **`--trusted-keys-out PATH` is spec §3 decision 9 and §4's last paragraph**: it writes the base64 *public* half, `0644`, requires `--generate-key` (the public key cannot be derived from a loaded `Signer`, which exposes only `KeyID()` and `Sign`), and — unlike `--key-out` — is **allowed** inside a checkout, because a public half is not a credential and the guardrail covers secrets and signing keys, not public material.

- [ ] **Step 1: Write the failing tests**

`internal/doctor/doctor_test.go`, table-driven, one case per check, each asserting the finding's severity **and that its `Fix` names a runnable command**:

- `TestDiagnoseReportsEachCheck` — a healthy temp-dir environment produces no `Err`.
- `TestDiagnoseReportsAMissingLedger`, `…AnUnwritableGapLog`, `…AMalformedTrustedKeysFile`, `…AnUnusableUpstreamURL`, `…AnUnparseableRulesFile` — each asserts `Err` and the fix.
- `TestDiagnoseDoesNotFailOnAMissingSigningKeyForReadPaths` — the finding is informational, not `Err`.
- `TestDiagnoseReportsABrokenChain` — reuses the tamper fixture.

`cmd/notary/doctor_test.go`:

- `TestDoctorCmdExitsNonZeroOnAnError` and `TestDoctorCmdExitsZeroOnWarningsOnly`.
- `TestDoctorCmdGenerateKeyPrintsAnEnvLineAndWritesNothing` — asserts the seeded line round-trips through `sign.NewSigner`, and that no file appeared.
- **`TestDoctorCmdGenerateKeyRefusesAPathInsideTheRepository`**.
- **`TestDoctorCmdTrustedKeysOutWritesThePairAndRequiresGenerateKey`** — with `--generate-key`, `--trusted-keys-out` writes a file that `sign.LoadTrustedKeys` parses into a keyring whose key id is the one `sign.NewSigner` reports for the generated seed; without `--generate-key` the flag is refused, naming it. That round trip through the real loader is what makes it a *pair* rather than two unrelated strings.
- `TestDoctorCmdSetupPageContainsNoKeyMaterial` — the canary.
- Registration: `root_test.go`'s list gains `doctor`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/doctor/ ./cmd/notary/ -run 'Doctor|Diagnose' -count=1`
Expected: FAIL — no such package / `newDoctorCmd` undefined.

- [ ] **Step 3: Implement the checks, the command, and `setup.html`**

`setup.html` is rendered by `internal/doctor` from its own embedded template — **not** by `internal/report`, so `doctor` ships without depending on the renderer (spec §11) and a setup page and an evidence page stay different documents.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/doctor/ ./cmd/notary/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/doctor/ cmd/notary/
git commit -m "feat(cmd): notary doctor, and the key nothing could generate"
```

### Task 3: `internal/report` — reading, filenames, and the three page kinds

**Files:**
- Create: `internal/report/report.go`, `internal/report/templates/*.html` (`go:embed`-ed)
- Test: `internal/report/report_test.go`

**Interfaces:**
- Produces:
  - `type Reader interface { ListRecords(from, to time.Time) ([]record.Record, error); ListRecordsByMemory(memoryID string) ([]record.Record, error) }` — satisfied by `*ledger.Ledger`, shaped like `export.Reader` and `replay.Reader`.
  - `type Request struct { MemoryID string; From, To time.Time; Scope record.Scope; IncludeSensitive, Verify bool; Breaks []ledger.Break; VerifiedAt time.Time }`
  - `type Result struct { Memories, Records, Redacted, Pages int }`
  - `func New(r Reader) *Reporter`, `func (rp *Reporter) Render(ctx context.Context, req Request, dir string) (Result, error)`
- Consumes: `ledger.ListRecords`, `ledger.ListRecordsByMemory`, `export.Render`, `export.ScopeMatches`, `ledger.Break`.

**Notes the implementer needs:** pages render from `export.Line`, never from a second reading of the record — `export.Render(rec, req.IncludeSensitive)` already yields the phrasing, the tier, the content-withholding decision and every hash, so the report and an exported line cannot describe a record differently. **The record page carries the chain fields including `signature`, collapsed with the other long hex** — spec §12's second resolution: the report may be the only artefact that survives a deployment, and a record representation missing the field a verifier actually checks is a partial one. `Request.Verify` is what distinguishes "verified clean" (`Verify` true, `Breaks` empty) from "not verified" (`Verify` false) — **`len(Breaks) == 0` cannot tell them apart, and rendering them the same is Review Focus 5's failure**. The break collection arrives already computed (the command owns the gap path and the trusted keys); the renderer never walks the chain itself.

- [ ] **Step 1: Write the failing tests**

Against a fake `Reader` and a temp-dir `dir`:

- `TestReportRendersAnIndexAndAPagePerMemoryAndRecord` — the files exist, and the index's links resolve to pages that exist.
- **`TestReportPageFilenamesAreDerivedNotTakenFromIds`** — a memory id and a record id carrying `#`, `:`, `/` and a control character produce filenames matching a safe pattern, and **nothing is written outside `dir`** (Review Focus 2). Assert the created tree, not just the absence of an error.
- **`TestReportEscapesStoredText`** — content containing `<script>alert(1)</script>` and `</textarea>` appears as text in every page that carries it (Review Focus 3).
- `TestReportWithholdsSensitiveContentByDefault` / `…RendersItWithTheFlag` — and each record's `hash`/`prev_hash` are identical between the two runs.
- `TestReportMemoryPageMatchesThePhrasingExportProduces` — for the same record, the page carries the string `export.Phrase` returns.
- `TestReportRecordsTheCommandThatProducedIt`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/report/ -count=1`
Expected: FAIL — no such package.

- [ ] **Step 3: Implement the reading, the derived filenames, and the three templates**

Filenames are `index.html`, `memory/<seq>-<hash8>.html`, `record/<seq>-<hash8>.html`, where `<seq>` is the record's `Seq` (a memory's being its first record's) and `<hash8>` is the first eight hex of `record.ContentHash(id)` — reusing the existing digest, never a new one.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/report/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/report/
git commit -m "feat(report): render a ledger slice as pages a reader can follow"
```

### Task 4: `internal/report` — assets, the filter, the chain-state page, and the guards

**Files:**
- Create: `internal/report/assets/report.css`, `internal/report/assets/report.js`, `internal/report/templates/verify.html`
- Modify: `internal/report/report.go`, `internal/report/templates/index.html`
- Test: `internal/report/report_test.go`

**Interfaces:**
- Produces: the `assets/` files written beside the pages, and `verify.html`; `Result.Pages` counts every file written.

**Notes the implementer needs:** `report.css` and `report.js` are `go:embed`-ed and linked **relatively** — no CDN, no fonts, no remote anything. The filter is plain DOM filtering over the index table (no embedded index data), and it is **progressive enhancement**: with the script absent or blocked, the table is complete and every link works. `verify.html` renders `Request.Breaks` — clean, or every break with its record id and field — and, when `Request.Verify` is false, says **that no verification ran** rather than showing an empty list as if it were a pass (Review Focus 5).

- [ ] **Step 1: Write the failing tests**

- **`TestReportOutputIsOffline`** — no generated file contains an absolute URL to any host (Review Focus 4). Scan every file for `http://` and `https://` outside the records' own content, and assert the assets are linked relatively.
- `TestReportChainStateReportsBothWays` — clean on an intact fixture; on a tampered one, the break's record id and field appear on `verify.html`.
- `TestReportNotVerifiedDoesNotReadAsClean` — with `Verify: false`, the page says no verification ran, and does not say clean (Review Focus 5).
- `TestReportWritesTheAssetsBesideThePages` — and the pages reference them by relative path.
- `TestReportIndexCarriesPerTierCounts` — the breakdown the spec's §12 resolution requires, not a single "highest tier".

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/report/ -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement the assets, the page, and the counts**

`report.js` filters the index table's rows in the DOM — no index data, no framework — and the table must be complete and every link usable when the script does not run. `verify.html` renders clean, or each break with its record id and field, and says explicitly when no verification ran.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/report/ -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/report/
git commit -m "feat(report): the chain state, the assets, and the offline guarantee"
```

### Task 5: `notary report`

**Files:**
- Create: `cmd/notary/report.go`
- Test: `cmd/notary/report_test.go`
- Modify: `cmd/notary/root.go` (one line), `cmd/notary/root_test.go` (the list)

**Interfaces:**
- Consumes: `report.New`, `report.Request`, `report.Result`, `ledger.CollectBreaks`, `sign.LoadTrustedKeys`.

**Flags:** `--out DIR` (required), `--memory`, `--from`, `--to`, `--include-sensitive`, `--no-verify`, `--force`, and the four scope flags.

**Notes the implementer needs:** exactly one subject — `--memory` **or** `--from` — enforced explicitly rather than with Cobra's declarative helper, so the message can say which was given; this is `explain`'s rule (`cmd/notary/explain.go`'s `explainRequest`) and its shape should be copied. The four scope flags narrow a range through `export.ScopeMatches` and are **refused with `--memory`**, where the subject already identifies its scope. `--out` **refuses a non-empty directory** unless `--force`, and writes nothing at all when it refuses. `--no-verify` must reach the renderer as `Verify: false`, not as an empty break list (Review Focus 5). `--force` is never implied. `report`'s help text names where the setup page lives (`notary doctor --out`), per the spec's §5.

- [ ] **Step 1: Write the failing tests**

Following `newTestExplainCmd`'s harness:

- `TestReportCmdRequiresExactlyOneSubject` — both, and neither, are refusals naming what was given.
- `TestReportCmdRefusesScopeFlagsWithMemory`.
- `TestReportCmdRefusesANonEmptyOutDirectory` — and asserts the directory is untouched.
- `TestReportCmdWritesThePagesIntoOut` — end to end against a fixture ledger.
- `TestReportCmdNoVerifyRendersAsNotVerified`.
- `TestReportCmdHelpNamesTheSetupPage`.
- Registration: `root_test.go`'s list gains `report`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./cmd/notary/ -run TestReportCmd -count=1`
Expected: FAIL — `newReportCmd` undefined.

- [ ] **Step 3: Implement the command and register it**

Copy `explainRequest`'s explicit mutual-exclusion shape rather than Cobra's declarative helper, so the message can name which subject was given, which is missing, and what to do. Compute the chain state through `ledger.CollectBreaks` **before** calling the renderer, and only when `--no-verify` was not given.

- [ ] **Step 4: Run the tests, then the whole suite, serially**

Run: `go test ./cmd/notary/ -count=1`, then `go test -p 1 ./... -count=1` (allow several minutes; `internal/ledger` alone is ~60s, and a cold build may exceed a two-minute shell cap, in which case re-run it).
Expected: PASS everywhere, including Task 1's differential test and the two new subcommands in `root_test.go`.

- [ ] **Step 5: Commit**

```bash
git add cmd/notary/
git commit -m "feat(cmd): notary report"
```

### Task 6: The documented surface

**Files:**
- Modify: `docs/superpowers/specs/2026-09-28-notary-v1-architecture-design.md` (§9's read-path table, §13's phase table, §14's delta table)
- Modify: `README.md` (both commands, and the phase count if it is stated)
- Modify: `.clinerules` (the folder tree)

**Interfaces:** none — documentation only.

- [ ] **Step 1: §9's read-path table gains a `report` row** — the single-subject/range framing is already there for the other commands; match its form rather than inventing one.
- [ ] **Step 2: §13's table gains row 11**, marked as post-v1, and §14's delta table gains `internal/report` and `internal/doctor`.
- [ ] **Step 3: The README gains both commands — derive every claim from the code**, not from memory: what `report` renders, that it is bounded by its subject, that it withholds sensitive text by default, that it needs no signing key, and that `doctor --generate-key` is the only thing that creates key material.
- [ ] **Step 4: `.clinerules`' folder tree gains the two packages**, matching what is on disk.
- [ ] **Step 5: Correct anything this phase made false**, checking each sentence against the code. `verify`'s orchestration moving is the one that most likely leaves a stale sentence behind.
- [ ] **Step 6: Commit**

```bash
git add docs/ README.md .clinerules
git commit -m "docs: record Phase 11"
```

---

## Risk, and where it is concentrated

- **Task 1 is a refactor of a shipped command, and its guard is a test that did not exist.** The differential test is what makes "the extraction changed nothing" checkable — without it, `verify`'s own tests only prove it still passes, not that it still says the same thing.
- **Task 3's hostile-id test is the phase's sharpest security edge.** A filename built from an id is a path-traversal risk in the one place this project writes user-influenced names to disk.
- **Review Focus 5 is the easiest thing to get wrong and the hardest to notice.** `Verify: false` with an empty `Breaks` renders, by the obvious implementation, exactly like a verified-clean ledger — a report claiming an assurance nobody established.
- **Least certain: whether `internal/doctor` should render its own page.** The spec chose that so `doctor` ships without the renderer; if the reviewer judges two HTML templates in two packages a worse cost than the dependency, that is a plan defect worth raising rather than working around.

## Sequencing, and why the order is forced

1 → 2 and 1 → 4 (both need the shared break collection); 3 → 4 (the assets and the chain page extend the renderer); 3, 4 → 5 (the command drives the renderer); 5 → 6. Tasks 2 and 3 are independent of each other, and of everything but Task 1.

## Spec coverage

| Spec section | Task |
| --- | --- |
| §3 decision 2, §6 — the multi-page site and its pages | 3, 4 |
| §3 decision 3, §7 — a little vanilla JS, progressive enhancement | 4 |
| §3 decision 6, §4 — `doctor` and the setup page | 2 |
| §3 decision 7, §6 — the verification section, `--no-verify` | 4, 5 |
| §3 decision 8 — one definition of intact | 1 |
| §4 — the checks, `--generate-key`, the repository guard | 2 |
| §5 — the subject selector, the flags, `--out`'s refusal | 5 |
| §6 — derived filenames, the two claims | 3, 4 |
| §7 — `go:embed`, offline, escaping | 3, 4 |
| §8 — what it must not do | 3, 4, 5 |
| §9 — the limitations | 6 |
| §10 — the tests | 1–5 |
| §12 — the three resolutions (setup page on `doctor`; the signature on the record page; per-tier counts) | 2, 3, 4 |

§1 (the audience and the artefact) and §2 (what must not be disturbed) are carried by the header's **Goal** and
by **Global Constraints** respectively, rather than by a task: they constrain every task instead of naming one.

## Type consistency

- `record.Record`, `record.Scope`, `record.ContentHash`, `record.RecordID` — existing, unchanged.
- `export.Line`, `export.Render(record.Record, bool) (Line, error)`, `export.Phrase(record.Record) (string, bool)`, `export.ScopeMatches(record.Scope, record.Scope) bool` — existing, unchanged, and the report renders from them.
- `ledger.Break`, `ledger.CollectBreaks(*Ledger, store.Store, string, *sign.Verifier) ([]Break, error)` — new in Task 1, consumed by Tasks 2 and 5.
- `sign.NewVerifier(map[string]ed25519.PublicKey)`, `sign.LoadTrustedKeys(string) (map[string]ed25519.PublicKey, error)` — existing.
- `report.Reader`, `report.Request`, `report.Result`, `report.New`, `(*Reporter).Render` — new in Task 3, consumed by Task 5.
- `doctor.Finding`, `doctor.Severity`, `doctor.Diagnose`, `doctor.RenderSetup` — new in Task 2, consumed by Task 2's own command only.
