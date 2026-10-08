# Notary — `serve`: Design (the local compliance console)

**Status:** draft for review. The decisions in §3 were put to the operator during brainstorming and
**delegated** ("what works best?", "what best fits", "choose for me"), so they are decided here on merit with
the reasoning and the cost of each kept in §3. Per the brainstorming gate, no implementation starts until this
spec is reviewed and a plan is written.

**Date:** 2026-10-06
**Depends on:** v1 complete (phases 0–10) and Phase 11 in progress — `1b5ea61` (the shared break collection,
`internal/ledger/breaks.go`) and `86919b1`…`40920b5` (`notary doctor`). `notary report`, Phase 11's other half,
is **not built**; this spec does not wait on it and does not build it.
**Phase table:** this is **Phase 12**, beyond the v1 table's ten rows and after Phase 11's `report`/`doctor`.

---

## 1. What this is for, and what the artefact is

Phase 11's spec named the audience in one line — *"a compliance lead who must explain an agent's memory
decisions without reading code or pulling in an engineer"* — and then chose, deliberately, to serve that person
**static HTML files** and **not** a localhost console, recording the cost as *"adding it later is a new phase,
not a flag."* This is that phase. The operator wants the console after all, and for a reason the static
artefact cannot serve: it is what he shows a compliance lead **during outreach**, live, on his own machine,
where the reviewer can set the date range and drill in themselves.

This phase adds one command:

- **`notary serve`** — a read-only HTTP dashboard over the existing ledger, bound to loopback, that renders
  the records view, the per-record and per-memory explain views, the gaps view and the chain-verify status.

**The artefact is a browser session, not a file.** That is the whole difference from Phase 11 and it is also
its cost: a server has a listening socket, and a socket is a surface a folder of files does not have. §8 is
about the properties that make the surface small; §10 records what it still does not do.

Nothing in this phase writes. Nothing in this phase verifies anything of its own invention: the chain state is
`notary verify`'s answer and the gaps list is `notary gaps`'s answer, through the shared definitions §3 and §4
name.

## 2. What is already fixed, and what this must not disturb

- **The tier model and its visual language.** `record.VisibilityTier` (`internal/record/tier.go`) is
  unforgeable, has exactly three values, and `docs/assets/banner.svg` already draws them as decreasing
  confidence: `Observed` gold `#D9A441` with a solid rule, `Reconstructed` grey-blue `#8593A5` with a dashed
  card, `Internal` dim `#47505E`/`#707B8A` with a dotted card. The dashboard reuses those exact tokens so the
  console and the banner tell one story.
- **Redaction is presentation, never storage.** `redactedSensitive` and the withhold-or-show decision live in
  `export.Render` (`internal/export/line.go:116`, `line.go:91`). The dashboard does not restate the rule; it
  calls the function. `Line.Redacted` non-empty means withheld because sensitive; `Line.Content == nil` means
  nothing was recorded; the two must never look alike.
- **The phrasing sentence.** `export.Phrase` is the one source of a claim's human sentence. `notary explain`
  already imports it (`internal/explain/explain.go`) so the single-subject view and the range view cannot
  drift; the dashboard does the same.
- **The one definition of "is the ledger intact".** `ledger.CollectBreaks` (`internal/ledger/breaks.go:44`)
  runs the chain walk, the gap cross-check and the gap log's own integrity, in the order `notary verify`
  reports them. The dashboard's chain state calls it and nothing else, so the page and `notary verify` cannot
  disagree.
- **`.clinerules`** forbids a new dependency without asking, forbids key material in the repository, and
  forbids refactoring prior-phase code without an explicit ask. §3 record 4 is the explicit ask for the one
  refactor this phase needs; §7 records the asset decision that keeps the first rule satisfied without adding
  a Go module.
