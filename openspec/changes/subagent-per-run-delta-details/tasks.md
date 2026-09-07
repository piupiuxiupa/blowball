## 1. Data model and migration

- [ ] 1.1 Add the sequential migration that creates `subagent_instances`, adds per-run identity/lineage/counter/snapshot-kind fields to `subagent_runs`, and enforces `(session_id, run_id)` plus `(session_id, agent_instance_id, run_no)` uniqueness.
- [ ] 1.2 Backfill one instance-metadata row per existing sub-agent snapshot, derive legacy `run_id` from persisted message identity where available, mark old payloads as legacy full snapshots, and initialize run/count metadata.
- [ ] 1.3 Update `internal/model` with sub-agent instance, per-run delta, snapshot-kind, status, lineage, and counter types while retaining JSON/database field mappings.

## 2. MySQL store and chain reconstruction

- [ ] 2.1 Implement MySQL methods for instance lookup, run listing, exact run lookup, transactional delta append/upsert, and compare-and-set advancement of `latest_run_id`.
- [ ] 2.2 Implement run-chain loading that orders runs, validates `run_no`, `previous_run_id`, `base_message_count`, `context_message_count`, and JSON readability, rejects broken/oversized chains, and supports a legacy full-snapshot baseline.
- [ ] 2.3 Add MySQL unit tests for multiple runs per instance, retry upsert, latest-pointer concurrency, legacy compatibility, cross-session isolation, and chain integrity failures.

## 3. Agent dispatch and resume

- [ ] 3.1 Refactor `SubAgentSnapshotStore` and `spawnCoordinator` persistence boundaries around instance metadata plus per-run delta records.
- [ ] 3.2 Capture dispatch start/end identity and the pre-run history length, then save only the suffix added by the terminal `SubAgent.Run` attempt with correct status and context counters.
- [ ] 3.3 Resume from the instance system prompt plus stitched delta chain, preserve stable instance identity and child-result history, and reject missing/cross-session/broken/oversized targets as bad_args.
- [ ] 3.4 Ensure dispatch-level retries reuse one run record and parallel resumes cannot fork the same instance chain.
- [ ] 3.5 Update dynamic sub-agent unit tests for initial delta, repeated resume, retry, partial/error status, nested dispatch, and concurrency behavior.

## 4. Transcript API and OpenAPI

- [ ] 4.1 Add a service-layer transcript projection that decodes one run delta and maps user/assistant/tool messages to explicit task/assistant/tool-result DTO items while omitting system and unknown roles.
- [ ] 4.2 Implement ownership-checked handlers for run metadata listing and per-run detail lazy loading, including consistent 404 behavior for missing sessions, instances, active runs, and mismatched run identities.
- [ ] 4.3 Register the authenticated session-scoped routes and wire the new dependencies through handler construction and role routing.
- [ ] 4.4 Update `api/openapi.yaml` with request parameters, response schemas, examples, authentication, and 404 contracts for both endpoints.
- [ ] 4.5 Add handler/service tests for owned-session access, cross-user hiding, list payload thinness, selected-run-only detail, tool-call pairing, and rejection of raw internal fields.

## 5. Integration and verification

- [ ] 5.1 Add integration coverage for migrating legacy snapshots, executing a new delta run, resuming the same instance, and lazily retrieving each run detail.
- [ ] 5.2 Verify existing `messages` persistence, SSE event ordering, run-stream replay, and main-agent context reconstruction remain unchanged.
- [ ] 5.3 Run focused store/agent/handler tests, migration integration tests, `make test`, and `make lint`; fix all failures and format generated Go/OpenAPI changes.
