# Proposal: session-message-queue

## Why

用户在一个会话里连续追问时，运行中 turn 的单槽互斥把新 POST 直接挡在 409 `SESSION_BUSY`，唯一出路是取消当前轮（丢弃已生成的部分输出）或干等到终局。这让"发送"与"轮次执行"强行耦合：用户想起一个补充说明、改个需求方向，都必须先等或先放弃。排队把两者解耦——消息先入队，上一轮终局后自动接续成下一轮，对齐主流对话产品的连续追问体验。

现有架构已为此付过钱：终局批次先于 claim 释放（harden-turn-persistence D2 ⇒「claim 空闲 = 历史完整」）、SSE 与 turn 解耦（resume 循环天然支持长等待）、run 状态机跨进程（Redis）。本变更本质是把 claim 单槽原地扩展为 claim+queue，而非叠加外挂。

## What Changes

- **会话级 FIFO 消息队列**（Redis LIST `queue:{sid}`，每条目为已解析的请求快照：content、model/effort selection、附件引用、user_id、入队时间、run_id=trace_id）。`messages.queue.enabled: true` 时，运行中会话的 POST 不再 409，而是入队并在上一轮终局后自动接续。
- **FIFO 原子性**：admission（POST 路径）与 pump（终局路径）各一个 Lua 脚本；不变量为「queue 非空 ⇒ 只能 RPUSH；claim 空闲且 queue 为空 ⇒ 才允许直接认领」。新消息无法插队，pump 弹头-认领原子完成。
- **POST 响应形态不变**：仍是 SSE 长连接 + `X-Run-Id`；排队消息的流上先发新事件类型 `queued`，随后沿用现有 replay+tail 订阅循环空转等待，pump 起轮后事件自然续上。客户端断开/重连沿用 resume 端点语义。
- **新 run 状态 `queued`**：`run:{rid}:meta.status = queued` 直到 pump 起轮置 `running`；`CancelTurn` 增加 queued 分支（出队 + `cancelled`），取消运行中轮后队列继续接续。
- **接续驱动**：turn goroutine 在 `Finalize` 之后同步 pump 队头；后台 reconciler（agent 角色，模式照抄 msgflush）兜底崩溃残留（claim 过期后弹头续跑）并为 queued run 的 run-key 家族续 TTL。优雅停机跳过 pump，条目留给重启后的 reconciler。
- **用户行落库时机**：排队消息的用户消息行在 **pump 起轮时**（认领之后、orchestrator 之前）写入——保持 send-time persist 的「写在 turn 全部历史读之后」不变量，并规避 mid-turn compaction `RecoverDurable` 把排队消息捞进运行中 turn 上下文的泄漏。等待期的消息不在 `GET /messages`，由新队列读 API 呈现。
- **队列读 API** `GET /api/v1/sessions/:session_id/queue`（前端刷新后恢复 pending 气泡的权威渠道）；session 列表/详情 additive 增补 `queued_count`。
- **深度上限**：`messages.queue.max_depth`（默认 8），满时 409 `QUEUE_FULL`；输入校验（1MB body、token 估算、model/effort 解析、附件校验）全部在入队前完成，即时 400。
- **配置与回退**：`messages.queue: {enabled: false, max_depth: 8}` 默认关——关闭时 409 行为 byte-for-byte 不变，回退 = 关开关（zero-behavior-change 约定）。
- **每条消息一轮**：接续轮与今天的 POST 轮共用同一条"起轮"路径——title cadence 照算（pump 时触发）、memory recall 在 pump 时查询（此时上一轮答案已在历史）、turn-start 压缩检查、`turn_usage`、独立 model/effort 全部照旧。
- **会话删除**：`DeleteSession` 顺带清除 `queue:{sid}`。

## Capabilities

### New Capabilities

- `session-message-queue`: 会话内消息排队与自动接续——admission/pump FIFO 语义、队列存储与条目快照、queued run 状态与 `queued` SSE 事件、接续驱动（pump + reconciler）、排队消息的落库时机、深度上限与 `QUEUE_FULL`、队列读 API、`messages.queue` 配置与回退、崩溃/停机/删除语义。

### Modified Capabilities

- `turn-run-lifecycle`: 「Session 单活动 run 互斥」在队列启用时 busy 认领失败不再 409 而是入队（claim 单槽不变量保留，admission 语义改变）；「按 run id 取消」覆盖 queued run（出队取消，且不再被 dead-run force-clear 误判为 interrupted）；终局序列在释放认领后增加 pump 步骤；崩溃/优雅关闭语义补排队条目的存续与 reconciler 恢复。
- `session-management`: 「Send message and stream response」的 busy 路径在队列启用时改为入队 + `queued` 事件流（禁用时不变）；「Session list / Session detail」additive 携带 `queued_count`。

## Impact

- **`internal/run`**: `Store` 接口新增队列原语（admit/pump/dequeue/queue 长度与续期）；`StatusQueued` 状态常量；`MemStore` 同步实现（测试就绪）。
- **`internal/store/redis`**: `runstore`/新文件实现队列 Lua 脚本与 LIST 操作。
- **`internal/handler`**: `SendMessage` 拆出「起轮核心」（HTTP 路径与 pump 路径共用）；claim 失败分支改入队；turn goroutine 尾部 pump；`CancelTurn` queued 分支；队列读 API；reconciler goroutine。
- **`internal/stream`**: 新事件类型 `queued`。
- **`internal/config`**: `messages.queue` 配置块（校验：max_depth 正整数、负数拒绝）。
- **`internal/service`**: `DeleteSession` 清 `queue:{sid}`。
- **`api/openapi.yaml`**: `queued` 事件、queue 读端点、`QUEUE_FULL` 错误、cancel 语义扩展、`queued_count` 字段；需同步 `blowball-frontend` 重新 generate。
- **无 migration**：队列完全在 Redis，不新增 MySQL 表。
- **测试**：`internal/run`（MemStore 队列原语）、`internal/handler`（admission/入队/pump 接续/取消/深度/停机）、`test/integration`（端到端排队接续、多副本语义用 MemStore 模拟）。
