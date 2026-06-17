## Why

当前 `SendMessage` 在启动 orchestrator 前同步把用户消息写入三层存储（Redis → FS → MySQL），阻塞了首 token 到达时间；同时 assistant 事件却在 turn 成功后异步批量写入。两条路径并存增加了复杂度，也让每次 turn 产生两次 batch 调用。我们希望降低延迟并统一写入路径。

## What Changes

- **BREAKING**: 将用户消息从 orchestrator 启动前的同步 `SaveMessage` 移除，改为与 assistant 事件一起在同一条异步 batch 中落盘。
- 每 turn 只触发一次 `SaveMessagesBatch`，batch 内顺序为 `[userMsg(msg_index=0), assistant events(msg_index=1..N)]`。
- orchestrator 成功时，user + assistant 一起写入；orchestrator 失败（含非取消错误）时，当前 turn 的 user + assistant 全部丢弃。
- 移除 `SendMessage` 中用户消息保存失败返回 500 的分支； persistence 错误改为在异步 goroutine 内记录日志，不阻塞 SSE 响应。
- 更新 handler 单测与集成测试，使其期望单 batch 写入并反映新的失败语义。
- 更新 `session-management` spec 中关于用户消息同步写入的要求。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `session-management`: 用户消息持久化要求从「orchestrator 启动前同步写入」改为「turn 成功后与 assistant 事件一起异步批量写入」；失败语义从「用户消息不受影响」改为「整 turn 全部丢弃」。

## Impact

- `internal/handler/session.go`：`SendMessage` 流程重排，用户消息构造移入异步 goroutine。
- `internal/handler/event_mapper.go`：可能需要新增/扩展 helper 用于构造用户消息的 `model.Message`。
- `internal/handler/session_test.go`、`test/integration/message_flow_test.go`：batch 调用次数、batch 长度、失败场景期望均需要调整。
- 客户端可见行为：首 token 更快；orchestrator 失败时该 turn 的用户输入不再保留；客户端断连后若 batch 未执行则用户消息可能丢失。
- 对下游消费（历史列表、RecoverMessages）无 schema 变更，消息行格式保持不变。
