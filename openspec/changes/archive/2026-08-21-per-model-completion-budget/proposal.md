# Proposal: per-model-completion-budget

## Why

model-effort-v2 之后,模型、思考等级、wire 家族已全部收敛为 turn 级属性(模型目录 + 部署默认 + 请求参数),但输出 token 配额 `agents.<name>.max_tokens` 仍留在 agent 配置上——这是 agent 身上最后一个 LLM 参数,与"一 turn 一 model"的现实错位:配额本质是**模型属性**(受 provider 输出限额与 thinking-budget 约束制约,例如 glm 思考模型的 budget 必须小于输出配额),同 turn 内三个 agent 共享同一模型,理应共享同一配额。同理,`openai.length_continue` 续写扩步(`expand_step`)取决于 provider 的输出限额梯度,全局一份无法适配多模型目录;且全局 opt-in 无法只给"会截断 tool_call args 的那个模型"单独开续写。

## What Changes

- **BREAKING**(配置):`agents.<name>.max_tokens` 字段移除,输出配额改由模型目录条目携带——`openai.models[].max_completion_tokens`(必填正整数,与 `max_context_tokens` 同级同校验强度,含 yaml 小数截断 shadow-check)。残留的 `agents.<name>.max_tokens` 在 load 时以 migration-pointing error 拒绝(沿用 model-effort-v2 移除 `agents.<name>.model` 的惯例)。
- **BREAKING**(配置):`openai.length_continue` 全局块整体移除,迁入每个目录条目的可选子块 `models[].length_continue: {expand_step, max_retries}`(零值/缺省 = 该条目续写关闭;任一字段非零即启用,缺省兄弟字段取默认 `expand_step: 8192`、`max_retries: 3`,负值 load 失败)。残留全局块同样 load 拒绝。续写激活从部署级 opt-in 变为 **per-entry opt-in**。
- turn 配置注入扩展:`ModelOverride` / `LLMRequest` 的配额字段更名为 `MaxCompletionTokens`,由请求解析的目录条目填充,随现有注入路径覆盖 Confucius/Chongzhi/Liang 全部 LLM 调用(含 wrap-up 回合)。内部字段随配置更名,wire 家族映射规则不变(`thinking: true` → `max_completion_tokens`;`thinking: false` → `max_tokens`)。
- 续写扩容基数改为解析条目的 `max_completion_tokens`(第 N 次尝试 = 条目配额 + N × 该条目 `expand_step`,上限该条目 `max_retries`);`runLLMRound` 的续写配置来源从 agent 持有的全局配置改为 turn 注入值。
- 写入预算引导(`xizhi_write_file`/`xizhi_modify_file` 描述注入的 `floor(配额 × 0.7)`)数字来源从 agent 配置改为解析条目的配额;按 turn 随解析结果现算注入。
- `GET /api/v1/models` 响应的每个条目新增 `max_completion_tokens` 字段(与 `max_context_tokens` 并列)。
- 标题生成与 compaction 摘要调用维持不设输出配额,行为不变(`title_model` 允许不在目录内,无条目预算可言)。

## Capabilities

### New Capabilities

(无——本变更是对现有能力的配置归属调整。)

### Modified Capabilities

- `per-request-model-selection`:目录条目 schema 新增 `max_completion_tokens`(必填)与 `length_continue` 子块(可选);残留键拒绝清单加入 `agents.<name>.max_tokens` 与 `openai.length_continue`;turn 级注入从(模型, 思考等级)扩展为(模型, 思考等级, 输出配额, 续写配置);`GET /api/v1/models` 响应新增字段。
- `llm-length-continuation`:激活语义从全局 `openai.length_continue` opt-in 改为 per-entry `models[].length_continue` opt-in;扩容基数从 agent 的 `max_tokens` 改为解析条目的 `max_completion_tokens`;`expand_step`/`max_retries` per-entry。
- `agent-reasoning-configuration`:wire 家族的配额来源与内部字段名从 `max_tokens` 更名为 `max_completion_tokens`(映射规则本身不变)。
- `agent-orchestration`:agent 配置面移除 `max_tokens`(name/system_prompt/tools/mcp/skills/output_schema/max_rounds/retry 不变);agent 不再携带任何 LLM 参数。
- `xizhi-tools`:写入预算引导数字来源从"该 agent 配置的 max_tokens"改为"该 turn 解析条目的 max_completion_tokens";差异粒度从 per-agent 变为 per-model。
- `llm-debug-logging`:请求侧 debug 日志的配额字段名随内部字段更名(`max_tokens` → `max_completion_tokens`)。

## Impact

- **配置/加载**:`internal/config/config.go`(`ModelCatalogEntry` schema、校验、残留键拒绝、`LengthContinueConfig` 归属)、`config.example.yaml`(目录条目示例、全局块删除、agent 示例删 `max_tokens`)。
- **请求解析与注入**:`internal/handler/model_selection.go`(`modelSelection`/`override()` 携带配额与续写配置)、`internal/agent/orchestrator.go`(`AgentFactory.Build` 注入)、`internal/agent/agent.go`(`ModelOverride`、`LLMRequest` 字段)。
- **agent 循环**:`internal/agent/{confucius,chongzhi,liang,roundcap,lengthcontinue}.go`(每轮 `LLMRequest` 配额、`runLLMRound` 的 lc 来源)、`internal/agent/tools.go`(写入预算注入来源)。
- **wire/日志**:`internal/agent/openai_client.go`(字段读取)、`llm-debug-logging` 的日志标签。
- **API**:`GET /api/v1/models` 响应(`api/openapi.yaml` 同步,前端 repo 重新生成类型)。
- **运维**:存量部署需把 `agents.*.max_tokens` 迁入目录条目、把全局 `length_continue` 迁入需要续写的条目,否则 load 失败(错误信息指明迁移路径)。
