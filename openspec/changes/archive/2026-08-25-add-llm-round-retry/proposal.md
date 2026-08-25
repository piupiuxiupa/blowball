# Proposal: add-llm-round-retry

## Why

一个 turn 的唯一致命弱点是 Confucius 自身的 LLM 轮：openai-go SDK 只重试流建立前的失败（429/5xx/连接错误），SSE 流一旦开始就无人兜底——一条中途断流或被 idle watchdog 掐死的 Confucius 流直接报废整个 turn，即使它只是一次瞬时扰动。子代理有精心设计的派发级重试，但最外层的编排者反而完全裸奔；子代理同样存在"干了 10 轮在第 11 轮撞 429 全部作废"的浪费。失败发生的时刻（LLM 调用失败于工具派发之前）恰好是**无副作用的原子点**，重试在构造上是安全的，缺失的只是机制。

## What Changes

- **新增轮次级瞬时重试**：`runLLMRound`（三个 agent 主循环 + wrap-up 轮共享的 helper）内部对单次 `StreamChat` 调用增加瞬时错误重试——瞬断时重发**逐字节相同**的请求（messages、budget 均不变），不重放轮次、不触碰已追加的对话状态。
- **三个 agent 全部启用**（Confucius / Chongzhi / Liang，含 round-cap 后的 wrap-up 轮）；title 生成与 compaction 摘要不经 `runLLMRound`，不在范围内。
- **默认启用**：`agents.<name>.retry.enabled` 默认 `true`（现为 Liang true / Chongzhi false / Confucius 死字段），`max_attempts` 默认 3（1 次原始 + 2 次重试，现默认 2），退避复用现有指数退避（500ms 起、4s 封顶）。
- **行为变更（有意）**：Chongzhi 的派发级重试默认由关→开、派发级 `max_attempts` 默认 2→3——安全性由既有的 `ToolCallTracker` 幂等闸门（执行过任何工具即不重试派发）保证，而非 enabled 开关；单一 `retry` 配置块统一治理该 agent 的两级瞬时重试。
- **失败 attempt 的 usage 折入真实开销**：网关报出的（通常为 0 的）失败 attempt usage 计入 `roundResult.Usage` → `total`/`by_agent`/`turn_usage`，与 length continuation 的"真实开销"哲学一致；部分 token 内容不并入（重试的完整输出取代）。
- **预算同池**：轮次级重试与派发级重试共用 turn 级 `retryBudget`（`agents.confucius.retry.budget_tokens`，现有字段，语义从"子代理重试预算"扩展为"全 turn 重试预算"）；预算经 context 注入（`WithRetryBudget`，随 WithAgentName 先例），子代理轮次重试透明计入同一池。
- **事件复用既有模式**：重试前发射 `agent_error`（code `retry`、`Meta.retry=true`），子代理事件自动携带 `parent_tool_call_id`；失败 attempt 已流出的部分 token 留在事件流中（与派发级重试现状一致的既有已接受瑕疵），前端无需改动。
- **取消与非瞬时错误绝不重试**：`ctx.Err()` 优先判定；`isTransientError` 分类器不变（watchdog 超时因含 "timeout" 自动可重试；length 耗尽错误文案刻意避开瞬时子串，保持不可重试）。
- **与 length continuation 正交**：重试包裹 continuation 循环内的单次调用——脚手架只在成功路径追加，瞬态重试不消耗 continuation 次数，continuation 也不消耗重试次数。

## Capabilities

### New Capabilities

- `llm-round-retry`: 所有 agent 的 LLM 轮次内单次流式调用的瞬时错误重试——重试单元、触发条件（仅瞬时、绝不取消）、退避与次数上限、真实开销记账、turn 级共享预算、事件信号、与 length continuation / 派发级重试 / SDK 重试的组合语义。

### Modified Capabilities

- `agent-orchestration`: "Transient error retry for sub-agent dispatch" 需求变更——`agents.<name>.retry` 配置块语义从"派发级重试开关"扩展为"该 agent 的两级瞬时重试统一策略"；默认值变更（Chongzhi 派发重试默认开启、max_attempts 默认 3）；幂等安全不变式明确为由 ToolCallTracker 闸门承载。

## Impact

- **代码**：`internal/agent/lengthcontinue.go`（runLLMRound 增加重试包裹 + policy 参数）、`internal/agent/confucius.go`/`chongzhi.go`/`liang.go`（传入各自 policy；Confucius 于 Run 入口注入预算）、wrap-up 路径（同 policy）、`internal/agent/retry.go`（预算 context 注入辅助）、`internal/config/config.go`（`applyRetryDefaults` 覆盖 Confucius、默认值 3）。
- **配置**：`config.example.yaml` 的 retry 示例块更新；既有部署未显式配置 retry 者行为改变（Chongzhi 派发重试 + 所有 agent 轮次重试默认生效）。
- **不受影响**：HTTP API 契约（`api/openapi.yaml` 零变化）、前端（`Meta.retry=true` 渲染已存在）、SSE 事件类型集合（无新类型）、持久化结构。
- **文档**：CLAUDE.md 的 Agent orchestration 与 Important conventions 段落补记该 capability。
