## Context

`internal/handler/session.go:SendMessage` 当前在启动 orchestrator 之前先同步调用 `SessionService.SaveMessage` 写入用户消息，目的是保证用户输入在流式响应开始前就已经落盘。assistant 事件则是在 orchestrator 成功后通过 goroutine 批量异步写入。两条写入路径并存：

- 用户消息：同步单条 batch（`SaveMessage` → `SaveMessagesBatch` 单元素切片）
- assistant 事件：异步批量 batch（`SaveMessagesBatch` 多元素切片）

这导致每次 turn 产生两次 `AppendMessages` 调用、两次 FS read-modify-write，并在首 token 前引入一次完整的三层存储往返。

## Goals / Non-Goals

**Goals:**
- 降低首 token 到达延迟：移除 orchestrator 前的同步写屏障
- 统一 persistence 路径：每 turn 只触发一次 `SaveMessagesBatch`
- 保持消息模型不变：用户消息仍然占用 `msg_index=0`，assistant 事件从 `1` 递增

**Non-Goals:**
- 不改 HTTP/SSE 协议、不改消息表 schema、不改 `model.Message` 字段
- 不解决并发 turn 的既有 race（当前代码已存在 FS RMW、Redis SetMessages 覆盖等问题）
- 不引入 session-level 锁或排队机制
- 不改变 `RecoverMessages` 的读取优先级与排序规则

## Decisions

### Decision 1: 用户消息与 assistant 事件放入同一个异步 batch

**选择**：在 `SendMessage` 中删除同步 `SaveMessage` 调用；在 orchestrator 成功后，把用户消息 prepend 到 assistant 事件列表，统一调用 `SaveMessagesBatch`。

**备选 A**：用户消息仍同步写 Redis，FS+MySQL 留给异步 batch。否决：路径未统一，且 Redis 失效时仍需同步全量写回退。

**备选 B**：用户消息单独一个异步 goroutine，assistant 事件另一个。否决：无法保证 batch 内 `msg_index` 顺序，且两个 goroutine 都会竞争 FS RMW。

### Decision 2: 用户消息时间戳仍采用请求到达时的时间

**选择**：在 `SendMessage` 入口处捕获 `userMsgTime := time.Now().UTC()`；异步 goroutine 中 assistant 事件使用 goroutine 启动时间 `now`。用户消息早于 assistant 事件，保留 `(msg_time, msg_index)` 的 turn 内排序语义。

**备选**：batch 内所有行共享同一个 `now`。否决：会让用户消息与第一个 assistant token 时间相同，依赖 `msg_index` 区分，语义上弱于当前设计。

### Decision 3: orchestrator 失败时丢弃整 turn 消息

**选择**：保持现有 `res.err != nil` 时直接 return 的行为，异步 goroutine 不触发。用户消息因此也随 assistant 事件一起丢弃。

**备选**：失败时仍异步写入仅包含 user message 的 batch。否决：用户明确要求「跟 assistant 事件同一个 batch」，即接受当前 success-only 语义。

### Decision 4: `MessageFromEvent` 不扩展用户消息，新增独立 helper

**选择**：在 `event_mapper.go` 新增 `UserMessage(sessionID, traceID, content string, msgTime time.Time) model.Message`，返回构造好的用户消息模型，保持 `MessageFromEvent` 只处理 `StreamEvent`。

**备选**：把用户消息包装成伪 `StreamEvent{Type: EventMessage}` 再走 `MessageFromEvent`。否决：`StreamEvent` 是 SSE 与 agent 之间的协议原语，混入 persistence-only 类型会污染其语义。

### Decision 5: `SaveMessagesBatch` 行为不变

**选择**：`SessionService.SaveMessagesBatch` 与 `appendToFS` 保持现有错误策略：Redis 尽力、FS 错误返回、MySQL 错误吞掉。batch 内消息按切片顺序写入。

**备选**：为 user message 添加更强保证（如重试）。否决：超出本变更范围，且与「统一路径」目标冲突。

## Risks / Trade-offs

- **[Risk] 崩溃/断连导致用户消息丢失** → 已在 Decision 3 接受。进程在 orchestrator 启动后、异步 batch 执行前崩溃，或客户端取消后 handler 提前 return，均会导致该 turn 用户消息未落盘。
- **[Risk] 异步 batch 失败（FS/MySQL）导致整 turn 丢失** → 客户端已收到 200 与完整 SSE，但后端未持久化，用户无重试信号。
- **[Risk] 同 session 并发 turn 的可见性 race 加剧** → 当前代码已存在。用户消息异步后，第二个请求的 `RecoverMessages` 可能看不到第一个请求的用户消息。缓解：本次变更不引入锁，后续如需可单独加 session-level 串行化。
- **[Risk] 测试断言大量调整** → `session_test.go` 与 `message_flow_test.go` 中 batch 调用次数、batch 长度、失败场景期望均需更新。
- **[Trade-off] 用 durability 换 latency 和统一性** → 明确接受。

## Migration Plan

1. 部署新代码（schema 无需变更）。
2. 老数据兼容：历史消息仍按原规则读取，新写入消息与旧消息在 `(msg_time, msg_index)` 排序下共存。
3. 回滚：直接回滚代码即可恢复同步用户消息写入；无需回滚 schema。

## Open Questions

- 是否需要为异步 batch 增加重试或死信机制？当前设计按现有 `SaveMessagesBatch` 错误策略直接吞掉 MySQL 错误。
- 是否需要监控指标（如 batch 失败率、batch 写入延迟）来观察 durability 损失？
