# context-compaction 变更规格

## MODIFIED Requirements

### Requirement: Compaction is disabled without a configured max context

The system SHALL treat a missing effective max-context value for the selected model as compaction-disabled: no trigger checks fire, no compaction records are written, and the model context path behaves exactly as before this capability existed. The effective max context for a turn SHALL be **exclusively** the request-selected catalog model's `max_context_tokens` (with the catalog mandatory, every turn resolves to an entry; the legacy top-level `openai.max_context_tokens` field no longer exists). A configured value MUST be a positive integer; any other value SHALL fail configuration load.

#### Scenario: Zero value disables compaction

- **WHEN** the selected model's effective max context is `0` (a catalog entry with `max_context_tokens: 0`)
- **THEN** no context pressure check runs and conversation turns behave identically to the pre-compaction system

#### Scenario: Invalid value fails config load

- **WHEN** a catalog entry's `max_context_tokens` is negative or non-integer
- **THEN** configuration loading returns an error and the server does not start
