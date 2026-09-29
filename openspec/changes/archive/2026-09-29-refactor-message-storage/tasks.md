
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

- [x] 3.1 双模读：终稿行按 `(trace, round)` join `tool_calls` 直组 wire format（含 tool_calls 数组与 role=tool 消息）；旧 chunk 行走现有合并；byte 对拍测试通过
- [x] 3.2 `newRoundHook` compaction：flush-first 改为 round 终稿批写（terminal 闭轮），boundary cursor 指向终稿行；`RecoverDurable` 同时返回 rows+calls
- [x] 3.3 历史端点：final 行附带 `tool_calls` 数组（`ToolCallRecord` schema）；placeholder 过滤保留服务遗留行（新数据无事件行，过滤天然惰性）
- [x] 3.4 集成测试重写为终稿形态：message_flow / subagent_run / dynamic_subagents / parallel_agent / reasoning / round_retry / mcp_skills / delete

## 4. 收尾

- [x] 4.1 `api/openapi.yaml`：Message 增 `round_no`/`reasoning_content`/`status`/`tool_calls`，新增 `ToolCallRecord` schema，端点描述双代存储说明
- [x] 4.2 前缀缓存对拍验证：`TestMessagesToAgentMessages_FinalRowsMatchLegacyByteForByte`（legacy 事件行 vs 终稿+join 产出逐 byte 一致）
- [x] 4.3 `gofmt`（改动文件全格式化）/ `go vet ./...` 零告警 / `make test`（race）：除 proposal 提交 a411d40 上即已失败的 `current_status` 时间注入家族（handler 13 + agent 1 + 集成 10）外全绿
