## Why

A registry-tool failure is signalled to the model **twice**: once in-band via the `{"status":1,"error":...}` envelope (model-facing, in the `role="tool"` message), and once out-of-band via an `agent_error` SSE event with `code=tool_error` (frontend-facing). The `agent_error` channel is redundant — the frontend already renders tool failures from the `status` field in the `tool_result` event (the tool bubble turns red via `isStatusError(status)`). Worse, the `agent_error` for tool failures is actively misleading: it latches the **whole agent segment** as `isError` (sticky through the subsequent `agent_end`), and on history reload it appends `[错误] <msg>` to the agent's reply text and marks the entire block red — even though the agent typically self-corrected and produced a normal answer. A routine, recoverable tool hiccup should not read as "the agent failed".

## What Changes

- **Stop emitting `agent_error` (code `tool_error`) when a registry tool call (`toolRegistry.Call`) returns an error.** This removes the redundant/misleading out-of-band channel for tool failures across all three agents' registry-tool dispatch sites (Confucius, Chongzhi, Liang).
- The model-facing `{"status":1,"error":...}` envelope produced by `renderToolResult` is **unchanged** — it is still emitted in the `role="tool"` message, so the model still sees the failure and can retry / explain it. The frontend's tool bubble still turns red via the `status` field.
- **Other `agent_error` codes are untouched**: `unknown_tool`, `bad_args`, `llm_error`, `retry`, `round_cap_exhausted` continue to be emitted exactly as today. These represent genuine agent-level failures or control signals, not routine tool errors.
- **BREAKING** (SSE event contract): tool failures no longer produce an `agent_error` event. A frontend that branches on `agent_error` to detect tool errors would no longer be notified — but the blowball frontend already derives tool-error state from the `tool_result` `status` field, so the only behavioral change is the removal of the misleading whole-segment error marking and the `[错误]` text-append on reload.

## Capabilities

### New Capabilities

(none)

### Modified Capabilities

- `tool-result-envelope`: the requirement "Envelope coexists with the agent_error stream event" (which currently mandates that both the status envelope AND an `agent_error` SSE event are emitted on every tool failure) is reversed — the status envelope becomes the single, authoritative channel for tool failures; no `agent_error` SSE event is emitted for a registry-tool failure.

## Impact

- **Code**: three registry-tool dispatch sites — `internal/agent/confucius.go` (`dispatchRegistryTool`), `internal/agent/chongzhi.go` (`dispatchOneRegistryTool`), `internal/agent/liang.go` (`dispatchOneRegistryTool`). Each removes the single `streamAgentError(..., "tool_error")` call in the `registry.Call` error branch; the `renderToolResult(out, err)` envelope return is unchanged.
- **SSE contract**: the `agent_error` event with `Meta.code=tool_error` is removed from the stream. Other `agent_error` codes are unaffected. `api/openapi.yaml` / CLAUDE.md prose describing "tool failures emit `agent_error`" must be updated.
- **Persistence**: fewer `EventTypeAgentError` rows are persisted for tool-failure turns; tool failures remain visible in the persisted `tool_result` row (`status:1`). No schema change.
- **Frontend** (`blowball-frontend`, separate repo): no code change required — it already renders tool errors from `status`. The side effect is that the streaming agent segment is no longer latched red by a tool failure, and history reload no longer appends `[错误] <msg>` to the agent's reply. Frontend OpenAPI should be re-synced after the contract doc updates.
- **Tests**: any unit/integration test asserting that a tool failure emits an `agent_error` event (code `tool_error`) must be updated to assert the envelope-only behavior.
