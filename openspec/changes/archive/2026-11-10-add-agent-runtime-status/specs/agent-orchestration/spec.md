## ADDED Requirements

### Requirement: Proactive tool-round budget guidance before round-cap termination
Confucius 与通用 SubAgent 的 tool-calling 循环 SHALL 在工具派发后的后续模型请求前提供基于该 agent 自身 `max_rounds` 的已用/剩余轮次提示，使模型能够在上限接近时主动综合结果。提示 SHALL 使用 `agent-runtime-status` 能力的独立、不可变、append-only cache-only 状态消息，且 SHALL NOT 改变既有 `max_rounds` 的轮次语义、并行调用计数、重试/续写计数或 tool-disabled wrap-up 契约。

#### Scenario: 工具轮次后提供剩余预算
- **WHEN** 任一 Confucius 或 SubAgent 完成一个工具派发轮次并继续请求模型
- **THEN** 新的最新状态携带该实例的 `rounds_used`、`max_rounds` 与 `rounds_remaining`
- **AND THEN** 同一响应中的并行 tool calls 仍只计为一个工具轮次，旧状态保持不变

#### Scenario: 达到上限后的行为保持兼容
- **WHEN** 倒计时达到 `rounds_remaining=0`
- **THEN** 系统按既有「Agent tool-calling loop round cap and graceful termination」执行一次 tool-disabled wrap-up
- **AND THEN** 不新增 tool-enabled 轮次、单个工具调用硬上限或额外 cap 事件

#### Scenario: 重试与 length continuation 不改变轮次提示口径
- **WHEN** 一个工具轮次内部发生瞬时重试或 length continuation
- **THEN** 倒计时不把新尝试计为新的 `max_rounds` 轮次
- **AND THEN** 瞬时重试复用 byte-identical 请求且不追加状态，续写的新逻辑请求只在既有 transcript 后追加当前轮次的最新状态
