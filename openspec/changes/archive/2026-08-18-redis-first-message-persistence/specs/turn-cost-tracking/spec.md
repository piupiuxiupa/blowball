## RENAMED Requirements

- FROM: `### Requirement: Usage written in same transaction as message batch`
- TO: `### Requirement: Usage written independently of message persistence`

## MODIFIED Requirements

### Requirement: Usage written independently of message persistence

`turn_usage` 的写入 SHALL 作为独立调用执行，SHALL NOT 与消息持久化共事务（消息批次自 Redis-first 改造后不再存在同步 MySQL 事务；即便消息仍处于入库队列中，usage 行也可独立先行落地）。usage 写入失败 SHALL NOT 影响消息持久化（usage 是观测数据，消息是业务数据，优先级不同）。

#### Scenario: Usage written as an independent call

- **WHEN** 一轮 turn 的消息批次完成 Redis 双写
- **THEN** `turn_usage` 行通过独立的 `SaveTurnUsage` 调用写入 MySQL，其时序与消息 flusher 的异步入库互不依赖（usage 行可能先于消息行落地）

#### Scenario: Usage write failure does not roll back messages

- **WHEN** `turn_usage` 插入失败（如临时 DB 错误）
- **THEN** 系统记录错误日志但不影响消息批次的队列写入与后续 flush
- **AND THEN** SSE 响应与消息持久化不受影响（usage 是观测数据，消息是业务数据，优先级不同）
