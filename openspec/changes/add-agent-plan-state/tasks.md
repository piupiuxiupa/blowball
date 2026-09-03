## 1. Plan State Model and Tool Contract

- [ ] 1.1 Add a turn-scoped plan model in `internal/agent` with ordered `PlanStep` values (`pending` / `in_progress` / `completed`), optional explanation, host-assigned revision, bounded validation, canonical normalization, and a concurrency-safe ledger that starts each Confucius turn at revision `1`; add table-driven tests for valid snapshots, invalid statuses, empty/oversized steps, oversized explanations, unchanged failures, and consecutive revisions
- [ ] 1.2 Define the synthetic `update_plan` JSON Schema and strict argument decoder in `internal/agent`, excluding model-supplied revisions and unknown fields; verify that malformed arguments become error tool results without mutating ledger state
- [ ] 1.3 Render `update_plan` only into Confucius's OpenAI tools payload and keep it absent from every generic sub-agent payload, including depth-eligible nested sub-agents; update tool-list tests accordingly

## 2. Confucius Dispatch Integration

- [ ] 2.1 Add a per-turn plan ledger to Confucius and intercept `update_plan` before ordinary registry dispatch, returning the normalized canonical snapshot as the tool result
- [ ] 2.2 Refactor Confucius tool dispatch into a deterministic pre-phase: reject multiple plan calls atomically, apply at most one valid plan update synchronously, then run all spawn and registry calls through the existing parallel errgroup path
- [ ] 2.3 Emit successful plan updates as a `plan_updated` event between the `update_plan` tool call and its tool result, before any same-round sub-agent or registry activity; add event-order tests for plan-plus-spawn and duplicate-plan batches
- [ ] 2.4 Add behavior tests proving sub-agent `completed` / `capped` / `error` statuses never automatically mutate semantic plan status and that only a subsequent `update_plan` can mark a step completed

## 3. Stream Event and Persistence Contract

- [ ] 3.1 Add `plan_updated` to `internal/stream` with a constructor that places canonical plan JSON in `Content` and host revision in `Meta.revision`, attributed to Confucius without sub-agent instance/run identity
- [ ] 3.2 Add the `plan_updated` message event type and explicit event-mapper behavior so persisted rows keep an empty role and intact canonical JSON content; add mapper and persistence round-trip tests
- [ ] 3.3 Keep `plan_updated` rows out of OpenAI chat-history reconstruction while preserving top-level `update_plan` tool call/result reconstruction; cover both normal and compacted-history behavior with tests
- [ ] 3.4 Update `api/openapi.yaml` with the additive `plan_updated` SSE event shape and canonical snapshot fields; validate the client-visible contract against stream constructors

## 4. Prompt Discipline and Isolation

- [ ] 4.1 Update the example Confucius system prompt to explain when to create/update a structured plan, how to mark parallel steps `in_progress`, and how to distill only the relevant step/context into `spawn_subagent` prompts
- [ ] 4.2 Add prompt/tool-behavior tests asserting the root plan is never automatically injected into a child message and children cannot observe or mutate the root ledger

## 5. Integration and Regression

- [ ] 5.1 Add an integration test covering a full turn that creates a plan, dispatches parallel sub-agents, receives mixed execution results, updates semantic status after verification, persists `plan_updated`, and starts the next turn at revision `1`
- [ ] 5.2 Run `make test` and `make lint`; fix all race, contract, and regression failures
