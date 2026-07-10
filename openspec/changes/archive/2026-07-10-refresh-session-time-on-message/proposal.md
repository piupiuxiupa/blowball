## Why

The session list is ordered by `sessions.update_time DESC`, but sending a new message to an existing session does not refresh `update_time`. As a result, recently-active sessions remain buried in the list instead of floating to the top, contradicting the expected "most recently updated first" behavior.

## What Changes

- Update `SessionService.SaveMessagesBatch` to call `MySQLStore.UpdateSessionTime` after persisting messages, so every new message (user or assistant) refreshes the parent session's `update_time`.
- Add unit/integration test coverage verifying that `SaveMessagesBatch` triggers a session time refresh and that the session list reflects the new order.
- Clarify the `session-management` spec to state that sending a message to an existing session SHALL refresh `sessions.update_time`.

## Capabilities

### New Capabilities

None.

### Modified Capabilities

- `session-management`: Add an explicit requirement that sending a message to an existing session refreshes `sessions.update_time`, ensuring the session list order reflects recent activity.

## Impact

- `internal/service/session.go` (`SaveMessagesBatch`)
- `internal/store/mysql/session.go` (existing `UpdateSessionTime` is reused)
- Tests in `internal/service/` and/or `test/integration/`
- Delta spec in `openspec/specs/session-management/spec.md`
