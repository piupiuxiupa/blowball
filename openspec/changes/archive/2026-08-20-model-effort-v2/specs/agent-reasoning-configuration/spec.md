# agent-reasoning-configuration 变更规格

## MODIFIED Requirements

### Requirement: Configuration validation

系统 SHALL 在启动时校验 `openai.default_reasoning_effort` 的取值。当取值不在 `{none, low, medium, high, xhigh, max}` 中时,启动失败并返回明确错误。agent 配置中思考相关字段的残留拒绝见 per-request-model-selection 的"已删除配置字段的残留拒绝"需求。

#### Scenario: Invalid default reasoning effort

- **WHEN** `openai.default_reasoning_effort` 设置为 `ultra`
- **THEN** 系统启动失败并提示取值无效

### Requirement: No unsupported sampling parameters for reasoning models

当 turn 的 wire 家族为 reasoning(所选目录条目 `thinking: true`)时,系统 SHALL 不向 provider 发送 `temperature`、`top_p`、`presence_penalty`、`frequency_penalty` 等 reasoning 模型不支持的采样参数。

#### Scenario: Reasoning request omits temperature

- **WHEN** turn 解析到 `thinking: true` 的目录条目
- **THEN** 系统发送的请求中不包含 `temperature` 字段

## ADDED Requirements

### Requirement: 部署级默认思考等级

系统 SHALL 支持 `openai.default_reasoning_effort` 作为唯一的思考等级默认来源,取值 `none | low | medium | high | xhigh | max`,未设置时 SHALL 解析为 `none`。agent 配置 SHALL NOT 包含思考开关或思考等级字段;turn 的生效等级 SHALL 为"请求参数 > 部署默认"(解析与门禁见 per-request-model-selection)。当任一 agent 配置了 `output_schema` 且部署默认 ≠ none 时,配置加载 SHALL 失败——结构化输出与 reasoning 互斥,无参数 turn 会必然冲突。

#### Scenario: 部署默认等级生效

- **WHEN** `openai.default_reasoning_effort: high` 且请求不携带 effort 参数
- **THEN** 该 turn 全部 LLM 调用发送 `reasoning_effort=high`

#### Scenario: 未设置默认解析为 none

- **WHEN** `openai.default_reasoning_effort` 未设置且请求不携带 effort 参数
- **THEN** 该 turn 全部 LLM 调用发送 `reasoning_effort=none`(thinking 条目)或不发送(thinking: false 条目)

#### Scenario: output_schema 与非 none 默认冲突拒绝启动

- **WHEN** 某配置了 `output_schema` 的 agent 存在,且 `openai.default_reasoning_effort` 为非 `none` 取值
- **THEN** 配置加载失败,服务不启动

### Requirement: Wire family follows catalog entry capability

LLM 请求的 wire 形态 SHALL 由所选目录条目的 `thinking` 能力决定:`thinking: true` 条目 SHALL 在每次请求中发送 `reasoning_effort`(取值包括字面 `none`),并将 `max_tokens` 映射为 `max_completion_tokens`(不发送 `max_tokens`);`thinking: false` 条目 SHALL NOT 发送 `reasoning_effort`,并使用 `max_tokens`。同一 turn 的全部 agent 调用 SHALL 落在同一 wire 家族(条目相同)。

#### Scenario: none 在思考条目上显式下发

- **WHEN** turn 解析到 `thinking: true` 条目且 effort 为 `none`
- **THEN** 请求携带 `reasoning_effort="none"` 与 `max_completion_tokens`,不携带 `max_tokens`

#### Scenario: 非思考条目不发送 effort

- **WHEN** turn 解析到 `thinking: false` 条目
- **THEN** 请求不携带 `reasoning_effort`,携带 `max_tokens`

## REMOVED Requirements

### Requirement: Per-agent thinking toggle

**Reason**: thinking 配置从 agent 层整体移除——模型与思考等级成为 turn 级属性(目录条目 + 部署默认 + 请求参数),agent 间不再有思考开关差异。
**Migration**: 删除 `agents.<name>.thinking`;思考等级由 `openai.default_reasoning_effort` 与请求 `reasoning_effort` 决定;wire 形态由所选条目 `thinking` 决定(见 "Wire family follows catalog entry capability")。

### Requirement: Configurable reasoning effort level

**Reason**: per-agent effort 字段随 per-agent thinking 一起移除,避免"三处来源"回归;部署级默认取而代之。
**Migration**: 删除 `agents.<name>.reasoning_effort`;如原各 agent 等级一致,将其提升为 `openai.default_reasoning_effort`(未设默认 `none`,不再是 `medium`)。

### Requirement: Reasoning request parameter mapping

**Reason**: 被 "Wire family follows catalog entry capability" 完整取代——token 参数映射不再以 agent 级 `thinking` 开关为键,改以目录条目能力为键,且覆盖了原需求的全部场景。
**Migration**: 无操作面变化;行为差异见新需求(`thinking: true` 条目在 effort=none 时同样发送 `reasoning_effort` 并使用 `max_completion_tokens`)。
