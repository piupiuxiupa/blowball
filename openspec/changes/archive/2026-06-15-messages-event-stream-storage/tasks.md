## 1. Schema & Model

- [x] 1.1 创建 `migrations/005_messages_event_type.sql`：新增 `event_type VARCHAR(16) NOT NULL DEFAULT 'token'`、`role` 改 NULL、`msg_time` 升 `TIMESTAMP(3)`；附 `UPDATE messages SET event_type='message' WHERE role='user'` 兼容存量数据
- [x] 1.2 更新 `internal/model/message.go`：`Message` 加 `EventType string` 字段（db/json tag `event_type`）；Role 字段注释更新为可为空
- [x] 1.3 更新 `migrations/004_messages.sql` 注释（agent 列从 `Confuse | Chongzhi | Liang` 扩到包含 `user`；role 注释说明 NULL 语义）以保持 schema 文档同步

## 2. Stream Event Mapping

- [x] 2.1 在 `internal/stream/event.go` 加常量 `EventMessage = "message"`，作为用户消息行的 event_type 取值（区别于流式事件类型）
- [x] 2.2 在 `internal/model/message.go` 加 `EventTypeMessage / EventTypeToken / EventTypeToolCall / EventTypeAgentStart / EventTypeAgentEnd / EventTypeAgentError` 常量集合，与 stream 包的事件类型对齐
- [x] 2.3 新增 helper `MessageFromEvent(e StreamEvent, sessionID, traceID string, msgIndex int, msgTime time.Time) (model.Message, error)`：把 StreamEvent 映射成 model.Message；tool_call 事件的 content 序列化成 `{"name":...,"args":...}`；marker 事件 role 留空

## 3. OrchestratorRunner Interface Change

- [x] 3.1 修改 `internal/handler/ports.go` 的 `OrchestratorRunner.Handle` 返回类型：`(assistantContent string, err error)` → `(events []stream.StreamEvent, err error)`
- [x] 3.2 重写 `orchestratorAdapter.Handle`：把 drain goroutine 里的 `content += e.Content` 改为 `events = append(events, e)`，通过 `eventsCh` 返回切片；保留向 caller hub 转发的逻辑
- [x] 3.3 更新 `internal/handler/session.go` 中 `SessionHandler.orch` 字段的接口调用点，适配新签名

## 4. MySQL Store

- [x] 4.1 在 `internal/store/mysql/message.go` 新增 `AppendMessages(ctx, msgs []model.Message) ([]int64, error)`：一条 INSERT 多 VALUES，返回自增 id 列表
- [x] 4.2 更新 `appendMessageSQL` 与新增的 `appendMessagesSQL` 同步包含 event_type 列
- [x] 4.3 修改 `listMessagesSQL`：`ORDER BY msg_index ASC` → `ORDER BY msg_time ASC, msg_index ASC`，SELECT 列表加 `event_type`

## 5. Redis Store

- [x] 5.1 在 `internal/store/redis/message.go` 新增 `AppendMessages(ctx, sessionID string, raws [][]byte) error`：TxPipeline 一次 RPUSH 多个元素 + Expire 刷新 TTL

## 6. Service Layer

- [x] 6.1 在 `internal/service/session.go` 新增 `SaveMessagesBatch(ctx, userID string, msgs []model.Message) error`：批量序列化为 raws，复用三层写入逻辑——Redis 调 `AppendMessages`、FS 一次 read-modify-write 追加多条、MySQL 调 `AppendMessages`；保留 Redis best-effort / MySQL 不阻塞 / FS 阻塞返回 的错误处理策略
- [x] 6.2 抽取 `appendToFS` 内部支持单条与批量（参数改为 `raws [][]byte`），单条 `SaveMessage` 复用同一实现

## 7. Handler Integration

- [x] 7.1 修改 `internal/handler/session.go` 的 `SendMessage`：用户消息构造改为 `Agent: "user"`、`EventType: model.EventTypeMessage`、`MsgIndex: 0`；保留 orchestrator 启动前 sync SaveMessage
- [x] 7.2 替换 assistant 持久化路径：移除当前的 `<-resultCh` 后单条 SaveMessage，改为 `res.err != nil` 时直接 return（不写 assistant 行），`res.err == nil` 时启 goroutine 用 detached ctx 调 `SaveMessagesBatch`
- [x] 7.3 在 goroutine 内用 `MessageFromEvent` 把 `res.events` 转换成 `[]model.Message`（msg_index 从 1 递增、msg_time 用 `time.Now().UTC()`、agent 来自事件、trace_id 透传）
- [x] 7.4 goroutine 内 `defer recover()` 防止入库失败导致 panic 影响进程；失败仅记日志（与 SaveMessage 的 best-effort 语义一致）

## 8. Tests

- [x] 8.1 `internal/handler/session_test.go`：把 stub `OrchestratorRunner` 实现的 Handle 返回值改为 `[]stream.StreamEvent`；更新所有断言
- [x] 8.2 `internal/handler/session_test.go`：新增测试用例覆盖 (a) 成功路径写入 N 行（包含 marker + token + tool_call）；(b) orchestrator 失败时不写 assistant 行但用户消息已写
- [x] 8.3 `internal/service/session_test.go`：新增 `SaveMessagesBatch` 的单元测试（mock 三层 store，校验调用顺序与参数）
- [x] 8.4 `internal/store/mysql/message.go` 对应测试（如有）：覆盖 `AppendMessages` 批量插入、`ListMessages` 按 `(msg_time, msg_index)` 排序
- [x] 8.5 `test/integration/message_flow_test.go`：把"2 行 user+assistant"断言改为"1 行 user + N 行 assistant 事件"；新增校验事件顺序（agent_start → token... → tool_call → ... → agent_end）；校验 user 行 `agent='user'`、`event_type='message'`、`msg_index=0`
- [x] 8.6 `test/integration/message_flow_test.go`：新增失败场景测试——orchestrator 返回 err 时 MySQL 只有用户消息行

## 9. Spec Sync & Validation

- [x] 9.1 运行 `openspec validate messages-event-stream-storage --strict` 通过
- [x] 9.2 跑全量 `go test ./...` 通过
- [x] 9.3 手动跑一次完整对话（curl + mysql client 校验 messages 表行数为 1 + N，且 event_type / agent / msg_index 字段符合预期）