- **`notary verify` refuses without a keyring** (`cmd/notary/verify.go`): `sign.NewVerifier(nil)` trusts no
  keys and reports **every** record as a signature break, so running the check anyway would accuse a healthy
  ledger. **`notary doctor` refuses the opposite way** (`internal/doctor/doctor.go`): *"A check that cannot be
  run is reported as not run, never as a pass and never as a failure."* §3 record 2 chooses which of the two
  the dashboard follows, and why.

## 3. The decisions, with their costs

**Delegated by the operator during brainstorming, decided here on merit:**

1. **`serve` stands alone; `notary report` keeps its own phase.** The console renders every record from
   **`export.Line`** — the canonical per-record shape (`internal/export/line.go`), produced by `export.Render`
   — and reads through `ledger`. It does not build `internal/report`, and it does not extract a shared
   view-model package. *Why:* the operator's brief scopes `serve` over the **existing** `export` / `replay` /
   `explain` / `gaps` / `verify` logic; `report` does not exist, so building it first is another phase's work,
   and a shared abstraction with a consumer that does not yet exist is speculation. *Cost:* when `report` is
   built, its templates and this console's templates are separate, and convergence is `report`'s decision to
   make. The **data** is single-sourced (both would render `export.Line`); only the HTML is not.

2. **Start without a keyring; the chain indicator says "not verified".** `serve` starts whether or not
   `NOTARY_TRUSTED_KEYS_PATH` is set. With a keyring it runs `ledger.CollectBreaks` and shows clean, or every
   break with its record id and field. Without one it shows **Not verified — no trusted keys configured**, and
   names the fix (`point NOTARY_TRUSTED_KEYS_PATH at a keyring of base64 ed25519 public keys; notary doctor
   checks it`). *Why:* it follows `doctor`'s rule — a check that cannot run is reported as **not run** — which
   is the only posture under which the console opens at all on a machine nobody has configured, and opening is
   the point of the outreach artefact. It can never masquerade as "verified", which is the failure mode that
   matters. *Cost:* a reviewer with no keyring sees "not verified", and the chain's **signatures** are not
   checked until they configure one. **A hash-and-link-only walk was considered and rejected**: `ledger`
   exposes no such path, so adding one would be new verification logic in a render layer, which the brief
   forbids.

3. **Reveal is a per-view toggle that resets on navigation and is logged to stderr.** There is **no
   `--include-sensitive` flag on `serve`**, so the safe default cannot be flipped globally; reveal exists only
   as `?reveal=1` on the view in hand. A page's own links never carry it, so navigating away re-redacts. Each
   request that reveals writes one line to the operator's stderr, naming the view. *Why:* "explicit and
   deliberate" is best served by a decision that is taken **per view** and leaves a trace, mirroring the CLI,
   where every `--include-sensitive` is a fresh, conscious act. *Cost:* revealing while drilling through
   several records means clicking reveal on each one. That friction is the feature.

4. **The gap report's orchestration is extracted into one shared function** *(the explicit §5 ask)*. Today the
   orchestration — `gap.Read` → `store.SeqEntries` → `ledger.GapBreaks` → `gap.Verify` — lives only in
   `runGaps` (`cmd/notary/gaps.go`). It moves into a package function beside `ledger.CollectBreaks`, and
   `notary gaps` is refactored to call it. *Why:* the *rules* are already shared package functions, but the
   *glue* is not, and a dashboard that re-glued it would be a second definition of "outstanding gaps" that can
   silently drift from the command — exactly the drift `CollectBreaks` was extracted to prevent last phase.
   A **differential test** over three fixtures (intact, unmatched gap entry, corrupt gap log) proves the
   console and `notary gaps` agree. *Cost:* it touches a shipped command, which `.clinerules` §5 forbids
   without an ask; the ask is this record, and the operator approved it. `notary gaps`'s **observable
   behaviour must not change**, and its existing tests are the guard.

**Decided here, presented with the design:**

