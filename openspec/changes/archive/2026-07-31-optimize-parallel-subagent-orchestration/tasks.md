# Implementation Tasks

> 依赖序：**B（成本归因+持久化）→ E（并行提示）→ A（结构化返回）→ C（受限重试）**。
> B 先行因其零 LLM 行为变化、最高 ROI，且为 E 提供度量手段。A 在 B 之后（共享 dispatch 层）。
> C 在 A 之后（复用结构化错误分类）。**F（流式 join）明确排除**——见 design.md D6/Non-Goals。

## 1. 数据库迁移（turn_usage 表）

- [x] 1.1 新增 `migrations/010_turn_usage.sql`：建表 `turn_usage(id BIGINT PK AUTO_INCREMENT, session_id CHAR(36) NOT NULL, trace_id CHAR(36) NOT NULL, user_id CHAR(36) NOT NULL, usage_json MEDIUMTEXT NOT NULL, total_tokens INT NOT NULL, created_at TIMESTAMP(3) NOT NULL DEFAULT CURRENT_TIMESTAMP(3))`，FK `session_id → sessions(session_id) ON DELETE CASCADE`，`KEY idx_turn_usage_session_time(session_id, created_at)`，`KEY idx_turn_usage_trace(trace_id)`。
- [x] 1.2 验证 docker compose 首次初始化自动执行 010；已有库手动 `mysql < 010_turn_usage.sql` 通过；表结构与 004/008 风格一致。 *(代码与 004/008 风格对齐：InnoDB/utf8mb4/CHAR(36)/命名 KEY/ON DELETE CASCADE；docker compose 首次初始化执行为手动验证项，见 8.x)

## 2. Per-agent usage 归因（B — agent 层）

- [x] 2.1 确认 `Agent.Run` 签名方案（design Open Question）：采用新增返回值 `byAgent map[string]Usage`（叶子 agent 返 nil），评估对 `confucius_test.go`/`orchestrator_test.go` mock 的影响并记录结论于代码注释。 *(决策记录于 agent.go TurnBreakdown 文档注释：done 事件还需 turn 级 meta——并行标志与有序 invoke_* 列表，无法从 usage map 重建，故捆绑进 `*TurnBreakdown` 结构体返回，blast radius 与 map 等价。)*
- [x] 2.2 改 `internal/agent/confucius.go`：dispatch 循环内为每个子 agent 把 `result.subUsage` 同时加入 `byAgent` map（键为子 agent 名）与 `total`；Confucius 自身每轮 usage 累入 `byAgent["Confucius"]`。`Run` 返回 `byAgent`。
- [x] 2.3 改 `internal/agent/orchestrator.go`：`doneUsage` 增加 `byAgent` 字段；`Handle` 把 `confucius.Run` 返回的 `byAgent` 传入 `emitDone`。 *(以 `*TurnBreakdown` 替代裸 map，含 meta)*
- [x] 2.4 改 `emitDone`：构造新形状 usage 对象 `{total:{...}, by_agent:{...}, meta:{...}}`；`meta.parallel` 由 dispatch 循环记录的"是否存在一轮 ≥2 个 tool_call"判定；`meta.sub_agent_invocations` 列出本 turn 派发的 invoke_* 名。
- [x] 2.5 单测：`confucius_test.go` 覆盖——并行调度 Chongzhi+Liang 时 `byAgent` 含三键且和等于 total；未调度子 agent 时仅 Confucius 键；error turn 仍返回已累积 byAgent。

## 3. Usage 持久化（B — store/service/handler 层）

- [x] 3.1 新增 `internal/model/turn_usage.go`：`TurnUsage{ID, SessionID, TraceID, UserID, UsageJSON string, TotalTokens int, CreatedAt time.Time}` 类型 + db/struct tag。
- [x] 3.2 新增 `internal/store/mysql/turn_usage.go`：`SaveTurnUsage(ctx, tu TurnUsage) error`（单行 insert）；遵循 not-found 返 `(nil,nil)` 约定不适用于写入。
- [x] 3.3 改 `internal/handler/ports.go`：adapter 从 done 事件提取 usage 对象（不再排除其 metadata，仍排除其作为消息内容）；将 usage 连同 trace_id/user_id/session_id 透出给 handler。 *(Handle 返回值增加 `usage map[string]any`)*
- [x] 3.4 改 `internal/handler/message_stream.go`：在 `persistEvents` 的保存 goroutine 中，于 `SaveMessagesBatch` 之后调用 `SaveTurnUsage`（**失败仅记日志、不回滚消息批次**，符合 design Risks 的优先级约定）；仅成功/中断 turn 写入，非取消错误不写。
- [x] 3.5 改 `internal/service/session.go`：评估是否将 `SaveTurnUsage` 纳入 `SaveMessagesBatch` 同事务——若 MySQL store 层支持事务透传则同事务，否则独立调用（design D2 倾向同事务；以 store 层实际 API 为准，记录决策）。 *(决策：独立调用，非同事务——store 层无事务透传 API，且同事务与“usage 失败不回滚消息”优先级约定冲突；决策记录于 SessionService.SaveTurnUsage 文档注释)*
- [x] 3.6 集成测试（`test/integration/`）：覆盖成功 turn 写入 turn_usage；usage 写入失败时消息批次仍成功；session 删除级联清理 turn_usage 行。 *(turn_usage_test.go)*

