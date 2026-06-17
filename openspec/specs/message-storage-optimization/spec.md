## Purpose

Define how streaming assistant events are compressed before persistence to reduce storage overhead while preserving the order and semantic boundaries of the event stream.

## Requirements

### Requirement: Merge consecutive assistant token events before persistence
The system SHALL merge adjacent `EventToken` events that share the same `Agent` into a single persisted message row. Non-token events (`agent_start`, `agent_end`, `agent_error`, `tool_call`) and token events from different agents SHALL NOT be merged.

#### Scenario: Pure token reply is merged
- **WHEN** an agent emits five consecutive `token` events with contents "H", "e", "l", "l", "o"
- **THEN** the system persists exactly one `token` message row with content "Hello"

#### Scenario: Token boundaries are broken by lifecycle events
- **WHEN** an agent emits `agent_start`, three `token` events, `agent_end`
- **THEN** the system persists four rows: `agent_start`, one merged `token`, `agent_end`

#### Scenario: Tokens from different agents are not merged
- **WHEN** the event sequence is `token` from Confuse, `agent_start` Liang, `token` from Liang, `agent_end` Liang, `token` from Confuse
- **THEN** the system persists five rows: Confuse token block, Liang agent_start, Liang token block, Liang agent_end, Confuse token block

#### Scenario: Tool calls remain independent
- **WHEN** an agent emits `token`, `tool_call`, `token`
- **THEN** the system persists three rows: merged token block, tool_call row, merged token block

### Requirement: Preserve total event order after merging
The system SHALL assign monotonic `MsgIndex` values to merged message rows such that the original event order is recoverable when messages are ordered by `(MsgTime, MsgIndex)`.

#### Scenario: Sub-agent hand-off keeps order
- **WHEN** the event sequence is Confuse token, Confuse tool_call invoking Liang, Liang agent_start, Liang token, Liang agent_end, Confuse token
- **THEN** the persisted rows appear in the same order with indices 1, 2, 3, 4, 5, 6

### Requirement: Live SSE stream remains unmerged
The system SHALL continue to emit individual `token` events to the SSE consumer; merging SHALL apply only to the persistence path.

#### Scenario: Client receives real-time tokens
- **WHEN** an assistant turn produces five token deltas
- **THEN** the HTTP SSE response contains five separate `token` events
- **AND** the database contains one merged `token` row for that turn
