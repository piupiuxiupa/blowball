## ADDED Requirements

### Requirement: Confucius has a structured plan update tool
Confucius SHALL have a synthetic `update_plan` function tool for submitting a whole semantic plan snapshot. The tool SHALL NOT be registered in the ordinary tool registry and SHALL NOT appear in any dynamic sub-agent tool list. A valid snapshot SHALL contain one or more ordered steps, each with nonempty text and status `pending`, `in_progress`, or `completed`, plus an optional explanation.

#### Scenario: Confucius submits a valid plan
- **WHEN** Confucius calls `update_plan` with ordered nonempty steps and valid statuses
- **THEN** the orchestration layer accepts and normalizes the snapshot
- **AND THEN** the normalized snapshot becomes the authoritative semantic plan for the current top-level turn

#### Scenario: Invalid snapshots are rejected without mutation
- **WHEN** Confucius calls `update_plan` with an empty step, an unknown status, no steps, or a snapshot exceeding the configured bounds
- **THEN** the call returns a structured argument error to Confucius
- **AND THEN** the previous plan snapshot and revision remain unchanged and no `plan_updated` event is emitted

#### Scenario: Sub-agents cannot update the root plan
- **WHEN** a dynamic sub-agent tool list is built, including a depth-eligible sub-agent that can call `spawn_subagent`
- **THEN** `update_plan` is absent from that tool list

### Requirement: Plan revisions are host-authoritative
The host SHALL assign a monotonic revision to every accepted plan snapshot, starting at `1` for each top-level Confucius turn. The model SHALL NOT supply or overwrite the revision. The `update_plan` tool result SHALL return the host-normalized canonical snapshot, including the assigned revision, normalized steps, and explanation when present.

#### Scenario: Host assigns consecutive revisions
- **WHEN** Confucius submits two valid plan snapshots in separate rounds of the same turn
- **THEN** the host assigns revisions `1` and `2` respectively
- **AND THEN** each tool result contains the corresponding normalized canonical snapshot

#### Scenario: Model cannot forge a revision
- **WHEN** a plan argument contains a model-supplied revision field
- **THEN** argument decoding rejects the unknown field or otherwise ignores it in a way that prevents the model from selecting the authoritative revision
- **AND THEN** only the host-assigned revision appears in the tool result and `plan_updated` event

### Requirement: Parallel steps may be in progress simultaneously
A semantic plan snapshot MAY contain multiple steps with status `in_progress`. The system SHALL NOT force plan updates to serialize work that Confucius dispatches as parallel `spawn_subagent` calls.

#### Scenario: Parallel work is represented in one snapshot
- **WHEN** Confucius marks three independent steps `in_progress` and dispatches three parallel sub-agents
- **THEN** the accepted plan snapshot retains all three steps as `in_progress`
- **AND THEN** no runtime rule automatically changes one of those steps to `pending` or `completed`

### Requirement: Plan updates precede same-round parallel execution
When one assistant response contains `update_plan` together with `spawn_subagent` or ordinary tool calls, the orchestration layer SHALL validate and apply at most one successful plan update before starting the other tool calls. If the response contains more than one `update_plan` call, the plan batch SHALL be rejected before mutation and the other tool calls SHALL still execute through the normal path.

#### Scenario: Plan event precedes parallel spawn activity
- **WHEN** one assistant response contains a valid `update_plan` call and two `spawn_subagent` calls
- **THEN** the host emits the `update_plan` tool call, `plan_updated`, and tool result sequence before either sub-agent starts
- **AND THEN** the two sub-agent calls subsequently execute through the existing parallel dispatch path

#### Scenario: Duplicate plan updates are rejected atomically
- **WHEN** one assistant response contains two `update_plan` calls
- **THEN** both plan calls return argument errors and no plan revision is created
- **AND THEN** any non-plan tool calls in the same response still execute

### Requirement: Plan updates emit a persisted structured event
Every accepted plan update SHALL emit a `plan_updated` SSE event attributed to Confucius. The event `Content` SHALL contain the complete canonical plan JSON, and `Meta.revision` SHALL contain the host-assigned revision. The event SHALL be persisted through the existing message path with event type `plan_updated`, empty role, and its canonical JSON content intact. Confucius `plan_updated` events SHALL NOT carry sub-agent instance or run identity.

#### Scenario: Client receives canonical plan state
- **WHEN** Confucius submits a valid plan update
- **THEN** the SSE stream contains a `plan_updated` event whose `Content` decodes to the canonical snapshot
- **AND THEN** `Meta.revision` equals the revision inside that snapshot

#### Scenario: Plan state survives message persistence
- **WHEN** the turn's event stream is persisted and later read as session history
- **THEN** the `plan_updated` row retains event type `plan_updated`, an empty role, and the canonical JSON content
- **AND THEN** a history consumer can select the last plan row for the turn to render the final plan

### Requirement: Semantic plan state remains separate from execution state
The system SHALL NOT automatically map a sub-agent execution status to semantic plan status. A sub-agent result of `completed`, `capped`, or `error` SHALL only be evidence used by Confucius; a plan step SHALL become `completed` only when Confucius subsequently submits a valid plan snapshot marking it completed.

#### Scenario: Completed execution is not automatic semantic completion
- **WHEN** a sub-agent returns `status: completed` but Confucius determines that its result does not satisfy the step's acceptance criteria
- **THEN** the plan step remains non-completed until a later valid `update_plan` changes it

#### Scenario: Capped execution remains semantically active
- **WHEN** a sub-agent returns `status: capped`
- **THEN** the plan does not automatically mark the corresponding semantic step completed or failed
- **AND THEN** Confucius may keep it `in_progress`, resume the instance, revise the plan, or dispatch different work

### Requirement: Sub-agent plan context is explicit and isolated
The root plan SHALL NOT be automatically injected into a dynamic sub-agent context. Confucius SHALL distill any required plan state into the self-contained `spawn_subagent` task and context arguments. Dynamic sub-agents SHALL NOT observe or mutate the root plan except through their final result returned to Confucius.

#### Scenario: Child receives only distilled state
- **WHEN** Confucius dispatches a sub-agent for one plan step
- **THEN** the child's initial user message contains only the task/context arguments supplied by Confucius
- **AND THEN** it does not automatically receive the complete root plan or sibling plan state

#### Scenario: Child result flows through the parent
- **WHEN** a sub-agent finishes work relevant to a plan step
- **THEN** its result is returned to Confucius as spawn tool output
- **AND THEN** only Confucius may submit the next root plan snapshot

### Requirement: Plan state is turn-scoped
The authoritative semantic plan ledger SHALL start empty for each top-level Confucius turn and SHALL NOT mutate after that turn completes. Persisted `plan_updated` rows SHALL remain historical display data. A subsequent user turn SHALL start a fresh plan ledger and revision sequence.

#### Scenario: Next turn starts a new plan
- **WHEN** a turn containing revision `4` completes and the user sends another message
- **THEN** the new turn's first accepted plan snapshot receives revision `1`
- **AND THEN** the prior turn's `plan_updated` rows remain unchanged as history

#### Scenario: Turns without plan calls are unchanged
- **WHEN** Confucius does not call `update_plan`
- **THEN** the turn emits no `plan_updated` event and existing orchestration behavior remains unchanged
