
## 1. Schema 与 store

- [ ] 1.1 `migrations/020_tool_calls.sql`：`tool_calls` 表（session_id / turn_id / round_no / tool_call_id / name / arguments / result / status / started_at / finished_at；UK `(session_id, tool_call_id)`，IX `(session_id, turn_id, round_no)`）；确认 `messages` reasoning 终稿列
- [ ] 1.2 `internal/model`：`ToolCall` 模型 + Message 终稿字段对齐
- [ ] 1.3 `internal/store/mysql`：tool_calls 批量幂等插入（INSERT IGNORE 复用 UK）、按 `(session, turn)` 查询；删除归档级联/镜像
- [ ] 1.4 store 单测：幂等重试、join 查询序数确定性

## 2. 写路径重构

- [ ] 2.1 `internal/handler/message_stream.go`：round 完成事件钩子——从 Redis run event log 累积器渲染终稿批（user/assistant 行 + tool_calls 行），跨表单事务写
- [ ] 2.2 分流：`agent_instance_id != ""` 的事件不进 messages 持久化路径
- [ ] 2.3 cancel / provider-error 终止路径：当前 round 累积内容作为截断终稿行 flush（interrupted 标注）
- [ ] 2.4 turn-end 批写复用现有 `turnPersistAttempts` / budget / drain 机制；确定性 ID 保证幂等
- [ ] 2.5 退役 msgflush chunk 管线（flusher / 队列 / claim / startup recovery）；保留用户消息 send-time 直写与删除前 drain 语义
- [ ] 2.6 handler 单测：round 边界触发、崩溃丢当前 round、cancel/error partial 落库、子 agent 分流

## 3. 读路径与 compaction

- [ ] 3.1 `message_reconstruct.go`：双模读——终稿行 + tool_calls join 直组 wire format；旧 chunk 行走现有合并；两种输入产出相同序列（对拍测试）
- [ ] 3.2 `newRoundHook` compaction：flush-first 改为 round 终稿批写，boundary cursor 指向终稿行
- [ ] 3.3 历史端点 `ListMessagesPaged`：双表 merge 聚合 DTO，兼容 placeholder/full 视图；`subagentPlaceholderFilter` 的 JSON_EXTRACT 过滤删除
- [ ] 3.4 compaction / 历史端点集成测试

## 4. 收尾

- [ ] 4.1 `api/openapi.yaml` 历史端点响应结构更新
- [ ] 4.2 前缀缓存对拍验证：同一会话新旧行混合重建的请求序列 byte 一致
- [ ] 4.3 `gofmt` / `go vet ./...` / `make test`
