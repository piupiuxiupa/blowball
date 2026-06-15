## Why

当前每轮对话只往 `messages` 表写 2 行（user + assistant），其中 assistant 行存的是 `orchestratorAdapter` 把 Confuse 的 token delta 现场拼接出来的整段文本——Chongzhi/Liang 的 token、所有 `tool_call`、`agent_start`/`agent_end`/`agent_error` marker 全部丢弃。这导致：

1. 数据库里看不到模型真正的原始输出，无法事后回放或审计一次 turn 的实际事件流。
2. `msg_index` 字段从未被赋值（全工程搜索确认），所有行都是 0，`ListMessages` 的 `ORDER BY msg_index ASC` 形同虚设，目前靠 PK 顺序碰巧能读对。
3. 用户消息的 `agent` 列被错填成 `Confuse`，与字段语义不符。

需要把 messages 表改造成真正的"事件流日志"：用户消息一行、assistant 一轮产生的所有 `StreamEvent` 各一行，按 `(msg_time, msg_index)` 严格有序读回。

## What Changes

- **BREAKING**：messages 表 schema 调整
  - 新增 `event_type` 列，取值 `message | token | tool_call | agent_start | agent_end | agent_error`
  - `role` 列改为可空（marker 事件无 OpenAI role 语义）
  - `msg_time` 精度从秒升到毫秒（`TIMESTAMP(3)`），避免秒内多行撞时间戳
  - 读路径排序从 `ORDER BY msg_index ASC` 改为 `ORDER BY msg_time ASC, msg_index ASC`
- **BREAKING**：`agent` 列语义扩展，用户消息行 `agent='user'`
- **BREAKING**：`OrchestratorRunner.Handle` 接口返回类型由 `(string, error)` 改为 `([]StreamEvent, error)`，让 handler 拿到原始事件流而非拼接字符串
- 入库语义改变：assistant 一轮产生的所有 `StreamEvent` 收集到内存，**仅在 orchestrator 成功返回后**通过 goroutine 批量写入三层存储
- 用户消息保留现状：在 orchestrator 启动前同步单条写入（崩溃不丢用户输入）
- `done` 事件不写库（usage 是流终止元数据，非 chat 内容）
- 新增 service / store 层批量写入方法：`SaveMessagesBatch` / `AppendMessages`

## Capabilities

### New Capabilities
（无）

### Modified Capabilities
- `session-management`: Message data model 要求扩展（新增 `event_type` 列、`role` 可空、`msg_time` 毫秒精度、`agent='user'` for 用户消息）；新增"assistant 事件批量入库"要求（仅成功路径、goroutine 异步、msg_index 每 turn 从 1 起）；消息恢复的排序要求改为 `(msg_time, msg_index)`

## Impact

- **数据库**：新增 migration `005_messages_event_type.sql`，需要对存量数据做兼容（存量行 `event_type` 填默认 `'token'`，`role` 已有值不受影响）
- **代码改动**：
  - `internal/handler/ports.go`：`OrchestratorRunner` 接口签名变更，`orchestratorAdapter.Handle` 改为累积 `[]StreamEvent`
  - `internal/handler/session.go`：user msg 字段调整；assistant 路径改为批量 goroutine 入库；err 时跳过写入
  - `internal/service/session.go`：新增 `SaveMessagesBatch`
  - `internal/store/mysql/message.go`：新增 `AppendMessages` 批量 INSERT；`ListMessages` 排序变更
  - `internal/store/redis/message.go`：新增 `AppendMessages` 批量 RPUSH
  - `internal/model/message.go`：新增 `EventType` 字段
- **测试**：`test/integration/message_flow_test.go` 期望从 2 行改为 N 行；`session_test.go`、`message_test.go` 等单元测试需同步更新
- **API 对外**：HTTP SSE 协议不变（前端看到的流式事件序列完全一致），变的是落库形状
