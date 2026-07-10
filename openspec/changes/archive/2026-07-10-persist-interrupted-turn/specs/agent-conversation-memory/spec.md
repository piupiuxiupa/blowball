## ADDED Requirements

### Requirement: Recovered history includes interrupted turns

The system SHALL include user messages and assistant events from interrupted turns when reconstructing conversation history from persistence.

#### Scenario: Interrupted turn appears in recovered history

- **WHEN** `RecoverMessages` returns rows from a turn that was interrupted by client cancellation
- **THEN** `MessagesToAgentMessages` includes the user message and the partial assistant content in the reconstructed `[]agent.Message`

#### Scenario: Interrupted turn with completed tool call pair

- **WHEN** an interrupted turn contains a `tool_call` row followed by a matching `tool_result` row
- **THEN** the reconstructed history includes the assistant tool-calling message followed by the `role=tool` messages

#### Scenario: Interrupted turn with unpaired tool call

- **WHEN** an interrupted turn contains a `tool_call` row with no matching `tool_result` row
- **THEN** the unpaired tool call is omitted from the reconstructed prompt
