## Context

Dynamic sub-agents already have two identities:

- `agent_instance_id`: stable across resumes and identifies one sub-agent thread;
- `run_id` / `parent_tool_call_id`: one dispatch of that thread, already stamped on every stream and message event.

The current `subagent_runs` table nevertheless uses `UNIQUE(session_id, agent_instance_id)` and stores one full OpenAI-chat snapshot per instance. Every resume upserts and replaces that snapshot, so previous executions are lost. The frontend therefore reconstructs sub-agent history from fragmented `messages` token/reasoning rows, which is slow for long content and parallel runs.

The event architecture remains unchanged:

```text
SSE / run:{run_id}:events  → live streaming and short-lived replay
messages                   → append-only display/history event ledger
subagent_runs              → durable, resumable model-context history
```

This change reshapes only the third area.

## Goals / Non-Goals

**Goals:**

- Preserve every completed sub-agent run as a per-run row.
- Store only the messages added by that run in each row's `messages_json`.
- Reconstruct a complete model context by stitching an instance's linear run chain.
- Expose run metadata and sanitized per-run transcripts through lazily loaded APIs.
- Preserve the external meaning of `agent_instance_id`, `run_id`, spawn arguments, SSE, and `messages`.
- Migrate existing full snapshots as readable legacy data without inventing overwritten history.

**Non-Goals:**

- Do not synthesize or replace long-lived `messages` event rows.
- Do not expose internal OpenAI-chat snapshots, system prompts, tool definitions, or resume metadata verbatim.
- Do not make active runs queryable from durable snapshots before they terminate.
- Do not introduce attempt-level rows for dispatch retries; attempts of one dispatch share one `run_id`.
- Do not change main-agent context reconstruction or sub-agent isolation.

## Decisions

### D1. Split instance metadata from run history

Introduce `subagent_instances` as the stable-thread table and repurpose `subagent_runs` as a per-execution table.

```text
subagent_instances
  session_id + agent_instance_id unique
  parent_instance_id, depth, name
  system_prompt, tools_json
  latest_run_id

subagent_runs
  session_id + run_id unique
  session_id + agent_instance_id + run_no unique
  previous_run_id, status
  messages_json = only this run's delta
  base/context counters and context_bytes
```

Instance metadata is stored once rather than repeated on every run. `latest_run_id` gives resume an inexpensive authoritative chain head; `run_no` and `previous_run_id` make ordering and lineage explicit.

*Alternative considered:* retain one denormalized `subagent_runs` table and repeat instance metadata on each row. That avoids a second table but duplicates system prompts and makes latest-run concurrency harder to enforce.

### D2. Store only the current run's message delta

Each new run row stores:

```text
the user task added by this dispatch
assistant messages produced by this dispatch
tool messages produced by this dispatch
round-cap wrap-up messages produced by this dispatch
```

It does not repeat:

```text
system prompt
prior tasks
prior assistant/tool messages
```

The dispatcher knows the history passed into `SubAgent.Run`. After the run it compares `LastRunMessages()` against that prefix, validates the prefix, and marshals only the suffix into `messages_json`.

Per-row counters support integrity and cheap resume eligibility:

- `base_message_count`: non-system messages before this run;
- `message_count`: messages in this delta;
- `context_message_count`: total non-system messages after appending this delta;
- `context_bytes`: serialized size of the complete system + stitched context.

*Alternative considered:* store a full cumulative snapshot in each run and use an offset to expose only the current run. That is simpler for resume but duplicates increasingly large context after every resume and conflicts with the requirement that each row contain only run-local data.

### D3. Reconstruct resume context by stitching the linear run chain

Resume loads the instance, resolves `latest_run_id`, reads its runs in `run_no` order, and appends their deltas. Before use it validates:

```text
first run.base_message_count == 0
run[i].base_message_count == run[i-1].context_message_count
run[i].run_no == run[i-1].run_no + 1
run[i].previous_run_id == run[i-1].run_id
```

The instance system prompt is prepended once. A broken chain, undecodable JSON, or missing predecessor makes the target ineligible and the spawn fails as bad_args.

### D4. Use transactional compare-and-set for latest run

Appending a run is transactional:

