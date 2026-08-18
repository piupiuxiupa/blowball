# Redis-first message persistence — deploy / migrate / rollback runbook

This runbook covers the operational side of the Redis-first write-behind
message persistence (the `message-write-behind` capability; see the change
`openspec/changes/redis-first-message-persistence/` and the Persistence
section of `CLAUDE.md` for the architecture).

## What changed, operationally

- A turn's message batch is no longer written synchronously to MySQL and the
  filesystem. It is dual-written into Redis in one pipeline — the per-session
  read cache `msgs:{session_id}` (24h TTL) and the global FIFO ingest queue
  `msgs:buffer` (no TTL) — and a background flusher moves the queue into the
  MySQL `messages` table in batches.
- **Redis is now the only synchronous durability layer.** It MUST run with
  AOF persistence (`appendonly yes`). Without AOF, a Redis restart loses every
  message still sitting in the flush window (≤ `messages.flush_interval`, 1s
  by default). The server probes `appendonly` at startup (best-effort
  `CONFIG GET`; probe failures are skipped) and logs a WARN when it is off.
- The filesystem warm tier is gone. `data/{userID}/sessions/` directories are
  never read or written again and can be cleaned up manually once the new
  build is confirmed stable. Messages that existed only in those files
  (written by the pre-change code when a MySQL insert failed) are **accepted
  as lost** — they are not migrated.
- Session deletion now proactively clears the Redis `msgs:{id}` /
  `session:{id}` keys after the database purge (previously TTL-only).
- The message-history endpoint has a read-your-writes window of ≤
  `messages.flush_interval` (default 1s) after a turn. The SSE stream itself
  is unaffected.

## New Redis keys

| Key | Shape | TTL | Purpose |
|-----|-------|-----|---------|
| `msgs:buffer` | LIST of canonical message JSON | **none** | global ingest queue, consumed by the flusher |
| `msgs:processing` | LIST of canonical message JSON | **none** | records claimed by an in-flight flush batch; residue is requeued at process startup |
| `msgs:{session_id}` | LIST of canonical message JSON | 24h | per-session read cache (pre-existing) |

Queue health: `LLEN msgs:buffer` (pending), `LLEN msgs:processing` (in-flight
/ crash residue). A persistently growing `msgs:buffer` means MySQL is down or
too slow — the flusher retries forever and never drops; check the
`msgflush flush failed` WARN logs (they carry `queue_len`) and restore MySQL.

## New MySQL migration — apply manually on existing databases

`migrations/012_client_msg_id.sql` adds `messages.client_msg_id CHAR(36)
NULL` plus the UNIQUE index `uk_messages_client_msg_id`. The docker-compose
initdb mount only runs on first volume init, so **existing databases must
apply `012` by hand**:

```bash
mysql -h <host> -u <user> -p <blowball> < migrations/012_client_msg_id.sql
```

Legacy rows keep `NULL` (MySQL UNIQUE indexes allow repeated NULLs), so old
data is unaffected. The old code ignores the column, so the migration can be
applied before or after the binary upgrade — applying it **before** closes
the small window in which the new code would run without its idempotency
guarantee.

## Deploy

1. **Pre-check**: enable AOF on Redis (`appendonly yes`; a restart of the
   Redis service is required to activate it — do this in a maintenance
   window, BEFORE deploying the new build). Verify with
   `redis-cli CONFIG GET appendonly`.
2. Apply `migrations/012_client_msg_id.sql` to the existing database (see
   above).
3. Deploy the new build. On startup the flusher logs `msgflush flusher
   started` (interval + batch size); the AOF probe logs a WARN if Redis still
   runs without AOF.
4. Optional `messages:` config block (defaults shown):

   ```yaml
   messages:
     flush_interval: 1s
     flush_batch_size: 100
   ```

5. Verify: send a test message and confirm the row appears in `messages`
   within ~1s (the flush interval), and `LLEN msgs:buffer` returns to 0.

For split-role deployments: the flusher goroutine runs only in the `agent`
and `all` roles; the `api` role never consumes the queue (it still drains
synchronously before deleting a session). At least one `agent`/`all` process
must be running for queued messages to reach MySQL.

## Rollback

1. **Wait for the queue to empty before rolling back** — the old build does
   not know about `msgs:buffer`, so anything still queued at downgrade time
   is lost. Confirm `LLEN msgs:buffer` and `LLEN msgs:processing` both return
   0 (and stay 0 through one flush interval of quiet traffic), or stop the
   traffic first.
2. Redeploy the previous build. The `client_msg_id` column is invisible to
   the old code (`INSERT` lists don't mention it; new rows get `NULL`), so no
   schema rollback is required.
3. Leftover queue keys from the new build (`msgs:buffer`, `msgs:processing`)
   can be deleted by the operator once the rollback is confirmed:
   `redis-cli DEL msgs:buffer msgs:processing`.
4. The `data/{userID}/sessions/` directories are irrelevant to the old build
   only if it predates the FS warm tier — a rollback to a pre-change build
   resumes writing them; no action needed.

## Post-deploy cleanup (optional, once stable)

- Remove the legacy `data/{userID}/sessions/` trees (`rm -rf
  {data-dir}/data/*/sessions`) after confirming the new persistence is
  healthy. They are dead weight: nothing reads them, and any message that
  never made it to MySQL in them is already accepted as lost.
