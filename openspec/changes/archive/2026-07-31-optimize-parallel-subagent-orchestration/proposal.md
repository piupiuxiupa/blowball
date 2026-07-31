## Why

并行子 agent 调度（Confucius → Chongzhi/Liang）是 blowball 多 agent 系统的核心价值，但当前实现存在三处真实缺口，限制了它的可观测性、质量与鲁棒性：

1. **Token 成本是纯一次性的。** done 事件被 `ports.go` 显式排除出持久化，`messages` 表没有任何 usage 列——一次 turn 的 token 用量在 SSE 闪现后永久消失，无历史成本、无 per-session 汇总、无 per-agent 归因。更糟的是，`agent-orchestration` spec 的 "Token usage observability" 要求**已经规定** "done 事件 Meta 包含各 agent 的 token 明细"，但 `emitDone` 实际只发射扁平的 `{prompt_tokens, completion_tokens, total_tokens}`，其注释自称 "emit the standard per-agent shape so the frontend can render a breakdown table" 与事实矛盾。**实现违反了自己的 spec。** Anthropic 公开指出多 agent 系统的 token 用量约为单 agent chat 的 15 倍——不测就没法控。
2. **子 agent 返回是自由文本，Confucius 的综合质量不可控。** invoke tool 的 schema 只定义输入 `{task, context}`，无输出契约；Liang（分析型）返回一坨 prose，Confucius 必须从 N 坨 prose 中做 synthesis，质量随并行度下降。
3. **失败处理一刀切。** 瞬时错误（429/超时/网络）与语义错误（bad_args）得到相同对待——错误文本喂回模型，无重试。Liang 这类只读 agent 重试本应安全，却从不重试；Chongzhi 这类有副作用的 agent 又缺乏幂等性约束。

本变更通过**成本归因 + 持久化、结构化返回、受限重试、并行决策提示**解决这四点，并明确将"流式 join / 部分结果降级"排除在外——OpenAI tool-calling 协议要求下一轮 completion 必须拿到本轮所有 tool_call 结果，真正的流式 join 在协议层不可行。

## What Changes

- **Per-agent 成本归因端到端打通。** Confucius 的 dispatch 循环已握有每个子 agent 的 `subUsage`，但 `total.Add(*result.subUsage)` 在折回父总量时丢弃了拆分。改为同时组装 `by_agent` map，沿 `Confucius.Run → Orchestrator.Handle → emitDone` 传递，done 事件发射真实 per-agent 明细（兑现 spec 与现有注释承诺）。
- **新增 `turn_usage` 持久化（新能力 `turn-cost-tracking`）。** 新 MySQL 表按 `(session_id, trace_id)` 存储 per-turn 的 usage JSON（含 `total` 与 `by_agent`），随 `SaveMessagesBatch` 同事务写入，保留消息日志干净。提供历史成本、per-session 汇总与未来成本护栏的基石。
- **结构化子 agent 返回（A，从 Liang 起步）。** `AgentConfig` 新增可选 `output_schema`（config.yaml，仅 Liang 配置）；当配置存在时，子 agent **终轮**（`finish_reason=stop` 那轮）启用 OpenAI structured output（`response_format: json_schema`）。Chongzhi 保持自由文本（其输出本质是文件改动）。Confucius 系统 prompt 声明 `invoke_liang` 返回结构化 JSON。`thinking: true`（reasoning 模型）时 model-gate 降级为纯 prompt 约束。
- **受限软重试（C）。** 对子 agent dispatch 的瞬时错误（LLM 429/5xx/超时）做指数退避重试，**per-agent 幂等感知**：Liang（只读）默认可重试；Chongzhi（写文件）仅在**未触发任何 tool_call**（即 LLM 调用本身失败、尚未碰文件系统）时可重试。`bad_args`/`unknown_tool` 等语义错误永不重试。配重试预算（每 turn 最大次数），依赖成本追踪防失控。
- **并行决策提示（E）。** Confucius 系统 prompt 新增显式并行指导：独立子任务在单 assistant turn 内 emit 多个 tool_calls；有依赖才串行；并行预算 2-3、避免 >5；禁止对同一子 agent 发重叠任务。纯 config，零代码风险，但需成本追踪才能度量其效果。
- **明确排除：流式 join / 部分结果降级（F）。** OpenAI 协议要求下一轮 completion 拿到本轮全部 tool_call 结果；"边出结果边喂回"不可行。turn 延迟已是 `max(子agent延迟)` 而非 `sum`，并行的吞吐收益已兑现。若未来出现具体 MCP 长尾痛点，将以"per-sub-agent 超时 + 部分结果"形式归入重试/失败处理，而非流式。

