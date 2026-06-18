## ADDED Requirements

### Requirement: Orchestrator receives full conversation history at the start of each turn
The orchestrator SHALL accept the complete session conversation history recovered from persistence, combined with the current user message, and pass it to the Confuse agent loop as the initial `messages` slice.

#### Scenario: OrchestratorRunner.Handle signature carries history
- **WHEN** `SessionHandler.SendMessage` invokes `OrchestratorRunner.Handle`
- **THEN** the call includes an `[]agent.Message` argument containing all prior user and assistant messages plus the current user message
- **AND THEN** it no longer accepts only a single `userMessage string`

#### Scenario: Confuse first LLM request includes history
- **WHEN** `Confuse.Run` receives the reconstructed history
- **THEN** its first `LLMRequest.Messages` consists of the system prompt followed by the full history ending with the current user message

#### Scenario: Sub-agent context remains isolated
- **WHEN** Confuse dispatches a sub-agent via `invoke_chongzhi` or `invoke_liang`
- **THEN** the sub-agent's `Run` still receives only its own system prompt plus a single user message assembled from the task and context arguments
- **AND THEN** the sub-agent does not see the user's full conversation history
