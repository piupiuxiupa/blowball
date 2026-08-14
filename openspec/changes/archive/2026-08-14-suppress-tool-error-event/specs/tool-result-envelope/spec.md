## REMOVED Requirements

### Requirement: Envelope coexists with the agent_error stream event
**Reason**: Emitting an `agent_error` SSE event for every registry-tool failure is redundant — the frontend already renders tool errors from the `tool_result` `status` field (`status:1` turns the tool bubble red) — and actively misleading. The `agent_error` for a tool failure latches the entire agent segment as `isError` (sticky through the subsequent `agent_end`), and on history reload it appends `[错误] <msg>` to the agent's reply text and marks the whole block red, even though the agent typically self-corrected and answered normally. A routine, recoverable tool hiccup must not read as "the agent failed".
**Migration**: Tool failures are no longer signalled via the `agent_error` SSE event. Consumers MUST derive tool-error state from the `tool_result` event's `status` field (`status:1`, with the message under `error`). The bundled `blowball-frontend` already does this; no client code change is required there.

## ADDED Requirements

### Requirement: Status envelope is the sole channel for registry-tool failures
The in-body `status` field SHALL be the single, authoritative signal for a registry-tool failure. When a tool dispatched through the registry returns a non-nil error, the system SHALL render the `role="tool"` message body as `{"status":1,"error":"<message>"}` (the model-facing channel) and SHALL NOT emit an `agent_error` SSE event for that failure. The model receives the failure in-band and may retry the tool or explain it to the user; no additional out-of-band event is produced. The rendering itself (`renderToolResult`) and its placement in the `role="tool"` message are unchanged — only the redundant `agent_error` emission for the `tool_error` code is removed, across all three agents' registry-tool dispatch sites (Confucius, Chongzhi, Liang).

#### Scenario: Tool failure emits the envelope only
- **WHEN** a registry tool call fails (e.g. `xizhi_read_file` on a missing file, `xizhi_modify_file` old-content mismatch, `xizhi_grep` regex compile failure, or a tool execution timeout)
- **THEN** the `role="tool"` message body fed to the model is `{"status":1,"error":"<err.Error()>"}` with no `result` field
- **AND THEN** no `agent_error` SSE event is emitted for that failure

#### Scenario: Tool failure still surfaces in the persisted stream as a tool_result
- **WHEN** a registry tool call fails during a turn
- **THEN** a `tool_result` event carrying the `{"status":1,"error":...}` body is persisted (so history reload renders the red tool bubble from `status`)
- **AND THEN** no `EventTypeAgentError` row is persisted for that tool failure

#### Scenario: Non-tool agent errors are unaffected
- **WHEN** an agent fails for a non-tool reason — an LLM call error (`llm_error`), a sub-agent dispatch with an unknown tool / nil registry (`unknown_tool`), malformed sub-agent invoke arguments (`bad_args`), an in-progress sub-agent retry signal (`retry` with `Meta.retry=true`), or a round-cap wrap-up that produced no content (`round_cap_exhausted`)
- **THEN** the corresponding `agent_error` SSE event is still emitted with its existing code, unchanged by this requirement
