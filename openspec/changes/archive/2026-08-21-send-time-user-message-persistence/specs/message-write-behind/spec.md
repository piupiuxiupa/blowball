# Delta: message-write-behind

## MODIFIED Requirements

### Requirement: Redis 入库队列与读缓存双写

一个 turn 的消息 SHALL 拆分为至多三批双写，每批在单个 Redis pipeline 中完成：①**发送时批次**（session-run claim 成功后、orchestrator 启动前）仅含用户消息行（`client_msg_id = {trace_id}:0`，msg_index=0）；②**mid-turn 批次**（上下文压缩触发时，见 context-compaction 能力）含截至触发点的 assistant 事件；③**终局批次**（orchestrator 结束后，成功/失败/取消路径均执行）含剩余 assistant 事件。每批双写 `RPUSH msgs:{session_id}`（读热层，保留 24h TTL）与 `RPUSH msgs:buffer`（全局 FIFO 入库队列），两处写入的元素为同一份消息规范 JSON。**不变量：该 turn 的用户消息行在两个 key 中各恰好出现一次**——后续批次 SHALL NOT 重复包含已持久化的用户消息行（确定性 `client_msg_id` 仅作为 MySQL 幂等兜底，不作为 list 去重依据）。`msgs:buffer` SHALL NOT 设置 TTL——队列未清空前过期即意味着消息丢失。批次 SHALL NOT 同步写入 MySQL，也 SHALL NOT 写任何文件系统会话文件。

#### Scenario: 发送时批次先入队

- **WHEN** 一条用户消息请求通过 session-run claim，turn 尚未启动
- **THEN** 用户消息行作为独立批次在同一 Redis pipeline 中双写两个 key，元素为该消息 JSON
- **AND** 该批次不触发任何同步 MySQL 写入

#### Scenario: 终局批次为事件后缀

- **WHEN** turn 结束（成功、失败或取消），终局批次持久化
- **THEN** 批次仅含 assistant 事件，按 merged 事件序以 msg_index 从上次已持久化位置续接
- **AND** `msgs:{session_id}` 与 `msgs:buffer` 中该 turn 的用户消息行各自仍恰好出现一次

#### Scenario: 发送时批次失败由终局批次兜底

- **WHEN** 发送时批次因错误未能写入
- **THEN** 终局批次 SHALL 包含用户消息行与全部 assistant 事件
- **AND** flusher 的幂等 INSERT（`uk_messages_client_msg_id`）使 MySQL 不产生重复行

#### Scenario: 入库队列无 TTL

- **WHEN** 消息进入 `msgs:buffer`
- **THEN** 该 key 不携带过期时间，其生命周期仅由 flusher 消费与 operator 介入界定

#### Scenario: Redis 写失败降级直写 MySQL

- **WHEN** 双写 pipeline 因 Redis 不可用而失败
- **THEN** 系统 SHALL 降级为同步直写 MySQL（幂等 INSERT），记录错误日志，不丢弃该批消息
