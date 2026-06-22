## Why

Blowball currently sends every request to OpenAI without any reasoning/thinking configuration. There is no way to take advantage of OpenAI reasoning models (o1 / o3 / o4-mini / GPT-5 reasoning variants) or to control their reasoning depth. Operators have to choose between a non-reasoning model and manually switching endpoints, which makes A/B testing and per-agent tuning impossible.

## What Changes

- Add per-agent `thinking: bool` and `reasoning_effort: low | medium | high` fields to `config.yaml`.
- Extend `AgentConfig`, `LLMRequest`, and `OpenAIClient.StreamChat` to carry and apply these fields.
- When `thinking` is enabled:
  - Send `reasoning_effort` to the OpenAI Chat Completions API.
  - Map `max_tokens` to `max_completion_tokens` because reasoning models reject `max_tokens`.
  - Keep `temperature` omitted (current behavior already does this).
- When `thinking` is disabled, preserve the existing request shape and parameters.
- Validate `reasoning_effort` values at startup and reject invalid combinations.
- Update `config.example.yaml` and add unit/integration tests for the new paths.

## Capabilities

### New Capabilities

- `agent-reasoning-configuration`: Per-agent toggle and effort level for OpenAI reasoning models.

### Modified Capabilities

- `agent-orchestration`: The "Agent configuration from file" requirement is extended to include `thinking` and `reasoning_effort` as valid agent configuration fields.

## Impact

- `internal/config/config.go` — new fields and validation.
- `internal/agent/agent.go` — `LLMRequest` gains reasoning fields.
- `internal/agent/confuse.go`, `chongzhi.go`, `liang.go` — populate `LLMRequest` from config.
- `internal/agent/openai_client.go` — map reasoning fields to openai-go v3 params (`ReasoningEffort`, `MaxCompletionTokens`).
- `config.example.yaml` — add reasoning examples.
- Unit tests for config, fake client, and agent loops; integration tests for the orchestrator.
