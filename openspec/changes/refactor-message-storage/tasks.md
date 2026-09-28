
## 1. Schema 与 store

- [x] 1.1 `migrations/020_tool_calls.sql`：`tool_calls` 表（UK `(session_id, tool_call_id)`，IX `(session_id, turn_id, round_no)`）；`messages` 补 `reasoning_content` / `status` / `round_no` 终稿列
- [x] 1.2 `internal/model`：`ToolCall` 模型 + Message 终稿字段对齐
- [x] 1.3 `internal/store/mysql`：tool_calls 批量幂等插入（INSERT IGNORE 复用 UK）、按 `(session, turn)` 查询；删除归档 `ON DELETE CASCADE`
- [x] 1.4 store 单测：幂等重试、join 查询序数确定性

## 2. 写路径重构

- [x] 2.1 round 渲染器 `round_persist.go`：闭轮检测（下轮首 token 即边界）、跨表事务批写、compaction hook 以 terminal 语义闭轮
- [x] 2.2 分流：`agent_instance_id != ""` 的事件在渲染器内跳过，不进 messages/tool_calls
- [x] 2.3 cancel / provider-error：terminal 渲染 + `interrupted` 标注最后一轮
- [x] 2.4 turn-end 批写复用 `turnPersistAttempts` / budget 重试；确定性 client_msg_id + tool_calls UK 幂等（drain 随队列退役删除）
- [x] 2.5 退役 msgflush（包删除、serve 接线移除、Redis 仅存读缓存）；用户消息 send-time 直写保留
- [x] 2.6 handler 单测：round 边界/幂等 ID/子 agent 分流/interrupted/慢轮落地，持久化序列测试重写为 round 语义

## 3. 读路径与 compaction

- [ ] 3.1 `message_reconstruct.go`：双模读——终稿行 + tool_calls join 直组 wire format；旧 chunk 行走现有合并；两种输入产出相同序列（对拍测试）
- [ ] 3.2 `newRoundHook` compaction：flush-first 改为 round 终稿批写，boundary cursor 指向终稿行
- [ ] 3.3 历史端点 `ListMessagesPaged`：双表 merge 聚合 DTO，兼容 placeholder/full 视图；`subagentPlaceholderFilter` 的 JSON_EXTRACT 过滤删除
- [ ] 3.4 compaction / 历史端点集成测试

## 4. 收尾

- [ ] 4.1 `api/openapi.yaml` 历史端点响应结构更新
- [ ] 4.2 前缀缓存对拍验证：同一会话新旧行混合重建的请求序列 byte 一致
- [ ] 4.3 `gofmt` / `go vet ./...` / `make test`
