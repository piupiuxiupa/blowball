## Why

The model-facing `spawn_subagent` contract still advertises `resume_agent_id`, which encourages reusing one sub-agent instance even though the product now prefers isolated dispatches and per-run transcript lazy loading. History reads also still return every sub-agent token/reasoning fragment by default, so an updated client cannot request a lightweight placeholder-only view.

## What Changes

- Make every `spawn_subagent` dispatch create a fresh sub-agent instance:
  - remove `resume_agent_id` from the model-facing tool schema and prompt guidance;
  - reject spawn arguments containing `resume_agent_id`;
  - remove the agent id from the model-visible spawn result so the model is not encouraged to continue an instance;
  - retain `status` in the spawn result.
- Preserve unique display attribution for every dispatch: the effective sub-agent name remains the caller label plus the unique instance suffix.
- Keep internal per-run transcript persistence and the existing run detail APIs; they are used for lazy loading, not model-driven reuse.
- Add a backward-compatible message-history read mode:

  ```http
  GET /api/v1/sessions/:session_id/messages?subagent_content=placeholder
  ```

  In placeholder mode, dynamic sub-agent token/reasoning/tool payload rows are omitted; lightweight lifecycle markers (`agent_start`, `agent_end`, `agent_error`) remain as placeholders carrying `agent_instance_id` and `run_id`. The parent's full `spawn_subagent` tool result is also omitted because it duplicates the child's final output. The client expands placeholders through the existing run detail endpoints.
- Preserve the default behavior of `GET /messages` (`subagent_content=full`) for existing frontends and reject unknown values with 400.
- Update configuration examples and API documentation.
- **BREAKING** for the model-facing tool contract: `resume_agent_id` is no longer accepted and model-visible spawn results no longer expose `agent_id`. Existing HTTP APIs remain backward compatible through the default full-content mode.

## Capabilities

### New Capabilities

- None.

### Modified Capabilities

- `dynamic-subagents`: spawn always creates a fresh uniquely named instance and no longer exposes or accepts model-driven resume identity.
- `agent-orchestration`: orchestration prompt/tool behavior no longer tells Confucius to resume prior instances.
- `session-management`: message history supports an explicit sub-agent placeholder mode while retaining full-content mode as the default.

## Impact

- **Agent layer**: `internal/agent` spawn schema/description, argument decoding, dispatch, result rendering, and tests.
- **HTTP API**: `GET /sessions/:session_id/messages` gains the validated `subagent_content` query parameter; placeholder filtering is applied before pagination so page size remains meaningful.
- **MySQL**: message pagination gains a placeholder-view query that omits dynamic sub-agent payload rows and duplicated parent spawn results while retaining lifecycle placeholders.
- **OpenAPI / config docs**: document the query mode and remove resume guidance from example prompts.
- **Compatibility**: historical `messages` and `subagent_runs` rows remain readable; old clients that do not opt into placeholder mode continue to receive the previous full event stream.
