## 1. Unique fresh sub-agent dispatch

- [x] 1.1 Remove `resume_agent_id` from the spawn tool schema, description, typed arguments, default prompt guidance, and config example.
- [x] 1.2 Refactor `spawnCoordinator.dispatch` to always allocate a fresh instance/name and remove model-driven snapshot loading from the dispatch path.
- [x] 1.3 Change model-visible spawn results to expose status only, without `agent_id`.
- [x] 1.4 Update agent/orchestration tests for rejected resume arguments, unique names/instances, status-only results, and fresh follow-up dispatch behavior.

## 2. Placeholder message history mode

- [x] 2.1 Add validated `subagent_content=full|placeholder` request handling with full as the default and 400 for invalid values.
- [x] 2.2 Extend the session service and MySQL store contracts with a placeholder pagination view that filters before pagination, retains lifecycle markers, omits child payload rows, and omits duplicated parent spawn results.
- [x] 2.3 Mirror the placeholder behavior in the integration MySQL fake and ensure session deletion/cursor semantics remain intact.
- [x] 2.4 Update OpenAPI for the query parameter, mode semantics, and 400 response.
- [x] 2.5 Add handler/store tests for default full mode, placeholder filtering, lifecycle identity retention, pagination, parent-result omission, invalid values, and legacy visibility.

## 3. Integration and verification

- [x] 3.1 Add end-to-end coverage for two same-name spawns producing unique instances/names and placeholder history backed by per-run detail lazy loading.
- [x] 3.2 Verify SSE, full history mode, per-run transcript APIs, and existing message persistence remain unchanged.
- [x] 3.3 Run focused tests, `make test`, `make lint`, and OpenSpec strict validation; fix and format all changes.
