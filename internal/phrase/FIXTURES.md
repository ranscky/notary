# Phrase provider response fixtures

These files are what the OpenAI-compatible chat-completions endpoint returns.
One is a **real recorded response**; the other two are **synthetic** fixtures for
response shapes the design research established. Every test in `internal/phrase`
reads a file here and nothing here is ever re-recorded by the test suite: the
tests are offline and deterministic, and the live capture is evidence, not a
test.

## `chat_completions_response.json` — real capture

A **real** response from the provider in use, captured on **2026-10-03**:

- Endpoint: `POST https://api.deepseek.com/chat/completions`
- Model: `deepseek-flash`
- It is the verbatim response body of a real call, not hand-written and not
  edited.

It was captured with a throwaway program (since deleted) that made one request
with **exactly** the body the client sends — `{"model": ..., "messages": [...]}`
and nothing else — using the same system prompt and a representative structured
claim (event, tier, reason kind, scope, memory id, content hash) as the user
message. The request carried **no memory content text**, so the response carries
none either: the model's sentence names only the metadata it was given.

No API key material appears in any file here — verified by scanning every
fixture for the key before it was committed. The key lives only in the gitignored
`.env`; it is never written down.

### What recording changed

The wire shape turned out to be **richer than the documented OpenAI shape**.
Recorded facts the client and its tests must respect:

- **The provider is a reasoning model and returns `choices[0].message.reasoning_content`
  alongside `choices[0].message.content`.** The client reads only `content`; the
  reasoning text is an unnamed field and is ignored, not refused. This is live
  corroboration of the design's "unknown fields are ignored" quirk — a real
  example, not a hypothetical one.
- **`usage` carries vendor extensions**: `prompt_cache_hit_tokens`,
  `prompt_cache_miss_tokens`, and `completion_tokens_details.reasoning_tokens`
  in addition to the standard token counts. Ignored.
- **`system_fingerprint` is present.** Ignored.
- **`choices` has exactly one element**, the normal case — see the synthetic
  single-element fixture below for the assertion that pins it.
- The `content` sentence restates the metadata verbatim (memory id, content
  hash, tier, reason) and invents nothing, which is the behaviour the prompt
  asks for.

## `single_choice_response.json` — synthetic

**Synthetic, hand-written**, not recorded. It exercises the documented, expected
case of a `choices` array with exactly one element, so
`TestSingleElementChoicesIsAccepted` asserts against a response that is a
complete, normal completion rather than an edge case.

## `unknown_fields_response.json` — synthetic

**Synthetic, hand-written**, not recorded. It carries unknown fields at the top
level (`service_tier`, `system_fingerprint`, `provider_extension`) and inside a
choice (`vendor_score`, `annotations`, `tool_calls`, `refusal`) and inside
`message`. It exists so `TestUnknownResponseFieldsAreIgnored` proves the client
ignores unknown fields rather than refusing the response — a property the real
capture happens to demonstrate too, but which a deliberate fixture pins.

## Do not re-record

Recording needs a live provider key, which deliberately is not in this repository
and must never be committed. These files are the record. The plan defers any
live-provider test to an opt-in build tag that is never a gate.
