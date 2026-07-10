## ADDED Requirements

### Requirement: Interrupted assistant turn is persisted

The system SHALL persist the user message and any assistant events emitted before a client-initiated cancellation of an in-progress turn.

#### Scenario: Client disconnects mid-response

- **WHEN** the client cancels the HTTP request while the orchestrator is streaming assistant events
- **THEN** the user message for that turn is persisted
- **AND** any `token`, `reasoning`, `tool_call`, and `tool_result` events emitted before cancellation are persisted

#### Scenario: Interruption before assistant emits content

- **WHEN** the client cancels the request before the assistant emits any event
- **THEN** only the user message is persisted
- **AND** no assistant message is created for that turn

### Requirement: Title generation runs on interrupted first turn

The system SHALL run first-turn title generation using assistant tokens collected before cancellation.

#### Scenario: First turn is interrupted

- **WHEN** a new session's first turn is canceled by the client
- **THEN** title generation is triggered with the partial assistant content emitted before cancellation
