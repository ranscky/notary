# Notary — `serve`: the Overview page: Design

**Status:** approved for implementation. The operator's brief fixes the content and the routes; the
decisions below are the ones it left open, settled here with their costs stated.

---

## 1. What this is for, and what the artefact is

The console's front page answers one question, for a reader who is not an engineer: **can I trust this
trail?**

Today the front page is the records table. That is the wrong first thing to show. A table of `seq`,
`tier`, `reason_kind` and event times is legible only to someone who already knows what a record is;
a compliance reviewer opening the console cold has to reverse-engineer the data model before they can
form an opinion. The Overview inverts that: a verdict first, then three facts about the ledger, then the
gap count — and, on a ledger with nothing in it yet, an explanation of what has to happen before there is
anything to review at all.

The routes move to match the inversion: **Overview at `GET /`**, **records at `GET /records`**.

This is a breaking change to the console's own URLs. It is deliberate. The front page should be the
question, not the evidence; the evidence lives one click away under a route that says what it is.

**The reader, stated plainly.** A compliance officer or CISO who has been handed a running console and
told "look at this". They do not know what a hash chain is. They do know what "no tampering found" and
"nobody has checked" mean, and they will notice the difference. Every string on this page is written for
them, and the page never uses a word it has not either explained or made unnecessary.

---

## 2. What is already fixed, and what this must not disturb

Carried over from the console's existing rules (`.clinerules`, and the `serve` design of 2026-10-06):

- **Strictly read-only and GET-only.** Every route is GET or HEAD; anything else is a 405 with no body.
  The ledger is built with a nil signer, so `Append` refuses.
- **No new dependency, no `go.mod` change.** `net/http`, `html/template`, `go:embed` are stdlib.
- **`html/template` autoescaping is never turned off.** No `template.HTML`, `template.JS` or
  `template.URL` anywhere. Memory text, ids and upstream detail are all untrusted.
- **No handler sets security headers.** The outermost `securityHeaders` middleware
  (`internal/serve/security.go`) sets all four on every response, including the new route.
- **Whole-ledger status is stated as whole-ledger.** The chain verdict covers the entire ledger, not the
  window a view happens to be showing; the page says so in words rather than leaving a reader to assume.
- **Every figure shown is reproducible by an existing CLI command, and the page names it** (decision 5).
- **No new verification logic.** The status card is driven by `loadChain` → `ledger.CollectBreaks`, and
  the gap line by `ledger.OutstandingGaps`, unchanged.
- **The console cannot drift from the CLI.** Records still render through `export.Render`.

---

## 3. The decisions, with their costs

**1. One aggregate read serves the ledger facts, and `extent()` delegates to it.**

`internal/serve/views.go` already has `extent()`, returning earliest/latest/count by walking every stored
row. Its doc comment says it is *"called ONLY on the records view's empty path — the common case must not
pay for the rare one."* The Overview inverts that: the facts are wanted on **every** load of the **most
frequently loaded page**. So a single read-only aggregate is added to the store —
`LedgerStats() (Stats, error)` — returning the row count, the earliest and latest event time, and the
count per tier, from two small SQL aggregates rather than a full table read.

`extent()` then becomes a thin adapter over `LedgerStats`, so the console has **one** definition of "how
many records, and what span". Two definitions would be the drift this project keeps extracting functions
to prevent.

*Cost, recorded rather than hidden:* `extent()` used to skip a row whose payload would not decode.
`LedgerStats` counts stored rows, so such a row is now counted, and a row whose `at` cannot be parsed is
an error rather than a silent omission. That is the honest reading — the row is in the ledger, and
`notary verify` names it as a break — but it is a behaviour change on a corrupt ledger, and it is why the
old comment's "skip the undecodable" nuance disappears.

**2. The nav is data, in one place.**

One slice of `{Href, Label, Section}` in `internal/serve`, rendered by the layout template. The current
section is marked by the same key each page already carries. Adding a page is one line, and the four
entries the brief asks for today are joined later without touching any template.

*Cost:* none worth naming. The alternative — four links written into `layout.html` — is what the brief
forbids ("build it as a list in one place").

**3. Three states, no fourth.**

The card's state is derived from the `chainView` the banner already computes: `Clean`, `NotVerified`, or
`Broken`. There is no new classification, and no path that can render a verdict without a run behind it.

*Cost:* the Overview and the per-page banner now show the same verdict twice when a reviewer is on a
record page. That is the intended redundancy — a verdict you have to navigate to find is a verdict most
readers never see.

**4. The first-run note is a read-only textarea, not a link.**

