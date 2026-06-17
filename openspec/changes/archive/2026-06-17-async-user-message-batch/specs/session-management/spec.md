## MODIFIED Requirements

### Requirement: Three-layer message storage

消息 SHALL 按顺序写入三层存储：Redis 缓存 → 用户目录文件 → MySQL。同一 turn 内，用户消息与 assistant 事件在 orchestrator 成功完成后通过同一个 goroutine 作为一批消息写入三层存储；orchestrator 失败时该 turn 的全部消息均不写入。

#### Scenario: Batch write user and assistant messages after success

- **WHEN** orchestrator 成功完成一次 turn
- **THEN** 系统 SHALL 通过 goroutine 将用户消息与全部 assistant 事件作为一批消息写入三层存储（Redis 批量 RPUSH、FS 一次 read-modify-write 追加、MySQL 一条多 VALUES INSERT）
- **AND THEN** batch 内消息顺序为 user message（msg_index=0）后接 assistant 事件（msg_index 从 1 严格单调递增）

#### Scenario: Skip persistence on orchestrator failure

- **WHEN** orchestrator 返回错误
- **THEN** 系统 SHALL 丢弃本次收集的所有 assistant 事件以及该 turn 的用户消息，不写入任何存储层

#### Scenario: Redis write failure

- **WHEN** Redis 不可用
- **THEN** 系统跳过 Redis 直接写文件和 MySQL，记录错误日志，不阻塞响应

#### Scenario: Goroutine persistence independent of request context

- **WHEN** 客户端在 SSE 流结束前断开连接（取消请求 ctx）
- **THEN** 批量入库 goroutine SHALL 使用派生自 `context.Background()` 的 detached ctx 继续完成写入（保留 trace_id），写入内容包含该 turn 的用户消息与 assistant 事件

#### Scenario: Batch write failure after SSE completes

- **WHEN** 异步批量写入发生 FS 或 MySQL 错误
- **THEN** 系统记录错误日志，该 turn 的用户消息与 assistant 事件均不保证持久化；客户端已收到的 SSE 响应不受影响
