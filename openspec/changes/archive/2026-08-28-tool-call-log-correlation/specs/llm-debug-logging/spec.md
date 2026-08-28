# Delta: llm-debug-logging

## MODIFIED Requirements

### Requirement: Trace correlation
The debug log entries SHALL include the trace_id carried by the request context so that LLM request/response logs can be correlated with the originating HTTP request. The debug log entries SHALL additionally include the session_id carried by the request context when present, omitted gracefully when absent (same convention as trace_id).

#### Scenario: Request with trace_id
- **WHEN** the supplied `context.Context` contains a trace_id
- **THEN** both the request and response debug log entries include that trace_id.

#### Scenario: Request without trace_id
- **WHEN** the supplied `context.Context` does not contain a trace_id
- **THEN** the debug log entries omit the trace_id field gracefully.

#### Scenario: Request with session_id
- **WHEN** the supplied `context.Context` contains a session_id (agent-role turn path, injected at the streaming handler entry)
- **THEN** both the request and response debug log entries include that session_id alongside the trace_id.

#### Scenario: Request without session_id
- **WHEN** the supplied `context.Context` does not contain a session_id (e.g. paths without a turn-scoped session)
- **THEN** the debug log entries omit the session_id field gracefully.
