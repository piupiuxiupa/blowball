# agent-reasoning-configuration Specification

## Purpose

定义 Blowball 中每个 Agent 的 OpenAI reasoning 模式开关与 reasoning effort 配置能力，使 reasoning 模型（o1/o3/o4-mini/GPT-5 reasoning 变体）可按 Agent 启用并调节思考深度。

## Requirements

### Requirement: Per-agent thinking toggle

每个 Agent 的配置 SHALL 支持 `thinking` 布尔字段。当 `thinking` 为 `true` 时，系统启用 OpenAI reasoning 模式；当 `thinking` 为 `false` 或省略时，系统保持原有非 reasoning 请求行为。

#### Scenario: Thinking enabled

- **WHEN** Agent 配置包含 `thinking: true`
- **THEN** 系统向 OpenAI 发送 `reasoning_effort` 参数

#### Scenario: Thinking disabled

- **WHEN** Agent 配置未设置 `thinking` 或设置为 `false`
- **THEN** 系统不向 OpenAI 发送 `reasoning_effort` 参数

### Requirement: Configurable reasoning effort level

当 `thinking` 为 `true` 时，Agent 配置 SHALL 支持 `reasoning_effort` 字段，取值为 `low`、`medium` 或 `high`。未设置时默认值为 `medium`。

#### Scenario: Explicit effort level

- **WHEN** Agent 配置包含 `thinking: true` 和 `reasoning_effort: high`
- **THEN** 系统向 OpenAI 发送 `reasoning_effort=high`

#### Scenario: Default effort level

- **WHEN** Agent 配置包含 `thinking: true` 但未设置 `reasoning_effort`
- **THEN** 系统向 OpenAI 发送 `reasoning_effort=medium`

### Requirement: Reasoning request parameter mapping

当 `thinking` 为 `true` 时，系统 SHALL 将 `max_tokens` 映射为 `max_completion_tokens`，并且不发送 `max_tokens`；当 `thinking` 为 `false` 时，系统保持使用 `max_tokens`。

#### Scenario: Reasoning model token limit

- **WHEN** Agent 配置 `thinking: true` 且 `max_tokens: 4096`
- **THEN** 系统向 OpenAI 发送 `max_completion_tokens=4096` 且不发送 `max_tokens`

#### Scenario: Non-reasoning model token limit

- **WHEN** Agent 配置 `thinking: false` 且 `max_tokens: 2048`
- **THEN** 系统向 OpenAI 发送 `max_tokens=2048` 且不发送 `max_completion_tokens`

### Requirement: Configuration validation

系统 SHALL 在启动时校验每个 Agent 的 `reasoning_effort` 取值。当 `reasoning_effort` 非空且取值不在 `{low, medium, high}` 中时，启动失败并返回明确错误。

#### Scenario: Invalid reasoning effort

- **WHEN** Agent 配置包含 `reasoning_effort: ultra`
- **THEN** 系统启动失败并提示 `reasoning_effort` 无效

### Requirement: No unsupported sampling parameters for reasoning models

当 `thinking` 为 `true` 时，系统 SHALL 不向 OpenAI 发送 `temperature`、`top_p`、`presence_penalty`、`frequency_penalty` 等 reasoning 模型不支持的采样参数。

#### Scenario: Reasoning request omits temperature

- **WHEN** Agent 配置 `thinking: true`
- **THEN** 系统向 OpenAI 发送的请求中不包含 `temperature` 字段
