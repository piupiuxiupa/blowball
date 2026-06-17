## Context

The assistant turn produces a `[]stream.StreamEvent` slice that is currently mapped one-to-one into `[]model.Message` and persisted across three storage tiers (Redis, filesystem, MySQL). A typical LLM reply emits many `token` events; each becomes a database row. Confuse, Chongzhi, and Liang also emit `agent_start`, `agent_end`, `tool_call`, and `agent_error` events that must stay in order so that history consumers can reconstruct which agent said what and when sub-agents or tools were invoked.

The live SSE path is separate: `stream.WriteSSE` consumes the raw event hub and must keep emitting individual token deltas so the frontend can render streaming text. The optimization is therefore isolated to the persistence path.

## Goals / Non-Goals

**Goals:**
- Reduce the number of persisted message rows for a single assistant turn by merging consecutive token deltas from the same agent.
- Preserve total ordering of all semantic events: token blocks, lifecycle markers, tool calls, and errors.
- Keep the public API and the SSE wire format unchanged.
- Make the merge logic unit-testable in isolation.

**Non-Goals:**
- Changing the database schema or `model.Message` structure.
- Merging `tool_call` events, even when they are adjacent or share the same tool name.
- Merging token events across different agents (e.g., Confuse → Liang → Confuse must remain three separate blocks).
- Changing how the frontend consumes the live stream.
- Retaining original token boundaries after persistence.

## Decisions

### 1. Place the merge step in the handler, not inside `orchestratorAdapter`

`orchestratorAdapter` already has a single responsibility: tap the inner hub, mirror events to the SSE hub, and return the collected event slice. Adding compression there would couple streaming mirroring with storage optimization. Placing a small `MergeEvents` helper in the handler keeps the adapter unchanged and makes the persistence pipeline explicit:

```
events from adapter
    │
    ▼
MergeEvents(events)
    │
    ▼
MessageFromEvent per merged event
    │
    ▼
SaveMessagesBatch
```

An alternative is to merge after mapping to `model.Message`. Mapping first is possible but slightly less clean because `agent_start`/`agent_end` rows carry empty `Role`, and the merge predicate would have to inspect `EventType` rather than the richer `StreamEvent.Type`. We will merge `StreamEvent` values first, then map.

### 2. Merge predicate: same `Agent` and `Type == EventToken`

Only adjacent events that share the same `Agent` and both have `Type == EventToken` are merged. Every other boundary (`agent_start`, `agent_end`, `agent_error`, `tool_call`, a change in `Agent`, or a non-token type) starts a new merged event. This keeps lifecycle markers and tool calls as first-class rows.

Rationale: `tool_call` events carry distinct `Meta["args"]` payloads, so merging them would require a new content schema and would break consumers that expect one row per invocation. Lifecycle markers are cheap rows and are useful for reconstructing agent boundaries in history.

### 3. Reuse `MsgIndex` after merging

The handler currently assigns `MsgIndex = i+1` for each event. After merging, the loop will enumerate the compressed slice and assign sequential indices. The user message remains `MsgIndex = 0`. Ordering is therefore preserved by `(MsgTime, MsgIndex)`.

### 4. No schema changes

`model.Message` already has `Agent`, `Role`, `EventType`, and `Content` fields. A merged token event maps to `EventTypeToken`, `RoleAssistant`, `Agent` from the event, and concatenated `Content`. No migration is needed.

## Risks / Trade-offs

- **[Risk]** Frontend history rendering that expects one token per row will now receive a single large token row.  
  → **Mitigation**: The API contract already returns `model.Message` rows with an `event_type` field; frontend should render any `token` row as assistant content. No API change is required.

- **[Risk]** `MsgIndex` values for a turn are no longer dense (they skip from 0 to 1, then to the next merged block), but they are still unique and ordered, so cursor pagination remains stable.

- **[Risk]** Merging drops per-token timing.  
  → **Mitigation**: All events in a turn currently share `MsgTime`, so no timing precision is lost. If we later want sub-second timing, we would need a new column regardless of merging.

- **[Risk]** Title generation currently scans raw events to concatenate Confuse tokens.  
  → **Mitigation**: Title generation happens before persistence and uses the raw `res.events` slice, so it is unaffected. We will keep that path unchanged.

## Migration Plan

No deployment migration is required. Existing persisted messages remain valid; only new assistant turns benefit from the reduced row count. The change is backward compatible for reads because the query order and row shape are unchanged.

## Open Questions

- Should `agent_start`/`agent_end` rows eventually be removed from persistence and inferred from token boundaries? For now we keep them for explicit history markers.
