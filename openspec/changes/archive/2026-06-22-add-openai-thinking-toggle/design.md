## Context

Blowball's agent layer already supports per-agent `model`, `max_tokens`, `tools`, `mcp`, and `skills` configuration. The OpenAI client (`internal/agent/openai_client.go`) maps these to `openai-go/v3` `ChatCompletionNewParams`. OpenAI reasoning models (o1, o3-mini, o3, o4-mini, and GPT-5 reasoning variants) introduce a `reasoning_effort` parameter and require `max_completion_tokens` instead of `max_tokens`; they reject `temperature`, `top_p`, and other sampling parameters.

Currently the configuration schema and client mapping have no support for reasoning/thinking controls, so operators cannot enable reasoning for an agent or tune its depth.

## Goals / Non-Goals

**Goals:**
- Allow each agent to independently enable/disable OpenAI reasoning mode via `config.yaml`.
- Allow each agent to configure reasoning effort (`low`, `medium`, `high`).
- Translate the configuration correctly into openai-go v3 parameters (`ReasoningEffort`, `MaxCompletionTokens`).
- Preserve existing behavior for non-reasoning agents.
- Validate configuration at startup and fail fast on invalid values.
- Update examples and tests.

**Non-Goals:**
- Support Anthropic/Google reasoning parameters.
- Introduce the Responses API.
- Automatically detect whether a model supports reasoning.
- Expose hidden reasoning-token counts in usage events.
- Add `temperature`/`top_p` configuration for non-reasoning models.
- Change the system prompt message role to `developer` for reasoning models (OpenAI accepts `system` messages for these models).

## Decisions

### 1. Two fields: `thinking` (bool) + `reasoning_effort` (string)
`thinking` gives a clear on/off switch matching the user's request; `reasoning_effort` lets operators tune depth. When `thinking: true` and `reasoning_effort` is omitted, the default is `medium`.

**Alternatives considered:**
- Single `reasoning_effort` field with empty meaning "off": less obvious as a boolean switch.
- Single `thinking` field always using a fixed effort: removes the ability to tune latency/cost.

### 2. `max_tokens` maps to `max_completion_tokens` when thinking is enabled
Reasoning models reject `max_tokens`. To keep the configuration surface simple, we reuse the existing `max_tokens` field and translate it to `MaxCompletionTokens` in the client when `thinking` is true. Non-reasoning agents continue to use `MaxTokens`.

**Alternatives considered:**
- Introducing a separate `max_completion_tokens` field: adds config complexity for a transitional API detail.
- Always using `MaxCompletionTokens`: would change the parameter sent to non-reasoning models unnecessarily.

### 3. Keep `temperature` omitted
The current client only sends `Temperature` when `req.Temperature != 0`, and no agent sets it today. We will not add temperature configuration now, so reasoning models will not receive an unsupported parameter.

### 4. Keep system prompt role as `system`
OpenAI treats `system` messages as `developer` messages for reasoning models. Changing the role to `developer` would require conditional role conversion and risk mixing roles if any other system message appears. The simpler and safer path is to keep the existing `system` role.

### 5. Validate `reasoning_effort` to `{low, medium, high}`
This matches the standard o-series API. openai-go v3 also exposes `none`, `minimal`, and `xhigh` for GPT-5 variants, but we can relax validation later if needed.

### 6. Let the API reject unsupported model/reasoning combinations
We will not hardcode model-name heuristics (e.g., "o1", "o3"). If an operator sets `thinking: true` on a non-reasoning model, the OpenAI API will return a clear error. This avoids maintaining a model-capability allowlist.

## Risks / Trade-offs

| Risk | Mitigation |
|---|---|
| Operator sets `thinking: true` on a non-reasoning model and gets an API error. | Document that `thinking` is only for OpenAI reasoning models; rely on the API's own validation rather than maintaining a model list. |
| Low `max_tokens` can leave no visible output because reasoning tokens consume the `max_completion_tokens` budget. | Document that reasoning tasks need larger values (e.g., 4k–8k+). |
| Restricting `reasoning_effort` to `low/medium/high` may limit future GPT-5 extended values. | Keep validation centralized; extending the allowlist later is a one-line change. |
| `MaxCompletionTokens` is counted differently than `MaxTokens`; existing cost expectations may shift. | Mention in release notes and example config. |
| Tests today assert exact `Usage.TotalTokens`; adding reasoning fields to the fake client requires updating several test fixtures. | Update the fake client and add dedicated reasoning-path tests alongside existing tests. |

## Migration Plan

No migration required. The change is purely additive:
1. Deploy the new build.
2. Optionally add `thinking: true` and `reasoning_effort: medium` to desired agents in `config.yaml`.
3. Existing configs without these fields behave exactly as before.

Rollback: remove the `thinking`/`reasoning_effort` lines from `config.yaml` and restart.

## Open Questions

1. Should `logLLMRequest` include the emitted `reasoning_effort` value for observability? (Recommended: yes, low-cost and useful for debugging.)
2. Should reasoning-token details (`CompletionTokensDetails.ReasoningTokens`) be added to `Usage` and the `done` event? Out of scope for this change, but worth a follow-up if cost observability becomes important.
