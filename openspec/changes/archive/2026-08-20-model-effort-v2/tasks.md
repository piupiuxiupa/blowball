# model-effort-v2 — 任务

前置:先归档 `turn-detach-resume` 与 `per-request-model`(openspec archive),本变更基于归档后的主 spec 实施。

## 1. 配置层(internal/config)

- [x] 1.1 `OpenAIConfig` 字段调整:`Model` → `TitleModel`(yaml `title_model`)、新增 `DefaultReasoningEffort`(yaml `default_reasoning_effort`)、删除顶层 `MaxContextTokens` 字段;`AgentConfig` 删除 `Model`/`Thinking`/`ReasoningEffort` 三字段
- [x] 1.2 校验重写:`openai.models` 必填(空目录 → load 失败);`default_reasoning_effort` 封闭集合 `none|low|medium|high|xhigh|max`(未设解析为 `none`);删除旧 agent thinking/effort 校验;新增 output_schema 交叉检查(任一 agent 带 output_schema 且默认 ≠ none → load 失败)
- [x] 1.3 残留字段拒绝:yaml 原始 shadow 扫描 `openai.model`、顶层 `openai.max_context_tokens`、`agents.<name>.model|thinking|reasoning_effort`,残留即报指向替代配置的迁移错误(沿用分数截断 shadow-check 先例)
- [x] 1.4 删除隐式目录机制:`ModelCatalog()` 的合成分支、`DefaultModelName()` 的 `openai.model` 回退、`RequestModelSelectionEnabled()`(参数永远启用);新增 `TitleModelName()` 辅助(未设 → `DefaultModelName()`)
- [x] 1.5 单测更新:`internal/config` 目录/校验/残留拒绝用例(含 model_catalog_test.go 重写)

## 2. 请求解析(internal/handler)

- [x] 2.1 `model_selection.go` 重写为双轴解析:删除 `Explicit`/隐式分支与 `Apply` 条件;`model = 请求 || default 条目`,`effort = 请求 || 全局默认`;非思考条目钳 none(显式请求非 none → 400 `INVALID_EFFORT`;默认撞上 → 钳 + WARN);output_schema 400 门禁保持
- [x] 2.2 `ModelOverride` 语义调整:结构保留但永远非零(Build 每回合注入解析结果),`Thinking` 字段改为 wire-family 标记(条目 `thinking` 拷贝);`message_stream.go`/`ports.go` 装配同步
- [x] 2.3 `model_list.go`:`GET /api/v1/models` 响应新增 `default_reasoning_effort`;删除隐式条目合成引用
- [x] 2.4 单测更新:model_selection_test.go / model_list_test.go / session_test.go / turn_run_test.go / compaction_test.go(handler 侧)

## 3. Agent 层(internal/agent)

- [x] 3.1 三 agent 的 `LLMRequest` 构造改从 turn 级配置取 Model/Effort/wire-family(不再读 AgentConfig 三字段);wrap-up 回合、sub-agent 工厂同步
- [x] 3.2 `orchestrator.go` `Build`:override 永远应用(删 `IsZero` 快路径),克隆逻辑保持
- [x] 3.3 `openai_client.go` wire 分支改为 B2:wire-family 标记为真 → 永远发 `reasoning_effort`(含字面 `none`)+ `max_completion_tokens`;为假 → 不发 effort、`max_tokens`;删除恒 0 的 temperature 死参数路径
- [x] 3.4 单测更新:model_override_test.go / orchestrator_test.go / openai_client_test.go(重点:none 显式下发、非思考条目不发、max_completion_tokens 映射)

## 4. 服务层(internal/service)

- [x] 4.1 `title.go`:读 `TitleModel`(装配处解析回退 default 条目),调用形态不变
- [x] 4.2 `compaction.go`:`SummaryModel` 启动回退从 Confucius 模型改为 `DefaultModelName()`;阈值来源只剩条目窗口(删 legacy 字段引用)
- [x] 4.3 单测更新:title_test.go / compaction_test.go

## 5. 装配与契约(cmd + api + 文档)

- [x] 5.1 `cmd/blowball/serve.go`:TitleService/CompactionService/ModelSelectionConfig 装配按新字段;api 角色的 api_key 警告逻辑不受影响复核
- [x] 5.2 `api/openapi.yaml`:`reasoning_effort` 参数描述(none 显式下发)、`/models` 响应加 `default_reasoning_effort`、配置错误描述同步
- [x] 5.3 `config.example.yaml` 重写 openai/agents 段(models 必填示例、title_model、default_reasoning_effort、迁移注记);`CLAUDE.md` 更新(per-request model 节、thinking 约定、compaction 阈值来源、路由表中 /messages 参数说明)

## 6. 集成测试与验证

- [x] 6.1 `test/integration` 更新:per_request_model_test.go 重写(双轴、none 下发、钳制、目录必填)、compaction_test.go(阈值来源)、role_ownership_test.go / harness_test.go 装配面
- [x] 6.2 `make test` + `make lint` 全绿;手动核对迁移路径:旧 config.yaml(带残留字段)→ 明确报错;新 config.yaml → 正常启动,`GET /models` 返回 default_reasoning_effort
