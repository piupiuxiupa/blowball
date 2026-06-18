# llm-debug-logging Specification

## Purpose

Define structured debug logging for LLM calls so developers can inspect the actual request sent to and response received from the underlying model when the application log level is debug.

## Requirements

### Requirement: LLM request debug logging
When the application log level is debug, the system SHALL emit a structured debug log entry for every call to `OpenAIClient.StreamChat` that describes the request being sent to the underlying model.

#### Scenario: Debug logging of request summary
- **WHEN** `OpenAIClient.StreamChat` is invoked with an `LLMRequest`
- **THEN** a debug log entry is written containing the model, message count, each message's role and a truncated content preview, tools count and tool-name preview, max_tokens, and the trace_id from the context.

### Requirement: LLM response debug logging
When the application log level is debug, the system SHALL emit a structured debug log entry after the streaming response is fully aggregated.

#### Scenario: Debug logging of response summary
- **WHEN** `OpenAIClient.StreamChat` has finished draining the SSE stream
- **THEN** a debug log entry is written containing finish_reason, content length and truncated preview, tool_call names and truncated argument previews, usage totals, and the trace_id from the context.

### Requirement: Trace correlation
The debug log entries SHALL include the trace_id carried by the request context so that LLM request/response logs can be correlated with the originating HTTP request.

#### Scenario: Request with trace_id
- **WHEN** the supplied `context.Context` contains a trace_id
- **THEN** both the request and response debug log entries include that trace_id.

#### Scenario: Request without trace_id
- **WHEN** the supplied `context.Context` does not contain a trace_id
- **THEN** the debug log entries omit the trace_id field gracefully.

### Requirement: Content truncation
The debug log entries SHALL truncate long message content and tool-call arguments to a bounded length to prevent excessive log volume.

#### Scenario: Long message content
- **WHEN** a message content exceeds the configured preview limit
- **THEN** the log entry records only the leading portion of the content followed by an ellipsis indicator.

### Requirement: No API key logging
The debug log entries SHALL NOT include the OpenAI API key or other authentication credentials.

#### Scenario: Request log excludes credentials
- **WHEN** the request debug log is emitted
- **THEN** it does not contain the `api_key`, `Authorization` header, or any equivalent credential.

### Requirement: Debug level gating
The LLM request/response debug logs SHALL only be emitted when the configured log level is debug or lower.

#### Scenario: Info level does not emit LLM logs
- **WHEN** the configured log level is info or higher
- **THEN** no LLM request/response debug log entries are produced.

#### Scenario: Debug level emits LLM logs
- **WHEN** the configured log level is debug
- **THEN** LLM request/response debug log entries are produced for every `StreamChat` call.
