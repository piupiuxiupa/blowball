## Context

The session list endpoint (`GET /api/v1/sessions`) returns sessions ordered by `sessions.update_time DESC` so that the most recently active session appears first. This ordering is already documented in the `session-management` spec and in the MySQL query (`listSessionsWithTitleSQL`).

However, the message persistence path (`SessionService.SaveMessagesBatch`) only writes to Redis, the filesystem session file, and the `messages` table. It never touches `sessions.update_time`. Consequently, sending a new message to an old session does not move that session to the top of the list.

`UpdateSessionTime` already exists in the MySQL store and is used by `TitleService.SetManualTitle` to bubble a session up after a manual title edit.

## Goals / Non-Goals

**Goals:**
- Ensure every successful message persistence refreshes the parent session's `update_time`.
- Keep the session list order aligned with recent activity without changing the API contract.
- Add automated test coverage that fails if `SaveMessagesBatch` does not update session time.

**Non-Goals:**
- Changing the list sort key or sort direction.
- Refreshing `update_time` on AI title generation (out of scope; title generation is not new activity).
- Adding a database transaction that couples message insert and session time update.

## Decisions

### Where to refresh `update_time`

**Decision:** Add the `UpdateSessionTime` call inside `SessionService.SaveMessagesBatch`, after the MySQL message insert.

**Rationale:**
- `SaveMessagesBatch` is the single central write path for all persisted messages (user messages and assistant events).
- The handler `SendMessage` already delegates persistence to this method, so no handler changes are required.
- Keeping the refresh in the service layer matches the existing manual-title behavior and avoids leaking persistence details into handlers.

**Alternatives considered:**
- Refresh in `SessionHandler.SendMessage`: Rejected because it scatters persistence logic and misses any future callers of `SaveMessagesBatch`.
- Refresh on every SSE event: Rejected because it is too chatty and persists no data until the batch path runs.

### Error handling

**Decision:** Log `UpdateSessionTime` failures but do not fail the `SaveMessagesBatch` call.

**Rationale:**
- The existing MySQL message insert already follows this pattern (`log.Error` without returning) so that SSE streaming is never blocked by storage hiccups.
- Messages are still persisted to Redis and FS, so failing the whole batch for an ancillary `update_time` refresh would be disproportionate.

### Test strategy

**Decision:** Add a unit test on `SessionService.SaveMessagesBatch` using the in-memory fake `MySQLStore` to assert that `UpdateSessionTime` is called with the correct `sessionID`.

**Rationale:**
- This directly verifies the new behavior at the unit of change.
- Integration tests can additionally assert end-to-end ordering, but the unit test provides fast regression protection.

## Risks / Trade-offs

| Risk | Mitigation |
|------|------------|
| Extra DB write per message batch slightly increases load | The write is a single-row `UPDATE` indexed on `session_id` PK; negligible overhead compared to the multi-value message insert. |
| `UpdateSessionTime` failure is silent | Consistent with existing error policy for the MySQL message tier; logs remain available for monitoring. |
| Concurrent turns on the same session could race on `update_time` | MySQL `NOW()` is evaluated at statement execution; the last writer wins, which is acceptable for a "recent activity" timestamp. |