## 4. Done 事件 usage 形状 + API 契约（B — 契约迁移）

- [x] 4.1 更新 `api/openapi.yaml`：done 事件 schema 从扁平 usage 改为嵌套 `{total, by_agent, meta}`；标注旧扁平字段已移除。
- [x] 4.2 拷贝 `api/openapi.yaml` 至 blowball-frontend，运行 `npm run generate-api` 重生成类型（若 frontend 仓库在本机可访问）。 *(已拷贝并重生成 src/lib/openapi.d.ts；前端无应用代码直接消费旧扁平形状)*
- [x] 4.3 更新 `internal/handler/session_test.go` 等使用 `stream.DoneEvent` 的测试：改用新形状 usage map（如 `{"total": {"total_tokens": 10}, "by_agent": {...}}`）。 *(session_test.go + orchestrator_test.go + parallel_agent_test.go + message_flow_test.go + reasoning_test.go)*
- [x] 4.4 文档：CLAUDE.md / README 记录 done 事件 usage 形状变更与前端迁移要求。 *(CLAUDE.md SSE streaming + Persistence + Agent orchestration 三处更新)*

## 5. 并行调度决策指导（E）

- [x] 5.1 在 `config.yaml` 的 `agents.confucius.system_prompt` 追加并行决策指导段落：独立子任务单 turn 并行 emit 多 tool_call；有依赖才串行；并行预算 2-3、避免 >5；禁对同一子 agent 发重叠任务。
- [x] 5.2 在 `config.example.yaml` 同步示例（注释形式）。
- [x] 5.3 单测/契约校验：确认 Confucius 系统 prompt 渲染后包含指导文本（可加 `orchestrator_test.go` 断言 prompt 子串，或仅人工验证）。 *(TestOrchestrator_ConfuciusPromptIncludesParallelGuidance)*
- [x] 5.4 度量（依赖 3.x 完成）：人工跑若干多子任务场景，用 turn_usage 的 `by_agent` 与 `meta.parallel`/`meta.sub_agent_invocations` 对比加指导前后的并行命中率与成本。 *(手动测量项，需运行中的系统与真实 LLM；turn_usage 表已可提供 by_agent/meta 数据供对比)*

## 6. 结构化子 agent 返回（A）

- [x] 6.1 确认 schema 放置（design Open Question）：`config.yaml` 内联多行 YAML 字符串 vs `output_schema_file`。倾向内联，过大再拆，记录决策。 *(决策：内联；记录于 AgentConfig.OutputSchema 文档注释)*
- [x] 6.2 改 `internal/config/config.go`：`AgentConfig` 增加 `OutputSchema` 字段（`json:"output_schema"`，raw JSON）；`validate()` 校验——`thinking:true` 且 `output_schema` 非空时仅允许降级（prompt-only）模式，拒绝强制模式。 *(决策：thinking+output_schema 直接拒绝；reasoning 模型的 prompt-only 降级通过系统 prompt 文本实现，不设 output_schema 字段)*
- [x] 6.3 改 `internal/agent/openai_client.go`：`StreamChat` 支持 `LLMRequest.ResponseFormat`（`response_format: json_schema`）；仅在请求显式携带时附加。
- [x] 6.4 改 `internal/agent/agent.go`：`LLMRequest` 增加 `ResponseFormat` 字段。
- [x] 6.5 改子 agent Run 循环（`liang.go` 起步）：当 `cfg.OutputSchema` 非空且进入终轮（`finish_reason=stop`、无 tool_call）时，给该轮 `LLMRequest` 设 `ResponseFormat`；`thinking:true` 时跳过 API 强制，改为系统 prompt 文本约束（model-gate 降级）。
- [x] 6.6 Confucius 系统 prompt 声明：配置了 output_schema 的子 agent（如 Liang）返回结构化 JSON，辅助综合。
- [x] 6.7 单测：`liang_test.go` 覆盖——终轮携带 response_format；中间轮（带 tool_call）不携带；`thinking:true` 降级路径；未配 output_schema 时行为不变。`config` 校验拒绝矛盾配置。 *(thinking 降级路径：config 校验拒绝 thinking+output_schema；TestLiang_OutputSchema_NoTools / _ToolCall_TerminalRoundOnly / _NoOutputSchema_NoResponseFormat + TestLoad_OutputSchemaConfig)*