5. **Ids travel in query strings, never path segments and never filenames.** Record ids in this codebase carry
   `#`, `:` and `/` (`corr-search-1#1`, `add_resolved:stored_by_mem0:<event-id>`), so a path segment would be
   both a routing hazard and the path-traversal class Phase 11 flagged for filenames. The route set is
   therefore fixed and small (`/record?id=…`), and the id is `url.QueryEscape`d exactly once in Go and handed
   unchanged to both the `href` and the `hx-get` (§6). *Cost:* slightly less pretty URLs. There are no filesystem paths in this phase at all, which removes
   the traversal class rather than mitigating it.

6. **A loopback `Host` check, and never a CORS header.** A request whose `Host` is not loopback is refused,
   and `Access-Control-Allow-Origin` is never set. *Why:* the console is a real listening socket, and
   DNS-rebinding is the one browser-reachable attack a read-only local server still has; refusing a
   non-loopback `Host` blunts it, and setting no CORS header means a foreign page cannot read a response even
   if it can send one. *Cost:* a reviewer who reaches the console by some other hostname must use
   `127.0.0.1`. That is the intended trust boundary (§8).

7. **The records view is bounded by a date range, defaulting to the last 30 days, capped like `export`.**
   `export` requires `--from` because an unbounded export is a footgun; the console gives the browser a
   sensible default instead of an error, and applies `export.DefaultMaxSpan` as the same cap. *Cost:* an empty
   console for an old ledger. That is handled by an explicit empty state (§5), not by an `--all`.

## 4. `notary serve` — the command

`cmd/notary/serve.go` adds `newServeCmd()`, registered in `newRootCmd` and added to the pinned
`rootSubcommands` list in `cmd/notary/root_test.go`.

- **`--port <n>`, default `4317`.** The **only** flag. There is no `--host`, no `--addr` and no
  `--include-sensitive`: the bind address is not configurable in this version, and the reveal default is not
  configurable at all. A test pins the flag set to exactly `{port}`. `--port 0` asks the OS for a free port and
  the command prints the real one, which is how the handler tests get a listener.
- **No signing key, no keyring required.** Like `explain`, the command needs neither to start; a keyring, when
  present, only enables the chain check (§3 record 2). It never loads a private key — it signs nothing.
- **It opens the ledger read-only in the structural sense:** `store.Open(cfg.DBPath)` then
  `ledger.New(st, nil, nil)` — a **nil signer**. `ledger.Append` refuses with *"no signer configured"*, so
  read-only-ness is a property of the construction and not of anyone remembering not to call `Append`. This is
  the same structural trick `explain` uses.
- **It prints where it is listening, to stderr**, as one line: `notary serve: dashboard on
  http://127.0.0.1:<port> (read-only; Ctrl-C to stop)`. Stdout stays empty.