## Capabilities

### New Capabilities

- `turn-cost-tracking`: 每个 chat turn 的 per-agent token 用量持久化到 `turn_usage` 表，支持历史成本查询、per-session 汇总与成本护栏。覆盖表结构、写入时机（随消息批次同事务）、读取契约与 done 事件 usage 形状的权威定义。

### Modified Capabilities

- `agent-orchestration`: 三类 requirement 变更——(1) **修正并强化** "Token usage observability"：done 事件 SHALL 发射 per-agent 拆分（`by_agent` map），修复当前扁平实现与 spec/注释的自相矛盾；(2) **新增** "Structured sub-agent return contract"：配置了 `output_schema` 的子 agent 终轮 SHALL 启用 structured output；(3) **新增** "Transient error retry for sub-agent dispatch"：瞬时错误 SHALL 受限重试，per-agent 幂等感知；(4) **扩展** "Parallel agent execution"：Confucius 系统 prompt SHALL 包含并行决策指导。

## Impact

- **Code**:
  - `internal/agent/`（confucius.go: 组装并传递 `by_agent`；dispatchSubAgent 重试包装与幂等判定；agent.go: AgentConfig 透传 `output_schema`；openai_client.go: 终轮 `response_format` 支持；orchestrator.go: `doneUsage`/`emitDone` 改造）。
  - `internal/handler/`（message_stream.go / ports.go: done 事件 usage 形状消费 + turn_usage 写入编排；event_mapper.go: 无需改，usage 不进消息日志）。
  - `internal/model/`（新增 `TurnUsage` 类型）。
  - `internal/store/mysql/`（新增 `turn_usage.go` store + `SaveTurnUsage`）。
  - `internal/service/session.go`（`SaveMessagesBatch` 同事务追加 turn_usage 写入）。
  - `internal/config/config.go`（`AgentConfig.OutputSchema`、重试配置 `AgentRetryConfig`）。
- **Migrations**: 新增 `010_turn_usage.sql`（`turn_usage` 表：`id`, `session_id`, `trace_id`, `user_id`, `usage_json`, `created_at`，FK 到 sessions ON DELETE CASCADE；与 `008_deletion_archive.sql` 风格一致可考虑加镜像表，本变更先不做）。
- **API**: done 事件 `Meta.usage` 形状变更——从扁平 `{prompt_tokens,...}` 变为 `{total:{...}, by_agent:{Confucius:{...},...}, meta:{...}}`。**向后兼容性注意**：这是 SSE 事件 payload 的非破坏性扩展（新增嵌套字段），但消费 `usage.total_tokens` 等顶层字段的旧前端需迁移到 `usage.total.total_tokens`。需同步更新 `api/openapi.yaml` 与 blowball-frontend。
- **Config**: `agents.liang.output_schema`（可选 JSON Schema）、可选 `agents.<name>.retry.{max_attempts, backoff}`（默认值内置，Liang 默认开启、Chongzhi 默认关闭）。`config.example.yaml` 补示例。
- **Dependencies**: 无新增第三方依赖（structured output 走 openai-go 既有 `response_format`；重试用标准库或现有 `errgroup`）。
- **Specs/OpenAPI**: 更新 `api/openapi.yaml` 的 done 事件 schema；本 change 的 delta spec 覆盖 `agent-orchestration` 与新增 `turn-cost-tracking`。
