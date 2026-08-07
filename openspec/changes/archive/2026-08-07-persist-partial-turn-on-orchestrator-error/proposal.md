## Why

When the model provider returns an error mid-turn (e.g. a `429 Too Many Requests`, `5xx`, or timeout), the orchestrator fails after content has already been streamed to the user — yet the handler discards both the user's message and every assistant event produced before the failure. On reload the conversation history no longer matches what the user actually saw, and the user's own input is lost entirely. This surfaced in production as a 2.7M-token turn that vanished after a `429`. The root cause is that `MessageStreamHandler.SendMessage` only persists partial turns for `context.Canceled`; every other orchestrator error is logged and dropped.

## What Changes

- Persist the user message and the partial assistant event stream when the orchestrator returns a **non-cancellation** error (provider/transport/timeout failures), not only on client disconnect.
- Persist the turn's token cost into `turn_usage` on such failed turns (the `done` event already carries usage on the error path).
- Keep title generation behavior on a failed first turn consistent with the interrupted path (uses partial assistant tokens).
- No change to the success path, the SSE wire format, the event model, the three-layer persistence write path, or the retry behavior of sub-agents.
- No change to the api/agent role split or route registration.

## Capabilities

### New Capabilities
<!-- None — this extends an existing capability. -->

### Modified Capabilities
- `interrupted-turn-persistence`: Extends "interrupted" to cover orchestrator failures from upstream/transport errors (429/5xx/timeout), not just client-initiated cancellation. The user message and assistant events emitted before the failure SHALL be persisted, and the turn's token cost SHALL be recorded in `turn_usage`.

## Impact

- **Code**: `internal/handler/message_stream.go` — the `res.err != nil` branch in `SendMessage` (currently discards non-cancellation errors); the existing `persistEvents` closure already writes the user message + merged events + `turn_usage` + title generation, so the change is to route provider-error turns through it.
- **Specs**: `interrupted-turn-persistence` delta (requirements + scenarios for provider-error turns).
- **Persistence**: More rows written to Redis (hot), FS warm tier, and MySQL on transient provider failures; the existing three-layer write path and "usage write failure does not roll back messages" guarantee are unchanged.
- **API/SSE**: No wire-format change. The `done` event already carries an `error` field on the error path; consumers are unaffected.
- **Frontend**: History reload will now show turns that ended in a provider error, matching what was streamed. No frontend code change required.
- **Tests**: New unit/integration coverage asserting partial content is persisted on a non-cancellation orchestrator error; existing client-disconnect tests remain green.
