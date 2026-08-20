# context-compaction 变更规格

## MODIFIED Requirements

### Requirement: Compaction is disabled without a configured max context
The system SHALL treat a missing effective max-context value for the selected model as compaction-disabled: no trigger checks fire, no compaction records are written, and the model context path behaves exactly as before this capability existed. The effective max context for a turn SHALL be the request-selected catalog model's `max_context_tokens` (with an explicit `openai.models` catalog) or the legacy `openai.max_context_tokens` (implicit catalog). A configured value MUST be a positive integer; any other value SHALL fail configuration load.

#### Scenario: Zero value disables compaction
- **WHEN** the selected model's effective max context is unset or `0` (implicit catalog with `openai.max_context_tokens: 0`)
- **THEN** no context pressure check runs and conversation turns behave identically to the pre-compaction system

#### Scenario: Invalid value fails config load
- **WHEN** a catalog entry's `max_context_tokens` or the legacy `openai.max_context_tokens` is negative or non-integer
- **THEN** configuration loading returns an error and the server does not start

### Requirement: Compaction triggers at 80 percent of configured max context with a non-empty middle
The system SHALL trigger compaction when measured context reaches `0.8 × effective max context` — the request-selected model's `max_context_tokens` for that turn — AND the compressible middle (everything after the first message and before the retained tail) is non-empty. The 80 percent threshold is fixed and not configurable.

#### Scenario: Threshold reached with compressible middle
- **WHEN** measured context is at or above `0.8 × the selected model's max_context_tokens` and messages exist between the first message and the retained tail
- **THEN** compaction runs

#### Scenario: Threshold reached but middle is empty
- **WHEN** measured context reaches the threshold but the conversation consists only of the first message and the retained tail
- **THEN** compaction is skipped and the conversation continues unchanged

#### Scenario: Model switch changes the threshold
- **WHEN** the previous turn used a 400k-context model and ended at 300k context tokens, and the new request selects a 200k-context model
- **THEN** the turn-start check compares the stored 300k against `0.8 × 200k` and compaction runs before the first LLM call

### Requirement: Summary generation uses the current agent model with a structured checkpoint template
The system SHALL generate the summary using the turn's request-selected model (the same model the three agents run for that turn) via the existing LLM client. The summarization instruction SHALL require a structured checkpoint (sections for primary intent, technical concepts, files and code, errors and fixes, pending jobs, current work, next step, critical context) preserving exact paths, commands, error strings, identifiers, and numeric values. Only returned text SHALL be retained; reasoning content and tool calls SHALL be discarded.

#### Scenario: Structured summary replaces the middle
- **WHEN** compaction completes
- **THEN** the shadowed middle is replaced in subsequent model contexts by a single user-role message framing the summary text

#### Scenario: Only text output is retained
- **WHEN** the summarization response contains reasoning content or tool calls alongside text
- **THEN** only the text is stored in the compaction record

#### Scenario: Summary call is observable
- **WHEN** the summarization LLM call executes through the OpenAI client
- **THEN** it is captured in `llm_raw_log` like any other call

### Requirement: Turn usage records end-of-turn context size
The system SHALL record, for every completed turn, the last LLM round's `prompt_tokens + completion_tokens` in `turn_usage.context_tokens` as a raw, model-independent size, enabling turn-start preventive compaction checks without estimation. The turn-start check SHALL compare this stored raw size against the CURRENT request's selected-model threshold.

#### Scenario: Context size persisted per turn
- **WHEN** a turn with at least one LLM call completes
- **THEN** its `turn_usage` row carries the final round's context size in `context_tokens`

#### Scenario: Turn start compacts before sending when context is already over threshold
- **WHEN** a new message is sent and the latest `turn_usage.context_tokens` is at or above `0.8 × the selected model's max_context_tokens` with a non-empty middle
- **THEN** compaction runs before the first LLM call of the turn