- **It blocks until the context is cancelled** and shuts down gracefully: `signal.NotifyContext` on
  `SIGINT`/`SIGTERM`, then `http.Server.Shutdown` with a bounded timeout. `cmd.Context()` is nil for a command
  not run through `Execute` (the project's established test seam), so it falls back to
  `signal.NotifyContext(context.Background(), …)`, as the other long-running commands do.
- **It binds `net.Listen("tcp", "127.0.0.1:<port>")` directly** — a literal loopback address, not
  `:port`, so there is no interface to misconfigure and a test can assert the bound address. A bind failure is
  a plain-language error naming the port and the likely cause (in use).

The command is a thin shell: it resolves configuration, opens the ledger, builds `serve.Server`, and runs it.
The routes, views and templates live in `internal/serve`, where they can be tested without a command.

## 5. Routes and pages, and what each claims

The route table is fixed and every route is **GET or HEAD**; any other method is `405` with an empty body (§8).
`serve.Server` builds one `http.ServeMux` and registers exactly these.

| Route | Renders |
|---|---|
| `GET /` | **Records view** — the scope + date-range filter, a row per record with a **tier badge**, the chain-status banner, and navigation |
| `GET /record?id=<record-id>` | **Per-record explain view** — the phrased sentence, event, reason kind, tier, both instants, and the content or the fact it was withheld; the chain fields in a collapsed `<details>` |
| `GET /memory?id=<mem0-id>` | **Per-memory explain view** — one entry per record for that memory, in `Seq` order, the same shape `explain --memory` prints |
| `GET /gaps` | **Gaps view** — every outstanding gap (kind, scope, correlation id, when, why) and any gap-log integrity break |
| `GET /verify` | **Chain state in full** — clean, or every break with its record id and field, or *not verified* with the fix |
| `GET /assets/<file>` | The embedded assets (§7): `htmx.min.js`, `app.css` |

**The records view is the console's front page.** Its filter is a GET form carrying `from`, `to` and the four
scope fields (`user_id`, `agent_id`, `app_id`, `run_id`), composing with AND exactly as `export`'s four scope
flags do (`export.ScopeMatches`). Defaults: `from` = now − 30 days, `to` = now. The table shows, per record,
its seq, event time, event, reason kind, **tier badge**, memory id, scope, the phrasing sentence, and the
content or the withheld marker; the memory id and the row link to `/memory` and `/record`. **An empty range is
stated in words**, naming the range it searched and saying how to widen it — the same discipline `export` uses
("no records" is a success that says so, never a broken invocation).

**The chain-status banner sits on every page and is attributed to the run.** It reads *"As of <instant>, this
tool's verification reported: …"* and carries the re-run command (`notary verify`, with the keyring path when
one is configured). It is the generator's finding about the ledger, never a property of the page — a page that
claimed *"verified"* without the run behind it is the unbacked claim this project exists to refuse. The
`/verify` page is the same answer in full.

**Every page that shows a hash says the page verifies nothing by itself**, and repeats the command that does.

**`/record` and `/memory` refuse an empty id** with a plain-language error rather than reading: the store
matches the empty `memory_id` column's DEFAULT, so an empty filter would quietly list the whole ledger wearing
one memory's clothes — the exact footgun `explain` refuses. A record whose subject names no memory (the store's
DEFAULT `''`) therefore gets **no memory link** on the records table or the record page, because there is no
memory to link to. A `/memory` whose memory has no records renders a "no records for this memory" page rather
than an empty success, so "this memory never appears" never reads as a quiet blank. A `/record` whose id is not
in the ledger says so, naming the id, matching `explain`'s `store.ErrNotFound` handling.

## 6. Rendering, redaction, and the reveal toggle

**One input, one direction.** Every record in every view is an `export.Line`:

```
store.SeqEntries / ledger.GetRecord / ledger.ListRecords / ledger.ListRecordsByMemory
        → record.Record
        → export.Render(rec, reveal)   ← tier label, redaction, the phrasing sentence: all decided here
        → export.Line
        → html/template
```

The records view and the memory view take their `record.Record` slice from `ledger` and their scope filter
from `export.ScopeMatches`; the per-record view uses `ledger.GetRecord`. In every case the view is a projection
of `export.Line`, so tier, redaction and phrasing have exactly one owner. The per-record and per-memory views
show the same claims the CLI's `explain` shows **because they are built from the same functions `explain`
calls**, so the two cannot drift — §11 makes that a differential test.

**Redaction is the CLI's rule, unchanged.** `export.Render(rec, false)` withholds sensitive content and sets
`Line.Redacted`; the page renders *"Content withheld — marked sensitive"*. `Line.Content == nil` renders as
*"No content recorded"*. They never look alike. **Reveal** (`?reveal=1`) passes `true` and is the only
difference: one boolean into one existing function, and **no hash differs between the two renders**, because
redaction was never in the hash (§11).

