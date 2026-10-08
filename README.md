<img src="docs/assets/banner.png" alt="Notary — a signed, tamper-evident audit trail for agent memory. Three chain links labelled OBSERVED, RECONSTRUCTED and INTERNAL, drawn solid, dashed and dotted to show decreasing confidence." width="100%">

# Notary

![Go](https://img.shields.io/badge/Go-1.25-00ADD8?logo=go&logoColor=white)
![SQLite](https://img.shields.io/badge/SQLite-modernc.org%2Fsqlite-003B57?logo=sqlite&logoColor=white)
![cgo](https://img.shields.io/badge/cgo-not_required-brightgreen)
![Dependencies](https://img.shields.io/badge/direct_dependencies-4-blue)
![tests](https://github.com/ranscky/notary/actions/workflows/test.yml/badge.svg)
![Scope](https://img.shields.io/badge/scope-ledger_reconciler_export_replay_explain_proxy_serve_report_doctor_(phases_1--12)-orange)

**A signed, tamper-evident audit trail for agent memory — one that records _why_ a memory was kept, dropped, or surfaced, not just that it was.**


Mem0 will tell you what it stored. It will not tell you why it *didn't* retrieve something — and for a
team under audit, "the model didn't return it" is not an answer anyone can accept.

Notary sits in front of your Mem0 calls and writes down what it witnessed, in a hash-chained ledger
you can verify offline. Where it cannot know the answer, it says so explicitly instead of guessing.

> **The central rule: every record is labelled with how Notary came to believe it.** A claim Notary
> watched happen is never presented with the same confidence as one it inferred — and a decision Mem0
> made privately is recorded as opaque, never dressed up as a reason.

---

## The three tiers

```mermaid
flowchart TD
    Q["An agent memory was kept, dropped, or surfaced."] --> W{"Who says so?"}
    W -->|"Notary watched it happen and can name the source"| O["Observed"]
    W -->|"Notary inferred it from a recorded basis"| R["Reconstructed"]
    W -->|"Mem0 decided internally and exposes no reason"| N["Internal"]
```

| Tier | Means | Example |
|---|---|---|
| **`Observed`** | Notary directly witnessed it. The source is recorded (`mem0_response` or `notary_instrumentation`). | `stored_by_mem0` — the memory appeared in a `get_all` listing. |
| **`Reconstructed`** | Notary inferred it. The basis and the rule version are recorded alongside. | `kept_by_content_match` — inferred from a content-hash match, not seen. |
| **`Internal`** | Mem0 decided internally and exposed no reason. An opacity marker, never a guess. | `removed_by_mem0` — history shows a DELETE with no reason given. |

This is enforced by construction, not by convention: `Observed`, `Reconstructed` and `Internal` are
built by **three separate constructors that take disjoint evidence types**. There is no builder that
accepts a tier as an argument, so "record a guess as a fact" does not type-check. Negative-compile
fixtures in `testdata/negative/` fail the build if anyone tries — including forging a tier, building a
`Reason` without a constructor, or fabricating `Observed` evidence from nothing.

---

## Architecture

```mermaid
flowchart LR
    App["Your application"] -->|"Add / Search"| IC["Notary interceptor"]
    IC -->|"the real call, always"| Mem0["Mem0 API"]
    IC -->|"Observed records"| Ledger[("Ledger<br/>hash-chained SQLite")]
    IC -.->|"if the ledger write fails,<br/>record a gap instead —<br/>the Mem0 call is never blocked"| Gap[("Gap log<br/>its own hash chain")]
    Ledger --> Verify["notary verify"]
    Gap --> Verify
    Ledger --> Gaps["notary gaps"]
    Gap --> Gaps
```

One hash chain, written by the interceptor on the request path and by the reconciler off it. The gap
log is deliberately a **separate file with its own chain and its own signed checkpoints**: it exists
for the case where the store is unavailable, so sharing a store would defeat it.

---

## What it records

| Event | When | Tier |
|---|---|---|
| `add_requested` | an add is acknowledged by Mem0 | `Observed` |
| `add_resolved` | the add's outcome is later polled | `Observed` |
| `search_performed` | a search runs (query, filters, `top_k`, `threshold`, result count) | `Observed` |
| `memory_surfaced` | one record per returned memory, with its score and rank | `Observed` |
| `memory_kept` | a memory is seen in a listing, or inferred by content match | `Observed` / `Reconstructed` |
| `memory_dropped` | a memory known to exist was not returned, or history shows a removal | `Reconstructed` / `Internal` |
| `audit_gap` | the audit trail itself failed | `Observed` |

---

## Guarantees

**Tamper-evident chain.** Each record's hash covers its canonical bytes, its position, and its
predecessor's hash: `sha256("notary/record/v1" ‖ canonical ‖ uint64-BE seq ‖ prev-hash)`. Every
variable-length field is length-prefixed and every hash is domain-separated, so a field boundary
cannot be shifted to make two different records collide.

**Truncation detection.** A hash chain cannot see its own removed tail — what remains is
self-consistent. So `verify --write-checkpoint` emits a **signed head checkpoint**, and
`verify --checkpoint` fails loudly when the chain is shorter than the checkpoint attests. Plain
`verify` passes on a shortened chain by design; the checkpoint is what catches it.

**Fail-open-loud.** An audit-write failure must never break the customer's application. The Mem0 call
always completes; the failure is recorded durably in the gap log and mirrored to a loud channel. The
gap log has its own chain, so a gap cannot be silently erased.

**Idempotent by construction.** Idempotency keys are deterministic and content-derived — never
wall-clock, never random. Re-appending an existing key is a no-op that returns the existing record,
so retries and re-runs cannot duplicate the trail.

**No secrets in output.** Keys come from the environment only, and key material is redacted by
construction on every rendering path — `%v`, `%+v`, `%#v` and `json.Marshal` included — with a
canary test asserting it.

---

## Quickstart

```bash
go build ./...
go run ./cmd/notary version
```

`gaps` needs no configuration at all — it is read-only and requires no signing key:

```bash
go run ./cmd/notary gaps
```

`doctor` looks at this deployment and says what is missing, with the exact command that fixes each finding;
it exits non-zero when any finding is an error, so it can gate a pipeline. It is also the one command that
creates key material, and one call produces both halves of a fresh key pair — the private half printed as
the `export` line to put in the environment, and `--trusted-keys-out` writing the matching public half, the
one-base64-line-per-key file `NOTARY_TRUSTED_KEYS_PATH` names. So the trusted-keys file `verify`, `replay`
and `report` read needs no external tool to produce it:

```bash
go run ./cmd/notary doctor                                  # what is wrong here, and how to fix it
go run ./cmd/notary doctor --generate-key --trusted-keys-out trusted-keys.txt
# prints:  export NOTARY_SIGNING_KEY=<base64 seed>    # the private half; nothing is written for it
# writes:  trusted-keys.txt (mode 0644)               # the public half, no external tool needed
```

`verify` needs a ledger and a keyring of trusted public keys (base64 ed25519 public keys, one per line —
the key ID is derived from the key, so the file can never disagree with itself). A signing key is needed
only by `--write-checkpoint` and `--write-gap-checkpoint`, which sign a fresh attestation; plain `verify`,
`--checkpoint` and `--gap-checkpoint` read and check, and never sign:

```bash
export NOTARY_DB_PATH=notary.db
export NOTARY_TRUSTED_KEYS_PATH=trusted-keys.txt

go run ./cmd/notary verify                            # walk the chain
go run ./cmd/notary verify --write-checkpoint cp.json # attest the head
go run ./cmd/notary verify --checkpoint cp.json       # detect a removed tail
```

`report` renders a slice of the ledger as a folder of static, self-contained HTML pages — an index, one page
per memory, one per record, and the chain state — to open from disk, attach to a ticket or hand to an
auditor. It is bounded by construction: exactly one subject, either `--memory <mem0-id>` or `--from`/`--to`
narrowed by the four scope flags, and there is no `--all`:

```bash
export NOTARY_DB_PATH=notary.db
export NOTARY_TRUSTED_KEYS_PATH=trusted-keys.txt

go run ./cmd/notary report --out ./report --from 2026-09-01T00:00:00Z
go run ./cmd/notary report --out ./report --memory <mem0-id>
```

`export` renders a range of the ledger to stdout as JSONL — one stable object per record — so an audit
can be diffed, grepped and reviewed. It is a read path: it never writes the ledger, and its only
output of its own is the optional checkpoint file.

```bash
export NOTARY_DB_PATH=notary.db
export NOTARY_SIGNING_KEY=<base64 ed25519 key>

go run ./cmd/notary export --from 2026-09-01T00:00:00Z
go run ./cmd/notary export --from 2026-09-01T00:00:00Z --checkpoint-out cp.json
go run ./cmd/notary verify --checkpoint cp.json    # verify the range the export covers
```

- **A signing key is required**, even when `--checkpoint-out` is not given. Like `reconcile`, `export`
  refuses to start without one rather than silently downgrade, so a keyless stdout-only export is not
  possible in v1.
- **Sensitive content is withheld by default.** A line whose stored content is marked sensitive carries
  the subject's `content_hash` and `"redacted":"sensitive"` instead of the text, so a reader can always
  tell "not shown" from "nothing was there". `--include-sensitive` prints the stored text instead; it
  changes only what is printed — never a hash, because rendering is a read path.
- **`--checkpoint-out <path>`** writes a signed head checkpoint in the exact format
  `verify --checkpoint` already reads, attesting the ledger head at export time (not the last record in
  the range), so a truncated tail is detectable later.
- **`--phrase` is opt-in** and adds an optional, display-only language-model paraphrase beside each
  record — never instead of it. It is off by default, so a provider is only billed for an export you
  asked for. It needs provider configuration from the environment: `NOTARY_PHRASE_MODEL` and
  `NOTARY_PHRASE_BASE_URL`, plus `NOTARY_PHRASE_API_KEY` for endpoints that need one. A phrasing
  failure never fails the export; it degrades to a note on stderr.
- **Scope flags** `--user-id`, `--agent-id`, `--app-id` and `--run-id` restrict the export to one
  entity scope and combine with AND, mirroring `reconcile`.
- **`--from` is required** and `--to` defaults to now. The range is bounded on each record's *event*
  time (`At`), not its write time (`RecordedAt`), so a record written late about an old event still
  belongs to that event's window.
- **`--max-span`** caps the range (default 366 days). The cost of an export scales with the range, so
  an over-long range is an error rather than an out-of-memory kill.

Redaction follows content marked sensitive at **write** time. An application marks it either per call
(`library.Sensitive()`) or declaratively, by pointing `NOTARY_SENSITIVITY_RULES` at a YAML rules file:

```yaml
# Any rule that matches marks the record's content Sensitive at write time.
rules:
  - name: health-data
    match:
      scope:
        user_id: patient-42      # optional; omitted means any scope
      metadata:
        category: health         # matches a proxied add or a surfaced memory; see the note below
```

A rule matches when **every** clause it specifies matches, and a rule specifying neither clause is
rejected at load time. The rules file is read by the write paths — an application that constructs the
interceptor, and `notary proxy` — so there is no `export` flag for it: `export` writes no records.

One asymmetry matters when you write a rules file: **whether a `metadata:` clause can match an add
depends on the mode.** `library.Add`'s signature carries no metadata at all, so in library mode a rule
whose only clause is `metadata:` can never mark an add — only a `scope:` clause can. `notary proxy`
decodes each add's `metadata` from the request body, so the same rule CAN mark a proxied add. If the
content you mean to protect arrives through `library.Add`, match on `scope:`.

All configuration is environment-only:

| Variable | Purpose |
|---|---|
| `NOTARY_MEM0_API_KEY` | Mem0 API key |
| `NOTARY_MEM0_BASE_URL` | Mem0 base URL (default `https://api.mem0.ai`) |
| `NOTARY_DB_PATH` | ledger database (default `notary.db`) |
| `NOTARY_GAP_LOG_PATH` | gap log (default `notary-gaps.log`) |
| `NOTARY_SIGNING_KEY` | base64 ed25519 signing key — a 32-byte seed or a 64-byte private key. The key material goes **in** this variable; nothing reads an env var for its name. |
| `NOTARY_TRUSTED_KEYS_PATH` | file of trusted public keys |
| `NOTARY_SENSITIVITY_RULES` | path to a YAML sensitivity-rules file, read by the write paths (an application constructing an interceptor, and `notary proxy`); there is deliberately no CLI flag — see the export section |
| `NOTARY_PHRASE_BASE_URL` | OpenAI-compatible base URL for `--phrase` (required by `--phrase`) |
| `NOTARY_PHRASE_MODEL` | model name for `--phrase` (required by `--phrase`) |
| `NOTARY_PHRASE_API_KEY` | API key for `--phrase`; optional for endpoints that need none |

---

## Status

This is the **core ledger, the reconciler, `export`, `replay`, `explain`, `proxy`, `serve`, `report` and `doctor`: phases 1–12** of the design's phase table, complete and tested. Phases 11 (`report` and `doctor`) and 12 (`notary serve`) sit beyond v1's ten rows; Phase 11 was designed first and numbered then, and `serve` was built and merged while it was still in progress, so Phase 11 landed last.

**Working today**

- Record schema, unforgeable tiers, canonical hashing, negative-compile fixtures
- SQLite store, hash-chained writes, crash safety, idempotent append
- Signing, keyring, signed head checkpoints, cross-domain replay protection
- `notary verify` (chain, truncation, gap cross-check) and `notary gaps`
- Gap log with its own chain and signed checkpoints
- Fail-open-loud interceptor and the in-process Mem0 interceptor (`Add`, `Search`)
- Gap reconciliation as a library API (`AuditWriter.ReplayGaps`) — **no CLI** yet, because a gap
  entry records which record is missing, not the record itself, so replay needs the caller's records
- **`notary reconcile`** — a one-shot, schedulable pass that derives and records the claims the
  ledger cannot observe directly: it polls Mem0 for unresolved adds, enumerates a scope to classify
  memories as kept or removed, and reads recorded searches to classify a memory the search did not
  return as dropped. It is bounded by `--since` (on each record's event time, not its write time) and
  the four scope flags, supports `--dry-run`, and keys every derived claim on its subject and reason
  kind (the rule version, where a rule justifies the inference) rather than on the run, so re-running
  it against an unchanged store appends nothing. Every
  inference is named by a rule in a versioned registry. `notary reconcile` is the first command to
  consume `NOTARY_MEM0_API_KEY`.
- **`notary export`** — streams a range of the ledger to stdout as JSONL, one stable object per
  record, and (with `--checkpoint-out`) writes a signed head checkpoint that `verify --checkpoint`
  reads back. It is bounded by `--from`/`--to` (on each record's event time, not its write time) and
  the four scope flags, and capped by `--max-span`. Sensitive content is withheld by default and its
  line says so; `--include-sensitive` prints it and changes no hash. `--phrase` adds an opt-in,
  display-only paraphrase that degrades to a note rather than failing the export. Like `reconcile`,
  it requires a signing key even when it writes no checkpoint.
- **`notary replay`** — streams the ledger as it stood at one instant to stdout as JSONL, one stable
  object per record, byte-identical to an exported line: the records whose `RecordedAt` (the write
  time — the knowledge instant — not the event time) is at or before the required `--at`, in Seq
  order, verified as a chain prefix before they are written. The four scope flags restrict the replay
  to one entity scope, and an unparseable `--at` is refused rather than silently treated as now.
  Sensitive content is withheld by default and its line says so; `--include-sensitive` prints it and
  changes no hash. Because it verifies rather than signs, it requires a trusted-key file and no
  signing key, and it reports any break in the prefix on stderr and exits non-zero, leaving stdout as
  the JSONL stream it promised.
- **`notary explain`** — answers one question about the ledger as prose: why a single record was
  written, or what happened to a single memory over its life. The subject is one `<record-id>`
  argument or `--memory <mem0-id>`, never both, and the memory view renders one line per record in
  Seq order, each line carrying the record's event, reason kind, tier and **both of its instants** —
  when the event happened (`At`) and when Notary wrote the claim (`RecordedAt`) — so a claim written
  long after the event it describes can be seen as that. `--json` prints the same records, in the
  same order, as one small JSON object instead of prose: `{"records": [...]}`, each record carrying
  `seq`, `id`, `memory_id`, `event`, `reason_kind`, `tier`, `at`, `recorded_at` and `sentence`, plus
  `content` only when the record carried content that was shown and `redacted` only when content was
  withheld. Sensitive content is withheld by default and the view says so; `--include-sensitive`
  prints it and changes no hash. It is a read path that signs and verifies nothing, so it needs
  neither a signing key nor a trusted-key file.
- **`notary serve`** — serves the read-only ledger dashboard on the loopback address `127.0.0.1`, so a
  reviewer can read the trail in a browser instead of a terminal: the same records `export`, `explain`,
  `gaps` and `verify` report, rendered as pages. The records view defaults to the last 30 days and is
  narrowed by range and scope in the page; each record opens its own page, each memory its lifecycle, and
  the gaps and verification pages report what `notary gaps` and `notary verify` report. Sensitive content
  is withheld by default and a per-view toggle reveals it for that view only, writing one audit line to
  stderr each time and never changing a hash. `--port` is its only flag: it binds `127.0.0.1` and only
  `127.0.0.1` (there is no host or address flag) and sets no CORS header, so a foreign page can neither
  reach nor read it. It is structurally read-only — the ledger is opened with no signer, so it can never
  append — and it needs neither a signing key nor a trusted-key file: a keyring, when one is configured,
  only enables the chain banner's verdict.
- **`notary report`** — renders a slice of the ledger as a folder of static HTML pages: an index, one page
  per memory, one page per record, the chain state in full, and the stylesheet and filter script they link
  to. The slice is bounded by construction — one subject, `--memory <mem0-id>` or `--from`/`--to` (bounded on
  each record's event time) narrowed by the four scope flags, with no `--all` — and `--out` is required,
  refused when the directory already holds files unless `--force` is given. Sensitive content is withheld by
  default and the page says so; `--include-sensitive` prints it and changes no hash. It needs no signing key
  (it never signs) and `--no-verify` needs no keyring at all — `verify.html` is still written, saying that no
  verification ran, because a skipped check must never read as a clean one. Its chain state is `verify`'s
  answer, through the same shared break collection, so the page and the command cannot disagree, and it
  covers the whole ledger rather than the slice. The ledger must already exist: `report` refuses a path with
  no ledger at it, or a zero-byte file, rather than creating an empty one, because a report over nothing and
  a report over a mistyped path must never look the same. It exits non-zero when the chain state it rendered
  is broken, **after** writing the pages, because a report of a tampered ledger is exactly the artefact its
  reader needs. It does not check that the file *is* a Notary ledger, and the code records why that belongs to
  a phase of its own: a file of exactly one byte is initialised into an empty ledger — or, in the same way, a
  foreign SQLite file given this project's schema — and renders 0 memories with exit 0. The **evidence block**
  on every record page is not filtered by `--include-sensitive`: the flag governs the record's stored content,
  the evidence is the record's own stored reason printed as the ledger hashed it, and `reconcile` records
  whole upstream objects rather than curated fields — so such a reason can quote memory text the sensitivity
  rules never marked. The index and every record page say so, and so does the help text.
- **`notary doctor`** — says what this deployment is missing and the exact command that fixes each finding:
  the ledger and the gap log (opened for real, and created when absent, with the finding saying when this run
  is the one that created them), the trusted keyring, the signing key (a warning rather than an error when it
  is absent, because the read paths do not need one), the Mem0 base URL, the Mem0 API key (required only by
  the commands that call Mem0 — `reconcile` — since `proxy` forwards the caller's own credential), the
  sensitivity rules when they are configured, and the ledger's chain state through the same shared break
  collection `notary verify` uses, reported as **not checked** — never as broken — when no trusted keys are
  configured. It exits non-zero when any finding is an error, so it can gate a pipeline; warnings alone do
  not. `--out DIR` writes the same findings to `DIR/setup.html`. **`--generate-key` is the only command that
  creates key material**, and no other command and no other flag ever does (the demo script below mints its
  own throwaway key, from `/dev/urandom`, and never writes it to disk): it prints a fresh ed25519 seed as an
  `export NOTARY_SIGNING_KEY=…` line to stdout and writes no file, `--key-out PATH` writes that private half
  to a file instead (mode `0600`, refused inside a checkout), and `--trusted-keys-out PATH`, with
  `--generate-key`, writes the matching **public** half (mode `0644`, allowed inside a checkout, because a
  public half is not a credential) as the base64 line `NOTARY_TRUSTED_KEYS_PATH` reads — so a first run needs
  no external tool to derive the public half from a key it has just made. It requires `--generate-key`
  because a loaded signer exposes no public key to recover, only its `KeyID` fingerprint and its `Sign`
  method. It needs no signing key and no Mem0 key to run.
- **`notary proxy`** — serves Mem0 traffic and records the `add` and `search` requests that pass through
  it. The application re-points its Mem0 base URL at Notary, which forwards every request to the real
  Mem0 and returns the response untouched, recording the same records library mode records for those two
  endpoints (the mode differences are noted under sensitivity rules above), with no code
  change in the application, in any language. `get_all`, history, event-status and delete pass through
  unrecorded by design. It needs a signing key (`NOTARY_SIGNING_KEY`, because ledger appends are signed),
  the upstream base URL from `NOTARY_MEM0_BASE_URL`, and the ledger and gap log paths (`NOTARY_DB_PATH`,
  `NOTARY_GAP_LOG_PATH`); its sensitivity rules come from `NOTARY_SENSITIVITY_RULES` with no flag. It
  needs **no** Mem0 API key — it forwards the caller's own `Authorization` header and holds no credential
  of its own. Its flags are `--addr`, `--queue-depth` and `--max-body`, and its stdout is unused: the
  listen banner and every drop marker go to stderr.

**Not built yet** — the README will be updated as these land rather than describing intent as fact

- **`ReconcileMode.InProcess`** — a reconcile mode that is **reserved, not implemented**: it is
  defined and explicitly rejected by validation, so a caller that runs only what this build
  implements gets a matchable error. `notary reconcile` is the one-shot command mode; no long-running
  in-process driver exists

`notary` is licensed under the **Apache License 2.0** — the full text is in [`LICENSE`](LICENSE).
Copyright 2026 Ransford Oduro.

---

## Development

```bash
go build ./...     # builds clean
go vet ./...       # clean
gofmt -l .         # no output expected
go test ./...
```

`go test ./...` runs every package the `./...` pattern matches — which is nearly every package in the tree,
because `go build`, `go vet` and `go test` all skip `testdata` directories. The two programs that live in
one are built or run explicitly instead: `internal/ledger/testdata/crashwriter`, which the crash test builds
(`internal/ledger/crash_test.go`), and `testdata/demo/seed`, which `scripts/demo.sh` runs.
`internal/ledger` is the slow one (a subprocess crash test SIGKILLs a writer mid-transaction, ~30s), so a
full run takes a couple of minutes — run packages individually if you are on a short timeout.

Tests never touch the network. The Mem0 client is tested against **recorded response fixtures** in
`internal/mem0/testdata/`, whose provenance is documented in `FIXTURES.md`. A live test is opt-in
behind a build tag and is never a gate.

That one live test exists because of what fixtures provably cannot catch: that a real add is resolved
**and** certified kept by a **single** reconcile pass, and that re-running appends nothing. It sits
behind the `mem0live` build tag, so `go test ./...` never compiles it and CI never runs it:

```bash
NOTARY_MEM0_API_KEY=... go test -tags mem0live -count=1 -run TestLive -v ./internal/reconcile/
```

It needs the network, a real key, and real Mem0 seconds, so run it deliberately rather than routinely.
`-count=1` is not decoration either: without it a re-run **replays the cached result** and prints
`ok (cached)`, a pass that never touched Mem0 — the same fiction as a skipped test reporting success.
It writes only to a scope it mints itself from a random suffix, and that generated scope is the only
one it ever hands to Mem0; each deletion is re-checked against it locally rather than trusting the
service to honour a filter. It wipes the scope afterwards when it passed or failed — barring a kill,
or a listing it could not read.

### See the surface work, offline

`bash scripts/demo.sh` builds the CLI into a throwaway temp directory, generates its own signing key
at runtime — a fresh throwaway key every run, held in the process environment and never written to
disk; only its public half reaches the temp directory, in the trusted-keys file `verify` reads — seeds
a small fixture ledger through the real write paths, and walks the audit trail in one run: `verify` →
`gaps` → `export` → `replay` → `explain <record-id>` → `explain --memory <mem0-id>`.
It then demonstrates the two properties the project exists for: an edit to a stored record caught by
`verify`, which names the exact record and field, and an unaudited operation reported by `gaps`. It
needs a Go toolchain and nothing else — no network, no Mem0, no LLM key, no configuration — and it
exits non-zero if any step fails, including a step that was supposed to fail and did not.
`cmd/notary/demo_test.go` runs it, so the demo cannot rot silently.

### The README banner

`docs/assets/banner.svg` is the source; `docs/assets/banner.png` is the rendered 2x raster that the
README actually displays. If you edit the SVG, re-render the PNG or the two will disagree:

```bash
google-chrome --headless --disable-gpu --force-device-scale-factor=2 --hide-scrollbars \
  --window-size=1600,400 --screenshot=docs/assets/banner.png docs/assets/banner.svg
```

The PNG is committed rather than referenced as an SVG because an SVG renders with the *viewer's*
fonts — on a machine without Inter, the fallback would reflow the text.

