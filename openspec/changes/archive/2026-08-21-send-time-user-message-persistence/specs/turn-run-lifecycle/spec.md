# Delta: turn-run-lifecycle

## MODIFIED Requirements

### Requirement: 恢复端点重放并续传运行中 turn 的事件流

系统 SHALL 提供恢复端点 `GET /api/v1/sessions/:session_id/turns/:run_id/events`（JWT + 归属校验），以 SSE 返回该 run 的事件：先重放已有事件日志（从头，或从 `Last-Event-ID` 指定的事件之后），再追 live 输出直至终局。SSE 的 `id:` 行 SHALL 等于事件在 Redis Stream 中的 entry id。多个并发订阅（多标签页）SHALL 互不干扰。事件日志 SHALL 只含 agent 事件——用户消息行不进事件日志（`message` 为持久化 sentinel，不上 SSE）；重连侧的完整视图 SHALL 由历史读取（含该 turn 发送时已持久化的用户消息行，见 session-management / message-write-behind 能力）与事件重放共同构成。

#### Scenario: 重开会话自动恢复

- **WHEN** 用户重开一个存在运行中 turn 的 session 并 attach 其 run
- **THEN** 端点回放自开始以来的全部事件，随后继续推送 live 事件直到终局
- **AND** 客户端经历史读取可见该 turn 的用户消息行（发送时已持久化），与事件重放共同构成含用户提问与 agent 输出的完整视图

#### Scenario: Last-Event-ID 断点续传

- **WHEN** 客户端携带 `Last-Event-ID` attach
- **THEN** 端点从该事件之后继续推送，无事件丢失且无重复

#### Scenario: attach 已终局的 run

- **WHEN** run 已终局但仍处保留窗口内
- **THEN** 端点回放全部事件（含终局事件）后关闭流

#### Scenario: attach 超出保留窗口的 run

- **WHEN** run 的 Redis key 已被清理（保留窗口已过或从未存在）
- **THEN** 端点返回 HTTP 410，客户端回落普通历史读取
