## MODIFIED Requirements

### Requirement: Redis 入库队列与读缓存双写
chunk 级全局入库队列（`msgs:buffer` / `msgs:processing`）与后台 flusher SHALL 退役：round 终稿批写直接走 turn 持久化路径（确定性 ID + 重试预算）。用户消息 send-time 写与 per-session 读缓存 SHALL 保留。

#### Scenario: round 批写不经过全局队列
- **WHEN** round 完成触发终稿批写
- **THEN** 写入路径为事务直写 + 幂等重试，无全局队列参与
