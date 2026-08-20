# agent-reasoning-configuration 变更规格

## MODIFIED Requirements

### Requirement: Per-agent thinking toggle

每个 Agent 的配置 SHALL 支持 `thinking` 布尔字段。当 `thinking` 为 `true` 时，系统启用 OpenAI reasoning 模式；当 `thinking` 为 `false` 或省略时，系统保持原有非 reasoning 请求行为。请求参数（`model`/`reasoning_effort`，见 per-request-model-selection 规格）SHALL 覆盖该配置：请求生效的 thinking 值派生自所选模型目录条目的 `thinking` 与请求 effort（`model.thinking && effort != none`）。未携带请求参数时，agent 配置 SHALL 原样生效。

#### Scenario: Thinking enabled

- **WHEN** Agent 配置包含 `thinking: true` 且请求未携带模型参数
- **THEN** 系统向 OpenAI 发送 `reasoning_effort` 参数

#### Scenario: Thinking disabled

- **WHEN** Agent 配置未设置 `thinking` 或设置为 `false`，且请求未携带模型参数
- **THEN** 系统不向 OpenAI 发送 `reasoning_effort` 参数

#### Scenario: Request override forces non-thinking

- **WHEN** Agent 配置包含 `thinking: true`，但请求选择了 `thinking: false` 的模型
- **THEN** 该 turn 该 agent 不发送 `reasoning_effort` 参数，走非 reasoning 请求分支

#### Scenario: Request override enables thinking

- **WHEN** Agent 配置未设置 `thinking`，但请求选择了 `thinking: true` 的模型并携带 effort ≥ low
- **THEN** 该 turn 该 agent 发送 `reasoning_effort` 参数，走 reasoning 请求分支

### Requirement: Configurable reasoning effort level

当 `thinking` 为 `true` 时，Agent 配置 SHALL 支持 `reasoning_effort` 字段，取值为 `low`、`medium`、`high`、`xhigh` 或 `max`（与现行配置校验一致）。未设置时默认值为 `medium`。请求参数 `reasoning_effort` SHALL 支持相同取值外加 `none`（关闭思考，见 per-request-model-selection 规格）；请求指定 `model` 时 effort 由目录派生（`thinking` 模型缺省 `medium`），agent 配置不参与。

#### Scenario: Explicit effort level

- **WHEN** Agent 配置包含 `thinking: true` 和 `reasoning_effort: high`，且请求未携带模型参数
- **THEN** 系统向 OpenAI 发送 `reasoning_effort=high`

#### Scenario: Default effort level

- **WHEN** Agent 配置包含 `thinking: true` 但未设置 `reasoning_effort`，且请求未携带模型参数
- **THEN** 系统向 OpenAI 发送 `reasoning_effort=medium`

#### Scenario: Request effort overrides agent config

- **WHEN** Agent 配置为 `reasoning_effort: low`，请求携带 `reasoning_effort: max` 且所选模型 `thinking: true`
- **THEN** 该 turn 系统发送 `reasoning_effort=max`

#### Scenario: Request model selection derives default effort

- **WHEN** 请求仅携带 `model`（`thinking: true`）而无 effort 参数
- **THEN** 该 turn 系统发送 `reasoning_effort=medium`，agent 配置的 effort 不参与
