## Context

Dynamic sub-agents currently expose `resume_agent_id` to the model and append the stable instance id to every spawn result. That contract predates the per-run transcript API and encourages the model to continue an existing instance. Message history also has only one read shape: every persisted sub-agent token, reasoning fragment, tool event, and parent spawn result is returned.

The runtime already has the ingredients needed for a lighter frontend flow:

```text
unique agent_instance_id / label per fresh dispatch
run_id on sub-agent lifecycle events
subagent_runs per-run transcript detail API
```

## Goals / Non-Goals

**Goals:**

- Make every model-initiated `spawn_subagent` call create a fresh, isolated instance.
- Stop advertising or accepting `resume_agent_id` in the model-facing tool contract.
- Keep effective sub-agent labels unique even when callers reuse the same short name.
- Add an opt-in `subagent_content=placeholder` history mode.
- Keep placeholder mode paginated at the storage layer.
- Preserve the existing full-history response as the default for old clients.

**Non-Goals:**

- Do not delete historical `subagent_runs` rows or the per-run detail APIs.
- Do not remove internal per-run delta persistence; it still powers lazy loading and auditing.
- Do not change SSE live streaming.
- Do not synthesize new persistent placeholder rows in `messages`; existing lifecycle marker rows are the placeholders.
- Do not collapse top-level Confucius output.

## Decisions

### D1. Enforce fresh dispatch at the tool contract boundary

Remove `resume_agent_id` from:

- the synthetic tool JSON schema;
- `SpawnToolArgs`;
- tool descriptions and prompt guidance;
- model-visible spawn result markers.

Argument decoding already uses `DisallowUnknownFields`, so a model that sends the old field receives a bad_args tool result. The spawn result keeps `status` but omits `agent_id`, preventing the model from inventing a continuation flow.

The dispatcher no longer loads prior instance history. Each accepted call allocates a new `agent_instance_id`; the effective label remains `<caller-label>#<unique-instance-suffix>`.

*Alternative considered:* accept and silently ignore `resume_agent_id`. That would produce ambiguous behavior and make old prompts appear to succeed while actually creating a fresh agent. An explicit bad_args result is easier to diagnose.

### D2. Placeholder mode is opt-in and defaults to full

```http
GET /sessions/:session_id/messages?subagent_content=full
GET /sessions/:session_id/messages?subagent_content=placeholder
```

Omitting the parameter means `full`. Unknown values return 400. This preserves the existing frontend and API consumers while allowing the updated frontend to avoid long child payloads.

### D3. Filter placeholder mode before pagination

The MySQL pagination query adds a placeholder predicate rather than filtering a page in the handler:

```text
retain rows with no dynamic instance identity
retain dynamic lifecycle markers: agent_start, agent_end, agent_error
omit dynamic token/reasoning/tool payload rows
omit parent spawn tool_result rows whose tool_call_id matches a subagent_runs.run_id
```

This keeps `page_size` meaningful and avoids a page that becomes empty after post-filtering. The parent spawn `tool_call` remains because it carries the spawn arguments and run identity; the corresponding result is omitted because it duplicates the child's final output.

Lifecycle markers are sufficient placeholders: they already carry:

- `agent`
- `agent_instance_id`
- `run_id`
- terminal/error semantics

The frontend can call the existing run-list/detail endpoints when a placeholder is expanded.

*Alternative considered:* synthesize one new `subagent_placeholder` event row. That would require a new event type, historical backfill, and duplicate information already present in lifecycle markers.

### D4. Legacy rows retain full visibility

Rows without dynamic `agent_instance_id`/`run_id` are treated as non-dynamic history and remain visible in placeholder mode. This avoids losing old Chongzhi/Liang content for which no per-run detail API exists.

### D5. Internal per-run storage remains

Although model-driven resume is removed, `subagent_instances` and `subagent_runs` continue to be written. Every new dispatch is expected to be `run_no=1`, but the per-run schema and APIs remain useful for transcript lazy loading and compatibility with historical resumed instances.

## Risks / Trade-offs

- [Models trained or prompted from old guidance may send `resume_agent_id`] → The strict argument decoder returns a bad_args result without interrupting the parent loop; prompt/tool schemas no longer advertise the field.
- [Placeholder mode can hide the parent spawn result] → The corresponding child lifecycle markers and detail API retain the authoritative per-run transcript.
- [A custom deployed system prompt may still mention resume] → The tool schema no longer accepts the argument, so the obsolete instruction cannot trigger reuse.
- [Placeholder SQL adds a correlated predicate] → It remains session-scoped and shares the existing session/time index; the mode is opt-in and intended for history pages.
- [Older identity-less sub-agent rows cannot be lazily expanded] → They remain visible in full rather than being dropped.

## Migration Plan

1. Update tool schema, spawn parsing, dispatch, result rendering, and prompt/config examples.
2. Add `subagent_content` request validation and service/store passthrough.
3. Add the placeholder pagination SQL and integration fake behavior.
4. Extend unit, handler, and integration tests.
5. Update OpenAPI.

No database migration is required. Existing rows are interpreted in place. Rollback restores the prior tool schema/prompt and removes the query mode; persisted rows are unchanged.

## Open Questions

None.
