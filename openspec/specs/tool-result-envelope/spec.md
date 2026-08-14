# tool-result-envelope Specification

## Purpose

Every built-in tool result fed to the model as a `role="tool"` message is wrapped in a uniform `{status, result|error}` envelope, rendered by one shared function across all three agents.

## Requirements

### Requirement: Uniform tool-result status envelope
Every built-in tool result that is fed back to the model as a `role="tool"` message SHALL be rendered as a JSON object of the form `{"status": <code>, ... }` where `<code>` is `0` for success and `1` for failure. On success the object SHALL carry the tool's logical return value under a `result` field: `{"status":0,"result":<value>}`. On failure the object SHALL carry the error message under an `error` field: `{"status":1,"error":"<message>"}`. The envelope SHALL apply to every tool dispatched through the registry (`xizhi_*`, `webfetch`, `bash`, `luban_*`, and MCP proxy tools), regardless of whether the tool's logical return value is a JSON object, array, or bare string.

#### Scenario: Successful object-returning tool
- **WHEN** a tool that returns a JSON object (e.g. `xizhi_list_files` returning `{path, entries}`) succeeds
- **THEN** the model receives `{"status":0,"result":{"path":...,"entries":[...]}}`

#### Scenario: Successful array-returning tool
- **WHEN** `luban_list_skills` succeeds and its logical return value is a bare array
- **THEN** the model receives `{"status":0,"result":[{...},{...}]}` with the array nested under `result`

#### Scenario: Successful bare-string-returning tool
- **WHEN** `luban_read_skill` succeeds and its logical return value is a bare string
- **THEN** the model receives `{"status":0,"result":"<the skill text>"}` with the string nested under `result`

#### Scenario: Failed tool
- **WHEN** a tool returns a non-nil error (e.g. file not found, invalid args, regex compile failure)
- **THEN** the model receives `{"status":1,"error":"<err.Error()>"}` and no `result` field

### Requirement: Central envelope rendering across all agents
The envelope SHALL be produced by a single shared rendering function invoked from every agent's tool-dispatch path (Confucius, Chongzhi, and Liang), replacing the prior per-call result marshaler. The renderer SHALL accept both the tool's return value and its error so that success and failure are handled symmetrically in one place.

#### Scenario: All three agents render the envelope
- **WHEN** any of Confucius, Chongzhi, or Liang dispatches a registry tool call
- **THEN** the resulting `role="tool"` message body is produced by the shared envelope renderer, not by inline per-agent marshaling

### Requirement: Envelope preserves byte-slice and string returns as text
When a tool's logical return value is a Go `[]byte` or `string`, the `result` field SHALL contain that value as a JSON string (the text itself), not a base64 encoding. This preserves the prior behavior where byte-slice returns were rendered as raw text.

#### Scenario: Byte-slice return is not base64-encoded
- **WHEN** a tool returns a `[]byte` (e.g. a skill body read as bytes)
- **THEN** the envelope's `result` is a JSON string containing the decoded text, not a base64 blob

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

### Requirement: Non-object tool descriptions declare the envelope shape
For tools whose logical return value is not a JSON object, the tool's model-facing description SHALL declare that the result is delivered inside the `{"status","result"}` envelope (rather than as a bare array or bare string), so the advertised shape matches the actual rendered output.

#### Scenario: luban_list_skills description reflects envelope
- **WHEN** `luban_list_skills` is registered and rendered to the model
- **THEN** its description declares the skill list is returned inside the status envelope, not as a bare top-level array

#### Scenario: luban_read_skill description reflects envelope
- **WHEN** `luban_read_skill` is registered and rendered to the model
- **THEN** its description declares the skill text is returned inside the status envelope, not as a bare string
