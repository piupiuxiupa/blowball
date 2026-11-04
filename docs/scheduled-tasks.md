# Scheduled agent tasks

Blowball supports persistent user-owned one-shot (`at`) and standard five-field
`cron` tasks. The management API is available on `api/all`; background
materialization, recovery, and execution run only on `agent/all` when explicitly
enabled. No additional process role, worker service, or message broker is
required.

## Deploy or upgrade

1. **Apply migration 026 before enabling execution.** Existing databases are
   not migrated automatically by the Docker initdb mount (that mount runs only
   on a fresh volume):

   ```bash
   mysql -h 127.0.0.1 -u blowball -p blowball < migrations/026_scheduled_agent_tasks.sql
   ```

   The migration adds `scheduled_tasks` and `scheduled_task_runs`, including
   the unique `(task_id, scheduled_for)` slot key, stable occurrence/run/session
   identities, lease fields, and due/history indexes. It does not rewrite
   existing user data and contains no JWT or LLM-token columns.

2. **Deploy with scheduling disabled (the default).** Omitted configuration or
   `scheduled_tasks.enabled: false` starts no scheduler or recovery loop and
   leaves interactive chat, queue admission, SSE, and credential behavior
   unchanged. Once migration 026 is present, the task management and run-history
   APIs work even while background execution is disabled.

3. **Enable on each intended `agent/all` process** after validating migration,
   backups, and test execution:

   ```yaml
   scheduled_tasks:
     enabled: true
     poll_interval: 5s
     max_concurrent: 4
     max_run_duration: 10m
   ```

   All three knobs must be positive when enabled. `max_concurrent` is per Agent
   process; it is not a physical CPU, memory, or LLM-quota partition from chat.
   The run timeout starts at the startup fence, not while an occurrence waits
   for capacity.

## API and result reading

Owner-authenticated endpoints:

- `POST /api/v1/scheduled-tasks`
- `GET /api/v1/scheduled-tasks`
- `GET /api/v1/scheduled-tasks/{task_id}`
- `PATCH /api/v1/scheduled-tasks/{task_id}`
- `DELETE /api/v1/scheduled-tasks/{task_id}`
- `GET /api/v1/scheduled-tasks/{task_id}/runs`

`at` uses an RFC3339 `run_at` with an offset and an explicit IANA timezone.
`cron` uses exactly five fields plus a separate IANA timezone; macros, `@every`,
seconds, and expression timezone prefixes are rejected. Absolute API times are
UTC. The default session policy creates a fresh session per occurrence;
`existing` explicitly targets an owner session.

Occurrence history is MySQL-backed and survives Redis replay retention.
Messages, tool calls, artifact declarations, and usage use the associated
session and stable run ID. If the user deletes the session, execution facts
remain but conversation content is no longer guaranteed to be readable.

## Execution and recovery semantics

- MySQL is the durable source of future and pending work; future tasks are not
  preloaded into Redis. Each scan is bounded to 100 records.
- A task slot and its `next_run_at` advance in one transaction. Missed cron
  slots coalesce to one catch-up; the next slot is strictly after the scan time.
  A consumed one-shot is not replayed on restart or re-enable.
- One task has at most one non-terminal occurrence. A later slot is recorded as
  `skipped/TASK_OVERLAP` rather than queued behind an active occurrence.
- Background admission is queue-aware and fail-closed. An active chat or a
  non-empty interactive FIFO yields `skipped/SESSION_BUSY`; Redis uncertainty
  never degrades to unlocked model execution.
- Preparation uses short leases and claim tokens. Only definitely-unstarted
  failures receive up to three persistent attempts with backoff. An expired
  claimant cannot cross the startup fence.
- Task, owner-active, and target-session authorization is checked before
  preparation and again at the fence. `USER_UNAVAILABLE` and
  `SESSION_UNAVAILABLE` pause the task. Pausing/deleting prevents unstarted
  occurrences but does not implicitly cancel an already-running turn.
- A short-lived execution JWT is signed only after authorization and passed in
  memory. Login JWTs and per-user LLM tokens are never stored in task or run
  rows. LLM credentials resolve at execution time using the existing per-user
  resolver and global fallback rules; a configured user-token failure never
  falls back to the global key.
- Timeout uses the ordinary Run cancellation path, preserves partial output,
  and records `cancelled/TASK_TIMEOUT`.
- Recovery watches live runs, backfills reliable terminal Run state, and marks
  dead or uncertain submissions `interrupted/RESULT_UNKNOWN`. It never blindly
  reruns an uncertain Agent execution; external tool side effects are not
  exactly-once.

## Shared workspace requirement

The existing workspace rules are unchanged. Multiple Agent hosts executing for
the same user must use `storage.workspace.backend: shared` with an
operator-mounted shared POSIX filesystem (MinIO-backed JuiceFS), including the
existing `--allow-other` requirement for the executor sandbox. Otherwise a new
session created on one host may have different files and artifacts on another.
Task and occurrence identity is shared through MySQL, but that does not make a
local workspace shared.

## Rollback

1. Set `scheduled_tasks.enabled: false` on every Agent process.
2. Restart those processes in order so each stops new materialization and
   dispatch, cancels/drains scheduler-owned runs, and finishes bounded terminal
   persistence. API-only processes need no execution rollback and may continue
   serving task management/history.
3. Keep migration 026 and all task/run history. Do not drop the tables, replay
   consumed one-shots, or replay cron slots covered by the normal coalescing
   policy. Interactive chat continues unchanged.

If execution must be paused without a process restart, disable tasks through
their normal `PATCH enabled:false` management operation. That is separate from
the deployment-wide rollback switch.
