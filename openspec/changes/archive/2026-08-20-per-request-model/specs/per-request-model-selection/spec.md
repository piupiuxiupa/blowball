# per-request-model-selection 变更规格

## ADDED Requirements

### Requirement: 模型目录配置

系统 SHALL 支持 `openai.models` 配置模型目录：每项包含 `name`（目录内唯一）、`max_context_tokens`（正整数）与 `thinking`（该模型是否支持 reasoning）；`openai.default_model` SHALL 指定缺省模型（未配置时取目录第一项）。所有目录模型 SHALL 共享现有 `openai.base_url`/`api_key`。`openai.models` 未配置时系统 SHALL 保持现状行为：隐式目录为单一模型（`openai.model` + `openai.max_context_tokens`），且请求参数 SHALL 不可用。目录配置错误（重名、非正整数 max_context_tokens、default_model 不在目录内）SHALL 使配置加载失败。

#### Scenario: 未配置目录时零行为变化

- **WHEN** `openai.models` 未配置
- **THEN** 系统按现状以 `openai.model` 运行，请求携带 `model` 或 `reasoning_effort` 参数返回 400

#### Scenario: 目录配置校验

- **WHEN** `openai.models` 存在重名条目，或某条目 `max_context_tokens` 非正整数，或 `default_model` 不在目录内
- **THEN** 配置加载失败，服务不启动

### Requirement: 请求选择模型

系统 SHALL 接受 `POST /messages` 请求体的可选 `model` 参数，其值必须在模型目录内；未知模型 SHALL 返回 400 `INVALID_MODEL`。`model` 缺省时 SHALL 使用 `default_model`。隐式目录（未配置 `models`）下携带 `model` 参数 SHALL 返回 400。

#### Scenario: 选择目录内模型

- **WHEN** 请求携带 `model: "glm-4.7"` 且该名称在目录内
- **THEN** 本 turn 使用 glm-4.7，且该模型名记录于 run 元数据与 `turn_usage.model`

#### Scenario: 未知模型被拒

- **WHEN** 请求携带 `model: "gpt-99"`（不在目录内）
- **THEN** 系统返回 HTTP 400，错误码 `INVALID_MODEL`

### Requirement: 请求选择思考等级与派生规则

系统 SHALL 接受可选 `reasoning_effort` 参数，取值 `none | low | medium | high | xhigh | max`。对目录中 `thinking: false` 的所选模型，仅允许 `none` 或缺省，其他取值 SHALL 返回 400 `INVALID_EFFORT`。请求生效的思考开关 SHALL 派生为 `所选模型 thinking && effort != none`：`effort=none` SHALL 不发送 `reasoning_effort` 并使用 `max_tokens`+`temperature` 分支；effort 为其他取值 SHALL 发送 `reasoning_effort` 并使用 `max_completion_tokens` 分支。请求未指定 `model` 时 `effort` SHALL 按缺省模型校验与应用。

#### Scenario: effort=none 关闭思考

- **WHEN** 所选模型 `thinking: true`，请求携带 `reasoning_effort: "none"`
- **THEN** 该 turn 的 LLM 请求不携带 `reasoning_effort`，使用 `max_tokens` 与 `temperature`

#### Scenario: effort 作用于非思考模型被拒

- **WHEN** 所选模型 `thinking: false`，请求携带 `reasoning_effort: "high"`
- **THEN** 系统返回 HTTP 400，错误码 `INVALID_EFFORT`

#### Scenario: 仅指定 effort 时按缺省模型派生

- **WHEN** 请求未携带 `model` 但携带 `reasoning_effort: "low"`
- **THEN** 系统按 `default_model` 的 `thinking` 能力校验该取值并应用

### Requirement: 三 agent 一致覆盖

请求指定 `model` 或 `reasoning_effort` 时，系统 SHALL 将解析结果同时应用于 Confucius、Chongzhi、Liang 三个 agent（同一模型、同一思考配置）。请求指定 `model` 时，三 agent 的思考配置 SHALL 完全由目录与 effort 参数派生（该 turn 不参考 agent 级 `thinking`/`reasoning_effort`）；未指定 `model` 时 agent 级配置 SHALL 照旧生效（未带 effort）或被 effort 参数覆盖（仅 effort）。若任一 agent 配置了 `output_schema` 且解析结果 effort ≠ none，系统 SHALL 返回 400（不静默降级）。各 agent 的 `max_tokens` 数值配置 SHALL NOT 被请求参数改变。

#### Scenario: 三 agent 同模型同思考等级

- **WHEN** 请求携带 `model: "gpt-5"` 与 `reasoning_effort: "high"`
- **THEN** 本 turn Confucius、Chongzhi、Liang 的全部 LLM 调用使用 gpt-5 与 `reasoning_effort=high`

#### Scenario: 仅指定模型时思考由目录派生

- **WHEN** 请求仅携带 `model: "glm-4.7"`（`thinking: false`）
- **THEN** 三 agent 以非思考模式运行，不发 `reasoning_effort`

#### Scenario: output_schema 冲突 fail fast

- **WHEN** 某配置了 `output_schema` 的 agent 存在，且请求解析结果 effort ≠ none
- **THEN** 系统返回 HTTP 400，请求不进入 orchestrator

#### Scenario: 未带参数时现状不变

- **WHEN** 请求不携带 `model` 与 `reasoning_effort`
- **THEN** 三 agent 按各自配置的模型与思考设置运行（现状行为）

### Requirement: 模型列表端点

系统 SHALL 提供 `GET /api/v1/models`（JWT 鉴权），返回模型目录（name、max_context_tokens、thinking）与缺省模型。隐式目录 SHALL 同样返回单条合成条目。

#### Scenario: 列出目录

- **WHEN** 已认证用户请求 GET /api/v1/models
- **THEN** 系统返回 HTTP 200，body 为 `{"models": [{"name", "max_context_tokens", "thinking"}...], "default": "<name>"}`

#### Scenario: 未认证被拒

- **WHEN** 未携带有效 JWT 请求 GET /api/v1/models
- **THEN** 系统返回 HTTP 401
