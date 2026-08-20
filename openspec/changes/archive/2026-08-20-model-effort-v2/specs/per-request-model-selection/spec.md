# per-request-model-selection 变更规格

## MODIFIED Requirements

### Requirement: 模型目录配置

系统 SHALL 要求 `openai.models` 模型目录**必填**(≥1 条):每项包含 `name`(目录内唯一)、`max_context_tokens`(正整数)与 `thinking`(该模型是否支持 reasoning,同时决定该模型的 wire 家族);`openai.default_model` SHALL 指定缺省模型(未配置时取目录第一项)。`openai.default_reasoning_effort` SHALL 为可选的部署级默认思考等级(取值 `none|low|medium|high|xhigh|max`,未设置时解析为 `none`)。`openai.title_model` SHALL 为可选的标题生成模型名(未设置时取缺省目录条目;不必属于目录)。所有目录模型 SHALL 共享现有 `openai.base_url`/`api_key`。目录配置错误(未配置目录、重名条目、非正整数 `max_context_tokens`、`default_model` 不在目录内、`default_reasoning_effort` 取值非法)SHALL 使配置加载失败。

#### Scenario: 未配置目录时加载失败

- **WHEN** `openai.models` 未配置或为空
- **THEN** 配置加载失败,服务不启动,错误信息指明目录必填

#### Scenario: 目录配置校验

- **WHEN** `openai.models` 存在重名条目,或某条目 `max_context_tokens` 非正整数,或 `default_model` 不在目录内,或 `default_reasoning_effort` 取值不在封闭集合内
- **THEN** 配置加载失败,服务不启动

#### Scenario: 标题模型缺省回退

- **WHEN** `openai.title_model` 未设置
- **THEN** 标题生成使用缺省目录条目的模型名

### Requirement: 请求选择模型

系统 SHALL 接受 `POST /messages` 请求体的可选 `model` 参数,其值必须在模型目录内;未知模型 SHALL 返回 400 `INVALID_MODEL`。`model` 缺省时 SHALL 使用 `default_model`。

#### Scenario: 选择目录内模型

- **WHEN** 请求携带 `model: "glm-4.7"` 且该名称在目录内
- **THEN** 本 turn 使用 glm-4.7,且该模型名记录于 run 元数据与 `turn_usage.model`

#### Scenario: 未知模型被拒

- **WHEN** 请求携带 `model: "gpt-99"`(不在目录内)
- **THEN** 系统返回 HTTP 400,错误码 `INVALID_MODEL`

### Requirement: 请求选择思考等级与派生规则

系统 SHALL 接受可选 `reasoning_effort` 参数,取值 `none | low | medium | high | xhigh | max`。请求解析 SHALL 为两条独立轴:模型 = 请求 `model` 或缺省目录条目;思考等级 = 请求 `reasoning_effort` 或 `openai.default_reasoning_effort`(未设置为 `none`)。对所选条目 `thinking: false` 的模型:请求显式携带非 `none` 取值 SHALL 返回 400 `INVALID_EFFORT`;部署默认为非 `none` 时 SHALL 钳为 `none` 并记 WARN 日志。`reasoning_effort: "none"` 在 `thinking: true` 条目上 SHALL 照常作为字面值下发(wire 形态见 agent-reasoning-configuration 的 wire 家族需求),不再表示"不发送参数"。

#### Scenario: effort=none 显式下发

- **WHEN** 所选模型 `thinking: true`,请求携带 `reasoning_effort: "none"`
- **THEN** 该 turn 的 LLM 请求携带 `reasoning_effort="none"` 并使用 `max_completion_tokens` 分支

#### Scenario: effort 作用于非思考模型被拒

- **WHEN** 所选模型 `thinking: false`,请求携带 `reasoning_effort: "high"`
- **THEN** 系统返回 HTTP 400,错误码 `INVALID_EFFORT`

#### Scenario: 部署默认撞上非思考条目时钳制

- **WHEN** `openai.default_reasoning_effort: high`,请求选择 `thinking: false` 的条目且未携带 effort
- **THEN** 该 turn effort 钳为 `none` 并记 WARN,请求正常进行(不返回 400)

#### Scenario: 无参数时走部署默认

- **WHEN** 请求不携带 `model` 与 `reasoning_effort`
- **THEN** 该 turn 使用缺省目录条目与 `openai.default_reasoning_effort` 解析出的等级

### Requirement: 三 agent 一致覆盖

模型与思考等级 SHALL 为 turn 级属性:系统 SHALL 将解析出的(模型,思考等级)同时应用于 Confucius、Chongzhi、Liang 三个 agent 的全部 LLM 调用;agent 配置 SHALL NOT 包含模型或思考字段(不存在 agent 间差异,亦不存在被请求"接管"的 agent 级配置)。若任一 agent 配置了 `output_schema` 且解析结果 effort ≠ none,系统 SHALL 返回 400(不静默降级)。各 agent 的 `max_tokens` 数值配置 SHALL NOT 被请求参数改变。

#### Scenario: 三 agent 同模型同思考等级

- **WHEN** 请求携带 `model: "gpt-5"` 与 `reasoning_effort: "high"`
- **THEN** 本 turn Confucius、Chongzhi、Liang 的全部 LLM 调用使用 gpt-5 与 `reasoning_effort=high`

#### Scenario: output_schema 冲突 fail fast

- **WHEN** 某配置了 `output_schema` 的 agent 存在,且请求解析结果 effort ≠ none
- **THEN** 系统返回 HTTP 400,请求不进入 orchestrator

#### Scenario: 无参数时同样统一

- **WHEN** 请求不携带 `model` 与 `reasoning_effort`
- **THEN** 三 agent 全部 LLM 调用使用缺省目录条目与部署默认等级

### Requirement: 模型列表端点

系统 SHALL 提供 `GET /api/v1/models`(JWT 鉴权),返回模型目录(`name`、`max_context_tokens`、`thinking`)、缺省模型名与部署级默认思考等级(`default_reasoning_effort`)。

#### Scenario: 列出目录

- **WHEN** 已认证用户请求 GET /api/v1/models
- **THEN** 系统返回 HTTP 200,body 为 `{"models": [{"name", "max_context_tokens", "thinking"}...], "default": "<name>", "default_reasoning_effort": "<level>"}`

#### Scenario: 未认证被拒

- **WHEN** 未携带有效 JWT 请求 GET /api/v1/models
- **THEN** 系统返回 HTTP 401

## ADDED Requirements

### Requirement: 已删除配置字段的残留拒绝

系统 SHALL 在配置加载时拒绝本变更删除的字段残留:`openai.model`、顶层 `openai.max_context_tokens`、`agents.<name>.model`、`agents.<name>.thinking`、`agents.<name>.reasoning_effort`。任一残留 SHALL 使配置加载失败并输出指向替代配置的错误信息,SHALL NOT 被静默忽略。

#### Scenario: 残留 thinking 字段被拒

- **WHEN** config.yaml 的 `agents.confucius` 仍包含 `thinking: true`
- **THEN** 配置加载失败,错误信息指向 `openai.default_reasoning_effort` 迁移

#### Scenario: 残留 openai.model 被拒

- **WHEN** config.yaml 仍包含 `openai.model`
- **THEN** 配置加载失败,错误信息指向 `openai.title_model` 迁移
