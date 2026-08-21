# Delta: interrupted-turn-persistence

## MODIFIED Requirements

### Requirement: Interrupted assistant turn is persisted

The system SHALL persist the user message and any assistant events emitted before an in-progress turn is interrupted, where an interruption is either an explicit cancellation via the cancel endpoint (by run id), or an orchestrator failure returned after the turn started (for example an upstream model-provider error such as a 429 or 5xx, or a transport/timeout failure). The user message SHALL already be persisted at send time — after the session-run claim succeeds and before the orchestrator starts (see the message-write-behind and session-management specifications) — so every interruption path, including a process crash, finds it durable. A client disconnecting from the SSE stream SHALL NOT interrupt the turn (the turn continues to completion; see the turn-run-lifecycle specification). For an interruption caused by an orchestrator failure or an explicit cancellation, the turn's token cost SHALL also be recorded in `turn_usage`. A process crash performs no at-crash persistence of assistant events: events already emitted remain replayable from the run event log, and no assistant message rows are persisted beyond what earlier mid-turn flushes wrote; the user message row survives the crash by virtue of the send-time write.

#### Scenario: Explicit cancel mid-response

- **WHEN** the client cancels the turn via the cancel endpoint while the orchestrator is streaming assistant events
- **THEN** the user message for that turn is persisted (written at send time)
- **AND** any `token`, `reasoning`, `tool_call`, and `tool_result` events emitted before cancellation are persisted

#### Scenario: Client disconnects mid-response without cancel

- **WHEN** the client disconnects the SSE connection while the orchestrator is streaming assistant events
- **THEN** the turn is not interrupted and continues to completion
- **AND** the full turn is persisted through the normal (non-interrupted) path

#### Scenario: Provider error mid-response

- **WHEN** the orchestrator returns a non-cancellation error after assistant events have been streamed (for example a 429 or 5xx from the model provider)
- **THEN** the user message for that turn is persisted (written at send time)
- **AND** any `token`, `reasoning`, `tool_call`, and `tool_result` events emitted before the failure are persisted
- **AND** the turn's token cost is recorded in `turn_usage`

#### Scenario: Provider error before assistant emits content

- **WHEN** the orchestrator returns a non-cancellation error before the assistant emits any event
- **THEN** only the user message is persisted (written at send time)
- **AND** no assistant message is created for that turn

#### Scenario: Explicit cancel before assistant emits content

- **WHEN** the turn is canceled via the cancel endpoint before the assistant emits any event
- **THEN** only the user message is persisted (written at send time)
- **AND** no assistant message is created for that turn

#### Scenario: Process crash mid-response

- **WHEN** the agent process dies while a turn is streaming assistant events
- **THEN** the events already appended to the run event log remain replayable via the resume endpoint
- **AND** the user message row is present in persisted history (written at send time before the turn started)
- **AND** no at-crash persistence runs for assistant events beyond what earlier mid-turn flushes wrote (the run is marked interrupted by a surviving observer)
