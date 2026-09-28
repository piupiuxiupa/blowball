## MODIFIED Requirements

### Requirement: Merge consecutive assistant token events before persistence
新数据 SHALL NOT 持久化 token 事件行：流式 chunk 留存 Redis，round 完成后写入 assistant 终稿行（见 message-final-persistence）。本需求的合并行为仅保留于遗留 chunk 行的读取适配路径。SSE 实时流仍逐 chunk 推送。

#### Scenario: 新数据无 token 行
- **WHEN** 一轮输出 500 个 token chunk
- **THEN** SSE 推送 500 个 token 事件，round 结束后数据库仅一条 assistant 终稿行

#### Scenario: 遗留 chunk 行仍可重建
- **WHEN** 读取迁移前的合并 token 行
- **THEN** 既有合并重建逻辑将其组装进请求序列
