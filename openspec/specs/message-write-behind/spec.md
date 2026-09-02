# message-write-behind Specification

## Purpose

定义消息的 Redis-first write-behind 持久化能力：Redis 入库队列（`msgs:buffer`/`msgs:processing` 双列表可靠队列语义）与后台 flusher 的分批幂等入库——LMOVE 认领、LREM 确认、失败重试不丢弃、FK 死信例外、启动回收、批量刷新 `sessions.update_time`、同步 drain 原语、配置参数与角色归属（agent/all），以及 Redis AOF 持久化的部署前置。
## Requirements
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

### Requirement: flusher 批量认领与确认

后台 flusher SHALL 通过 `LMOVE msgs:buffer → msgs:processing` 逐条原子认领记录（单轮累计不超过配置的批阈值），批量幂等插入 MySQL，插入成功后逐条 `LREM msgs:processing` 确认移除。多个进程各自运行的 flusher SHALL 能并发消费同一队列而不重复、不丢失记录。

#### Scenario: 正常批量入库

- **WHEN** `msgs:buffer` 非空且 flush 周期到达
- **THEN** flusher 认领至多批阈值条记录，一次多值幂等 INSERT 写入 messages 表，随后全部从 `msgs:processing` 移除

#### Scenario: 多进程并发安全

- **WHEN** 两个 agent 角色进程的 flusher 同时消费同一 `msgs:buffer`
- **THEN** 每条记录经由 `LMOVE` 只被一个 flusher 认领，不出现同一条记录进入两个批次或被整体遗漏

### Requirement: 入库失败重试不丢弃

flusher 的 MySQL 插入失败时，已认领记录 SHALL 留在 `msgs:processing` 中，flusher 在退避（默认 1s）后重试，SHALL NOT 丢弃或跳过。进程启动时 SHALL 将 `msgs:processing` 中的残留记录推回 `msgs:buffer` 重新投递（回收上次崩溃的 in-flight 批）。

#### Scenario: MySQL 故障时记录保留重试

- **WHEN** 批量插入因 MySQL 不可用而失败
- **THEN** 该批记录保留在 `msgs:processing`，flusher 退避后重试同一批记录，直到插入成功

#### Scenario: 崩溃后启动回收

- **WHEN** 进程在"插入已提交、LREM 未完成"或"认领后插入前"崩溃，新进程启动
- **THEN** 启动流程将 `msgs:processing` 残留推回 `msgs:buffer`，由 flusher 重新认领入库

### Requirement: 幂等插入

messages 表 SHALL 增加 `client_msg_id CHAR(36)` 列与 UNIQUE 索引（遗留行允许 NULL）。消息批次持久化时 SHALL 为每条消息 mint 一个 `client_msg_id`。flusher 与降级直写路径的 INSERT SHALL 使用 `INSERT IGNORE` 语义，使重复投递的同一条记录只落一行。

#### Scenario: 持久化时 mint 幂等键

- **WHEN** 一轮 turn 的消息批次进入 Redis 双写
- **THEN** 批内每条消息携带唯一 `client_msg_id`

#### Scenario: 重复投递不产生重复行

- **WHEN** 同一条消息记录因崩溃回收或重试被插入两次
- **THEN** 第二次插入被 UNIQUE 索引忽略，messages 表中该 `client_msg_id` 仅存在一行

#### Scenario: 遗留行不受影响

- **WHEN** migration 在既有库上为 messages 加 `client_msg_id` UNIQUE 索引
- **THEN** 遗留行的 NULL 值不违反唯一约束，旧数据读写不受影响

### Requirement: 会话已删的死信例外

对因会话已删除导致的 MySQL 外键错误（错误码 1452），flusher SHALL 将该记录从 `msgs:processing` 移除并记录 ERROR 日志（含记录摘要），SHALL NOT 无限重试。其他错误 SHALL 保持重试。

#### Scenario: FK 错误按死信丢弃

- **WHEN** 一条记录插入时命中外键错误（其 session 已被删除）
- **THEN** 该记录从 `msgs:processing` 移除并记 ERROR 日志，flusher 继续处理后续记录

#### Scenario: 非 FK 错误不丢弃

- **WHEN** 插入失败但错误码不是外键错误（如连接超时）
- **THEN** 该批记录保留重试，不进入死信路径

### Requirement: flusher 配置与触发

flusher 的行为 SHALL 可配置：`messages.flush_interval`（默认 1s）为定时触发间隔，`messages.flush_batch_size`（默认 100）为单轮认领上限与队列长度提前触发阈值。配置中的非正数值 SHALL 在加载时被拒绝。优雅停机时 SHALL 执行一次有界的最终 drain。

#### Scenario: 定时触发

- **WHEN** 到达 flush_interval 且队列非空
- **THEN** flusher 执行一轮认领-插入-确认

#### Scenario: 队列超阈值提前触发

- **WHEN** 队列长度在两次定时触发之间超过 flush_batch_size
- **THEN** flusher 提前执行一轮冲刷，不等下一个间隔

#### Scenario: 优雅停机最终 drain

- **WHEN** 进程收到停机信号
- **THEN** flusher 停止主循环后执行一次有界 final drain，超时则放弃并记 WARN

### Requirement: 会话 update_time 随批刷新

flusher 在一批插入成功后，SHALL 对批内出现的每个 distinct session_id 刷新一次 `sessions.update_time`；刷新失败 SHALL 仅记录日志，不影响消息入库与后续批次。

#### Scenario: 批量刷新 update_time

- **WHEN** 一批消息插入成功且涉及多个 session
- **THEN** 每个涉及的 session 各执行一次 update_time 刷新

#### Scenario: 刷新失败不阻断

- **WHEN** update_time 刷新失败
- **THEN** 系统记录错误日志，消息行与后续 flush 周期不受影响

### Requirement: 同步 drain 原语

系统 SHALL 提供同步 drain 能力：一次性将 `msgs:buffer` 与 `msgs:processing` 中当前全部记录认领并插入 MySQL（含重试语义），供会话删除流程与读路径 miss 场景复用。drain SHALL 有界（不无限等待）。

#### Scenario: drain 清空队列

- **WHEN** 调用同步 drain 且队列中存在记录
- **THEN** drain 返回前 buffer 与 processing 中的记录全部完成插入（或因有界超时报错）

### Requirement: flusher 角色归属

flusher SHALL 仅在 agent 与 all 角色进程中构造并启动；api 角色 SHALL NOT 构造 flusher。

#### Scenario: api 角色不运行 flusher

- **WHEN** 进程以 `--role api` 启动
- **THEN** 不存在消息 flusher goroutine，也不连接消息入库队列的消费路径

### Requirement: Redis 持久化部署前置

Redis 启用 AOF（`appendonly yes`）SHALL 作为部署要求。进程启动时 SHALL 尽力探测 Redis 的 `appendonly` 配置，探测失败或无权限时跳过，未开启时 SHALL 记录 WARN 日志（提示丢数据风险窗口），SHALL NOT 阻断启动。

#### Scenario: AOF 未开启时启动告警

- **WHEN** 启动时探测到 Redis `appendonly` 为 `no`
- **THEN** 记录一条 WARN 日志说明 flush 间隔内的消息可能因 Redis 重启丢失，进程正常启动