**The toggle is a control, not a state.** Reveal is a button on the view in hand that re-requests *that view's
own URL* with `reveal=1` (htmx swaps the content). No cookie, no session, no server-side flag. Links the page
generates never carry `reveal`, so any navigation re-redacts — the reset is structural, not a timer. Each
revealing request writes one line to stderr, e.g. `serve: revealed sensitive content for record <id>`.

**Escaping is `html/template`'s, and it is never turned off.** Templates parse with the default autoescaping;
no value is ever `template.HTML`, `template.JS` or `template.URL`, and no stored string is ever
concatenated into markup. Memory text is arbitrary agent- or user-supplied content rendered into a browser
document, and a memory id, a scope value, a gap detail and a phrasing sentence are all stored or derived text
that goes through the template the same way. **Ids in URLs** ride in **query values**, and the id is
`url.QueryEscape`d **exactly once, here in Go** — the same escaped string is then handed to every URL
attribute a link carries, `href` and htmx's `hx-get` alike. `html/template`'s contextual escaper cannot be
relied on to do it: it URL-encodes only its fixed set of URL attributes (`href`, `action`, `src`, …), and it
merely **HTML-escapes** a custom attribute such as `hx-get`. An id left raw for the escaper therefore leaves
`/`, `#` and `:` unescaped in `hx-get`, and an id containing `#` starts a URL fragment there — the server
never sees the rest, and the request reads the wrong record. (This corrected an earlier claim in this
spec that `html/template`'s URL-context escaper encodes the id on its own: that is true for `href` and false
for `hx-get`.) Escaping once in Go and reusing that one string for both attributes removes the divergence: a
pre-escaped value survives a URL attribute and an HTML attribute intact and round-trips through
`r.URL.Query()`. The template's `urlquery` is still deliberately **not** used (it would be a second encoder),
and the id is escaped in exactly one place — `recordHref`/`memoryHref` in `internal/serve/render.go` — so no
view can escape it twice or not at all. An id's `..` is left literal, which is harmless precisely because an
id is only ever a query value — never a path segment or a filename (§3 record 5). A test pins the exact
rendered `href` and the `hx-get` for a hostile id, so this is a verified property rather than a hope about a
contextual escaper.

**Styling is the banner's language.** `app.css` carries the banner's tokens — `Observed` gold `#D9A441` with a
solid rule, `Reconstructed` `#8593A5` dashed, `Internal` `#47505E`/`#707B8A` dotted — over the same dark
ground (`#0A0D12`/`#141A23`, text `#EEF1F5`, muted `#98A2B3`). A **tier badge carries the tier name as text**
as well as its colour and rule, so it survives colour-blindness, a greyscale printout and a screenshot.

## 7. Assets, and the no-dependency rule

- **`internal/serve/assets/htmx.min.js` is vendored as one file** and embedded with `go:embed` (standard
  library). It is the only script, and it is pinned to **htmx 2.0.7** — 51,076 bytes, SHA-256
  `60231ae6ba9db3825eb15a261122d5f55921c4d53b66bf637dc18b4ee27c79f9`, licensed **Zero-Clause BSD (0BSD)**,
  which asks no attribution. The version, that digest and the licence are recorded in a header comment beside
  the file, and the licence text ships with it as `internal/serve/assets/htmx.LICENSE`; the vendored bytes are
  unmodified and are served from `/assets/htmx.min.js`. Vendoring an asset
  is not adding a Go module, so `.clinerules` §5 ("do NOT install new dependencies without asking") is
  satisfied without a `go.mod` change — and the brief asks for it by name.
- **`internal/serve/assets/app.css` is hand-written** and embedded the same way. No CSS framework, no
  preprocessor, no build step.
- **No CDN, no fetched font, no remote anything.** The console must render on a machine with no outbound
  network at all, and a page that fetched a CDN would leak the fact that it was opened, and to whom. A test
  asserts no `http://` or `https://` URL to any host appears in any rendered page.
