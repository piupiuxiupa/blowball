# Design: add-llm-round-retry

## Context

当前重试覆盖呈不对称形态：openai-go SDK（默认 2 次）只重试流建立前的失败；`dispatchSubAgent` 对子代理做派发级重试（四闸门：policy enabled / isTransientError / ToolCallTracker 幂等 / turn 级 token 预算）；但 Confucius 自身的 LLM 轮没有任何 agent 级重试——`runLLMRound` 一失败即 `agent_error(llm_error)` + turn 失败（`internal/agent/confucius.go:206`）。子代理虽可被派发级重试救回，但一个执行过多轮工具调用的子代理失败后整体作废（tracker 闸门正确拒绝重放），其中的每轮 LLM 调用同样没有局部重试。

关键结构性事实：**能杀死 turn 的瞬时失败点只有 `runLLMRound` 返回 err 这一处**——工具调用失败走 status envelope、子代理失败走 toolResult 错误内容，均不杀 turn；length 耗尽走专门路径。且 LLM 调用失败发生时，本轮的工具尚未派发，失败点在构造上无副作用。三个 agent 的主循环与 round-cap wrap-up 轮都已共享 `runLLMRound`（`internal/agent/lengthcontinue.go:98`）。

## Goals / Non-Goals

**Goals:**
- 所有 agent（Confucius/Chongzhi/Liang，含 wrap-up 轮）的单次 LLM 流式调用对瞬时错误（429/5xx/超时/网络抖动/watchdog 掐流）具备有界重试。
- 重试零副作用风险：重发逐字节相同的请求，不重放工具、不重放轮次、不重复追加对话状态。
- 与既有机制（length continuation、派发级重试、SDK 重试、retryBudget、retry 事件信号）正交组合而非相互破坏。
- 真实开销如实入账（失败 attempt 的 usage 折入 roundResult.Usage）。

**Non-Goals:**
- 不为 title 生成与 compaction 摘要加重试（它们不经 `runLLMRound`，失败已按 WARN + 降级处理）。
- 不改变 `isTransientError` 分类器、退避公式、`retryErrorEvent` 事件形状。
- 不引入每级独立的配置子块（不拆 `retry.round` / `retry.dispatch`）。
- 不做前端改动（`Meta.retry=true` 渲染已存在，`agent_error` 按 agent 名通用渲染）。

## Decisions

### D1: 重试单元 = `runLLMRound` 内部的单次 `StreamChat` 调用

在 continuation 循环体内（`lengthcontinue.go:107` 的 `client.StreamChat` 调用处）包裹重试，而不是：

- **重试整个 `Agent.Run`**（Confucius 场景）：重放全部轮次——重新执行子代理调用与工具（烧钱、可能重复写文件）、事件流全量重复。否决。
- **在 `confucius.go:187` 外层包裹 `runLLMRound` 调用**：continuation 序列中途瞬断时，`round` 已被 `scaffoldLengthRound` 追加过脚手架（`lengthcontinue.go:150`），外层重入需要重建 req 并面临脚手架双重追加或状态重放问题。否决。

单次 `StreamChat` 是最纯的原子：重试时 `req`（Messages + 该 attempt 的 MaxCompletionTokens）逐字节不变；每次 continuation attempt 各自独立可重试；脚手架只在成功路径（`finish_reason=length`）上追加，瞬态失败路径不产生任何状态变更。

### D2: 重试判定顺序——取消绝对优先

失败后按序判定：(1) `ctx.Err() != nil` → 直接返回取消错误，**绝不重试**（用户取消 / graceful shutdown / hub 关闭导致的 `onToken` 返回 ctx 错误都归此类）；(2) attempt 已达 `max_attempts` 上限 → 返回错误；(3) `!isTransientError(err)` → 返回错误（`lengthExhaustedMessage` 刻意不含瞬时子串，天然落入此类）；(4) `retryBudget` 不再允许 → 返回错误。全过才发射 `retryErrorEvent(agentName, err)`、按 `computeBackoff` 退避、重发同一 `req`。退避 sleep 用 `select { <-time.After / <-ctx.Done() }`（与 `dispatchSubAgent` 相同模式，等待期取消立即生效）。

### D3: 配置——单一 `retry` 块统一治理两级重试，三个 agent 默认启用

`agents.<name>.retry`（既有 `AgentRetryConfig`，字段不变）语义扩展为"该 agent 的瞬时错误重试统一策略"：enabled / max_attempts / backoff 同时作用于该 agent 自身的轮次级重试与（对子代理的）派发级重试；`agents.confucius.retry.budget_tokens` 语义从"子代理重试预算"扩展为"全 turn 重试预算"。

- **默认值变更（有意）**：`applyRetryDefaults` 改为对三个 agent 一律 `enabled=true`；`defaultRetryMaxAttempts` 2 → 3（1 次原始 + 2 次重试）。行为变更：Chongzhi 派发级重试默认由关→开。理由：派发级重试的副作用安全本就由 `ToolCallTracker` 闸门（该次调用执行过任何工具即拒绝重试）承载，而非 enabled 开关——spec 既有场景 "Side-effecting agent retried only before any tool call" 已认可该路径安全；关闭默认值在只有一级重试的时代是双保险，在两级共享一个开关后反而让 Chongzhi 失去轮次级保护。
- **备选（否决）**：拆 `retry.round` / `retry.dispatch` 两个子块——保留全部旧默认，但配置面翻倍、嵌套 enabled 语义混乱；统一块的"该 agent 的重试策略"心智模型更简单。
- 配置校验复用既有守卫（max_attempts ≥ 0、backoffs ≥ 0、initial ≤ max），无新增键。

