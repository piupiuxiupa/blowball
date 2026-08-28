# Delta: executor-tools

## MODIFIED Requirements

### Requirement: Audit logging
The system SHALL emit a structured audit log entry for every command execution. The entry SHALL carry the `trace_id` and `session_id` from the invoking context when present (omitted gracefully when absent), alongside the existing fields.

#### Scenario: Log bash execution
- **WHEN** the agent calls `bash`
- **THEN** the system logs the command string, tool name, user ID, exit code, output byte size, and duration

#### Scenario: Audit entry carries correlation ids
- **WHEN** the agent calls `bash` within a turn whose context carries `session_id` and `trace_id`
- **THEN** the audit log entry includes both ids, so the execution can be correlated with the turn's other logs
