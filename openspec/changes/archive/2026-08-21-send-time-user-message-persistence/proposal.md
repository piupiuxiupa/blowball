# Proposal: send-time-user-message-persistence

## Why

现状用户消息与 assistant 事件一起，在 turn 终局才由 `persistEvents` 落库（`internal/handler/message_stream.go`，在 `res := <-resultCh` 之后）。turn 运行全程，这条用户消息不存在于任何可读位置：不在 Redis 读缓存 `msgs:{sid}`、不在入库队列 `msgs:buffer`、不在 MySQL、也不在 run 事件日志（`EventMessage` 是纯持久化 sentinel，从不上 SSE）。由此产生三个问题：

1. **刷新重进运行中会话看不到自己发的消息**：历史端点（`GET /messages`，直读 MySQL）没有该行，attach 重放（`GET /turns/:rid/events`）只有 agent 事件——用户看得到 agent 打字，看不到自己的提问，直到 turn 结束。
2. **硬崩溃丢用户消息**：graceful shutdown 会 cancel 并走 persistEvents，但进程硬崩（kill -9 / panic）时用户消息彻底丢失，连落库机会都没有——违反 "messages always win" 的精神（interrupted-turn-persistence spec 明文接受这一现状："no message rows are persisted beyond what earlier mid-turn flushes wrote"）。
3. **可见性行为不一致**：mid-turn compaction 的 flush-first 路径会把 user row 中途落库——长会话（上下文压力过阈值、触发过 compaction）刷新后能看到用户消息，短会话看不到。纯副作用，非设计。

这是 `2026-06-17-async-user-message-batch` 的既有 trade-off（该 proposal 的 Impact 明言"客户端断连后若 batch 未执行则用户消息可能丢失"）：当时为砍首 token 延迟，把用户消息从"orchestrator 启动前同步写"移进 turn 结束批次。后来 turn-detach-resume 补上了 agent 侧的事件重放，用户消息侧的窗口一直没补。

## What Changes

- **发送时持久化 user row**：`POST /messages` 在 session-run claim 成功、标题触发、run meta 初始化之后，orchestrator goroutine 启动之前，**同步**把该 turn 的用户消息行（`client_msg_id = {trace_id}:0`，msg_index=0，msg_time=请求到达时刻）通过既有 `SaveMessagesBatch` 写入 Redis 双写路径（`RPUSH msgs:{sid}` + `RPUSH msgs:buffer`）。此后 turn 任意时刻该行对历史读取可见（MySQL 侧 ≤ `flush_interval` 窗口）。
- **后续批次跳过 user row**：turn 终局保存与 mid-turn compaction flush 渲染批次时 SHALL NOT 再次包含已发送时持久化的 user row——Redis `msgs:{sid}` list 没有去重语义，重复 append 会让历史读到重复行（MySQL 侧 UNIQUE 索引只是兜底）。以独立于 `flushed` 计数的 `userPersisted` 标志控制；`buildTurnMessages` 的 user-row 包含条件从 `from == 0` 改为 `from == 0 && !userPersisted`。
- **失败语义**：标志仅在 `SaveMessagesBatch` 返回 nil 时置位；返回 error（幂等键预盖确定性 id 后实际不可达：mint 不触发、marshal 纯结构体）时不置位，turn 终局批次照旧包含 user row（at-least-once，确定性 id 在 MySQL 收敛）。Redis 双写失败沿用既有降级（内部同步直写 MySQL，返回 nil）——两层全挂的 lost-and-logged 与今天终局路径同约定，不引入新行为。
- **不变项**：assistant 事件仍在 turn 终局批量持久化（6/17 延迟优化的核心成果保留——本变更只把用户消息这一行提前，代价是 claim 后一次 Redis pipeline，ms 级）；SSE 不新增 `message` 事件（sentinel 约定保持，可见性走存储侧而非事件日志）；无 API / schema / 前端变更，无 DB 迁移。

## Capabilities

### New Capabilities

（无。）

### Modified Capabilities

- `session-management`："Redis-first message storage"——用户消息从"turn 结束与 assistant 事件同批写入"改为"claim 后发送时先行写入，assistant 事件终局后缀批次写入"；新增 mid-turn 可见与拒绝路径零写入场景。
- `message-write-behind`："Redis 入库队列与读缓存双写"——一个 turn 的写入从单批次拆为"发送时 user 批次 +（可选 mid-turn 批次）+ 终局事件后缀批次"，新增不变量：user row 在两个 key 中各恰好出现一次。
- `interrupted-turn-persistence`："Interrupted assistant turn is persisted"——进程崩溃语义修正：崩溃不再丢用户消息（发送时已持久化），assistant 事件仍仅限已 mid-turn flush 的部分。
- `turn-run-lifecycle`："恢复端点重放并续传运行中 turn 的事件流"——明确事件日志只含 agent 事件，重连侧完整视图 = 历史读取（含发送时已持久化的 user row）+ 事件重放。

## Impact

- **handler**：`internal/handler/message_stream.go`（发送时写入点；`buildTurnMessages` / `turnFlushState` 的 `userPersisted` 维度）。
- **测试**：`internal/handler/session_test.go`（batch 次数/内容期望：每 turn 两次调用）、`internal/handler/compaction_test.go`（`buildTurnMessages` 门控三态；flush 后 `msgs:{sid}` 无重复 user row）、`test/integration/message_flow_test.go`（新增 mid-turn 可见性用例：慢 LLM fake 下 turn 运行中 `GET /messages` 含 user 行、无 assistant 行）、`test/integration/message_writebehind_test.go`（批次拆分后的队列/缓存断言）。
- **前端**：零改动（乐观 UI 与 reconcile 语义不受影响——推演见 design 决策 6）。
- **文档**：CLAUDE.md 请求流段落同步（claim → title → user-row persist → orchestrate）。
- 无 DB 迁移、无 OpenAPI 变更。