### D4: 预算经 context 注入，全 turn 同池

`Confucius.Run` 入口创建 `retryBudget`（现有逻辑）后通过 `WithRetryBudget(ctx, budget)` 注入（与 `WithAgentName`/`WithSessionID` 同一先例、同一包）；`runLLMRound` 在重试判定处从 ctx 读取，nil 视为无限额（nil-safe，与现有 budget 语义一致）。派发上下文本就派生自 turn ctx，子代理轮次的重试自动计入同一池，无需改 `Agent.Run` 接口签名。每次瞬态失败 attempt 以其 `resp.Usage` 计费（charged before retry）；派发级重试维持现状（以失败 Run 的完整 usage 计费）。

### D5: 记账——失败 attempt 的 usage 折入，内容丢弃

瞬态失败的 attempt：(1) 其 `resp.Usage` 累加进 `res.Usage`（真实开销哲学，与 length continuation 折算所有 attempt 一致）→ 最终进入 `total`/`by_agent`/`turn_usage`；(2) 其部分 content **不**并入 `res.Content`（重试的完整输出取代；已流出的事件流 token 是无法撤回的既有瑕疵，见 R1）。context_tokens 维持只取最终成功 attempt 的 usage（D8 既有规则）。

### D6: 事件——复用 `retryErrorEvent`，退避前发射

重试前发射 `agent_error`（code `retry`、`Meta.retry=true`、携带触发错误），与派发级重试完全同形。子代理场景下事件流经 tagged hub，自动携带 `parent_tool_call_id`；Confucius 顶层事件不带该键，前端按 agent 名通用渲染。持久化为普通 `agent_error` 事件行（与派发级重试现状一致）。

### D7: 组合语义矩阵

| 组合 | 规则 |
|---|---|
| × length continuation | 计数器独立：瞬态重试不消耗 continuation 次数，continuation attempt 也不消耗重试次数。continuation 中途瞬断 → 以**当次已扩张的预算**原样重发（req 未变）。 |
| × 派发级重试 | 嵌套有界：轮次重试先在子代理内部消化瞬时错误，耗尽后 Run 失败才触发派发级判定（tracker 闸门照旧）。最坏 HTTP 尝试 = 派发 max × 轮次 max × SDK 3（默认 3×3×3=27 上限，仅持续故障时到达；实际有指数退避间隔 + 预算封顶）。 |
| × SDK 重试 | 互补：SDK 覆盖流建立前（连接错误/429/5xx，短退避），轮次重试覆盖流中断与 watchdog 超时（SDK 不管流中），互不重复触发同一失败帧。 |
| × idle watchdog | 每次重试 attempt 是一次新的 `StreamChat` 调用，watchdog 重新起表；`ErrStreamIdleTimeout` 含 "timeout" → 自动可重试。这是本变更的最大收益场景。 |

### D8: 覆盖的调用方

`runLLMRound` 的全部调用方传入各自 agent 的 `cfg.Retry`：三个主循环 + `runWrapUpRound`（wrap-up 轮继承所属 agent 的 policy——round-cap 后的收尾轮瞬断同样救回）。title 生成与 compaction 摘要直接调 `StreamChat`，不接入。

## Risks / Trade-offs

- **[R1] 失败 attempt 的部分 token 已流出事件流**（流中断前 onToken 已推送）→ 无法撤回；接受为既有瑕疵（派发级重试今天就有同一行为），重试 marker 在视觉上分隔；持久化行按 MergeEvents 合并，重载后显示为拼接文本。缓解：无（协议层面不可撤回）。
- **[R2] 持续故障下的尝试乘法**（最坏 派发×轮次×SDK）→ 全部路径均有指数退避；token 可见开销进 turn 级预算（budget_tokens 可收紧）；操作员可按 agent 调低 max_attempts。文档明示乘法公式。
- **[R3] 预算对失败 attempt 计费不准**（网关常在流尾才报 usage，中断 attempt 报 0）→ 已知局限，诚实记录报出的部分；预算是止损机制而非精确计量。派发级计费（整 Run usage）不受影响。
- **[R4] Chongzhi 派发重试默认开启的行为变更** → 安全由 tracker 闸门保证（执行过工具即拒绝），spec 场景已认可；显式 `retry: {enabled: false}` 可退回。
- **[R5] 顶层（无 parent_tool_call_id）retry marker 的前端渲染** → 事件形状与子代理重试完全一致，前端按 agent 名渲染 `agent_error`，理论上通用；实现任务中含一条前端验证（回归渲染路径），如有样式问题在 blowball-frontend 单独跟进。

## Migration Plan

纯代码变更，无数据迁移、无 schema 变化、无新配置键。未显式配置 `retry` 的既有部署获得新默认（三 agent 轮次重试 + Chongzhi 派发重试开启）；显式配置过的部署行为不变（字段语义兼容，`budget_tokens` 语义扩展为全 turn）。回滚 = 回退代码（config 兼容，无需回滚配置）。

## Open Questions

- 派发级重试在轮次级重试存在后是否应默认调低（乘法的主力来源）？→ 暂维持统一默认 3，等真实故障数据再评估；预算已提供封顶手段。
