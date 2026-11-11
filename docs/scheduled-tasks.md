# Scheduled agent tasks (standalone Scheduler)

Blowball supports persistent user-owned one-shot (`at`) and standard five-field
`cron` tasks. As of **extract-scheduler-service** the scheduling runtime is no
longer embedded in the main service:

```
API (api/all) ──gRPC(mTLS)──> Scheduler <──gRPC(mTLS)── Agent runtime (agent/all)
    │                           │                          │
    │ task resources,           │ scheduled_tasks,         │ scheduled_* sessions,
    │ task sessions,            │ scheduled_task_runs      │ execution receipts
    │ execution receipts        │                          │
    └──────────── shared MySQL / shared workspace ─────────┘
```

* **Scheduler** owns `scheduled_tasks`, `scheduled_task_runs`, time-rule
  materialization, claims, start authorization and the scheduling projection.
  It never constructs an Agent, loads an LLM key or mounts a user workspace.
* **Main service** owns task sessions (`scheduled_sessions` + child tables),
  `.cron` resource versions and the authoritative execution receipts. The API
  role serves task/resource/session management; the agent role accepts and runs
  scheduled executions through the same turn engine as interactive chat.
* **`all` never embeds a scheduler.** `scheduled_tasks.enabled: true` now fails
  config load with a migration error.

## Deploy or upgrade

Existing databases must apply migrations manually; the Docker initdb mount only
runs on a fresh volume.

1. **Apply the additive migrations first** (026 → 030):

   ```bash
   for f in migrations/026_scheduled_agent_tasks.sql \
            migrations/027_scheduled_service_fields.sql \
            migrations/028_scheduled_session_store.sql \
            migrations/029_scheduled_execution_control.sql; do
     mysql -h 127.0.0.1 -u blowball -p blowball < "$f"
   done
   ```

   027 marks every pre-existing occurrence `session_source=legacy_interactive`
   and pauses `session.mode=existing` tasks with
   `SESSION_POLICY_MIGRATION_REQUIRED`. It does not move or copy any ordinary
   session data.

2. **Stop the old embedded scheduler** (`scheduled_tasks.enabled: true` on the
   previous binary) and let its non-terminal occurrences settle. Then apply the
   data-only quiesce:

   ```bash
   mysql -h 127.0.0.1 -u blowball -p blowball < migrations/030_legacy_scheduled_quiesce.sql
   ```

   030 cancels unstarted legacy occurrences, marks started/unknown ones
   `interrupted`/`RESULT_UNKNOWN`, and gives their tasks a persistent
   `execution_hold_occurrence_id`. The hold blocks later occurrences until an
   operator supplies a stop proof; nothing is replayed.

3. **Deploy the main service** with `internal_rpc` enabled (see
   `config.example.yaml`). The service starts even when the Scheduler is
   offline; task HTTP endpoints then return 503, the scheduled-task agent tools
   are not registered, and `.cron` resource writes fail closed. Ordinary chat,
   sessions and workspace behavior are unaffected.

4. **Deploy the Scheduler** (`make build-scheduler` →
   `bin/scheduler -f services/scheduler/config.yaml -d /var/lib/blowball`) with
   execution **off** (`scheduler.execution_enabled: false`). Task management and
   history work; no materialization/submission happens.

5. **Validate** with a disabled test task, a resource publication, a manual
   trigger (enable a future `at`), history reads, cancellation, a restart and
   the rollback drill. Then set `scheduler.execution_enabled: true`.

6. **Legacy `existing` tasks** are paused. The owner must submit an update
   (which always migrates the task to one dedicated session per occurrence);
   its historical ordinary conversation is untouched and remains readable.

## Configuration

Main service (`config.yaml`):

