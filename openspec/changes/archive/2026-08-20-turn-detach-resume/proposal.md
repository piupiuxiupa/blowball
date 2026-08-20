# Proposal: turn-detach-resume

## Why

A turn's lifetime is currently bound to the SSE connection that started it: closing the frontend page cancels the agent loop mid-generation (`internal/handler/message_stream.go` binds the orchestrator goroutine to `c.Request.Context()`). Users who navigate away lose the answer they asked for and must resend; there is also no way to cancel deliberately without losing the connection, and no way to re-attach to a still-running turn. This decouples turn lifetime from HTTP connection lifetime so generation survives page closes, can be cancelled explicitly by id, and can be resumed on session reopen.

## What Changes

- **BREAKING**: A client disconnecting from the SSE stream no longer cancels the turn. The orchestrator runs on a turn-scoped context owned by a server-side run registry; generation always runs to completion (done / error / explicit cancel). No configuration switch — this is the new unconditional behavior.
- Every accepted message request mints a **run id** (the request's "context id"), delivered to the client via the first SSE event's meta and an `X-Run-Id` response header.
- A session with a running turn **rejects new messages with 409 `SESSION_BUSY`** (response carries the active `run_id` so the frontend can attach instead). The mutex is a Redis `SET NX` claim on `session:{sid}:run`, so the check holds across agent-role processes.
- New endpoint **cancel running turn** (takes the run id): cancels the running request and releases resources — registry cancel, Redis run-state cleanup. When the owning process is gone (crash / other replica), the endpoint force-clears the run state after ownership validation.
- New endpoint **resume turn events** (`GET .../turns/:run_id/events`, SSE): replays the turn's event log from the beginning (or from `Last-Event-ID`) and then tails live output until the turn ends. A user reopening a session whose last request is still running uses this to continue receiving output automatically.
- Turn events and run state are buffered in **Redis**: a per-run event Stream (source for replay + live tail), a per-run meta Hash (session/user/status/model), and the per-session active-run key. Keys are cleaned up at turn end; a TTL backstops crashed processes.
- Crash semantics: if the agent process dies mid-turn the turn is dead (not resumed elsewhere), the replayable event log survives, and the run is marked `interrupted`; the session unblocks via explicit cancel or TTL expiry.
- `GET /api/v1/sessions` list entries gain a `generating` flag (derived from the active-run key) so clients can show which sessions still have a turn running.
- The existing partial-turn persistence path is reused unchanged for explicit cancels and errors; the `done` event enters the Redis run log but remains excluded from `messages` persistence (status quo).

## Capabilities

### New Capabilities
- `turn-run-lifecycle`: Run registry and Redis-backed run state — run id minting and delivery, session-busy mutual exclusion, explicit cancel by run id (including force-clear of dead runs), resume/replay of running-turn event streams, crash/interrupted semantics, and run-state resource cleanup.

### Modified Capabilities
- `session-management`: "Send message and stream response" changes — disconnect no longer cancels; 409 `SESSION_BUSY` on concurrent send; run id issuance on the response. "Session list" gains the `generating` flag. "Assistant event stream collection" feeds the Redis run event log in addition to the existing collection.
- `interrupted-turn-persistence`: The trigger for interrupted-turn persistence narrows — client disconnect no longer interrupts a turn; interruption now comes from explicit cancel, agent error, or process crash. Partial persistence behavior itself is unchanged.
- `service-roles`: Route ownership — the cancel and resume-events endpoints join the agent-role route partition (they need the run registry / run state); the session list `generating` flag rides the existing api-role list endpoint.

## Impact

- **Code**: `internal/handler/message_stream.go` (turn context decoupling, run minting, 409 check), new run-registry package (process-side running-turn table + fan-out), Redis store additions (stream append/read, meta, session-run claim), `internal/handler/router.go` (two new agent-partition routes), session list handler/serializer (`generating`), `api/openapi.yaml` (+ frontend `npm run generate-api` downstream).
- **Systems**: Redis gains three key families (`run:{rid}:events`, `run:{rid}:meta`, `session:{sid}:run`) with TTLs; no MySQL schema change; graceful shutdown must decide in-flight turn handling (cancel vs. wait).
- **Behavioral**: Frontend must handle 409 `SESSION_BUSY` + attach flow; token spend continues after page close by design (mitigated only by the cancel endpoint) — a product-level cost behavior change.
