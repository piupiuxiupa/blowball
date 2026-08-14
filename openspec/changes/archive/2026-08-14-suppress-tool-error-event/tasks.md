# Tasks

## 1. Remove the tool_error agent_error emission

- [x] 1.1 In `internal/agent/confucius.go` `dispatchRegistryTool`, remove the `streamAgentError(hub, ctx, c.Name(), err.Error(), "tool_error")` call in the `toolRegistry.Call` error branch (around line 482). Keep the `return toolResult{content: renderToolResult(out, err), isError: err != nil}` line unchanged. Update the preceding comment (currently "Frontend channel: keep emitting agent_error for tool failures…") to state that the status envelope is now the sole channel and no `agent_error` is emitted for tool failures.
- [x] 1.2 In `internal/agent/chongzhi.go` `dispatchOneRegistryTool`, remove the `streamAgentError(..., "tool_error")` call (around line 276) and update its preceding comment ("Frontend channel: agent_error still fires…") the same way.
- [x] 1.3 In `internal/agent/liang.go` `dispatchOneRegistryTool`, remove the `streamAgentError(..., "tool_error")` call (around line 269) and update its preceding comment the same way.
- [x] 1.4 Verify `streamAgentError` is still used by the remaining codes (`unknown_tool`, `bad_args`) and the `roundcap.go`/`retry.go` paths — confirm no "declared and not used" / dead-code lint from the removal (it remains referenced, so no symbol removal is needed).

## 2. Update contract documentation

- [x] 2.1 In `CLAUDE.md`, update the tool-result-envelope prose: the line stating "It is a model-facing signal only — the `agent_error` SSE event is still emitted independently on failure for the frontend" must be changed to state the envelope is the sole channel for tool failures and no `agent_error` is emitted for them; note other `agent_error` codes are unaffected.
- [x] 2.2 Check `api/openapi.yaml` for any enumeration/description of `agent_error` codes; if `tool_error` is listed as emitted on tool failure, remove/adjust it and note the BREAKING SSE-contract change. Re-copy `api/openapi.yaml` into `blowball-frontend/` and run `npm run generate-api` there (no FE logic change expected).

## 3. Update tests

- [x] 3.1 Grep tests for assertions that a registry-tool failure emits an `agent_error` event with code `tool_error` (search `internal/agent/*_test.go` and `test/integration/` for `tool_error`, `AgentError`, `EventAgentError`). Update each to assert the envelope-only behavior: the `role="tool"` body is `{"status":1,"error":...}` and no `agent_error` event is produced. *(No test asserted the literal `tool_error` code; one test — `TestChongzhi_FlatTopology_NoInvokeTools` — asserted an `agent_error` for a registry-miss, which is now envelope-only; updated it. `event_mapper_test.go` / `event_test.go` test the generic mapping/constructor and are unchanged.)*
- [x] 3.2 Add/adjust a test asserting the negative: a tool failure produces no `agent_error` event (so a latched whole-agent-segment error cannot occur), while the tool bubble's `status:1` is still present. *(Added `TestChongzhi_ToolFailure_NoAgentError`.)*
- [x] 3.3 Add/adjust a test asserting non-tool failures still emit their `agent_error` codes (`llm_error` / `unknown_tool` / `bad_args` / `round_cap_exhausted`) — guards against over-broad removal. *(Already covered by existing passing tests: sub-agent failure → agent_error in `TestConfucius_SubAgentFailure_StreamsError`, retry in the confucius retry test, `round_cap_exhausted` in `roundcap_test.go`; verified they still pass.)*

## 4. Verify

- [x] 4.1 `make build` — confirm it compiles.
- [x] 4.2 `make test` — confirm unit + integration tests pass with race detection. *(All packages touched by this change pass — `internal/agent`, `internal/handler`, `internal/stream`, `test/integration`. The only failures are two pre-existing, unrelated `internal/prompt` render-test failures on clean master from commit `316d813`, confirmed via stash; out of scope.)*
- [x] 4.3 `make lint` — confirm no static-analysis regressions (e.g. unused `streamAgentError` callers, stale comments). *(`go vet ./...` exits 0.)*
