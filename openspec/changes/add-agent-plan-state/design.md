## Context

Blowball now uses a dynamic sub-agent topology. Confucius is the root coordinator and dispatches generic isolated agents through the synthetic `spawn_subagent` tool. The spawn coordinator owns depth, concurrency, total-dispatch budgets, stable `agent_instance_id` values, per-run identities, usage attribution, and authoritative resume snapshots in `subagent_runs`. Sub-agent execution state is therefore already structured and persistent.

The missing layer is semantic orchestration state. When Confucius decomposes work, that plan currently exists only in assistant prose. That is fragile for parallel dispatch: the UI cannot distinguish planned work from narration, and later LLM rounds must rely on natural-language context to know which semantic steps remain. The design follows the host-managed-state principle: the model submits structured state through a tool; the host validates, owns, versions, renders, persists, and returns it.

## Goals / Non-Goals

**Goals:**

- Give Confucius a structured, turn-scoped semantic plan ledger.
- Keep plan updates whole-snapshot and deterministic.
- Support multiple simultaneously in-progress steps to match parallel `spawn_subagent` dispatch.
- Make the host, not model prose, the authority for normalized plan state and revisions.
- Expose plan state to clients through a structured, persisted SSE event.
- Preserve dynamic sub-agent isolation and keep semantic plan state separate from execution-instance state.
- Make dispatch ordering deterministic when plan updates and tool calls appear in the same assistant round.

**Non-Goals:**

- No durable cross-turn or cross-session task database.
- No `TaskCreate` / `TaskGet` / `TaskList` / `TaskUpdate` ID CRUD API.
- No dependency DAG, ownership transfer, blocked-by relations, or background task queue.
- No child-to-parent or sibling-to-sibling plan writes.
- No local sub-agent plans in this change.
- No plan-approval mode; `update_plan` is progress state, not a read-only planning gate.
- No automatic translation from `subagent_runs.status` to semantic plan status.

## Decisions

### D1: `update_plan` is a root-only synthetic orchestration tool

Confucius's tool JSON will include `update_plan` alongside `spawn_subagent`; ordinary sub-agent tool JSON will not include it. Dispatch will intercept it before consulting `tool.Registry`, just as it intercepts `spawn_subagent`.

This avoids a registry dependency on agent-loop state and prevents the tool from becoming a workspace capability that a child can inherit or request through tool narrowing. Plan state is orchestration control-plane state, not a user-space tool.

Alternative considered: register a stateless registry tool and capture a turn ledger in its closure. Rejected because it blurs the capability boundary, complicates per-turn construction, and risks unintended inheritance by dynamic children.

### D2: Use a whole-plan snapshot with host-assigned revisions

The model will submit an entire plan:

```json
{
  "steps": [
    {"step": "Inspect event stream", "status": "completed"},
    {"step": "Design plan state", "status": "in_progress"}
  ],
  "explanation": "Investigation is complete."
}
```

The host will validate and normalize the snapshot and assign a monotonic revision starting at `1` for each top-level Confucius turn. The tool result will return the canonical normalized snapshot, not a bare acknowledgement.

Initial status values are only:

- `pending`
- `in_progress`
- `completed`

Unlike Claude Code/Codex-style single-active-step conventions, multiple `in_progress` steps are allowed because Blowball explicitly supports parallel spawn calls in one assistant round.

Bounded input keeps malformed plans from becoming a context or UI denial of service: at least one and at most twenty nonempty steps, at most 1000 runes per step, and at most 2000 runes for the optional explanation. Invalid submissions leave the previous snapshot and revision unchanged.

Alternative considered: ID-based task CRUD. Rejected for this change because dynamic sub-agent instances already have stable IDs and persisted execution state; introducing semantic task IDs creates multi-writer and consistency concerns that are unnecessary until tasks outlive a turn or require dependencies.

### D3: Process plan updates before parallel execution

Current tool dispatch executes a round's tool calls concurrently. If the model emits `update_plan` together with spawn or registry calls, the plan update must be applied first.

Dispatch will therefore split an assistant round into two phases:

1. Validate and apply at most one `update_plan` call synchronously.
2. Execute all spawn and registry calls through the existing parallel path.

If a round contains more than one `update_plan`, both plan calls return an argument error and no plan revision or `plan_updated` event is emitted. This pre-validates the batch before mutation and avoids nondeterministic revision ordering.

Alternative considered: rely on prompt guidance to make `update_plan` a separate round. Rejected because parallel dispatch is a core runtime behavior; semantic ordering should not depend only on model compliance.

