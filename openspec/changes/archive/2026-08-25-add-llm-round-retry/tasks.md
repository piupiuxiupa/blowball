## 1. Config: 统一策略与默认值

- [x] 1.1 `internal/config/config.go`：`defaultRetryMaxAttempts` 2 → 3；`applyRetryDefaults` 改为对 Confucius/Liang/Chongzhi 一律默认 `enabled=true`（保留"显式配置整块为零值才覆盖"的既有规则；更新注释：Chongzhi 派发重试默认开启的安全性由 ToolCallTracker 闸门承载）
- [x] 1.2 `config.example.yaml`：更新 retry 示例块（三 agent 默认启用、`budget_tokens` 的全 turn 语义、max_attempts 默认 3）
- [x] 1.3 单测：零值配置下三 agent 均得 `enabled=true, max_attempts=3`；显式 `enabled: false` 被保留；既有校验守卫（max_attempts ≥ 0、backoff ≥ 0、initial ≤ max）不回归

## 2. Core: runLLMRound 轮次级重试

- [x] 2.1 `internal/agent/retry.go`：新增 `WithRetryBudget` / `retryBudgetFromCtx`（context 注入与读取，nil 视为无限额；沿 WithAgentName 先例）
- [x] 2.2 `internal/agent/lengthcontinue.go`：`runLLMRound` 签名增加 retry policy 参数；在 continuation 循环体内包裹单次 `client.StreamChat` 调用——判定顺序：`ctx.Err()` 优先（绝不重试）→ attempt 上限 → `isTransientError` → 预算 allows；全过后发射 `retryErrorEvent(agentName, err)`、`select { <-time.After(computeBackoff) / <-ctx.Done() }` 退避、以逐字节相同的 `req` 重发；失败 attempt 的 `resp.Usage` 折入 `res.Usage`、其 content 不并入 `res.Content`
- [x] 2.3 调用方接线：`confucius.go`/`chongzhi.go`/`liang.go` 主循环与 `runWrapUpRound` 传入各自 `cfg.Retry`；`Confucius.Run` 入口在创建 `retryBudget` 后 `WithRetryBudget` 注入 turn ctx（预算随派发 ctx 传播至子代理轮次）
- [x] 2.4 `confucius.go`：`RetryPolicy()` 注释更新（轮次级重试读取自身 cfg.Retry，派发级接口语义不变）

## 3. 单元测试（internal/agent）

- [x] 3.1 瞬断重试成功：fake client 第一次返回瞬时错误（含 watchdog 超时文案）、第二次成功——轮正常完成、req 两次逐字节一致、`agent_error(Meta.retry=true)` 恰好一次、usage 为两 attempt 之和、`res.Content` 只含成功 attempt
- [x] 3.2 取消不重试：父 ctx 已取消时失败直接返回 ctx 错误、零 retry 事件、零退避
- [x] 3.3 非瞬时不重试：普通错误直接浮出（含 length 耗尽文案验证）
- [x] 3.4 次数耗尽：连续瞬时错误至 max_attempts 后错误浮出、retry 事件数 = max_attempts-1
- [x] 3.5 与 continuation 组合：continuation attempt 瞬断后以已扩张预算原样重发、continuation 计数不消耗、脚手架不重复追加
- [x] 3.6 预算：注入有限预算的 ctx，失败 attempt 计费后耗尽 → 不再重试；子代理 ctx 能读到 Confucius 注入的同一实例
- [x] 3.7 wrap-up 轮：runWrapUpRound 路径的瞬断按所属 agent 策略重试

## 4. 集成与回归

- [x] 4.1 `test/integration`：fake LLM 首流瞬断后成功的 turn 端到端——SSE 含 retry 标记、turn 正常完成、消息持久化含 retry 的 agent_error 行、turn_usage 反映两 attempt
- [x] 4.2 回归：既有 dispatch 重试测试、length-continuation 测试、stream-idle-watchdog 测试全绿（`make test` + `make lint`）
- [x] 4.3 前端验证：顶层（无 parent_tool_call_id）retry marker 在 blowball-frontend 的渲染路径通用性确认（只读验证；如有样式问题在该仓库单独立项）

## 5. 文档

- [x] 5.1 CLAUDE.md：Agent orchestration 段落与 Important conventions 补记 `llm-round-retry`（重试单元、默认值、两级共用 retry 块、预算同池、乘法上界公式）
