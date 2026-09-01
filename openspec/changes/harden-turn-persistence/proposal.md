# Proposal: harden-turn-persistence

## Why

一次线上事故暴露了 turn 终局持久化的两个真实缺口：`llm_raw_log` 完整记录了 LLM 调用，但 `messages` 表只剩 send-time 写入的用户消息行，整个 assistant 事件批次丢失。根因是终局持久化运行在 fire-and-forget goroutine 里（无人 await，优雅停机只靠 `sleep 250ms` 启发式覆盖），且 session claim 在持久化完成**之前**释放——同时 `SaveMessagesBatch` 在 Redis 双写与 MySQL 直写双层失败时无条件吞错返回 `nil`，使 send-time 用户消息的 at-least-once 兜底（终局批补写用户行）实际永远无法触发。

## What Changes

- **错误语义真实化**：`SessionService.SaveMessagesBatch` 的 MySQL 降级直写失败时返回错误（Redis 单挂仍内部降级返回成功）；双挂不再静默 "batch lost"。
- **恢复 send-time at-least-once**：send-time 用户消息写入失败时不再 `markUserPersisted()`，终局批次重新包含用户行（`{trace_id}:0` 幂等键在 MySQL 收敛为单行）。
- **终局持久化进入 turn 生命周期**：`persistEvents` 从 fire-and-forget goroutine 挪入 turn goroutine，位于 `Finalize` 之前；使用有界 context + 进程内退避重试（双挂时最多占用 turn goroutine 一个有界窗口，消息不丢优先于立即解锁）。
- **claim 释放时序修正**：session claim（`session:{sid}:run`）在终局消息批次 durable 后才释放，使客户端状态机信号 `generating:false ⇒ 数据已持久化` 成立；持久化期间维持 run 心跳，避免 attach 端因 alive 过期误判死 run。
- **终局同步 drain（可独立取舍）**：终局批入队后、claim 释放前执行一次有界 drain（复用 `msgflush.Drain`），使 `generating:false ⇒ MySQL 历史完整`，消除历史端点（纯 MySQL 读）的 flush-lag 可见性窗口。
- **优雅停机简化**：`WaitAll` 天然覆盖终局持久化，移除 `serve.go` 中的 `time.Sleep(250ms)` 启发式等待。
- **不改变**：SSE 事件流协议、done 事件的产生时机、mid-turn compaction 批次、flusher 的可靠队列语义、幂等键规则。

## Capabilities

### New Capabilities

（无——本变更是对既有能力的加固与语义修正）

### Modified Capabilities

- `message-write-behind`: 降级直写失败时的错误传播语义——Redis 双写失败且 MySQL 直写失败时 `SaveMessagesBatch` SHALL 返回错误（现为实现吞错返回 `nil`，违反既有 "不丢弃该批消息" 场景）；Redis 单挂仍 SHALL 内部降级并返回成功。补双挂场景与 send-time 兜底联动。
- `turn-run-lifecycle`: claim 释放时序——由「turn 终局时释放」改为「终局消息批次持久化（含可选 drain）完成后释放」，持久化期间心跳不中断；优雅停机等待覆盖终局持久化完成。

## Impact

- `internal/service/session.go`：`SaveMessagesBatch` / `fallbackDirectWrite` 错误返回。
- `internal/handler/message_stream.go`：send-time 标志位、`persistEvents` 挪入 turn goroutine、Finalize 前持久化 + 可选 drain。
- `internal/run/`：drainer 心跳与持久化阶段的生命周期衔接（persist 期间维持心跳）。
- `cmd/blowball/serve.go`：移除停机 250ms 启发式。
- `internal/msgflush`：Drain 原语复用（无语义变化）。
- 测试：`internal/service/session_test.go`、`internal/handler/message_stream`/`session_test`、turn 生命周期测试、优雅停机集成路径。
- 行为变化（可观测）：turn 终局后 session 解锁延迟从「立即」变为「一次 Redis pipeline（毫秒级）～ 有界上限（双挂重试窗口）」；取消路径 SSE 关闭同样推迟至持久化完成（有界）。