### D4: Canonical state is emitted and persisted as `plan_updated`

A successful update emits:

```text
event: plan_updated
```

with:

- `Agent = Confucius`
- `Content = canonical plan JSON`
- `Meta.revision = <host-assigned revision>`

The canonical JSON contains the revision, normalized steps, and optional explanation. Putting the complete snapshot in `Content` is intentional: message persistence retains event content, while arbitrary Meta fields are not part of the persisted message body. `plan_updated` message rows will use an empty role, preserving the existing convention for marker/UI events.

The event is additive for SSE and turn event logs. Old clients that ignore unknown event types remain functional.

Alternative considered: only include the plan in tool result events. Rejected because the UI would need to parse generic tool envelopes and could not distinguish a rendered plan snapshot from an activity log.

### D5: Prompt history uses tool call/result; plan events stay display state

`plan_updated` rows will not be reconstructed as OpenAI chat messages. The model sees plan updates through the top-level `update_plan` tool call and canonical tool result, which already participate in the existing tool-call history reconstruction path. Sub-agent event rows continue to be excluded from the main prompt context.

Because the V1 ledger is turn-scoped, mid-turn compaction cannot orphan the live in-memory plan. After the turn ends, prior `plan_updated` rows are historical UI data; a later turn starts with revision `1` and may create a new plan.

Alternative considered: make the latest session plan a durable canonical record now. Deferred because cross-turn plan continuity, supersession, and compaction rehydration deserve a separate change once product semantics are settled.

### D6: Semantic status is not copied from sub-agent execution status

`subagent_runs.status` describes an execution attempt:

- `completed`
- `capped`
- `error`

Plan status describes Confucius's semantic judgment. A completed child whose result fails acceptance does not complete a plan step. A capped child may leave the step `in_progress` while Confucius resumes it. An error may lead to retrying, narrowing, replanning, or abandoning the semantic step.

Only a subsequent valid `update_plan` snapshot can mark a semantic step completed.

Alternative considered: automatically complete a step when its referenced sub-agent completes. Rejected because it conflates execution completion with result verification and removes Confucius's coordination responsibility.

### D7: Sub-agents receive explicitly distilled plan context

The root plan will not be automatically injected into child prompts. Confucius must put the relevant step, acceptance criteria, constraints, paths, dependencies, and prerequisite results into `spawn_subagent.task` and `context`, which are assembled into the existing self-contained child user message.

This preserves the dynamic-subagent isolation model and avoids leaking sibling work or overinflating child contexts.

Alternative considered: inject the entire plan into every child. Rejected because it turns the plan into an implicit shared blackboard and weakens the existing no-sibling-communication boundary.

## Risks / Trade-offs

- [Model may narrate plans in prose as well as calling `update_plan`] → Update Confucius prompt discipline to say the harness renders structured plans and duplicated full-plan prose is unnecessary.
- [Whole-snapshot updates can accidentally drop or rewrite steps] → Keep snapshots small and bounded, return the canonical snapshot in the tool result, and rely on revision monotonicity; add tests for invalid and unchanged updates.
- [Plan semantics may be confused with sub-agent execution status] → Explicitly prohibit automatic status mapping and cover completed/capped/error child outcomes with tests.
- [Same-round ordering could regress if dispatch is refactored] → Add an integration-style dispatch test asserting `plan_updated` precedes parallel tool activity.
- [Historical clients might not understand `plan_updated`] → The SSE event is additive; clients that ignore unknown events retain current behavior.
- [Turn-scoped plans do not survive a later user request] → This is an explicit V1 limitation; represent future cross-turn plans in a separate durable-plan change rather than overloading `subagent_runs`.

## Migration Plan

1. Add the synthetic tool, turn-local ledger, event constructor, and dispatch pre-phase behind the existing Confucius construction path.
2. Extend event mapping and the OpenAPI SSE contract additively.
3. Update Confucius's configured prompt guidance.
4. No SQL migration or deployment-only configuration is required.
5. Rollback removes the model-facing tool and stops emitting `plan_updated`; persisted rows can be ignored as unknown historical events.

## Open Questions

- Should the UI render only the latest plan in a turn or every revision as a timeline? The contract supports both; frontend choice can remain incremental.
- Whether to add optional `agent_instance_id` correlation fields to plan steps can be decided during implementation as a non-required display enhancement, provided semantic status remains root-owned.
- Cross-turn/session-scoped plan continuity is intentionally deferred and should be reassessed after V1 usage.
