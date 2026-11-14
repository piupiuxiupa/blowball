# message-write-behind Specification

## Purpose

定义消息 Redis-first write-behind 持久化能力的**退役状态**：全局入库队列（`msgs:buffer`/`msgs:processing`）与后台 flusher 已由 `refactor-message-storage` / `harden-turn-persistence` 变更移除，round 终稿改为事务直写 MySQL（持久化行为见 `message-final-persistence` 与 `message-storage-optimization`）；本能力仅保留仍存活的两个切面——确定性幂等键约束与 per-session 读缓存的 best-effort 语义。

## Requirements

### Requirement: 入库队列与后台 flusher 退役

chunk 级全局入库队列（`msgs:buffer` / `msgs:processing`）与后台 flusher SHALL 退役：round 终稿批写直接走 turn 持久化路径（确定性 ID + 重试预算，单事务跨 `messages`/`tool_calls`），SHALL NOT 再有任何生产写入路径入队。遗留的队列 key 常量与运维排空助手 SHALL 仅保留用于迁移残留的人工排空，SHALL NOT 被生产装配消费。用户消息 send-time 写与 per-session 读缓存 SHALL 保留（见下）。

#### Scenario: round 批写不经过全局队列

- **WHEN** round 完成触发终稿批写
- **THEN** 写入路径为事务直写 + 幂等重试，无全局队列参与

#### Scenario: 队列配置残留不再生效

- **WHEN** 配置中仍出现 `messages.flush_interval` / `messages.flush_batch_size`
- **THEN** 不存在任何消费这些参数的后台 flusher（键本身待随代码清理移除）

### Requirement: 幂等插入

messages 表 SHALL 保留 `client_msg_id` 列与 UNIQUE 索引（遗留行允许 NULL）。turn 各持久化路径（send-time 用户行写、round 终稿批写、中断恢复）SHALL 使用确定性 `client_msg_id = {trace_id}:{msg_index}` 并依赖 UNIQUE 约束的幂等 INSERT 语义，使重复投递的同一条记录只落一行。

#### Scenario: 重复投递不产生重复行

- **WHEN** 同一条消息记录因重试或恢复路径被插入两次
- **THEN** 第二次插入被 UNIQUE 索引忽略，messages 表中该 `client_msg_id` 仅存在一行

#### Scenario: 遗留行不受影响

- **WHEN** migration 在既有库上为 messages 加 `client_msg_id` UNIQUE 索引
- **THEN** 遗留行的 NULL 值不违反唯一约束，旧数据读写不受影响

### Requirement: per-session 读缓存 best-effort

Redis `msgs:{session_id}`（24h TTL）SHALL 仅作为 MySQL 之上的读缓存：终稿持久化后以 best-effort 方式追加缓存，缓存写入失败 SHALL 仅记录降级日志并继续，SHALL NOT 影响落库或 turn 结果；读路径遇缓存 miss 或错误 SHALL 回源 MySQL 并回填。

#### Scenario: 缓存写失败不阻断

- **WHEN** round 终稿已写入 MySQL，追加 Redis 缓存时 Redis 报错
- **THEN** turn 正常完成，后续读路径回源 MySQL 重建缓存

#### Scenario: 缓存 miss 回源

- **WHEN** 读历史时缓存 key 不存在或已过期
- **THEN** 从 MySQL 读取并在成功后回填缓存
