## Context

The agent loop keeps its live round in memory in the provider's wire shape: one assistant message can carry content, reasoning, and tool calls, followed by tool results. Round-final persistence intentionally stores that round differently: one assistant final row, tool placeholder rows, and canonical `tool_calls` rows. Reconstruction then rebuilds an LLM request from those durable rows.

The observed mismatch has three independent sources:

```text
live round                         durable replay
---------------                    ----------------
assistant{content,                assistant{content, reasoning}
          reasoning,        →     assistant{tool_calls}
          tool_calls}
arguments{"a": 1, "b": 2}         arguments{"a":1,"b":2}
calls in LLM response order       calls in event/dispatch order
```

Consecutive tool-only rounds can also be coalesced because the renderer only closes an open round when a later token or reasoning event arrives. Memory-recall pairs replay through the same tool-pair machinery, but the evidence from the investigated session shows that ordinary tool history changes before a new recall pair; recall is a regression scenario, not the root cause.

## Goals / Non-Goals

**Goals:**

- Make a completed LLM round replay in its original wire shape.
- Preserve tool-call argument JSON bytes emitted by the provider.
- Make call and tool-result message order follow the LLM response order.
- Preserve round boundaries between consecutive tool-only responses.
- Keep compaction row attribution aligned with merged assistant/tool units.
- Prove live-to-durable-to-live prefix stability with byte-level tests.

**Non-Goals:**

- Change memory recall retrieval, rendering, dedupe, or ordering.
- Change the HTTP API or SSE event types.
- Add a database migration in the first implementation.
- Backfill historical `tool_calls.arguments` from `llm_raw_log`.
- Rewrite the context-compaction algorithm.

## Decisions

### D1 — Use the live assistant response as the canonical replay shape

An assistant final row and the placeholders belonging to the same round reconstruct as one `agent.Message` carrying `Content`, `ReasoningContent`, and `ToolCalls`, followed by that round's `role=tool` messages. Splitting the live assistant message to match the current reconstruction output was rejected because it changes model semantics and contradicts the existing wire-format contract.

The reconstruction scanner will treat an assistant final row as the opening anchor of a round even when its content and reasoning are empty. Adjacent placeholders join that round until the next final row, user row, or another established boundary. This retains the legacy row adapter while making new final-row history deterministic.

### D2 — Preserve original argument bytes

When tool-call arguments are already represented as `json.RawMessage`, stream-event construction will retain that raw value rather than passing it through `json.Marshal`. Values supplied as other Go types may still be marshaled deterministically. Persistence and reconstruction will carry the resulting argument string unchanged.

Canonicalizing all arguments at ingestion was considered. It would make future requests internally stable, but it would intentionally discard the exact bytes already seen by the provider and cause another prefix change during deployment. Preserving raw JSON is preferred for cache continuity.

### D3 — Emit call events in response order before parallel execution

Before dispatching a round's tools, the agent will emit every `ToolCallEvent` synchronously in `resp.ToolCalls` order. Parallel execution begins only after all call placeholders are on the event stream. Result events may still complete in any order; persistence pairs them by call id and replay emits tool messages in call order.

This removes goroutine scheduling from persisted call order. It also makes a later `tool_call` unambiguously mean a new LLM round when it follows a completed tool result. Special paths such as `update_plan` must participate in the same initial response-order emission while retaining their existing result semantics.

### D4 — Close a completed round before a subsequent tool-first round

Round rendering will close the open round when a new tool-call event arrives after a tool result. This rule is safe only because all calls belonging to one response are emitted before any of their results. It prevents this sequence from being stored as one round:

```text
LLM round 1: call, call, result, result
LLM round 2: call, result
```

An explicit persisted `round_id` or `response_id` was considered as a stronger identity. It is not required for the corrected event ordering and would require a migration; it remains a fallback if real histories expose remaining ambiguity.

### D5 — Keep compaction attribution on the merged unit

When reconstruction merges a final row with placeholders, its row attribution will cover the final row and all joined placeholder rows. A compaction boundary can therefore cut before or after the whole assistant/tool unit without attributing the merged assistant message only to its first row. Existing boundary selection and tail-retention behavior otherwise remain unchanged.

### D6 — Test the full live-to-recovered prefix

Tests must compare more than the shape of reconstructed `agent.Message` values. At least one oracle will serialize the live request messages and the recovered request messages through the same request-conversion path and require the recovered request's prior-prefix bytes to equal the live request. Focused tests will also inspect exact argument strings and call ids.

Test matrices must include:

- content + reasoning + one tool call;
- content + reasoning + parallel tool calls;
- tool-first and consecutive tool-only rounds;
- insignificant whitespace in nested argument JSON;
- `update_plan` mixed with executable calls;
- pending or missing tool results;
- legacy event rows mixed with final rows;
- a synthetic `recall_memories` pair;
- compaction boundaries around merged tool units.

## Risks / Trade-offs

- [Adjacent final rows could be ambiguous in legacy data] → Use the existing event types and placeholder ordering first; add explicit round identity only if a real mixed-history case cannot be classified.
- [Historical arguments are already compacted] → Accept a one-time cache miss for old sessions; do not rewrite history in this change. A later raw-log backfill can be proposed separately.
- [Emitting all call events before execution changes SSE visual timing] → Parallel calls were already semantically simultaneous; showing all call cards before results is deterministic and matches response order.
- [`update_plan` has special pre-dispatch behavior] → Refactor event emission separately from execution so control-plane tools keep their result semantics without reordering call placeholders.
- [Merging rows changes compaction attribution] → Add boundary tests and retain the maximum source-row attribution for the merged assistant/tool unit.

## Migration Plan

Deploy the behavior without a schema migration. Newly persisted rounds and all newly recovered requests use the stable shape. Existing sessions may experience one further prefix-cache miss because their already-compacted argument bytes cannot be recovered from `tool_calls`; after that, newly formed provider cache entries use the corrected representation. Rollback restores the previous reconstruction behavior and likewise introduces a one-time prefix change; no data migration must be undone.

## Open Questions

None blocking. Explicit round identity remains a follow-up design if corrected event ordering cannot classify a real legacy-history case.
