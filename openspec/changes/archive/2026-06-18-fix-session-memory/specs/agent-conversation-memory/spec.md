## ADDED Requirements

### Requirement: Persisted event stream can be reconstructed into LLM prompt messages
The system SHALL provide a function that converts the ordered `[]model.Message` rows returned by `MessageService.RecoverMessages` into an `[]agent.Message` slice suitable for the OpenAI chat completion format.

#### Scenario: User messages are preserved
- **WHEN** `RecoverMessages` returns rows with `event_type = message` and `role = user`
- **THEN** each such row maps to `agent.Message{Role: "user", Content: <content>}` in the same order

#### Scenario: Adjacent assistant tokens are merged into one assistant message
- **WHEN** consecutive rows have `event_type = token`, `role = assistant`, and the same `agent` value
- **THEN** their `content` values are concatenated into a single `agent.Message{Role: "assistant", Content: <combined>}`
- **AND THEN** a change in `agent` value starts a new assistant message

#### Scenario: Marker events are excluded from the prompt
- **WHEN** rows have `event_type` of `agent_start`, `agent_end`, or `agent_error`, or an empty `role`
- **THEN** those rows SHALL NOT appear in the reconstructed `[]agent.Message`

### Requirement: Tool calls and tool results are paired in reconstructed history
The system SHALL map a `tool_call` row and its matching `tool_result` row(s) into an assistant message with `ToolCalls` followed by one or more `role = tool` messages, using `tool_call_id` for correlation.

#### Scenario: Tool call row carries tool_call_id
- **WHEN** the system persists a `tool_call` event
- **THEN** the persisted JSON content includes `"tool_call_id"` alongside `"name"` and `"args"`

#### Scenario: Tool result row carries tool_call_id and output
- **WHEN** the system persists a `tool_result` event
- **THEN** the persisted JSON content includes `"tool_call_id"` and `"output"`

#### Scenario: Reconstruction pairs call and result
- **WHEN** a `tool_call` row is followed by `tool_result` rows with the same `tool_call_id`
- **THEN** the reconstructed slice contains an assistant message with the corresponding `ToolCall` followed by `role = tool` messages for each result, in order

#### Scenario: Unpaired tool calls are omitted
- **WHEN** a `tool_call` row has no matching `tool_result` row in the recovered history
- **THEN** the tool call SHALL be omitted from the reconstructed prompt to avoid presenting an incomplete tool-calling turn to the model

### Requirement: Reconstructed messages preserve chronological order
The system SHALL emit the reconstructed `[]agent.Message` in the same order as the original event stream, after merging and filtering.

#### Scenario: Mixed user, assistant, and tool turns
- **WHEN** the recovered history contains user messages, assistant token groups, and tool call/result pairs
- **THEN** the output slice contains them in chronological order: user, assistant, tool call, tool result, next user, etc.