```yaml
internal_rpc:
  enabled: true
  listen: ":9092"
  timeout: 5s
  tls: {cert_file: /etc/blowball/tls/main.crt, key_file: ..., ca_file: ...}
  scheduler_endpoint: "scheduler.internal:9091"
  scheduler_tls: {cert_file: ..., key_file: ..., ca_file: ..., server_name: blowball-scheduler}

scheduled_runtime:        # per-instance admission guard, not a CPU/memory quota
  poll_interval: 5s
  lease_duration: 30s
  max_concurrent: 4
  max_prepare_attempts: 3
  scan_batch: 50

scheduled_resources:      # `.cron` publication + pinned-context bounds
  max_files: 1000
  max_total_bytes: 104857600
  max_file_bytes: 104857600
  max_context_files: 100
  max_context_bytes: 4000
```

Scheduler (`services/scheduler/config.example.yaml`): MySQL DSN, its own mTLS
listener, main-service API/agent endpoints, and
`scheduler.execution_enabled` / `poll_interval` (5s) / `max_concurrent` (4) /
`max_run_duration` (10m). Every bound must be positive; non-positive values fail
startup. `max_run_duration` becomes the grant deadline enforced by the Runtime.

Internal service identities are certificate CommonNames: `blowball-scheduler`
on the Scheduler side and `blowball-main` / `blowball-api` / `blowball-agent` on
the main side. Per-method allowlists and deadline enforcement are in
`internal/rpcx`; external clients cannot forge an actor.

## Execution lifecycle

1. **Materialize** (Scheduler, one MySQL transaction): claim due tasks with
   `FOR UPDATE SKIP LOCKED`, insert one occurrence per `(task_id, scheduled_for)`
   slot with stable `occurrence_id`/`run_id`/`session_id` and an immutable
   configuration/resource snapshot, then advance `next_run_at` (cron: strictly
   after now; at: cleared). Missed cron slots coalesce to the earliest saved
   slot. A task-level execution hold produces `skipped`/`TASK_EXECUTION_UNCERTAIN`
   rows instead of executable ones; an active occurrence produces
   `skipped`/`TASK_OVERLAP`.
2. **Submit** (Scheduler → Agent RPC): claim with a lease/attempt budget (3
   persistent retries), persist `launching`, submit the stable identity +
   non-secret configuration fingerprint. `SubmitScheduledRun` is idempotent: the
   same identity+fingerprint resolves to the same receipt/session; a different
   one is rejected. The Runtime fetches the authoritative occurrence and
   re-validates owner, model, resources and fingerprint before creating
   anything.
3. **Prepare** (Agent Runtime): CAS/lease the receipt, validate the pinned
   resource revision, and admit through the existing background admission path
   (fail-closed when Redis cannot confirm mutual exclusion). An admitted turn
   persists `start_intent` FIRST, then calls `AuthorizeStart`.
4. **Authorize** (Scheduler): atomically checks task enabled/not-deleted, the
   execution hold, claim/attempt, and the receipt generation; persists the
   unique grant `(generation, logical started_at, fixed deadline)`; pause/delete
   committed first wins. A lost grant response is resolved by `GetStartGrant`;
   if the outcome cannot be confirmed the receipt becomes `uncertain` and the
   turn never starts.
5. **Run** (Agent Runtime): persist `running` with the grant, then start the
   ordinary turn (`scheduled_sessions` table group). The watchdog is measured
   from the grant deadline and does not depend on the Scheduler being online; on
   timeout it cancels the run, persists partial output and records
   `cancelled`/`TASK_TIMEOUT`.
6. **Finalize**: the terminal message/tool batch is written first; only then is
   the terminal receipt fact written (with a positive stop proof). If the final
   batch is not durable, the receipt records `error`/`RESULT_PERSIST_FAILED`
   instead of claiming success. The Scheduler reconciles by querying
   `GetScheduledExecution`; it never relies on a callback or Redis TTL.
