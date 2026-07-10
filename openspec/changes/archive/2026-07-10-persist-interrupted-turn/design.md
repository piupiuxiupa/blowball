## Context

`SessionHandler.SendMessage` in `internal/handler/session.go` runs the agent orchestrator in a goroutine, streams events to the client via SSE, and waits for the orchestrator result on `resultCh`. The success path persists the user message plus the full event stream in a detached-context goroutine. However, when the client disconnects and the request context is canceled, the handler returns early at the `context.Canceled` branch and drops both the user message and any partial assistant output that was already emitted.

The orchestrator adapter (`internal/handler/ports.go`) already collects every event sent to the hub into `res.events`, so the data needed for partial persistence is available; it is just discarded.

## Goals / Non-Goals

**Goals:**
- When a user interrupts an in-progress assistant turn, persist the user message and all assistant events collected up to the point of cancellation.
- Run first-turn title generation using the partial assistant tokens already emitted.
- Keep non-cancellation errors unchanged so failed turns do not pollute conversation history.
- Add test coverage for the cancellation persistence path.

**Non-Goals:**
- Frontend changes or an explicit "stop generation" button.
- Incremental persistence while streaming.
- Database schema changes.
- Modifying message reconstruction rules; existing `MessagesToAgentMessages` behavior is reused as-is.

## Decisions

1. **Persist only on `context.Canceled`, not on every orchestrator error.**
   - *Rationale*: The user explicitly asked for interruption behavior. Other errors (LLM failures, tool errors) are still transient or malformed and should not become permanent history.

2. **Reuse the existing `SaveMessagesBatch` goroutine with a detached context.**
   - *Rationale*: It already writes to Redis, filesystem, and MySQL, and it does not block the HTTP response. Using the same path keeps the persistence behavior consistent.

3. **Use `res.events` from the orchestrator adapter without adding an "interrupted" marker event.**
   - *Rationale*: The user asked to preserve the content, not to annotate it. The existing event types (`token`, `reasoning`, `tool_call`, `tool_result`) are sufficient. A UI-level interrupted indicator can be added later if needed.

4. **Run title generation on cancellation the same way as on success.**
   - *Rationale*: The user wants a title even for an interrupted first turn; the partial assistant content is the best available signal.

5. **Keep `MessagesToAgentMessages` unchanged.**
   - *Rationale*: It already merges adjacent tokens, pairs tool calls with results, and drops unpaired tool calls. Interrupted turns naturally fit into this model.

## Risks / Trade-offs

- **[Risk]** A truncated assistant message is persisted as a complete `role=assistant` turn, which the model will see as context in the next request. This could be slightly confusing if the user expected a fresh retry.
  - *Mitigation*: This is inherent to preserving context. If it becomes problematic, a future change can append a system note or add an `interrupted` marker event.

- **[Risk]** If cancellation happens before any assistant events, the history contains a lone user message with no response.
  - *Mitigation*: This is acceptable and matches user intent (the question was asked).

- **[Risk]** Completed `tool_call`/`tool_result` pairs from an interrupted turn are kept, while unpaired `tool_call` rows are silently dropped by reconstruction.
  - *Mitigation*: This is the desired behavior; unpaired calls would present an incomplete tool-calling turn to the model.

## Migration Plan

No migration is required. The change is a backend-only logic update in `internal/handler/session.go`. Existing persisted sessions are unaffected; only interrupted turns going forward will be saved.

## Open Questions

None. All decisions were confirmed with the requester.
