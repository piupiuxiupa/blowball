## MODIFIED Requirements

### Requirement: Interrupted assistant turn is persisted
取消与 orchestrator 失败路径 SHALL 持久化 turn 已完成 round 的终稿，并将进行中 round 的已累积内容作为截断 assistant 终稿行落库（interrupted 标注）。用户消息 send-time 持久化保留。进程崩溃时无 at-crash 持久化：已完成 round 已落库，进行中 round 丢失（见 message-final-persistence）。

#### Scenario: Explicit cancel mid-response
- **WHEN** 客户端在 round N 流式中途取消
- **THEN** round 1..N-1 终稿已落库，round N 累积内容作为截断终稿行落库并标注 interrupted

#### Scenario: Provider error mid-response
- **WHEN** orchestrator 在 round N 流式中途返回非取消错误
- **THEN** 同取消路径落库，且 turn token 成本记入 `turn_usage`

#### Scenario: Process crash mid-response
- **WHEN** 进程在 round N 进行中崩溃
- **THEN** round 1..N-1 已落库，round N 丢失，run 事件日志在保留期内仍可重放
