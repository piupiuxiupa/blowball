# Delta: message-write-behind

## MODIFIED Requirements

### Requirement: Redis 入库队列与读缓存双写

一个 turn 的消息 SHALL 按以下批次双写，每批在单个 Redis pipeline 中完成：①**发送时批次**（session-run claim 成功后、orchestrator 启动前）仅含用户消息行（`client_msg_id = {trace_id}:0`，msg_index=0）；②**增量闭合批次**（turn 进行中，周期默认 1s）：仅含已闭合的 merged 事件前缀（截至快照点 `len(merged)-1` 条；最后一条 merged 事件可能继续生长，SHALL NOT 提前写入），按 merged 序数续接 msg_index 与确定性 `client_msg_id`，与终局视图逐事件一致；③**mid-turn 压缩批次**（上下文压缩触发时，语义不变，与增量批次经共享游标互斥串行）；④**终局批次**（orchestrator 结束后，成功/失败/取消路径均执行）含全部剩余事件（未闭合尾条与增量未落部分）。每批双写 `RPUSH msgs:{session_id}`（读热层，24h TTL）与 `RPUSH msgs:buffer`（全局 FIFO，无 TTL），元素为同一份消息规范 JSON。**不变量：该 turn 的用户消息行在两个 key 中各恰好出现一次**——后续批次 SHALL NOT 重复包含已持久化的用户消息行；任意两个批次的 merged 序数区间 SHALL NOT 重叠（`msgs:{session_id}` 无去重，重叠即重复行）。`msgs:buffer` SHALL NOT 设置 TTL。增量/压缩批次 SHALL NOT 同步等待 MySQL；终局批次路径维持 harden-turn-persistence 的 drain-before-release。增量批次失败 SHALL 仅告警并跳过（不推进游标），由下一周期或终局批次兜底，SHALL NOT 阻塞或中断 turn。

#### Scenario: 发送时批次先入队

- **WHEN** 一条用户消息请求通过 session-run claim，turn 尚未启动
- **THEN** 用户消息行作为独立批次在同一 Redis pipeline 中双写两个 key，元素为该消息 JSON
- **AND** 该批次不触发任何同步 MySQL 写入

#### Scenario: 闭合事件增量落库

- **WHEN** turn 进行中，事件流已形成 ≥2 个 merged 事件且增量周期到达
- **THEN** 前 `len(merged)-1` 个已闭合事件（扣除已持久化游标）作为增量批次双写，msg_index 与 client_msg_id 续接
- **AND** 最后一条（可能继续生长的 token/reasoning 段）不被写入

#### Scenario: 增量批次失败不阻塞不丢段

- **WHEN** 增量批次 SaveMessagesBatch 失败
- **THEN** 仅记录 WARN，游标不推进，生成不受影响
- **AND** 下一周期重试同一区间；turn 终局批次最终保证该区间落库

#### Scenario: 终局批次为剩余后缀

- **WHEN** turn 结束（成功、失败或取消），终局批次持久化
- **THEN** 批次仅含游标之后的剩余事件（含最后一条与增量漏写区间），用户消息行仅在发送时批次失败时包含
- **AND** `msgs:{session_id}` 与 `msgs:buffer` 中该 turn 的用户消息行各自仍恰好出现一次，全 turn 各行恰好出现一次

#### Scenario: 发送时批次失败由首个后续批次兜底

- **WHEN** 发送时批次因错误未能写入（含双写与降级直写均失败）
- **THEN** `SaveMessagesBatch` 返回错误，发送时路径不标记用户行已持久化
- **AND** 首个覆盖序数 0 的后续批次（增量或终局）包含用户消息行与对应事件
- **AND** flusher 的幂等 INSERT（`uk_messages_client_msg_id`）使 MySQL 不产生重复行

#### Scenario: 入库队列无 TTL

- **WHEN** 消息进入 `msgs:buffer`
- **THEN** 该 key 不携带过期时间，其生命周期仅由 flusher 消费与 operator 介入界定

#### Scenario: Redis 写失败降级直写 MySQL

- **WHEN** 双写 pipeline 因 Redis 不可用而失败，且降级直写 MySQL 成功
- **THEN** 系统 SHALL 降级为同步直写 MySQL（幂等 INSERT），记录错误日志，不丢弃该批消息
- **AND** `SaveMessagesBatch` 返回成功，不向调用方放大 Redis 抖动

#### Scenario: 双层失败上抛错误

- **WHEN** 双写 pipeline 失败且降级直写 MySQL 同样失败
- **THEN** `SaveMessagesBatch` SHALL 返回携带失败原因的错误，并记录包含批次规模的 ERROR 日志
- **AND** 该批消息不被标记为已持久化，调用方可据此重试或兜底
