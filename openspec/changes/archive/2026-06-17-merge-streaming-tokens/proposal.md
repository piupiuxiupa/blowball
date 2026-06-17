## Why

Currently every streaming `token` event emitted by an assistant agent is persisted as a separate `model.Message` row. A single reply can be split into dozens or hundreds of token fragments, inflating Redis, filesystem, and MySQL storage without adding information. We should merge consecutive token fragments from the same agent into one complete message before saving, while preserving the order of lifecycle events (`agent_start`/`agent_end`) and `tool_call` events.

## What Changes

- Add an event-compression step before `SaveMessagesBatch` in the message persistence path.
- Merge only adjacent events that share the same `Agent` and `Type == EventToken`; concatenate their `Content` strings.
- Keep `agent_start`, `agent_end`, `agent_error`, and `tool_call` events as independent rows so frontend history and debugging traces remain accurate.
- Re-index `MsgIndex` after merging so ordering is preserved.
- Leave the live SSE stream untouched: clients still receive raw token fragments in real time.
- Add unit tests for the merge logic, including sub-agent hand-off and mixed event sequences.
- No database schema changes; `model.Message` fields stay the same.

## Capabilities

### New Capabilities

- `message-storage-optimization`: Merge consecutive assistant token events into a single persisted message row while keeping lifecycle and tool-call boundaries intact.

### Modified Capabilities

- None. The change is purely an implementation/storage optimization; the public API and session-management requirements remain unchanged.

## Impact

- `internal/handler/session.go` (or `internal/handler/ports.go` adapter): add merge step before persisting assistant events.
- `internal/handler/event_mapper.go`: mapping logic remains valid, but operates on merged events.
- Unit tests in `internal/handler/session_test.go` and possibly a new test file for the merge helper.
- Reduced row counts in Redis lists, session JSON files, and MySQL `messages` table.
