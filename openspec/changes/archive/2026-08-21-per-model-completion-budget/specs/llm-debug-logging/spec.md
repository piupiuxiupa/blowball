# Delta: llm-debug-logging

## MODIFIED Requirements

### Requirement: LLM request debug logging
When the application log level is debug, the system SHALL emit a structured debug log entry for every call to `OpenAIClient.StreamChat` that describes the request being sent to the underlying model.

#### Scenario: Debug logging of request summary
- **WHEN** `OpenAIClient.StreamChat` is invoked with an `LLMRequest`
- **THEN** a debug log entry is written containing the model, message count, each message's role and a truncated content preview, tools count and tool-name preview, the output quota field (`max_completion_tokens`, sourced from the resolved catalog entry for agent calls; the fixed internal budget for compaction summaries; absent for uncapped calls such as title generation), and the trace_id from the context.
