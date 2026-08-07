## MODIFIED Requirements

### Requirement: Interrupted assistant turn is persisted

The system SHALL persist the user message and any assistant events emitted before an in-progress turn is interrupted, where an interruption is either a client-initiated cancellation of the HTTP request or an orchestrator failure returned after the turn started (for example an upstream model-provider error such as a 429 or 5xx, or a transport/timeout failure). For an interruption caused by an orchestrator failure, the turn's token cost SHALL also be recorded in `turn_usage`.

#### Scenario: Client disconnects mid-response

- **WHEN** the client cancels the HTTP request while the orchestrator is streaming assistant events
- **THEN** the user message for that turn is persisted
- **AND** any `token`, `reasoning`, `tool_call`, and `tool_result` events emitted before cancellation are persisted

#### Scenario: Provider error mid-response

- **WHEN** the orchestrator returns a non-cancellation error after assistant events have been streamed (for example a 429 or 5xx from the model provider)
- **THEN** the user message for that turn is persisted
- **AND** any `token`, `reasoning`, `tool_call`, and `tool_result` events emitted before the failure are persisted
- **AND** the turn's token cost is recorded in `turn_usage`

#### Scenario: Provider error before assistant emits content

- **WHEN** the orchestrator returns a non-cancellation error before the assistant emits any event
- **THEN** only the user message is persisted
- **AND** no assistant message is created for that turn

#### Scenario: Client disconnect before assistant emits content

- **WHEN** the client cancels the request before the assistant emits any event
- **THEN** only the user message is persisted
- **AND** no assistant message is created for that turn

### Requirement: Title generation runs on interrupted first turn

The system SHALL run first-turn title generation using assistant tokens collected before an interruption, where an interruption is either a client-initiated cancellation of the HTTP request or an orchestrator failure returned after the turn started.

#### Scenario: First turn is interrupted by client disconnect

- **WHEN** a new session's first turn is canceled by the client
- **THEN** title generation is triggered with the partial assistant content emitted before cancellation

#### Scenario: First turn fails with a provider error

- **WHEN** a new session's first turn ends with a non-cancellation orchestrator error
- **THEN** title generation is triggered with the partial assistant content emitted before the failure