- **htmx is progressive enhancement.** The filter form is an ordinary GET form and every link is an ordinary
  link, so with JavaScript disabled the console is fully usable — the swap is an improvement, not a
  requirement. A test renders each page and asserts the links and the form action are real, resolving URLs.

## 8. Read-only and the security posture

Two properties make the surface small, and both are asserted, not asserted-by-virtue:

- **Structurally read-only.** The ledger is built with a nil signer (§4), so no code path can append. No
  handler calls `Append`, calls `gap.Open` (which would create and heal the log — the audit path reads through
  `gap.Read`/`gap.Verify`, which never open for append), or reaches reconcile at all. `internal/phrase` is not
  imported, so no handler can bill a language model.
- **GET-only.** One middleware rejects every method but GET and HEAD with `405` and no body, so there is no
  request shape that could carry a mutation even if a handler were ever added carelessly. There is no CSRF
  surface because there is no state to change.

Explicitly, and recorded rather than implied: **there is no authentication.** Anyone who can reach loopback on
this machine can read this ledger. The trust boundary is the loopback interface plus the operator's user
account; the `Host` check and the absent CORS header (§3 record 6) narrow the browser-reachable path but are
not a substitute for auth. Auth is deferred (§10), not forgotten.

## 9. What it must not do

- **No endpoint writes** the ledger, the gap log, or anything else, and none triggers reconcile (§8).
- **No flag that widens the bind address**, and no `--include-sensitive` — §3 records 2 and 3 say why each
  absence is deliberate.
- **No new Go dependency and no `go.mod` change.** `net/http`, `html/template` and `go:embed` are stdlib;
  htmx is a vendored asset, not a module.
- **No reimplementation of tiering, redaction or phrasing.** They come from `record`, `export.Render` and
  `export.Phrase`, always.
- **No second definition of "intact" or of "outstanding gaps."** The first is `ledger.CollectBreaks`; the
  second is the function §3 record 4 extracts.
- **No claim it cannot back** (§5).

## 10. Limitations recorded, not hidden

- **No authentication.** §8.
- **The chain state is whole-ledger, not view-scoped.** A hash chain admits no partial verification: an intact
  prefix of a tampered chain is still tampered. Every page therefore states the **whole ledger's** integrity,
  and says so rather than implying the view was checked.
- **Without a keyring, nothing is signature-verified.** The indicator says *not verified*, and the record
  pages' chain fields are shown as the stored hex, not as a verification result.
- **The records view is not paginated.** It is bounded by the date range, capped at `export.DefaultMaxSpan`
  (366 days), which is a deliberate choice (§3 record 7); a range wide enough to produce a very long page is a
  range the reviewer asked for.
- **Rendering is not streamed.** A view builds in memory before it is written. The date bound is what keeps
  that acceptable, and it is the bound that would have to change first.
- **No renderer is shared with `notary report`**, which does not exist (§3 record 1). The data is shared
  (`export.Line`); the templates are not.
- **The console is a second way to read the ledger.** `export`, `explain`, `gaps` and `verify` remain the
  authoritative commands, and every page names the one that answers its question so a finding can be
  reproduced outside a browser.

## 11. Testing

Every test runs offline; none needs a key, a network or a provider, and the suite runs `-p 1` in CI.

- **The console renders a fixture ledger**: every route returns `200`, the records table lists the fixture's
  records, and the memory and record pages exist for them.
- **Differential against the CLI — the load-bearing tests.**
  - The records table's rows equal the lines `notary export --from --to` writes for the same range and scope.
  - The per-memory page shows the same sentences, tiers and withholding decisions as
    `notary explain --memory <id> --json`.
  - The chain state equals what `notary verify` reports for the same ledger — clean on the intact fixture, and
    naming the exact record and field on a tampered one (`testdata/tamper/` documents the procedure).
  - The `/gaps` view equals what `notary gaps` reports — over three fixtures: intact, an unmatched gap entry,
    and a corrupt gap log. The third is the case a chain walk alone cannot see, and the one that proves §3
    record 4's extraction was necessary rather than decorative.
  - The extraction itself gets `cmd/notary/gaps.go`'s **existing** tests as its guard: `notary gaps`'s
    observable behaviour does not change.
