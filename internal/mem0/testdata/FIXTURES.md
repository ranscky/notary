# Mem0 response fixtures

Every file here is a **real recorded response** from the hosted Mem0 platform
(`https://api.mem0.ai`), captured on **2026-09-28** while designing the
`internal/mem0` client. None are hand-written.

## How they were captured

One throwaway scope was used for every call, with deliberately innocuous
content so no real data is recorded:

- `user_id`: `notary-fixture-user-a1b2c3`
- content: `"Notary fixture probe: the shared office printer lives on floor 3."`

The memories created during recording were **deleted afterwards** (see
`delete_response.json`), and a follow-up `get_all` confirmed the scope was
empty again. No account identifiers, email addresses, org/project IDs, or API
key material appear in any file here — verified by scanning every fixture for
them before committing.

Sequence, in order:

1. `POST /v3/memories/add/` with `messages` + `user_id` + `infer: true`
   → `add_response.json` (`{"event_id": ..., "status": "PENDING"}`)
2. *waited ~20s for the asynchronous pipeline*
3. `GET /v1/event/{event_id}/` → `event_status_response.json`
4. `POST /v3/memories/search/` with `query`, `filters: {user_id}`, `top_k: 5`,
   `threshold: 0.1`, `rerank: true` → `search_response.json`
5. `POST /v3/memories/` with `filters: {user_id}` → `get_all_response.json`
6. `GET /v1/memories/{id}/history/` → `history_response.json`
7. `DELETE /v1/memories/{id}/` → `delete_response.json` (also the cleanup)
8. `POST /v3/memories/add/` with an empty `messages` list → `add_error_400.json`
   (a genuine 400 body, for the error-path test)

## What recording changed

These fixtures exist because the wire shapes turned out to be **richer and less
uniform than the documented shapes suggested**. Recorded facts the client and
its tests must respect:

- **`search` returns `score_breakdown`** (`semantic`, `bm25`, `entity`) as well
  as `score`. That is meaningfully better evidence for "why was this surfaced"
  than a single number.
- **`get_all` returns `replaced_by` and `synthesized`**, which bear directly on
  the `Reconstructed`/`Internal` tier logic: `replaced_by` reveals supersession,
  `synthesized` reveals platform-generated content.
- **`get_all` returns `structured_attributes`** (a derived date decomposition).
- **`event_status` is far larger than `{status, event_id}`**: it carries
  `event_type`, a full `payload` echo of the request, `results[]` with each
  result's own `event` (`"ADD"`) and memory id, plus `source`, `latency`,
  `graph_status`, and `error`.
- **Field presence is inconsistent between endpoints.** `search` emits
  `agent_id`/`app_id`/`run_id` as explicit `null`; `get_all` omits them
  entirely. `metadata` is `{}` in one and `null` in another. Decoding must
  tolerate both, so optional fields cannot be assumed present.
- **No `hash` field was observed** on any endpoint. The client may keep a
  `Hash` field for completeness, but nothing here proves it exists.
- **Timestamps are not uniform.** The same instant is reported as
  `...T22:07:50+00:00` (search, `created_at`), `...T15:07:50-07:00` (get_all),
  and with both second precision and microsecond precision within a single
  object. Any code that compares or orders stored times must normalise — this
  is live corroboration of why the ledger stores fixed-width UTC instants.
- **`add` succeeded with no `message` field** — only `event_id` and `status`.
- **Error bodies are `{"error": "<string>"}`**, where the string is a
  Python-repr of the failing validation, not structured JSON.
- **`delete` returns `{"message": "Memory deleted successfully!"}`.**

## Do not re-record

Recording needs a live API key, which deliberately is not in this repository
and must never be committed. These files are the record. The plan defers any
live-API test to an opt-in `//go:build mem0live` build tag that is never a gate.
