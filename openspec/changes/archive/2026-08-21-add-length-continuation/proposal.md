# Proposal: add-length-continuation

## Why

A `finish_reason=length` response silently kills the turn today. The agent
loops never inspect `resp.FinishReason`: content-only truncation is delivered
as the final answer mid-sentence; `length` arriving with `tool_calls` makes
`shouldDispatchToolCalls` treat the round as terminal
(`internal/agent/agent.go:281`), so the turn ends on a "let me analyze…"
preamble with the truncated calls discarded; and an all-empty truncation
(thinking consumed the whole budget) ends with an empty answer. Background:
the 2026-08-20 glm-5.2 incident truncated long `tool_call` arguments at
`max_tokens` (8192) — on that gateway `finish_reason` lied (`tool_calls`, not
`length`), so this change also adds prompt-side prevention (write-chunking
guidance + an `append` mode) while the runtime recovery below keys strictly on
an honest `length`.

## What Changes

- **Runtime recovery — continuation, not regeneration**: when a round's LLM
  response carries `finish_reason=length`, the agent keeps the partial output
  and re-requests with an expanded budget instead of ending the turn.
  - Content-only truncation: append the partial assistant message + a
    continuation instruction (user role) to the round, re-request.
  - `tool_calls` truncation (B-call-1): append the assistant message with the
    half-truncated calls; calls whose `arguments` parse as JSON dispatch
    normally (real results appended — the wire protocol requires every
    tool_call to be answered before the next request); truncated calls get a
    synthetic not-executed tool result instructing the model to re-issue in
    smaller chunks, and emit a `tool_call` + `tool_result` SSE event pair
    (matching the `bad_args` visibility precedent).
  - Each continuation adds `expand_step` to `MaxTokens`; the SSE stream is
    unchanged — continuation tokens flow seamlessly onto the same run and
    `MergeEvents` folds them into one persisted row.
- **Exhaustion**: after `max_retries` (3) continuations the response still
  ends `length` → emit `agent_error` (code `length_exhausted`) + `agent_end`,
  the turn ends with `done.error`, and `finalContent` is the accumulated
  content (nothing was discarded).
- **Prevention — write budget guidance**: the `xizhi_write_file` /
  `xizhi_modify_file` tool descriptions gain chunking guidance with a
  per-agent computed budget of `max_tokens × 70%`. The number is injected at
  agent construction time (tools[] rendering), because `tool.Registry` is
  process-wide while `max_tokens` is per-agent.
- **Prevention — append mode**: `xizhi_write_file` gains a `mode` parameter
  (default preserves today's create-or-overwrite; `"append"` creates the file
  if missing or appends to it), making "split large content into multiple
  writes" literally executable.
- **New config** `openai.length_continue: { expand_step: 8192, max_retries: 3 }`
  — global, opt-in; unset reproduces today's silent-terminal behavior.
  Negative values fail config load.
- **Scope**: the four streaming call sites — Confucius/Chongzhi/Liang main
  loops and `runWrapUpRound` — adopt one shared helper. Title generation and
  compaction summaries are out of scope.

No SSE schema changes, no API contract changes, no DB migrations, no frontend
changes.

## Capabilities

### New Capabilities

- `llm-length-continuation`: opt-in continuation of `finish_reason=length`
  rounds — trigger semantics, per-attempt `max_tokens` expansion, the two
  continuation shapes (prose / truncated tool_calls), retry bound and
  `length_exhausted` termination, the shared-helper scope, and the
  zero-change SSE/persistence invariants it relies on.

### Modified Capabilities

- `agent-orchestration`: the agent-loop termination contract — a `length`
  round no longer silently terminates the loop when the feature is enabled;
  continuation attempts do not consume `max_rounds`, and loop requirements
  reference the continuation capability instead of treating non-stop
  finish reasons as terminal.
- `xizhi-tools`: `xizhi_write_file` gains the `mode` parameter (append
  semantics + result shape), and the tool-description requirement extends to
  the per-agent write-budget guidance on the write/modify descriptions.

## Impact

- `internal/config/config.go` — `OpenAIConfig.LengthContinue` block
  (`expand_step`, `max_retries`), load-time validation, `config.example.yaml`
  documentation.
- `internal/agent/` — new shared continuation helper (new file), adopted by
  `confucius.go` / `chongzhi.go` / `liang.go` main loops and `roundcap.go`
  (`runWrapUpRound`); `tools.go` description-budget injection at tools[]
  render time.
- `internal/tool/xizhi/` — `register.go` (`mode` param + schema + static
  description pattern advice), `write.go` (append branch, result fields).
- Tests: config validation; helper continuation/expansion/exhaustion paths
  (fake LLM); prose and truncated-tool_calls sub-cases; complete-call
  dispatch inside the continuation loop; `xizhi_write_file` append mode;
  description injection; `MergeEvents` single-row invariant.
- Operational: no migration; roll back by unsetting `openai.length_continue`.
