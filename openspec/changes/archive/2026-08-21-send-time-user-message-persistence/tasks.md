# Tasks: send-time-user-message-persistence

## 1. 持久化状态与批次渲染

- [x] 1.1 `internal/handler/message_stream.go`：`turnFlushState` 增加 `userPersisted` 维度（`markUserPersisted()` / `userPersisted()`，走既有互斥锁，置位后不可逆——语义对齐 `mark` 的 monotonic）
- [x] 1.2 `buildTurnMessages` 的 user-row 包含条件从 `from == 0` 改为 `from == 0 && !userPersisted`（调用方显式传入 bool，保持 `turnPersistInfo` 值语义）
- [x] 1.3 单测（`compaction_test.go` / `event_mapper` 侧）：三态覆盖——未持久化（含 user row）、已持久化（不含）、`from > 0`（不含）

## 2. 发送时写入点

- [x] 2.1 `SendMessage`：在 run meta 初始化（`InitMeta`）之后、turn goroutine spawn 之前，以既有 `persist` 字段构造 `UserMessage` 调 `SaveMessagesBatch`（同步，handler goroutine 上）；返回 nil 时 `flushed.markUserPersisted()`
- [x] 2.2 写入返回 error 时仅 WARN 不置位（终局批次照旧含 user row）；确认位置晚于 turn-start stitching 块与 title 触发（决策 1 的不变量），加注释锚定

## 3. 后续批次适配

- [x] 3.1 round hook（mid-turn compaction flush）渲染传入 `userPersisted`：flush 批次变为纯事件后缀
- [x] 3.2 `persistEvents`（终局批次，成功/失败/取消三路径共用）同样传入 `userPersisted`

## 4. 测试

- [x] 4.1 `internal/handler/session_test.go`：每 turn 两次 `SaveMessagesBatch`（发送时 `[user]` + 终局 `[events]`）；终局批次不含 user row；409/400/404 路径零调用
- [x] 4.2 `internal/handler/compaction_test.go`：mid-turn flush 后 `msgs` 序列中 user row 恰好一次；flush → 终局的 suffix 续接不重不漏
- [x] 4.3 `test/integration/message_flow_test.go`：慢 LLM fake 下 turn 运行中 `GET /messages` 含本 turn user 行、无 assistant 行（问题 1 的回归锚点）
- [x] 4.4 `test/integration/message_writebehind_test.go`：批次拆分后 `msgs:buffer` / `msgs:{sid}` 内容断言（user 一次、事件后缀、顺序）
- [x] 4.5 回归：`make test` 全量（race）

## 5. 文档

- [x] 5.1 CLAUDE.md 请求流段落：claim → title → **user-row persist（send-time）** → orchestrate → SSE → persist（事件后缀）
- [x] 5.2 `openspec archive` 归档本变更