A `<textarea readonly>` holding the note, so "select and forward" is one gesture and needs no JavaScript.
It is server-rendered text inside the page, so it works with scripting off and carries no absolute URL.

*Cost:* a textarea is a form control on a page with no other input, which reads slightly oddly. Accepted
for the affordance: a `<pre>` is selectable but gives the reader nothing to click.

**5. Each figure names the command whose output contains it.**

- Record count and the chain verdict — `notary verify`.
- The time span and the per-tier counts — `notary export`, whose JSONL carries `at` and `tier` per line.
- The gap count — `notary gaps`.

*Why not a new flag:* no existing command prints a per-tier tally, and adding `--stats` to `export` would
be new CLI surface in a phase whose subject is a page. Naming the command whose output *contains* the
figure is truthful, and the reviewer or engineer can reproduce it exactly.

*Cost:* reproducing the tier split means piping `notary export` through a counter rather than reading one
number off a line. The page says what to run; it does not pretend the number is printed.

**6. The tier labels are the plain ones, with the shape and the text label kept.**

"Seen directly" / "Inferred" / "Not visible to Notary" for Observed / Reconstructed / Internal, reusing
`.tier-observed` (solid), `.tier-reconstructed` (dashed) and `.tier-internal` (dotted). The badge always
carries its name as text, so the shape is redundant with the word rather than the only signal.

*Cost:* two vocabularies now exist — the ledger's (`Observed`/`Reconstructed`/`Internal`, used by
`export`, `explain` and the records table) and the Overview's. The page maps between them explicitly and
says so, rather than translating anywhere else.

**7. The old records URL is not redirected.**

`GET /` is the Overview; a bookmark of `/?from=…` now lands on the Overview, which ignores the query. A
redirect from `/` to `/records` carrying the query was considered and rejected: it would make the Overview
unreachable at the root for anyone with a stale bookmark, and a front page that redirects on some inputs
is a front page whose URL does not mean one thing.

*Cost:* a saved records link opens the Overview. The nav makes the records view one click away.

---

## 4. Routes and navigation

| Route | Page | Section key |
|---|---|---|
| `GET /` | **Overview** (new) | `overview` |
| `GET /records` | Records (moved from `/`) | `records` |
| `GET /record?id=` | One record | `records` |
| `GET /memory?id=` | One memory | `records` |
| `GET /gaps` | Gaps | `gaps` |
| `GET /verify` | Chain detail | `verify` |
| `GET /assets/` | Embedded assets | — (no nav) |

The nav is **Overview · Records · Gaps · Verify**, in that order, rendered from the one list — the brief
calls the last entry "Verify" while its route is `/verify`.

Every existing link that points at the records view is updated: the nav, the footer's "back to records",
the per-record and per-memory back links, the records empty state's "show them" link
(`widerWindow`, which builds `/?from=…`), and the filter form's `action` **and** `hx-get` — both, because
the form is an ordinary GET that htmx upgrades, and a mismatch would make the scripted and unscripted
paths disagree.

The root handler keeps the `r.URL.Path != "/"` guard that 404s any path no route claims; that guard moves
with the handler.

---

## 5. The status card

Three states, driven only by `chainView`. The wording is fixed here so it cannot drift from the banner's
rules, which are: a verdict is never shown without the run behind it, and the re-run command is always
carried.

**Intact**

> **No tampering found as of 2026-10-21 20:58:09 GMT**
> Every record's hash and signature checks out across the whole ledger.
> Re-run: `notary verify`

**Not verified**

> **No trusted keys are configured, so signatures were not checked**
> The ledger's chain can still be walked, but without a keyring Notary cannot tell you the records are
> authentic — only that they are internally consistent. Point `NOTARY_TRUSTED_KEYS_PATH` at a keyring of
> base64 ed25519 public keys; `notary doctor` checks it.
> Re-run: `notary verify`

**Problem found**

> **8 breaks found across the whole ledger**
> Something in the trail does not match what Notary recorded. Open the chain page for the record and field
> that changed.
> → See the detail (`/verify`)
> Re-run: `notary verify`

Rules the card obeys, all inherited: it never says *verified* or *intact* without the run that produced
the verdict; the instant is the run's own `AsOf`; "whole ledger" is stated rather than implied.

---

## 6. The ledger facts

| Figure | Shown as | Reproduced by |
|---|---|---|
| Records | `1,284 records` | `notary verify` |
| Span | `From 2026-05-02 09:14 GMT to 2026-10-21 18:02 GMT` | `notary export` |
| Tiers | `Seen directly 1,102 · Inferred 96 · Not visible to Notary 86`, each with its badge shape and text | `notary export` |