7. **Recovery**: `accepted`/`preparing` executions are safe to reclaim
   (generation bump fences the old worker). `start_intent` or later with an
   unknown outcome becomes `uncertain`; it is never replayed automatically. The
   Scheduler projects `interrupted`/`RESULT_UNKNOWN` and sets the task-level
   hold. Only a generation-bound positive stop proof (normal terminal
   finalization or controlled reconciliation) clears the hold. Late workers
   cannot overwrite a terminal/newer generation.

## Task sessions

Every occurrence gets one physical session in the scheduled table group
(`scheduled_sessions`, `scheduled_messages`, `scheduled_tool_calls`,
`scheduled_titles`, `scheduled_turn_usage`, `scheduled_context_compactions`,
`scheduled_subagent_instances/runs`, `scheduled_attachments` + archives). They
are never visible to ordinary session endpoints, and ordinary session IDs never
resolve on scheduled endpoints (404 both ways). The turn engine, compaction,
sub-agent transcripts, usage and memory behavior are shared; only the storage
adapter differs.

Read-only API (api role; no send-message or manual re-run exists):

| Method | Path | Notes |
|--------|------|-------|
| GET | `/api/v1/scheduled-sessions?task_id=&limit=&offset=` | default 20, max 100 |
| GET | `/api/v1/scheduled-sessions/{session_id}` | includes the immutable snapshot + actual model when the Scheduler is reachable |
| GET | `/api/v1/scheduled-sessions/{session_id}/messages` | same cursor/order/`subagent_content` contract |
| DELETE | `/api/v1/scheduled-sessions/{session_id}` | 409 until execution stop is proven; never deletes the task/occurrence/`.cron` |
| POST | `/api/v1/scheduled-sessions/{session_id}/runs/{run_id}/cancel` | agent role; explicit cancel, not a stop proof |

Task history (`GET /api/v1/scheduled-tasks/{task_id}/runs`) remains readable
after the task is soft-deleted; each occurrence reports its explicit
`session_source` (`scheduled` / `legacy_interactive`) and `content_deleted` when
the associated conversation has been deleted.

## `.cron` resources

Layout inside the user's workspace:

```
.cron/.blowball-managed.json          # namespace ownership marker
.cron/<task_id>/.task.json            # task ownership marker
.cron/<task_id>/draft/...             # editable files (ordinary files only)
.cron/<task_id>/revisions/<rev>/      # immutable published files + manifest.json
.cron/<task_id>/.staging/             # controlled publication staging
```

* Resources are optional. A prompt-only task has an empty reference. To add
  resources: create a disabled task, edit `.cron/<task_id>/draft/` through the
  controlled workspace endpoints/tools, publish, then bind the returned
  `resource_revision` + `manifest_hash` with an expected task revision.
* Existing unmanaged `.cron` content conflicts (`RESOURCE_NAMESPACE_CONFLICT`)
  and is never overwritten.
* Publication walks only regular files, rejects symlinks/devices/FIFOs/escaping
  paths, enforces `scheduled_resources` limits, copies into staging, hashes the
  ACTUAL copied bytes, writes a path-sorted SHA-256 manifest, re-checks the
  draft revision/generation fence, and atomically renames the staging tree into
  `revisions/<rev>/`. The same `request_id` is idempotent; concurrent edits
  return `409 REVISION_CONFLICT`.
* Write protection is shared by REST text/upload/create/delete/rename, the
  Office save callback and the `xizhi_*` write/modify/delete tools: only paths
  strictly below `draft/` are writable (the task root, `revisions/`, `.staging/`
  and markers are read-only). Every bash sandbox mounts `.cron` read-only (an
  empty read-only namespace before initialization). Reads remain allowed.
* Execution pins `resource_revision`/`manifest_hash`: the Runtime re-verifies
  the manifest hash, every file's SHA-256 and the absence of extra files, then
  injects a bounded resource pointer into the turn's model context. The stored
  `Task.prompt` remains the original text; contents are read on demand through
  the ordinary tools and are never auto-executed.

