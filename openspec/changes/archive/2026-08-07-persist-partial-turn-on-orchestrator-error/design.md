## Context

`MessageStreamHandler.SendMessage` (`internal/handler/message_stream.go`) is the only handler that couples the CRUD data plane to agent execution. It defers ALL persistence — the user message plus the assistant event stream — until the orchestrator returns, then writes them in one asynchronous batch via the `persistEvents` closure (user message → `MergeEvents` → `MessageFromEvent` → `SaveMessagesBatch` → `SaveTurnUsage` → optional title gen).

Today the post-orchestrator branch has three outcomes:

1. **Success** → `persistEvents(res.events, res.usage)`.
2. **`context.Canceled`** (client disconnect) → `persistEvents(res.events, res.usage)` (the "interrupted turn" path).
3. **Any other error** (429 / 5xx / timeout / transport) → `logger.Error("orchestrator failed")` then `return`. **Nothing is persisted.**

The data for outcome 3 is already collected: `orchestratorAdapter.Handle` (`internal/handler/ports.go`) accumulates every non-`done` event into `res.events` and captures usage from the `done` event **regardless of error**, and the orchestrator (`internal/agent/orchestrator.go`) still emits the terminal `done` event (with usage + an `error` field) on the error path. So this is purely a handler-side routing decision, not a missing-data problem.

This surfaced in production: a multi-round turn (`total_tokens: 2739160`) vanished after a `429`, leaving the user's input and all streamed assistant content unrecoverable and the reloaded history inconsistent with what the client rendered.

The existing `interrupted-turn-persistence` spec already mandates partial persistence for client-disconnect (outcome 2). This change extends the same guarantee to outcome 3.

## Goals / Non-Goals

**Goals:**
- Persist the user message and the partial assistant event stream when the orchestrator returns a non-cancellation error.
- Record the failed turn's token cost in `turn_usage` (the `done` event already carries usage on the error path).
- Keep first-turn title generation consistent across all interruption causes.
- Reuse the existing `persistEvents` closure and three-layer write path unchanged.

**Non-Goals:**
- Retrying provider errors at the handler/orchestrator top level (sub-agent retry is a separate, already-shipped capability; Confucius-level retry is out of scope).
- Changing the SSE wire format, event model, or the `done` event's `error` field.
- Changing the api/agent role split, route registration, or the persistence write ordering.
- Distinguishing "transient" vs "permanent" provider errors at the handler layer.
- Frontend changes (history reload will naturally reflect the newly-persisted partial turns).

## Decisions

### Decision 1: Persist on ALL non-cancellation orchestrator errors, not just "transient" ones

The handler routes every non-`context.Canceled` error through `persistEvents`, with no attempt to classify the error.

**Rationale:** By the time the orchestrator runs, the request body is parsed, the session is resolved and ownership-checked, and the user message is valid. There is no "malformed request" case left at this layer that would justify dropping content. Classifying errors as transient/permanent at the handler would require inspecting opaque wrapped error strings (`confucius: stream chat: openai client: stream: POST ...: 429 ...`), which is brittle and couples the handler to the LLM client's error wording. Treating all orchestrator errors uniformly is simpler, safer, and matches the existing client-disconnect guarantee.

**Alternative considered:** Persist only on transient errors (429/5xx/timeout), keep dropping on others. Rejected because (a) the brittle error-classification problem above, and (b) any orchestrator error after content was streamed already represents a consistency hazard if dropped.

### Decision 2: Always persist the user message, even when zero assistant events were produced

When a provider 429s on the very first LLM call with no streamed events, the handler still writes the user message row (and nothing else).

**Rationale:** The user's own input must never be silently lost — that is the minimum acceptable guarantee, and it is exactly what the existing "Interruption before assistant emits content" scenario already mandates for client disconnects. Applying it symmetrically keeps the two interruption causes consistent. The duplicate-on-retype tradeoff is identical to the existing client-disconnect case and is acceptable.

### Decision 3: Reuse `persistEvents` verbatim; do not fork a new persistence path

The existing closure already does: user message → `MergeEvents` → `MessageFromEvent` (which handles `agent_error` events via the `EventAgentError` branch in `event_mapper.go`) → `SaveMessagesBatch` → `buildTurnUsage`/`SaveTurnUsage` (tolerates error-path usage) → optional title gen. No new code is needed; the change routes provider-error turns through the same closure.

**Rationale:** A single persistence path means a single set of guarantees ("usage write failure does not roll back messages", detached `saveCtx` survives disconnect, panic recovery). Forking would duplicate or diverge these.

**Alternative considered:** Inline a trimmed persistence path that skips `turn_usage` on errors. Rejected — the `done` event already carries usage on the error path (the orchestrator emits it deliberately), and recording the cost of a failed 2.7M-token turn is valuable observability.

### Decision 4: Keep the error log, augment with `event_count`

The existing `logger.Error("orchestrator failed", ...)` stays (operators still need to see provider failures), with an added `event_count` field and a message indicating the partial turn is being persisted. The client-disconnect path keeps its distinct `Warn`-level log.

## Risks / Trade-offs

- **[More storage writes on flapping providers]** A provider that 429s repeatedly will now write a partial turn per failure. → Mitigation: acceptable — these are real turns the user saw; dropping them is the worse failure. Storage cost is bounded by the existing per-turn event volume. No new unbounded growth vector.
- **[Orphan user messages on first-call 429]** Persisting only the user message when no assistant content was produced means the next turn's `RecoverMessages` includes it. → Mitigation: consistent with the existing client-disconnect behavior; users may retype and create a duplicate user row, which is benign (message ordering by `(msg_time, msg_index, id)` is preserved).
- **[Frontend renders a turn ending in an error event]** History reload will now surface turns whose last persisted event is `agent_error`. → Mitigation: `MessageFromEvent` already maps `agent_error` to a persisted row with content; the frontend already handles `agent_error` events streamed live. No new event type is introduced.
- **[turn_usage rows for failed turns]** `turn_usage` gains rows for failed turns with a non-zero `total_tokens`. → Mitigation: this is desired (cost observability for failed turns); the `usage.error` field on the `done` event is already serialized into `usage_json`, so failed turns are distinguishable in queries.

## Migration Plan

- Single-commit code change in `internal/handler/message_stream.go`; no schema migration (no new table/column — `turn_usage`, `messages` already exist), no config change, no deploy sequencing.
- Rollback: revert the commit; behavior returns to the pre-change drop-on-error semantics. No data cleanup needed (any partial rows written under the new behavior are valid history).
- Deploy: standard `make build` + restart the agent/all role. The api role is unaffected (it has no streaming handler).
