## ADDED Requirements

### Requirement: Redis 入库队列与读缓存双写

每轮 turn 的消息批次 SHALL 在单个 Redis pipeline 中双写：`RPUSH msgs:{session_id}`（读热层，保留 24h TTL）与 `RPUSH msgs:buffer`（全局 FIFO 入库队列）。`msgs:buffer` SHALL NOT 设置 TTL——队列未清空前过期即意味着消息丢失。两处写入的元素为同一份消息规范 JSON。该批次 SHALL NOT 同步写入 MySQL，也 SHALL NOT 写任何文件系统会话文件。

#### Scenario: turn 成功后双写两个 key

- **WHEN** 一轮 turn 的消息批次（用户消息 + assistant 事件）完成持久化
- **THEN** 系统通过一个 Redis pipeline 同时 `RPUSH msgs:{session_id}`（刷新 TTL）与 `RPUSH msgs:buffer`，元素为同一份消息 JSON
- **AND THEN** 该批次不触发任何同步 MySQL 写入

#### Scenario: 入库队列无 TTL

- **WHEN** 消息进入 `msgs:buffer`
- **THEN** 该 key 不携带过期时间，其生命周期仅由 flusher 消费与 operator 介入界定

#### Scenario: Redis 写失败降级直写 MySQL

- **WHEN** 双写 pipeline 因 Redis 不可用而失败
- **THEN** 系统 SHALL 降级为同步直写 MySQL（幂等 INSERT），记录错误日志，不丢弃该批消息

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