- **Redaction by default, reveal on request**: a sensitive record's text is absent from every page by default;
  present with `?reveal=1`; and **no hash on any page differs between the two runs**.
- **The reveal toggle is per-view and logged**: following a link from a revealed page lands on a redacted
  page, and each reveal writes one line to stderr.
- **A hostile id is escaped, not followed**: an id containing `/`, `..`, `#`, a space and a control character
  is rendered escaped in every `href` and form value, and appears in no path or filename (there are none).
- **Stored text cannot become markup**: `<script>` and `</textarea>` placed in memory content, a memory id and
  a gap detail render as text, never as elements.
- **The output is offline**: no absolute URL to any host in any page (§7).
- **Read-only is asserted both ways**: every method but GET/HEAD returns `405` with an empty body, and the
  ledger file's bytes are unchanged after every route has been exercised — a store or ledger wrapper that fails
  the test if a write is ever attempted is the mechanism.
- **The bind is loopback only**: the listener's address is `127.0.0.1:<port>`, and `serve`'s flag set is
  exactly `{port}`.
- **The chain indicator's three states**: clean, broken (naming the break), and *not verified* with no keyring
  — asserted to be three different renderings, so "could not verify" can never look like "verified".
- **The `Host` check and the absent CORS header**: a non-loopback `Host` is refused; no response carries
  `Access-Control-Allow-Origin`.
- **No key material in any page**, with the same canary discipline `export`, `replay` and `doctor` use.
- **The htmx asset is the vendored file**: served byte-identically from `/assets/htmx.min.js`, and its
  recorded SHA-256 matches.

## 12. Sequencing

1. **The shared gap report** (§3 record 4) — extract the orchestration into one function, refactor
   `notary gaps` to call it, and add the differential test. **First**, because the gaps view depends on it and
   because `gaps` is a shipped command whose behaviour must be provably unchanged before the console relies on
   the extraction. It is self-contained and merges cleanly on its own.
2. **`internal/serve`: the data layer** — the view models assembled from `export.Line`, the gaps function and
   `ledger.CollectBreaks`, plus the chain-status three-state resolution. No HTTP yet, so it is testable
   directly.
3. **`internal/serve`: the server and the routes** — `net/http`, the GET-only and `Host` middleware, the
   loopback bind, the embedded templates and assets, and htmx for the filter and the reveal swap.
4. **`notary serve`** — the command, `--port`, the startup line, `signal.NotifyContext` and graceful shutdown,
   and the registration in `root.go` plus the pinned `rootSubcommands` list.
5. **Docs** — the architecture spec's §9 read-path table gains a row, §13's phase table gains row 12, §14's
   delta table gains `internal/serve`; the README gains `serve`; `.clinerules`' folder tree gains the package;
   and this spec's deferred work is reflected in §10.

## 13. Questions that were open, and how they resolved

1. **Whether this is its own phase or a flag on `report`.** **Resolved: its own phase.** `report` is not built,
   and Phase 11's own design said adding the console is "a new phase, not a flag."
2. **Whether a hash-and-link-only verification could back the indicator without a keyring.** **Resolved: no.**
   `ledger` exposes no such path, and adding one would put new verification logic in a render layer. The
   indicator reports *not verified*, per §3 record 2.
3. **How the operator's browser reaches the console.** **Resolved: `http://127.0.0.1:<port>`, printed on
   startup.** No browser-opening flag in this version; the operator clicks the printed URL. Opening a browser
   is a nicety the console does not need, and a command that launches a browser is a command that must know
   the platform's browser, which is a dependency this phase does not take.
