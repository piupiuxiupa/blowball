# model-effort-v2

## Why

per-request-model-selection 引入模型目录后,"模型/思考"配置出现三处来源(`openai.model`、`agents.<name>.model/thinking/reasoning_effort`、请求参数),D2 四行解析表复杂、agent 间模型可以漂移,隐式目录的"零行为变化"分支让代码与 spec 各自背上双路径。同时,请求级 `reasoning_effort: "none"` 走"不发参数"分支——OpenAI 兼容网关(如 glm)上的思考型模型收不到显式关闭信号,按模型自身默认等级继续思考,none 形同虚设。

本部署的 operator 即唯一用户,值得用一次 breaking 配置迁移换取:模型与思考等级各有**一条**配置来源 + 请求覆盖,`none` 显式下发。

## What Changes

- **BREAKING** `openai.models` 目录变为**必填**:隐式目录合成、"参数未启用 → 400" 整条路径、顶层 `openai.max_context_tokens` 全部移除;未配置目录时配置加载失败
- **BREAKING** `openai.model` 改名 `openai.title_model`,只服务标题生成(不必属于目录);未设时回退 default 条目
- **BREAKING** 删除 `agents.<name>.model`、`agents.<name>.thinking`、`agents.<name>.reasoning_effort` —— agent 配置只剩 prompt/配额/能力面(name、system_prompt、max_tokens、tools、mcp、skills、max_rounds、output_schema、retry),模型与思考等级成为纯 turn 级属性
- 新增 `openai.default_reasoning_effort`(`none|low|medium|high|xhigh|max`,未设 → `none`)作为唯一的 effort 默认来源
- 请求解析坍缩为两条独立轴:`model = 请求 || default 条目`,`effort = 请求 || 全局默认`;非思考条目把 effort 钳为 `none`(请求显式非 none → 400 `INVALID_EFFORT`,全局默认撞上 → 钳 + WARN)
- **B2 wire 家族**(wire 形态跟条目能力走):`thinking: true` 条目**永远**发送 `reasoning_effort`(含 `none`)+ `max_completion_tokens`;`thinking: false` 条目照旧不发 `reasoning_effort`、走 `max_tokens`
- 仅指定 `model` 的请求,effort 派生从硬编码 `medium` 改为全局默认("仅 model 接管 thinking"特例随 per-agent 配置一起消失)
- `GET /api/v1/models` 响应新增 `default_reasoning_effort` 字段(增量,前端选择器预选)
- 被删字段的残留配置(`openai.model`、顶层 `max_context_tokens`、agent 三字段)在加载时**显式报迁移错误**,而非静默忽略
- `output_schema` × effort 冲突:配置级检查改为"任一 agent 带 `output_schema` 且 `default_reasoning_effort ≠ none` → 加载失败";请求级 400 门禁不变(仍以解析后 effort ≠ none 为界)

## Capabilities

### New Capabilities

(无 —— 全部落在既有能力的需求变更上)

### Modified Capabilities

- `per-request-model-selection`: 目录必填(隐式目录需求改写);请求解析改双轴;effort=none 的 wire 语义(B2);三 agent 一致覆盖需求简化(不再存在 agent 级配置可接管);配置面新增 `title_model`/`default_reasoning_effort`;`/models` 响应增量字段
- `agent-reasoning-configuration`: per-agent `thinking` 开关与 per-agent effort 等级需求**移除**,改为部署级默认等级 + B2 wire 家族(token 映射与采样参数排除改以条目能力为键)
- `agent-orchestration`: "Agent configuration from file" 的字段清单变化(model/thinking/reasoning_effort 移出);structured-output 子 agent 与 reasoning 冲突的场景措辞改以 effort 表达
- `context-compaction`: 有效 max context 唯一来源 = 所选目录条目的 `max_context_tokens`,legacy 字段引用移除

## Impact

- **代码**: `internal/config/config.go`(字段增删 + 迁移报错 + 校验)、`internal/handler/model_selection.go` / `model_list.go` / `message_stream.go`、`internal/agent/agent.go`(ModelOverride) / `orchestrator.go` / `confucius|chongzhi|liang.go` / `openai_client.go`(wire 分支)、`internal/service/title.go`(TitleModel) / `compaction.go`(SummaryModel 回退)、`cmd/blowball/serve.go`(装配)
- **契约**: `api/openapi.yaml`(`/models` 新字段、effort=none 描述、配置错误描述);前端需同步 `openapi.d.ts` 重新生成
- **文档**: `config.example.yaml` 重写 openai/agents 段、`CLAUDE.md` 多处(model 选择、思考配置、compaction 阈值来源)
- **Operator 迁移**(一次性,breaking):`openai.model → title_model`、目录补齐为必填、agent 三字段删除、(建议)显式设置 `default_reasoning_effort`
- **测试**: `internal/config`、`internal/handler`(model_selection/model_list/session)、`internal/agent`(model_override/orchestrator/openai_client)、`internal/service`(title/compaction)、`test/integration`(per_request_model/compaction/role_ownership/harness)

**前置顺序**: 本变更的 delta 基于归档后的主 spec 文本,须先归档 `turn-detach-resume`、再归档 `per-request-model`,然后才实施本变更。