The section is titled "What is in this ledger", and carries one line naming the commands, so a reader
never has to wonder where a number came from.

An empty ledger renders the first-run state (§8) **instead of** this section, not alongside a row of
zeroes.

---

## 7. The gaps line

> **3 moments Notary did not record** → See them (`/gaps`)

or, when there are none:

> **None found** — every operation Notary watched was recorded.

Counted from `ledger.OutstandingGaps` alone — the same source `notary gaps` reports — so the Overview and
the gaps page cannot disagree. Reproduced by `notary gaps`.

---

## 8. The first-run empty state

Shown when the ledger holds no records at all. Two or three sentences of explanation, then the note.

The explanation says what Notary does and why the page is empty: it records an agent's memory activity,
nothing is recording into this ledger yet, and once an engineer connects the application there will be
something here to review.

The note is the part the reviewer forwards. It names the three things that must be set up, with this
deployment's own paths filled in by the server:

```text
Notary is running, but nothing is recording into this ledger yet.

To connect it:
  1. Point your application's memory calls at Notary -- either construct the
     library interceptor, or set the application's Mem0 base URL to `notary proxy`.
  2. Make sure it writes to this ledger: NOTARY_DB_PATH=C:\Dev\notary\notary.db
  3. Signing is what lets the trail be checked rather than just displayed. Set
     NOTARY_SIGNING_KEY, write the public half with
     `notary doctor --generate-key --trusted-keys-out <path>`, then set
     NOTARY_TRUSTED_KEYS_PATH to that file.

Full details: docs/OPERATIONS.md
```

The ledger path is the one this console actually opened, so the note is specific to this deployment rather
than generic advice. The note is escaped page text — it is not a template and contains no markup a stored
value could influence.

*Cost:* this is the one place a reviewer-facing page names environment variables. It is deliberate: the
note's reader is the engineer it is forwarded to, and vague advice ("ask your administrator to configure
Notary") is what makes a first-run page useless.

---

## 9. What it must not do

- **No new verification.** The card and the gap line read the existing verdict and report; they do not
  compute one.
- **No write, no endpoint, no flag.** Nothing on this page can append, reconcile, or open the gap log for
  writing.
- **No absolute URL.** Every link is relative, so the page still works offline and from disk.
- **No `template.HTML`.** A memory's text, an id, a gap's detail: all escaped.
- **No figure without a command.** If a number cannot be reproduced, it is not shown.
- **No fourth state.** A ledger that is empty is the first-run state; a ledger whose check could not run is
  *not verified*; it is never "probably fine".

---

## 10. Limitations recorded, not hidden

- The Overview states the verdict for the **whole ledger**. It cannot tell a reviewer whether a particular
  memory is trustworthy; that is the record and memory pages' job.
- A ledger whose rows will not decode at all is still reported as *not verified* / *broken* rather than as
  empty, because "empty" would be a claim the console cannot support.
- The tier split is from stored `tier` columns; a hand-edited tier column that disagrees with its payload
  is a `decode` break on `/verify`, not a correction here.

---

## 11. Testing

New and updated tests, all in `internal/serve`:

1. **Three states, differentially.** The intact fixture, the tampered fixture (the one `notary verify`
   fails on), and a no-keyring fixture each render their own card, and the intact/tampered pair is checked
   against `notary verify`'s own answer for the same ledger — the console may not disagree with the
   command.
2. **Empty ledger.** A ledger with no records renders the first-run page, with the note, a 200, and no
   error; it must not render an empty facts table or a "0 records" card that reads as a finding.
3. **Tier counts match the ledger.** Counted from the fixture's own records, not from the page.
4. **Stored text cannot become markup.** A memory whose content is `<script>alert(1)</script>` appears
   escaped, and no `<script>` element appears in the body.
5. **No absolute URLs.** No `http://`, `https://` or `//` in any rendered page's links.
6. **Nav on every page**, with the current section marked, and the four entries in order.
7. **Route move.** `/records` serves the records view; `/` serves the Overview; the old
   `/?from=…` behaviour is gone; the filter form's `action` and `hx-get` both point at `/records`.
8. **`LedgerStats` itself**, in the store's own tests: counts, span and per-tier tally over a known set,
   and an empty ledger returning a zero stat without error.

---

## 12. Sequencing

1. The aggregate read (`store.LedgerStats`), `extent()` delegating to it, tests first.
2. The nav list and the route move, with the pinned route tests updated.
3. The Overview page: card, facts, gaps, empty state.
4. Tests 1-7 above.
5. README and `docs/OPERATIONS.md`.

Each step lands green before the next, per the project's phase discipline.
