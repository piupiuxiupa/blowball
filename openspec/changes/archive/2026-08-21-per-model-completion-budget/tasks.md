# Tasks: per-model-completion-budget

## 1. 配置层(internal/config)

- [x] 1.1 `ModelCatalogEntry` 新增 `MaxCompletionTokens int`(`yaml:"max_completion_tokens"`)与 `LengthContinue LengthContinueConfig`(`yaml:"length_continue"`)子块;校验:配额必填正整数 + yaml 小数截断 shadow-check(复用 `max_context_tokens` 的复核机制),`length_continue` 逐条目复用现有负值/非整数校验
- [x] 1.2 移除 `OpenAIConfig.LengthContinue` 全局字段与 `AgentConfig.MaxTokens` 字段;残留拒绝清单加入 `agents.<name>.max_tokens`(指向 `openai.models[].max_completion_tokens`)与 `openai.length_continue`(指向 `openai.models[].length_continue`),错误风格对齐现有 migration-pointing 惯例
- [x] 1.3 更新 `config.example.yaml`:目录条目示例补 `max_completion_tokens` 与注释(wire 家族翻译说明、`thinking: false` 发 `max_tokens` 的别扭明示)、按需给示例条目配 `length_continue`、删除 agent 段的 `max_tokens` 行与全局 `length_continue` 块、补迁移对照注释
- [x] 1.4 更新 `internal/config/config_test.go`:条目缺配额/负数/小数截断拒绝、per-entry `length_continue` 缺省与部分配置补默认、条目间互不串扰、两个新残留键的拒绝用例

## 2. 解析与注入(handler / agent 接口)

- [x] 2.1 `internal/agent/agent.go`:`ModelOverride` 新增 `MaxCompletionTokens int` 与 `LengthContinue config.LengthContinueConfig`;`LLMRequest` 字段更名 `MaxTokens` → `MaxCompletionTokens`(全链路机械更名:confucius/chongzhi/liang/roundcap/lengthcontinue 的构造点与 openai_client 的读取点)
- [x] 2.2 `internal/handler/model_selection.go`:`modelSelection` 携带条目配额与续写配置,`override()` 渲染进 `ModelOverride`
- [x] 2.3 `internal/agent/orchestrator.go`:`buildConfucius`/`buildChongzhi`/`buildLiang` 不再透传 `f.cfg.OpenAI.LengthContinue`,改由 `ModelOverride` 注入;`buildAgentRegistry` 渲染 tools[] 时把 `turn.MaxCompletionTokens` 传入 `injectWriteBudgetGuidance`(替换原 `agentCfg.MaxTokens` 来源)
- [x] 2.4 `internal/agent/tools.go`:写入预算引导输入参数更名并确认注入层拿到的是 turn 解析值

## 3. agent 循环与 wire

- [x] 3.1 `internal/agent/lengthcontinue.go`:`runLLMRound` 的 `lc` 改从 turn 注入取值(签名不变则改调用点传参来源),`baseTokens` 即条目配额;确认四个调用点(三主循环 + `runWrapUpRound`)全部切换
- [x] 3.2 `internal/agent/openai_client.go`:wire 家族映射读取 `req.MaxCompletionTokens`(thinking:true → `params.MaxCompletionTokens`;thinking:false → `params.MaxTokens`),映射规则与零值省略行为不变;`logLLMRequest` 的配额日志标签改为 `max_completion_tokens`,无配额调用(标题/compaction)不输出该字段
- [x] 3.3 标题生成(`internal/service/title.go`)维持不设配额、compaction 摘要(`internal/service/compaction.go`)维持固定内部预算 4096(原行为,非条目来源),二者随字段更名机械适配且行为不变

## 4. API 面

- [x] 4.1 `GET /api/v1/models` 响应条目新增 `max_completion_tokens` 字段(`length_continue` 不暴露);更新 handler、`api/openapi.yaml`,并在完成后将 openapi.yaml 拷至 blowball-frontend 仓库执行 `npm run generate-api`(前端类型再生)

## 5. 测试与收尾

- [x] 5.1 单测:目录条目配额注入三 agent(同 turn 同配额)、per-entry 续写启用/禁用分流(同部署两 turn 各选一条目)、扩容序列 = 条目配额 + N × 条目 step(沿用 `lengthcontinue_test.go` 断言并改造成条目驱动)、写入预算数字随条目区分(8192→5734 / 4096→2867)、wire 家族两分支的参数名断言
- [x] 5.2 更新受影响的既有测试(构造 `AgentConfig`/`LLMRequest`/`ModelOverride` 的所有测试点)与集成测试 harness 的目录条目构造;`make test` 全绿
- [x] 5.3 `make lint` 通过;CLAUDE.md 中涉及 `agents.<name>.max_tokens`、全局 `length_continue`、写入预算来源、models 端点响应的段落同步更新
