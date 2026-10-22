# Notary — `serve`: the As-of page: Design

**Status:** approved for implementation. The operator's brief fixes the semantics, the route and the
states; the decisions below are what it left open, settled here with their costs stated.

---

## 1. What this is for, and what the artefact is

The console can show the ledger's current contents and answer "can I trust this trail?". It cannot answer
the question an auditor actually arrives with: **what had Notary recorded by this moment?**

That is `notary replay`'s question, and the answer already exists in `internal/replay`. The As-of page is
the browser form of it: a reviewer picks an instant, and sees the records Notary had recorded at or before
it. The value is the *as-of* framing — a record written after the chosen moment is **correctly absent**, and
the page says so — because "this memory was dropped" and "this drop had not been recorded yet" are
different facts, and only a point-in-time view can tell them apart.

**The reader** is an auditor or a compliance reviewer who has a date — the date of an incident, a customer
complaint, a breach notification — and wants to know what the trail said at that point. They do not know
what `RecordedAt` means, and the page must not require them to.

**It is not a new read path.** Every rule the page applies is `internal/replay`'s: the inclusive boundary,
the chain-prefix verification, the scope conjunction, the redaction. The console parses a form into a
`replay.Request`, runs the real `replay.Replayer`, and renders what came back.

---

## 2. What is already fixed, and what this must not disturb

- **Read-only, GET-only.** No route writes; the ledger is built with a nil signer.
- **No new dependency, no `go.mod` change.** `internal/replay` is already imported by `cmd/notary`.
- **No reimplementation** of replay, the prefix check, the scope filter or redaction. If the console needs
  behaviour `internal/replay` does not have, that is a package change, not a console one.
- **Autoescaping never off.** No `template.HTML`. The date, time, zone and scope values are the reader's
  own input and are echoed back — they are exactly what a hostile echo would target.
- **Security headers from the outermost middleware only.** No handler sets one.
- **500s go through `internalError`.** Validation messages use the existing `data.Err` pattern.
- **No absolute URL** on the page.
- **Never claim a verified or intact result without the run behind it.**

---

## 3. The decisions, with their costs

**1. The page runs `replay.Replayer.Replay` and decodes its own JSONL.**

`Replayer.Replay` writes export lines to an `io.Writer`; the console needs the records to render a table. It
passes a `bytes.Buffer`, then decodes each line into `export.Line` — the type the Records view already
renders — and builds the same `recordRow`s through `badgeFor`.

*Why not read the ledger directly:* calling `ledger.ReplayAsOf` and then applying `export.ScopeMatches` and
`export.Render` in the console would be a second implementation of exactly the ordering, scope conjunction
and redaction rules the brief forbids reimplementing. The buffer makes the console a *renderer* of replay's
output rather than a peer of it.

*Cost:* one JSON round-trip per record. A decode failure can only mean replay's encoder and the export line
type disagree, which is an internal error — it goes through `internalError`, never to the reader.

**2. The records table becomes a shared partial.**

The table markup lives inline in `index.html`. It moves to `partials.html` as `recordTable`, taking a
caption and the rows, and both pages render it. *Why:* the brief says the As-of results use **the same**
table and badges as the Records view; two copies would be two definitions that drift, which is the failure
mode this project keeps extracting shared functions to prevent.

*Cost:* `indexData` gains a small struct and the records handler assembles it. The markup is moved
verbatim, including the `data-*` attributes, so the existing differential test (`parseRows`) keeps working —
and can be reused for the As-of page, which is how the new differential is written.

**3. The time zone is a required choice with no default.**

A date and a time without a zone is the bug this page exists to avoid, so the form has three fields — date,
time, zone — and the zone select starts on "Choose a time zone", not on UTC. A submission missing a zone is
a validation message, not a silent assumption.

*Cost:* one more click for every request. That friction is the feature: the alternative is a page that
silently answers a question the reader did not ask, in a zone they did not choose.

*The offsets offered* are `UTC` plus a short list of fixed offsets (`-08:00`, `-05:00`, `+00:00`, `+01:00`,
`+05:30`, `+10:00`), and the parser accepts any `±HH:MM` the form is ever changed to send.

**4. The "recorded after the instant" count is not shown.**

`replay.Result` exposes `Records`, `Redacted` and `Breaks` — no count of records written after the instant,
and computing one in the console would mean a second read of the ledger with a different boundary rule.
The brief says not to invent it. The page shows the count it has, and the sentence explaining that later
records are absent does the explanatory work.

*Cost:* a reader cannot see *how much* is hidden by the instant. Saying that plainly in the design is
better than a number the package does not stand behind.

**5. No keyring is a stated state, not an error page.**

