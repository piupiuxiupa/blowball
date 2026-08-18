# add-context-compaction — 上下文压缩能力

## Why

长会话的模型上下文无限增长：每 turn 全量恢复历史并原样发送（`RecoverMessages` → `MessagesToAgentMessages`），没有任何 token 上限保护，最终撞上模型上下文窗口的硬墙 —— provider 返回 400，turn 失败且无法自愈。需要一个软着陆机制：上下文接近窗口时把中间历史压缩成摘要，让会话可以无限延续。

## What Changes

- **新增全局配置 `openai.max_context_tokens`**：模型最大上下文窗口，单一值；压缩阈值为它的 80%（固定）。
- **上下文压缩策略**：上下文达到 `80% × max_context_tokens` 时触发压缩。**首条消息不压缩**（原始任务锚点），**结尾 5 条 agent 消息不压缩**（近期上下文），中间部分由**当前 agent 配置的模型**生成结构化 checkpoint 摘要。
- **mid-turn 触发**（对话进行中）：Confucius 工具轮之间以上一轮 LLM 响应的 `usage`（prompt+completion，权威计量）检查，达到阈值时暂停对话 → 先按现有持久化逻辑把当前 turn 已产生的事件落库 → 对完整历史执行压缩 → 用压缩后的上下文（首条 + 摘要 + 尾部）重写内存对话并继续循环。
- **turn 间触发**：发送消息时按 `sessions.context_compacted` 标记决定是否拼接压缩上下文 —— 已压缩会话的模型上下文 = 首条消息 + 摘要（user role，`<compacted-summary>` 框架包装）+ 边界游标之后的消息。
- **存储**：新表 `context_compactions`（append-only，每次压缩一行，摘要 + 边界复合游标 + 观测列，拼接取最新一行）；`sessions` 新增 `context_compacted` 标记列；Redis 新增 `compaction:{session_id}` 热缓存键（MySQL miss 回填，同现有三段式模式）。
- **显示路径完全不变**：`GET /sessions/:id/messages` 与 Redis `msgs:` 缓存不读压缩数据；`messages` 表 append-only，原始消息永不删除。
- **配套改动**：mid-turn 落库引入确定性 `client_msg_id`（`{trace_id}:{msg_index}`）使 turn 结束批量保存幂等去重；`turn_usage` 新增 `context_tokens` 列（turn 末轮上下文大小）支撑 turn 开始时的预防性压缩检查。

## Capabilities

### New Capabilities

- `context-compaction`：上下文压缩全生命周期 —— 触发条件与计量、压缩范围选择（首条/尾部保留、配对原子性边界）、摘要生成、压缩记录存储（MySQL 表 + Redis 缓存 + sessions 标记）、模型上下文拼接、mid-turn 暂停-压缩-继续流程、落库幂等、失败降级。

### Modified Capabilities

（无。压缩是 `agent-conversation-memory` 重建逻辑之上的叠加层：`MessagesToAgentMessages` 行为不变，拼接发生在重建之后、传入编排器之前；`session-crud` / `interrupted-turn-persistence` 等现有需求的可见行为不受影响。）

## Impact

- **配置**：`internal/config/config.go`（`OpenAIConfig` 新增 `max_context_tokens`，正值校验）、`config.example.yaml` 注释示例。
- **数据库**：`migrations/012_context_compaction.sql` —— 新表 `context_compactions`（FK 级联删除，随会话清理）、`sessions.context_compacted` 列、`turn_usage.context_tokens` 列。
- **存储层**：`internal/store/mysql/`（compaction CRUD、turn_usage 加列）、`internal/store/redis/`（`compaction:{sid}` get/set/del）。
- **服务层**：新 `CompactionService`（internal/service）：触发判断、范围选择、摘要调用编排、压缩记录写入（写序：表 → Redis → 标记）。
- **Agent 层**：`internal/agent/confucius.go` 轮间压缩检查钩子（经 orchestrator 注入，携带已收集事件与最新 usage）；`internal/agent/orchestrator.go` 钩子接线。
- **Handler 层**：`internal/handler/message_stream.go` —— 发送入口拼接压缩上下文、turn 开始预防检查、mid-turn flush 与 turn 结束保存的增量去重；`internal/handler/event_mapper.go` —— 确定性 `client_msg_id` 生成。
- **API**：无新端点、无契约变更（`api/openapi.yaml` 不动）。
- **文档**：CLAUDE.md 架构说明补充压缩段落。
