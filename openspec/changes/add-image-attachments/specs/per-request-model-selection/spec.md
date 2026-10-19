## MODIFIED Requirements

### Requirement: 模型目录配置

系统 SHALL 要求 `openai.models` 模型目录**必填**(≥1 条):每项包含 `name`(目录内唯一)、`max_context_tokens`(正整数)、`thinking`(该模型是否支持 reasoning,同时决定该模型的 wire 家族)、`max_completion_tokens`(该模型的输出 token 配额,正整数;`thinking: true` 条目在 wire 上映射为 `max_completion_tokens` 参数,`thinking: false` 条目映射为 `max_tokens` 参数——字段语义为"输出配额",wire 家族负责翻译)。每项还可携带可选布尔字段 `vision`(缺省 `false`:该条目是否可接收图片附件消息)。每项还可携带可选子块 `length_continue: {expand_step, max_retries}`(per-entry 续写开关,语义见 llm-length-continuation)。`openai.default_model` SHALL 指定缺省模型(未配置时取目录第一项)。`openai.default_reasoning_effort` SHALL 为可选的部署级默认思考等级(取值 `none|low|medium|high|xhigh|max`,未设置时解析为 `none`)。`openai.title_model` SHALL 为可选的标题生成模型名(未设置时取缺省目录条目;不必属于目录)。所有目录模型 SHALL 共享现有 `openai.base_url`/`api_key`。目录配置错误(未配置目录、重名条目、非正整数 `max_context_tokens`、非正整数 `max_completion_tokens`、`default_model` 不在目录内、`default_reasoning_effort` 取值非法、`length_continue` 子块含负值或非整数)SHALL 使配置加载失败;`vision` 缺省或不配置时 SHALL 视为 `false`,不产生加载错误。

#### Scenario: 未配置目录时加载失败

- **WHEN** `openai.models` 未配置或为空
- **THEN** 配置加载失败,服务不启动,错误信息指明目录必填

#### Scenario: 目录配置校验

- **WHEN** `openai.models` 存在重名条目,或某条目 `max_context_tokens` 或 `max_completion_tokens` 非正整数(含 yaml 小数被加载器 shadow-check 复核发现的截断),或 `default_model` 不在目录内,或 `default_reasoning_effort` 取值不在封闭集合内,或某条目 `length_continue` 含负值
- **THEN** 配置加载失败,服务不启动

#### Scenario: 标题模型缺省回退

- **WHEN** `openai.title_model` 未设置
- **THEN** 标题生成使用缺省目录条目的模型名

#### Scenario: 条目缺 max_completion_tokens 拒绝加载

- **WHEN** 某目录条目未设置 `max_completion_tokens`(或设为 0/负数)
- **THEN** 配置加载失败,错误信息指明该条目名与必填字段

#### Scenario: vision 缺省为 false

- **WHEN** 某目录条目未配置 `vision`
- **THEN** 该条目按 `vision: false` 解析,配置加载正常通过

### Requirement: 请求选择模型

系统 SHALL 接受 `POST /messages` 请求体的可选 `model` 参数,其值必须在模型目录内;未知模型 SHALL 返回 400 `INVALID_MODEL`。`model` 缺省时 SHALL 使用 `default_model`。当请求携带非空 `attachment_ids` 且解析出的目录条目 `vision` 为 false 时,系统 SHALL 返回 HTTP 400 `MODEL_NOT_VISION`,拒绝 MUST 发生在占用 turn 槽与任何持久化之前,不做静默降级或转述。

#### Scenario: 选择目录内模型

- **WHEN** 请求携带 `model: "glm-4.7"` 且该名称在目录内
- **THEN** 本 turn 使用 glm-4.7(含其 `max_completion_tokens` 配额与 `length_continue` 续写配置),且该模型名记录于 run 元数据与 `turn_usage.model`

#### Scenario: 未知模型被拒

- **WHEN** 请求携带 `model: "gpt-99"`(不在目录内)
- **THEN** 系统返回 HTTP 400,错误码 `INVALID_MODEL`

#### Scenario: 携带附件但模型无 vision 能力

- **WHEN** 请求携带 `attachment_ids: ["a1"]`,且解析出的目录条目 `vision` 为 false
- **THEN** 系统返回 HTTP 400,错误码 `MODEL_NOT_VISION`,不创建 run、不写消息、不产生 LLM 调用

#### Scenario: 携带附件且模型具备 vision 能力

- **WHEN** 请求携带 `attachment_ids: ["a1"]`,且解析出的目录条目 `vision` 为 true
- **THEN** 模型解析正常进行,附件按 image-attachments 的渲染要求进入本轮模型请求

### Requirement: 模型列表端点

系统 SHALL 提供 `GET /api/v1/models`(JWT 鉴权),返回模型目录(`name`、`max_context_tokens`、`max_completion_tokens`、`thinking`、`vision`)、缺省模型名与部署级默认思考等级(`default_reasoning_effort`)。条目的 `length_continue` 子块 SHALL NOT 出现在响应中(运维续写开关,非用户选择轴)。

#### Scenario: 列出目录

- **WHEN** 已认证用户请求 GET /api/v1/models
- **THEN** 系统返回 HTTP 200,body 为 `{"models": [{"name", "max_context_tokens", "max_completion_tokens", "thinking", "vision"}...], "default": "<name>", "default_reasoning_effort": "<level>"}`

#### Scenario: 未认证被拒

- **WHEN** 未携带有效 JWT 请求 GET /api/v1/models
- **THEN** 系统返回 HTTP 401
