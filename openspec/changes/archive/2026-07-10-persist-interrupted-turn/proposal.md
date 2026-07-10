## Why

When a user interrupts an in-progress assistant response (e.g. by closing the browser tab or losing network), the backend currently discards the entire turn because `SessionHandler.SendMessage` returns early on `context.Canceled`. This loses both the user's question and any partial assistant output, making it impossible to continue the conversation from where it was interrupted. We need to persist those messages so the session context survives user-initiated cancellation.

## What Changes

- In `internal/handler/session.go`, change the `context.Canceled` path of `SendMessage` to persist the user message and the partial event stream collected up to the point of cancellation, using the same detached-context `SaveMessagesBatch` goroutine used for successful turns.
- Run first-turn title generation even when the turn is interrupted, using the partial assistant tokens already emitted.
- Keep the existing behavior for non-cancellation errors: those turns remain discarded.
- Add unit/integration coverage for the cancellation persistence path.

## Capabilities

### New Capabilities

- `interrupted-turn-persistence`: When an assistant turn is canceled by the client, the user message and any assistant events generated before cancellation are persisted to the three-tier message store (Redis, filesystem, MySQL) so the session can be resumed.

### Modified Capabilities

- `agent-conversation-memory`: Add the requirement that recovered history includes user messages and partial assistant/tool events from interrupted turns, not only from completed turns. Existing reconstruction rules (merging tokens, pairing tool calls, omitting unpaired tool calls) remain unchanged.

## Impact

- Backend: `internal/handler/session.go` and its tests.
- No database schema changes.
- No frontend changes.
- Downstream consumers of `RecoverMessages` will now see additional rows for interrupted turns; `MessagesToAgentMessages` already handles incomplete tool-call pairs correctly.