`notary replay` refuses to run without trusted keys, because a replay it cannot verify is not evidence.
The page says the same thing in the same words the Overview card uses — *no trusted keys are configured, so
signatures were not checked* — and runs nothing.

*Cost:* the As-of page is unusable on an unconfigured deployment, exactly as the CLI is. That is the
consistent choice: a console that showed unverified replays would be a console that let a reviewer read an
unverified prefix as "what Notary knew".

**6. A verified prefix with breaks is shown, with the verdict.**

`Result.Breaks` is not an error: replay still renders the prefix. The page renders the rows **and** the
verdict, in the Overview card's language, naming the count and linking to `/verify` for the detail — a
replay of a damaged ledger is evidence of the damage, and hiding the rows would hide that.

*Cost:* a reader can see rows and a "breaks found" banner together. The banner is what makes the rows
readable as evidence rather than as truth.

---

## 4. Route and navigation

| Route | Page | Section key |
|---|---|---|
| `GET /asof` | **As of** (new) | `asof` |

The nav list gains `{Href: "/asof", Label: "As of", Section: sectionAsOf}` **between Records and Gaps**, so
the bar reads Overview · Records · As of · Gaps · Verify — the evidence views together, the point-in-time
one next to the one it is a view of.

---

## 5. The form

```text
Date        [2026-09-28]      Time [12:00]      Time zone [Choose a time zone ▾]
Person      [                ]   Agent [        ]   Application [        ]   Session [        ]
                                                    [ Show me ]
```

- `method="get" action="/asof"`, with `hx-get="/asof" hx-target="#asof" hx-select="#asof" hx-swap="outerHTML"`
  — the same enhancement the Records filter uses, so the page works with scripting off.
- Date `YYYY-MM-DD`, time `HH:MM` or `HH:MM:SS`, zone `UTC` or `±HH:MM`.
- Scope fields are labelled in the reader's words (Person, Agent, Application, Session) and carry the
  ledger's names (`user_id`, `agent_id`, `app_id`, `run_id`). They compose with AND, as `export`'s do.
- No `reveal` checkbox in the form: the reveal toggle is the view's own, as on the record and memory pages.

---

## 6. The states

| State | What the page says |
|---|---|
| **No instant given** | The form, and one line explaining what it will answer. No error, no request run. |
| **Invalid or ambiguous input** | A plain message naming the field: "the date must be YYYY-MM-DD", "choose a time zone". Never a storage error, never a 500. |
| **No trusted keys** | The not-verified wording from the Overview card, and the fix. Nothing is run. |
| **Instant before the first record** | "Nothing had been recorded by this moment." |
| **Empty ledger** | The same sentence: there is nothing to have recorded. |
| **Verified prefix** | The echo, the sentence, the count, the table, the reproduce line. |
| **Prefix with breaks** | The same, plus the verdict naming the break count and linking to `/verify`. |
| **Unreadable ledger** | 500 through `internalError`, generic body, detail to the log. |

The echo reads: **"Showing what Notary had recorded by 2026-09-28 12:00:00 UTC (2026-09-28T12:00:00Z)."**
followed by **"Records written after this moment are correctly absent — this is what Notary knew then, not
what it knows now."**

---

## 7. The reproduce line

```text
Reproduce: notary replay --at 2026-09-28T12:00:00Z --user-id 'u1' --run-id 'run-7'
```

Built from the resolved instant (RFC3339, in UTC) and only the scope flags actually used. Values are
shell-quoted: a value with a space, a quote or a `$` must land in the command as one argument, and a value
that could close the quoting must not be able to inject a second command. A test asserts a hostile scope
value comes out quoted and inert.

---

## 8. Testing

1. **The boundary is inclusive.** A record whose `RecordedAt` equals the instant is shown; one a nanosecond
   later is not. This is the rule the whole page rests on.
2. **Differential against `replay.Replay`.** For the same request on the same fixture, the console's record
   set — read out of the rendered table with the existing `parseRows` — equals what `replay.Replay` returns.
3. **A missing and a garbage instant** render the explanation, not a default and not a 500.
4. **Two zones naming the same moment** echo the same instant.
5. **Hostile input** in the date, zone and scope fields is escaped in the echo.
6. **Redaction**: sensitive content is withheld by default and shown only with the toggle.
7. **Scope filters compose with AND.**
8. **A no-keyring fixture** renders the not-verified wording and runs nothing.

---

## 9. Sequencing

1. The shared table partial, with the records view still green.
2. The route, the nav entry, and the instant parser (`date` + `time` + `zone` → instant), tested alone.
3. The page: the replay call, the decode, the states, the echo and the reproduce line.
4. Tests 1-8.
5. README, `docs/OPERATIONS.md`, and the pinned route / offline sweeps.

Each step lands green before the next.
