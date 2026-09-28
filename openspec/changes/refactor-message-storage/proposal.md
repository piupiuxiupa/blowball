## Why

`messages` 表按事件粒度存行：每个（合并后的）token/reasoning 片段、tool_call、tool_result 都是独立行，子 agent 并发时行交错膨胀。三个读路径全量扫再过滤——prompt 重建把子 agent 行读出来再在应用层丢弃，placeholder 视图用 `JSON_EXTRACT` + 关联子查询过滤，查询与存储压力随会话长度线性恶化。写入侧为扛 chunk 行数专门建了 msgflush 全局队列 + claim 协议 + crash recovery 子系统。

## What Changes

- **落库粒度改为 round 边界**：一轮（一次完整 LLM 回复 + 其工具执行）结束才写库；流式 chunk 只留在 Redis（复用现有 run event log），新数据不再产生 `token`/`reasoning`/`tool_call`/`tool_result`/marker 事件行。
- **`messages` 只存终稿**：user 行 + assistant 终稿行（完整 content + reasoning）；assistant 行不再内嵌 tool_calls。
- **新增 `tool_calls` 表（canonical）**：一行 = call + result 配对（`tool_call_id` 同行 UK）；`messages` 增 `tool_call_id` 占位列——占位行按 `msg_index` 锚定交互位置并单键 join，请求序列 byte-stable，前缀缓存不受影响。
- **子 agent 内容不进 `messages`**：transcript 继续走已有 `subagent_runs.messages_json`（run 结束快照），分流点在持久化层按 `agent_instance_id` 判断。
- **msgflush chunk 队列退役**：round 批写走现有 turn-end 持久化重试机制（3 attempts / 30s budget / 确定性 client_msg_id 幂等）。
- **崩溃语义**：crash 丢当前进行中 round（已完成 round 已落库）；cancel/provider-error 仍持久化 partial 内容，但形状从事件行变为截断终稿行。
- **遗留 chunk 行不迁移**：读路径按 event type 双模（新终稿行直读，旧 chunk 行走现有合并逻辑），靠归档自然淘汰。

## Capabilities

### New Capabilities

- `message-final-persistence`: round 边界终稿持久化本体——Redis in-flight 缓冲、round 完成事务批写（messages + tool_calls）、tool_calls 表 schema 与 join 重建、崩溃/取消/错误的丢失边界、子 agent 分流、遗留行双模读。

### Modified Capabilities

- `message-storage-optimization`: token 事件合并需求被终稿行取代（新数据无 token 行）；SSE 实时流不变。
- `message-write-behind`: chunk 级入库队列与 flusher 退役；用户消息 send-time 写保留。
- `interrupted-turn-persistence`: cancel/error 的 partial 持久化保留，形状改为截断终稿行；crash 丢当前 round、已完成 round 已落库。
- `context-compaction`: mid-turn compaction 的 flush-first 依赖保留，boundary cursor 指向 round 边界终稿行。

## Impact

- **迁移**：新增 `020_tool_calls.sql`；`messages` 表可能需补 reasoning 终稿列（实现时确认 006 现状）；存量行不动。
- **代码**：`internal/handler/message_stream.go`（持久化路径重写、round hook）、`internal/handler/message_reconstruct.go`（双模读）、`internal/handler/event_mapper.go`（大部分退役）、`internal/service/session.go`、`internal/msgflush`（退役）、`internal/store/redis`（turn 缓冲 API）、`internal/store/mysql`（tool_calls store、消息写入）、`internal/service/compaction.go`（boundary 适配）。
- **API**：历史端点 DTO 组装改为双表 merge；`api/openapi.yaml` 相应更新。SSE 实时流、turn resume 端点不变。
- **删除归档**：`tool_calls` 需随 session 删除级联或镜像归档。
- **行为变化**：crash 中途丢当前 round（此前 ≤1s 增量 flush 可留下 partial 事件行）；历史刷新可见延迟从 ~1s 变为 ~1 round。
