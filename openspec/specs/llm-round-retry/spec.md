# llm-round-retry Specification

## Purpose

轮次级瞬时 LLM 重试：重试单位是 agent 轮次内部的单次 `StreamChat` 流式调用（`runLLMRound` 里的 `streamChatWithRetry`），覆盖 Confucius/Chongzhi/Liang 三个主循环与 round-cap 后的 tool-disabled wrap-up 轮（标题生成与 compaction 摘要不覆盖）。瞬态失败（`isTransientError` 判定：429/5xx/网络抖动/流中断/watchdog 空闲超时）以逐字节相同的请求重发——Messages 与该 attempt 的 `MaxCompletionTokens` 均不变，不重派工具、不追加 continuation 脚手架，重试计数与续写计数相互独立；上下文取消与语义错误绝不重试，取消判定优先于一切。失败 attempt 的报出 usage 折入真实花费（`total`/`by_agent`/`turn_usage`），其部分内容被丢弃。轮次级与派发级重试共用同一 turn 级 `retryBudget`，由 `agents.confucius.retry.budget_tokens` 配置。三个 agent 默认 `enabled: true`、`max_attempts: 3`，退避复用既有指数退避默认。

## Requirements

### Requirement: Transient retry of individual LLM streaming calls in agent rounds
系统 SHALL 在 `runLLMRound` 内部对单次 `StreamChat` 流式调用执行瞬时错误重试：所有 agent（Confucius/Chongzhi/Liang）的主循环与 round-cap 后的 wrap-up 轮均受覆盖。重试触发条件 SHALL 为：错误经 `isTransientError` 判定为瞬时（LLM 429/5xx/超时/网络抖动/流中断/watchdog 空闲超时）、未达 `max_attempts` 上限、且 turn 级重试预算允许。上下文取消（`ctx.Err() != nil`）与语义错误（`bad_args`、`unknown_tool`、length 耗尽等非瞬时错误）SHALL 绝不重试，取消判定 SHALL 优先于一切重试判定。

#### Scenario: Mid-stream transient failure retried
- **WHEN** 某 agent 轮次的 LLM 流在中途因 429/网络错误/EOF 断流（流已开始，SDK 不再兜底）
- **THEN** 系统发射 `agent_error` 事件（code `retry`、`Meta.retry=true`、携带触发错误）后按指数退避等待，再重发该次调用
- **AND THEN** 重试成功后本轮正常继续，turn 不失败

#### Scenario: Idle-watchdog timeout retried
- **WHEN** 流被空闲 watchdog 掐断（`ErrStreamIdleTimeout`，错误文案含 "timeout"）
- **THEN** 该错误被分类为瞬时并按上述机制重试，每次重试 attempt 重新起表 watchdog

#### Scenario: Cancellation never retried
- **WHEN** 失败源于父上下文取消（用户取消、graceful shutdown、hub 关闭导致的 onToken 返回 ctx 错误）
- **THEN** 系统直接返回取消错误，不发射 retry 事件、不等待退避

#### Scenario: Non-transient error fails fast
- **WHEN** 错误不匹配任何瞬时信号子串（如 length 耗尽的刻意文案、请求参数被网关拒绝）
- **THEN** 系统立即走既有失败路径，不重试

#### Scenario: Attempts exhausted surfaces the error
- **WHEN** 重试次数达到 `max_attempts` 仍失败
- **THEN** 最后一次错误按既有路径浮出（Confucius 为 `agent_error(llm_error)` + turn 失败；子代理为错误 toolResult 喂回父级）

### Requirement: Retry re-issues a byte-identical request with no side effects
轮次级重试 SHALL 以逐字节相同的请求重发：Messages 与该 attempt 的 `MaxCompletionTokens` 均不变。重试 SHALL NOT 重新派发工具、SHALL NOT 重新追加 continuation 脚手架（脚手架仅在成功且 `finish_reason=length` 的路径上追加）。瞬态重试与 length continuation 的次数 SHALL 相互独立：重试不消耗 continuation 预算，continuation attempt 也不消耗重试次数。

#### Scenario: Continuation mid-sequence transient failure
- **WHEN** 一次已扩张预算的 continuation attempt 瞬断
- **THEN** 重试以当次已扩张的预算原样重发，continuation 计数不变
- **AND THEN** 后续若再遇 `finish_reason=length` 仍按剩余 continuation 预算继续

#### Scenario: No duplicate scaffolding on retry
- **WHEN** 一次瞬态失败被重试
- **THEN** 对话切片（`round`）不因重试发生任何追加，重发的请求与首次逐字节一致

### Requirement: Real-spend usage accounting for retried attempts
瞬态失败的 attempt 中网关报出的 usage（`resp.Usage`，可能为 0）SHALL 累加进该轮的 usage 汇总（`roundResult.Usage` → `total`/`by_agent`/`turn_usage`）；其部分输出内容 SHALL NOT 并入轮内容（重试的完整输出取代）。`context_tokens` SHALL 维持只取最终成功 attempt 的 usage。

#### Scenario: Failed attempt usage folded into turn usage
- **WHEN** 一次失败 attempt 网关报出了非零 usage，随后重试成功
- **THEN** done 事件的 usage 与 `turn_usage` 包含失败 attempt 与成功 attempt 的 token 总和
- **AND THEN** 最终内容仅含成功 attempt 的输出

### Requirement: Turn-wide shared retry budget
轮次级重试与派发级重试 SHALL 共用同一 turn 级 `retryBudget`，预算上限取 `agents.confucius.retry.budget_tokens`（0 = 不限额，语义由"子代理重试预算"扩展为"全 turn 重试预算"）。预算 SHALL 经 turn 上下文注入并向下传播至子代理轮次；每次瞬态失败 attempt 的报出 usage SHALL 在重试决策前计入预算；预算耗尽 SHALL 阻止该 turn 内任何 agent 的后续重试（轮次级与派发级一致）。

#### Scenario: Budget shared across agents in one turn
- **WHEN** Confucius 自身的轮次重试与某子代理的轮次重试消耗同一预算池至耗尽
- **THEN** 该 turn 内后续任何瞬时失败（无论哪个 agent、哪一级）不再重试，错误直接浮出

### Requirement: Default-enabled policy for every agent
`agents.<name>.retry` 配置块 SHALL 统一治理该 agent 的轮次级与派发级瞬时重试；三个 agent SHALL 默认 `enabled: true`、`max_attempts: 3`（1 次原始 + 2 次重试）、退避复用既有指数退避默认（500ms 起、4s 封顶）。显式配置 SHALL 覆盖默认（如显式 `enabled: false` 关闭该 agent 的两级重试）。

#### Scenario: Zero-value retry blocks get retry for all agents
- **WHEN** 配置未显式设置任何 `retry` 块
- **THEN** Confucius、Chongzhi、Liang 的轮次级重试均默认启用且 max_attempts 为 3

#### Scenario: Explicit disable honored
- **WHEN** 某 agent 显式配置 `retry: {enabled: false}`
- **THEN** 该 agent 的轮次级与派发级重试均关闭，行为等同变更前的关闭语义

#### Scenario: Wrap-up round inherits the agent policy
- **WHEN** 某 agent 触发 round-cap 后进入 tool-disabled wrap-up 轮且其 LLM 调用瞬断
- **THEN** wrap-up 轮按该 agent 的同一 retry 策略重试