1. insert the run row keyed by `(session_id, run_id)`;
2. update `subagent_instances.latest_run_id` only when it still equals the observed predecessor;
3. commit.

If the latest-pointer update affects zero rows, another dispatch resumed the same instance first. The losing dispatch returns an error rather than creating a fork. This keeps every instance as a linear chain:

```text
run 1 → run 2 → run 3
```

Initial runs use a NULL predecessor. A dispatch-level retry upserts the same `(session_id, run_id)` row and does not allocate a new run.

### D5. Preserve legacy snapshots explicitly

Existing rows contain a full snapshot and cannot be split into historical runs because previous full snapshots were overwritten. Migration therefore:

1. creates one `subagent_instances` row per existing `(session, instance)`;
2. backfills the legacy run's `run_id` from the last `messages.run_id` for that instance when available, otherwise `legacy:<agent_instance_id>`;
3. marks the row `snapshot_kind='legacy_full'`;
4. sets counters from that full snapshot;
5. leaves its `messages_json` as a full context baseline.

New rows use `snapshot_kind='delta'`. Resume stitching treats a legacy full row as the baseline and appends later delta rows to it. The API does not fabricate older per-run rows that no longer exist.

### D6. Expose sanitized transcript DTOs, not internal snapshots

Add authenticated, session-scoped routes:

```text
GET /api/v1/sessions/:session_id/subagents/:agent_instance_id/runs
GET /api/v1/sessions/:session_id/subagents/:agent_instance_id/runs/:run_id
```

The list endpoint returns run metadata only. The detail endpoint decodes one run delta and maps:

```text
system     → omitted
user       → task
assistant  → assistant content/reasoning/tool_calls
tool       → tool_result
unknown    → omitted with a warning
```

Responses do not expose `messages_json`, `tools_json`, `system_prompt`, `resume_eligible`, or context counters as internal fields. Session ownership is checked before instance/run lookup; missing or cross-session objects all return 404 without disclosing existence.

### D7. Terminal snapshots only

Rows are written after a run terminates, matching the current save timing. An active run has no durable snapshot row yet; live clients continue using SSE or the run stream, and the detail endpoint returns 404 until the terminal write. This avoids partial-row lifecycle and crash-state ambiguity.

## Risks / Trade-offs

- [Delta chain corruption blocks resume] → Validate predecessor, run number, counters, and JSON before using a snapshot; fail as bad_args without exposing internal details.
- [Parallel resumes can fork an instance] → Update `latest_run_id` with a transactional compare on the observed predecessor and let only one writer win.
- [Legacy snapshots are not true per-run deltas] → Mark them `legacy_full`, treat them as a baseline, and do not fabricate overwritten history.
- [Large contexts require reading several delta rows on resume] → Store `context_message_count`/`context_bytes` and reject over-limit chains before unnecessary decoding where possible.
- [Retry attempts are not individually durable] → Keep one row per dispatch `run_id`; attempt-level observability remains available through existing event/debug paths.
- [Terminal-error partial stream output may not be in model-context delta] → The API returns the persisted model delta; the existing event/run stream remains the source for live partial display. Extending display-only partial capture is intentionally deferred.
- [API DTO changes could leak internal context accidentally] → Define explicit response structs and tests that reject raw snapshot/system/tool metadata fields.

## Migration Plan

1. Add the `subagent_instances` schema and per-run columns/indexes while keeping the old snapshot table readable.
2. Backfill instance rows and the legacy run identity/kind/counters.
3. Remove the old `(session_id, agent_instance_id)` uniqueness from `subagent_runs` and enforce:
   - `UNIQUE(session_id, run_id)`;
   - `UNIQUE(session_id, agent_instance_id, run_no)`.
4. Switch the coordinator write path to delta rows and the read path to chain stitching behind the existing `SubAgentSnapshotStore` boundary.
5. Add the API routes and OpenAPI contract.
6. Run store, coordinator, handler, integration, and migration tests.

Rollback before release can restore the previous single-snapshot write/read implementation; after release, do not remove the instance table or delta rows because they are the only durable record of historical executions.

## Open Questions

None. The intended first implementation is terminal, model-context delta storage with sanitized per-run detail APIs.
