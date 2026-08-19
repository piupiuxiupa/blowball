# Proposal: add-llm-stream-idle-watchdog

## Why

A stalled upstream LLM stream hangs a chat turn forever. On 2026-08-18 a Chongzhi
sub-agent's stream stopped mid-generation (chunks flowing at ~30ms cadence, then
zero bytes for 8m36s with the connection open and no error frame), and
`OpenAIClient.StreamChat` blocked on `stream.Next()` indefinitely — there is no
read deadline anywhere in the LLM client. The entire turn froze, the user had to
disconnect manually, and the turn's 2.9M tokens were sunk. The existing
transient-error retry machinery never engaged because a silent stall never
becomes an error: `isTransientError` only fires once a call *fails*, and a hung
stream fails at nothing, forever.

## What Changes

- Add an **inter-chunk idle timeout** to `OpenAIClient.StreamChat`: a watchdog
  timer that starts when the streaming call begins (covering time-to-first-frame)
  and resets on every accepted SSE frame. When it fires, the stream is aborted
  and the call returns a typed `stream idle timeout` error instead of hanging.
- New config field `openai.stream_idle_timeout` (duration, short suffixes,
  `${VAR}` expansion). Zero/unset disables the watchdog — byte-for-byte prior
  behavior, mirroring the `tools.timeouts` opt-in convention. A negative value
  is rejected at config load. `config.example.yaml` documents a recommended
  value of `2m`.
- The watchdog error message contains "timeout", so the existing
  `isTransientError` substring classifier (`internal/agent/retry.go`) already
  treats it as transient — **no retry-logic changes**; the existing sub-agent
  retry path (policy, budget, idempotency tracker) picks it up unchanged.
- Raw capture (`llm-raw-capture`) records a watchdog abort as a `kind=error`
  row (distinct from a client-disconnect partial-`response` row), carrying the
  gap duration and frames received for post-mortem.
- Scope: every `StreamChat` call through the shared `OpenAIClient` — the three
  agent loops, round-cap wrap-up rounds, title generation, and compaction
  summary generation. This also fixes a quieter leak: a stalled title-generation
  stream runs on a `Background`-derived context and today can never be canceled
  at all.

No SSE schema changes, no API contract changes, no DB migrations, no frontend
changes.

## Capabilities

### New Capabilities

- `llm-stream-watchdog`: Streaming LLM calls are bounded by a configurable
  inter-frame idle timeout; a stalled stream is aborted with a typed error that
  the existing transient-retry path classifies as retryable; the abort is
  visible in logs and raw capture.

### Modified Capabilities

- `llm-raw-capture`: A watchdog-aborted call ends as a `kind=error` row with
  diagnostic fields (idle gap, frames received) rather than the partial
  `kind=response` row reserved for local context cancellation — a new,
  distinguishable way a captured call can end.

## Impact

- `internal/config/config.go` — `OpenAIConfig.StreamIdleTimeout` field,
  validation (negative rejected, zero = disabled), `config.example.yaml`
  comment block.
- `internal/agent/openai_client.go` — watchdog goroutine + derived cancel
  context inside `StreamChat`; typed error; capture path branch.
- Tests: `internal/agent` (watchdog fires / resets / parent-cancel precedence),
  raw-capture row shape on watchdog abort, config validation.
- Operational: no migration, no deploy-order dependency; safe to roll back by
  unsetting the config field.
