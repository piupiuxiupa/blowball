# Design: per-model-completion-budget

## Context

model-effort-v2 已把模型/思考等级/wire 家族收敛为 turn 级属性(`openai.models` 目录 + 部署默认 + 请求参数,经 `ModelOverride` 注入三个 agent)。现存两处 LLM 参数仍留在别处:

- 输出配额:`agents.<name>.max_tokens`(per-agent,`AgentConfig.MaxTokens` → `LLMRequest.MaxTokens`,wire 家族在 `openai_client.go:210-219` 按 `thinking` 映射到 `max_completion_tokens`/`max_tokens`)。
- 续写配置:全局 `openai.length_continue`(`{expand_step, max_retries}`,opt-in),扩容基数取 per-agent `max_tokens`。

消费者共三处:每轮 `LLMRequest`、`runLLMRound` 续写扩容(`lengthcontinue.go:104,128`)、写入预算引导注入(`tools.go:69,92`,`floor(max_tokens × 0.7)`)。

## Goals / Non-Goals

**Goals:**

- 目录条目成为模型 LLM 参数的唯一归属:`{name, max_context_tokens, thinking, max_completion_tokens, length_continue}`。
- `ModelOverride` 携带配额与续写配置,沿用现有注入路径覆盖三 agent 全部调用点。
- 续写激活从部署级 opt-in 变为 per-entry opt-in(可只给会截断的模型开)。
- 残留旧键在 load 时以 migration-pointing error 拒绝,与 model-effort-v2 惯例一致。

**Non-Goals:**

- 不引入 per-agent 预算 override(一 turn 一 model,三 agent 共享配额;差分需求出现时再议)。
- 不改 wire 映射规则、SSE 事件面、持久化面。
- 不给标题生成/compaction 摘要设配额(维持无 cap;`title_model` 允许不在目录内)。
- 不改请求参数面(`POST /messages` 不新增 per-request 预算参数)。

## Decisions

### D1:字段名与位置——`openai.models[].max_completion_tokens`,必填正整数

与 `max_context_tokens` 同级同校验强度:必填、正整数、yaml 小数截断 shadow-check(加载器对原始值复核,防 yaml.v3 静默截断 `8192.5` → `8192`)。**备选**:"可选 + 缺省 8192"被否——目录是强制配置面,缺省会掩盖迁移遗漏;`max_context_tokens` 已确立必填先例。

命名采用 OpenAI 的 `max_completion_tokens`(与 thinking 家族 wire 参数同名)。已知别扭:`thinking: false` 条目在 wire 上仍发送 `max_tokens`(语义不同的旧参数)——字段实质是"该模型的输出配额",wire 家族负责翻译。此别扭写进 spec 与 `config.example.yaml` 注释,防误读。

### D2:内部字段全链路更名 `MaxCompletionTokens`

`ModelCatalogEntry.MaxCompletionTokens` → `modelSelection` → `ModelOverride.MaxCompletionTokens` → `LLMRequest.MaxCompletionTokens` → `openai_client.go` 读取(`thinking: true` → `params.MaxCompletionTokens`;`thinking: false` → `params.MaxTokens`)。约 26 个构造点为机械更名。**备选**:只改 yaml 键、内部保留 `MaxTokens` 被否——一个概念两个名字,且 llm-debug-logging 的日志标签会与配置永久错位。debug 日志标签随之改为 `max_completion_tokens`。

### D3:整个 `length_continue` 块进条目(含 `max_retries`)

条目子块 `length_continue: {expand_step, max_retries}`:未配置/全零 = 该条目续写关闭;任一非零启用,缺省兄弟取默认(`expand_step: 8192`、`max_retries: 3`);负值/非整数 load 失败(复用现有 `LengthContinueConfig` 校验逻辑,仅换归属与逐条目调用)。激活语义干净:entry 配了就开。**备选**:"`expand_step` 进条目、`max_retries` 留全局"被否——激活需要两处配置联动(全局块 enabled × per-entry step),且 step × retries 联动决定最大扩幅,拆开难推理。

扩容公式改为:第 N 次尝试(0 基)= `entry.max_completion_tokens + N × entry.expand_step`,上限 `entry.max_retries`。`runLLMRound` 的 `lc` 参数来源从 agent 构造时捕获的全局配置改为 turn 注入的 `ModelOverride.LengthContinue`(值类型,零值 = 禁用,`Resolve()` 行为不变)。

### D4:写入预算引导按解析条目现算

`injectWriteBudgetGuidance` 的输入从 `agentCfg.MaxTokens` 改为 turn 解析条目的 `MaxCompletionTokens`(`floor(预算 × 0.7)`)。注入层本来就是 per-turn 的(`buildAgentRegistry` 每请求重建、渲染 tools[]),`ModelOverride` 在 `Build` 处可得,顺流而下即可。引导锚定**基础配额**而非"配额 + retries × step"——引导的语义是"单次写入别顶到 cap",锚定基础值保守且简单。差异粒度从 per-agent 变为 per-model(同 turn 内三 agent 数字一致——同模型同配额,天然如此)。

### D5:`GET /api/v1/models` 暴露 `max_completion_tokens`,不暴露 `length_continue`

与 `max_context_tokens` 并列进入每个条目对象。`length_continue` 是运维续写开关而非用户选择轴,不进前端契约。`api/openapi.yaml` 同步,前端 repo `npm run generate-api` 重新生成。

### D6:标题/compaction 调用维持原有配额形态

`TitleService.callLLM` 的 `LLMRequest` 本就不设 `MaxTokens`,更名后仍不设;`title_model` 不必在目录内,无条目预算可取。compaction 摘要维持其**固定内部预算**(`compactionSummaryMaxTokens = 4096`,与条目无关)——探索期记录的"compaction 无 cap"与代码不符,以代码为准(行为逐字节不变,仅字段更名)。

### D7:残留键拒绝清单扩充

`agents.<name>.max_tokens` → 指向 `openai.models[].max_completion_tokens`;`openai.length_continue` → 指向 `openai.models[].length_continue`。与现有残留拒绝(`agents.<name>.model` 等)同一机制、同一错误风格(migration-pointing)。

## Risks / Trade-offs

- [升级即断] 存量 config.yaml 带旧键,升级后 load 失败 → 缓解即设计意图(fail-fast + 指明迁移路径的错误信息);`config.example.yaml` 给出迁移前后对照注释。
- [失去 per-agent 差分] 三 agent 共享条目配额 → 接受(一 turn 一 model);真实需求出现时再加可选 override,不在本变更预埋。
- [wire 命名别扭] `max_completion_tokens` 在非思考条目上发 `max_tokens` → spec 与配置注释明示;内部只有一个配额概念,无歧义空间。
- [续写回归风险] `runLLMRound` 的 lc 来源切换动到四个调用点的公共路径 → 现有 `lengthcontinue_test.go` 断言扩容序列,补 per-entry 启用/禁用的目录级用例。

## Migration Plan

1. 运维编辑 config.yaml:把各 `agents.<name>.max_tokens` 的值(通常三处同值)迁入每个目录条目的 `max_completion_tokens`;曾启用全局 `length_continue` 的,把块迁入需要续写的条目(通常只有截断问题的模型)。
2. 删除 agent 段与 openai 段的旧键,重启服务(load 拒绝会兜住任何遗漏)。
3. 回滚:还原二进制 + 还原 config.yaml(无 DB 迁移、无运行时状态)。

## Open Questions

(无——max_retries 归属、models 端点暴露面、内部更名范围均已在探索阶段拍板,见 D2/D3/D5。)
