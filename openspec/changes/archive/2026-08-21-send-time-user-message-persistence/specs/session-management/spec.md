# Delta: session-management

## MODIFIED Requirements

### Requirement: Redis-first message storage

消息 SHALL 按顺序写入 Redis 的两类 key：`msgs:{session_id}`（读热层，24h TTL）与 `msgs:buffer`（入库队列，无 TTL），二者在同一 pipeline 中批量 RPUSH。**用户消息 SHALL 在 session-run claim 成功后、orchestrator 启动前，作为独立批次先行双写**（`client_msg_id = {trace_id}:0`，msg_index=0，msg_time=请求到达时刻）；**assistant 事件在 orchestrator 结束后作为后缀批次双写**（含 mid-turn 上下文压缩触发的中间批次，见 context-compaction 能力），后续批次 SHALL NOT 再次包含已先行持久化的用户消息行——`msgs:{session_id}` list 无去重语义，该 turn 的用户消息行 SHALL 恰好 append 一次。先行写入返回错误时，turn 结束批次 SHALL 照旧包含用户消息行（确定性幂等键使 MySQL 不产生重复行）。MySQL 写入由后台 flusher 异步批量完成（见 message-write-behind 能力）；系统 SHALL NOT 再写任何文件系统会话文件。orchestrator 失败（含客户端中断）时，已产生的部分事件 SHALL 照常持久化（用户消息此时已先行持久化；与 interrupted-turn-persistence 能力一致）。

#### Scenario: User message persisted at send time

- **WHEN** 用户发送消息且 session-run claim 成功，turn 开始执行
- **THEN** 系统 SHALL 在 orchestrator 启动前将用户消息行（含确定性 `client_msg_id = {trace_id}:0`，msg_index=0）通过同一 Redis 双写路径写入（`RPUSH msgs:{session_id}` 刷新 TTL + `RPUSH msgs:buffer`），元素为同一份消息 JSON
- **AND** 该行在 turn 运行全程对消息恢复可见；MySQL 侧于 ≤ `messages.flush_interval` 窗口内可读

#### Scenario: Turn-end batch is an assistant-event suffix

- **WHEN** orchestrator 完成（成功、失败或取消），turn 结束批次持久化
- **THEN** 批次 SHALL 仅含 assistant 事件（msg_index 从已持久化位置严格单调续接），SHALL NOT 再次包含用户消息行
- **AND** `msgs:{session_id}` 中该 turn 的用户消息行恰好出现一次

#### Scenario: Send-time write failure falls back to turn-end batch

- **WHEN** 发送时的用户消息批次写入返回错误
- **THEN** turn 结束批次 SHALL 照旧包含用户消息行
- **AND** 确定性幂等键（`uk_messages_client_msg_id`）使 MySQL 侧不产生重复行

#### Scenario: Rejected requests write nothing

- **WHEN** 请求因校验失败（400）、会话不存在（404）或已有运行中 turn（409 `SESSION_BUSY`）被拒绝
- **THEN** 系统 SHALL NOT 为该请求写入任何消息行

#### Scenario: Persist partial turn on orchestrator failure

- **WHEN** orchestrator 返回错误（含上游 429/5xx、超时、取消）
- **THEN** 系统 SHALL 照常持久化已收集的部分 assistant 事件（同一 Redis 双写路径），使重载的会话历史与用户实际看到的内容一致
- **AND** 该 turn 的用户消息行已在发送时持久化，不受影响

#### Scenario: Mid-turn visibility after refresh

- **WHEN** turn 运行中用户刷新页面并重进该会话，前端读取历史并 attach 事件流
- **THEN** 历史读取 SHALL 包含该 turn 的用户消息行（发送时已持久化）
- **AND** 事件重放照旧只含 agent 事件（用户消息行不进事件日志），二者共同构成完整视图

#### Scenario: Redis write failure

- **WHEN** Redis 双写 pipeline 失败（Redis 不可用）
- **THEN** 系统 SHALL 降级为同步直写 MySQL（幂等 INSERT），记录错误日志，不丢弃该批消息，不阻塞响应

#### Scenario: Goroutine persistence independent of request context

- **WHEN** 客户端在 SSE 流结束前断开连接（取消请求 ctx）
- **THEN** 批量入库 goroutine SHALL 使用派生自 `context.Background()` 的 detached ctx 继续完成写入（保留 trace_id），写入内容包含该 turn 的 assistant 事件