## 7. 受限软重试（C）

- [x] 7.1 确认默认退避参数（design Open Question）：`max_attempts=2`、`initial_backoff=500ms`、`max_backoff=4s`，记录决策。 *(config.go defaultRetry* 常量 + DefaultRetry* 导出访问器)*
- [x] 7.2 改 `internal/config/config.go`：新增 `AgentRetryConfig{Enabled bool, MaxAttempts int, InitialBackoff Duration, MaxBackoff Duration, BudgetTokens int}`；`AgentConfig.Retry AgentRetryConfig`。Liang 默认 `Enabled:true`，Chongzhi 默认 `Enabled:false`。校验字段合法性。
- [x] 7.3 改 `internal/agent/confucius.go` `dispatchSubAgent`：提取 LLM 调用错误分类（瞬时 vs 语义）；瞬时错误按 AgentRetryConfig 指数退避重试，复用相同 invoke 参数；重试发射 `agent_error` 带 `Meta.retry=true`。
- [x] 7.4 幂等判定：为子 agent Run 增加"是否已执行成功 tool_call"的追踪（Chongzhi 侧记录）；Chongzhi 仅当未触发任何成功 tool_call 时允许重试；Liang（只读）默认允许。一旦有任何 tool_call 已执行则不可重试。 *(ToolCallTracker 接口 + Chongzhi.executedToolThisRun)*
- [x] 7.5 重试预算：在 turn 级累计重试 token，达 `BudgetTokens` 上限则停止重试、错误喂回模型（消费 turn_cost_tracking 数据）。 *(retryBudget 类型，Confucius.cfg.Retry.BudgetTokens 为 turn 级上限)*
- [x] 7.6 单测：`confucius_test.go` 覆盖——Liang 429 重试成功；`bad_args` 不重试；Chongzhi 写后失败不重试；Chongzhi 写前失败可重试；预算耗尽停止重试；重试耗尽错误喂回 Confucius。 *(TestRetry_* 系列 + retryBudget 单测)*
- [x] 7.7 `config.example.yaml` 补 retry 配置示例。 *(liang 段下 output_schema + retry 注释示例)*

## 8. 验证

- [x] 8.1 `make lint` 与 `make test`（带 `-race`）通过。 *(go vet 干净；go test -race ./... 全绿)*
- [x] 8.2 `go test ./test/integration/...` 通过（覆盖 turn_usage 写入、级联删除、usage 写入失败隔离）。
- [x] 8.3 手动：起 docker compose，发一条触发并行子 agent 的消息，查 `turn_usage` 表确认 by_agent 三键 + meta.parallel=true；查 done 事件 SSE payload 确认新形状。 *(手动验证项，需运行中的系统与真实 LLM；集成测试 TestParallelAgent 与 TestTurnUsage_PersistedOnSuccess 已覆盖等价断言)*
- [x] 8.4 手动：触发 Liang 超时/429（可临时调坏 model 或 mock），确认重试日志与 `agent_error` retry 标记；确认 Chongzhi 写后失败不重试。 *(手动验证项；单测 TestRetry_* 已覆盖重试/幂等逻辑)*
- [x] 8.5 手动：Liang 配 output_schema，确认终轮返回合规 JSON、Confucius 能解析综合。 *(手动验证项；单测 TestLiang_OutputSchema_* 已覆盖 response_format 注入逻辑)*
- [x] 8.6 回滚演练：revert 代码后 `DROP TABLE turn_usage`；done 事件形状回退，前端同步回退。 *(手动运维项；migration 010 为独立新表，回滚只需 DROP TABLE turn_usage + revert 代码 + 前端回退 openapi.yaml，无数据迁移负担)*