Resource endpoints (api role):

| Method | Path | Notes |
|--------|------|-------|
| GET | `/api/v1/scheduled-tasks/{task_id}/resources` | draft revision/files + version metadata |
| POST | `/api/v1/scheduled-tasks/{task_id}/resources/publish` | `{expected_draft_revision, request_id}`; idempotent |

Agent tools (registered only when the Scheduler is configured):
`scheduled_task_create/list/get/update/pause/resume/delete` and
`scheduled_task_resources_get/publish`. The actor always comes from the trusted
execution context; tools share the same validation/revision/ownership rules as
HTTP and never expose SQL, crontab or arbitrary proxy execution.

## Rollback

Rollback returns to **scheduling disabled**, not to the embedded scheduler:

1. Set `scheduler.execution_enabled: false` and stop the Scheduler process.
   Already accepted Runtime work continues under its own bounded lifecycle;
   restart the agent role first if it is down so receipts can finalize.
2. Keep the main service's new tables, resources and read services. Do not drop
   data, copy rows into ordinary tables, or start the old embedded scheduler
   (`scheduled_tasks.enabled: true` now fails config load).
3. Do not re-submit an uncertain execution or a consumed `at` slot after a
   rollback. Task history remains readable through the scheduled-session APIs.
4. If the main-service binary itself must be rolled back, first disable/drain
   scheduled execution and take SQL/workspace backups; the older binary cannot
   read the scheduled table group.

## Service identity and database write boundaries

Issue separate MySQL accounts so neither process can write the other's tables:

```sql
-- Scheduler: task definitions/occurrences only.
GRANT SELECT, INSERT, UPDATE ON blowball.scheduled_tasks      TO 'blowball_scheduler'@'%';
GRANT SELECT, INSERT, UPDATE ON blowball.scheduled_task_runs  TO 'blowball_scheduler'@'%';

-- Main service: scheduled conversations, receipts and resource metadata.
-- It also keeps its existing ordinary-session/user/write grants.
GRANT SELECT, INSERT, UPDATE, DELETE ON blowball.scheduled_sessions             TO 'blowball_main'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON blowball.scheduled_messages             TO 'blowball_main'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON blowball.scheduled_tool_calls           TO 'blowball_main'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON blowball.scheduled_titles               TO 'blowball_main'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON blowball.scheduled_turn_usage           TO 'blowball_main'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON blowball.scheduled_context_compactions  TO 'blowball_main'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON blowball.scheduled_subagent_instances   TO 'blowball_main'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON blowball.scheduled_subagent_runs        TO 'blowball_main'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON blowball.scheduled_attachments          TO 'blowball_main'@'%';
GRANT SELECT, INSERT         ON blowball.scheduled_execution_receipts           TO 'blowball_main'@'%';
GRANT SELECT, INSERT, UPDATE, DELETE ON blowball.scheduled_resource_drafts      TO 'blowball_main'@'%';
GRANT SELECT, INSERT, DELETE ON blowball.scheduled_resource_versions            TO 'blowball_main'@'%';
-- Plus the *_deleted archive mirrors used by session deletion.
```

The Scheduler's `AuthorizeStart` is the only path that writes the start grant;
the Runtime's receipts are the authoritative execution facts, and the
Scheduler never mutates them directly (it reads them through the Runtime RPC).
A systemd unit example lives at
`deploy/scheduler/blowball-scheduler.service`; the binary is `make
build-scheduler` (`bin/scheduler`).

## Testing prerequisites

* `make test` covers unit/package tests. MySQL-backed Scheduler/receipt/resource
  tests require `MYSQL_TEST_DSN` (point it at a disposable database).
* The bash `.cron` read-only mount is verified with a real `bwrap` on Linux;
  macOS/Windows development does not provide that isolation and skips the
  sandbox test.
* Cross-process fault-matrix tests live in `test/integration/`.
