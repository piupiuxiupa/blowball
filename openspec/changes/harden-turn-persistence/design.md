# Design: harden-turn-persistence

## Context

现行 turn 终局流程（`internal/handler/message_stream.go`）：

```
turn goroutine:  orchestrator 返回 → hub.Close → 等 drainDone → Finalize(终态/释放claim/retire) → resultCh
handler:         writeRunEvents(SSE) → 等 resultCh → persistEvents(异步 goroutine, 无人 await)
```

事故链：`Finalize` 释放 claim 后，终局消息批次才由 fire-and-forget goroutine 持久化；进程崩溃/停机竞态时该 goroutine 无人等待（优雅停机仅 `sleep 250ms` 启发式覆盖），assistant 事件全丢。同时 `SaveMessagesBatch`（`internal/service/session.go`）在 Redis 双写失败后调用 `fallbackDirectWrite`，无论直写成败均返回 `nil`——双挂时批次以 "batch lost" ERROR 日志静默丢弃，且 send-time 路径因此错误地 `markUserPersisted()`，终局批不再补写用户行，既有 spec 的 at-least-once 兜底（发送时批次失败由终局批次兜底）在实际实现中永远无法触发。

## Goals / Non-Goals

**Goals:**

- 终局消息批次在 run 终结（claim 释放、终态写入）**之前** durable，失败时有界重试。
- `SaveMessagesBatch` 的错误语义真实化：双挂上抛，Redis 单挂仍内部降级。
- send-time 用户消息 at-least-once 兜底实际可用。
- `generating:false` 成为客户端可依赖的「数据已持久化」信号（含可选的 MySQL 可见性 drain）。
- 优雅停机等待自然覆盖终局持久化。

**Non-Goals:**

- 本地 spool 文件兜底（扛双挂长故障 + 进程死亡）——留待后续变更。
- run 事件日志 MAXLEN、alive 误判防护、队列深度监控——独立后续变更。
- 历史端点读路径改造（仍纯 MySQL；可见性由 drain-before-release 保证）。
- SSE 协议、done 事件产生时机、mid-turn 批次、flusher 可靠队列语义均不变。

## Decisions

### D1: 错误传播只到「双挂」层

`fallbackDirectWrite` 返回错误；`SaveMessagesBatch` 仅在「Redis 双写失败 **且** MySQL 直写失败」时返回该错误。Redis 单挂仍内部降级、直写成功即返回 `nil`（保持既有「SSE 路径不被存储抖动阻塞」约定）。

备选：所有失败都上抛——被否，会把 Redis 抖动放大为调用方重试风暴，且直写已成功的批次重试只会被幂等键白白吸收。

### D2: persist 挪入 turn goroutine，位于 Finalize 之前

新顺序：`orchestrator 返回 → hub.Close → 等 drainDone → persistEvents(同步、有界) → Finalize → Finish → resultCh`。

`resultCh` 仍传递 events/usage/err（接口不变），但发送时持久化已完成。`persistEvents` 内部的 panic recover、MergeEvents、`buildTurnMessages` 后缀逻辑、turn_usage 写入（失败仅 WARN，遵循 turn-cost-tracking 的「usage 不回滚消息」约定）、memory capture（保持 fire-and-forget，辅助数据）均原样保留，只是执行位置变化。

备选一：保持异步 + 停机时显式 await persist WaitGroup——修不了崩溃窗口，且停机路径复杂化。备选二：失败重入 `msgs:buffer`——双挂时 Redis 本身不可用，重入同样失败，逻辑不成立。

### D3: 持久化期间维持心跳

drainer 在 final drain 后退出，而 persist 阶段可能持续数秒（重试）——期间 `run:{rid}:alive`（15s TTL）会过期，attach 端将 running+!alive 误判为死 run 并强制释放 claim，破坏 D2 的时序保证。方案：persist 阶段由 turn goroutine 以 `HeartbeatEvery` 周期调用 `store.Heartbeat`（复用 compare-and-rearm 语义），Finalize 前停止。备选：延长 drainer 生命周期至 persist 完成信号——需要新增 goroutine 间握手，收益相同复杂度更高。

### D4: 有界重试，常量而非配置

persist 总预算 30s（常量），内部如 3 次尝试 × 退避 1s，超预算后 ERROR（含 trace/session/批次规模）并继续 Finalize。镜像 `msgflush` 的 `errorBackoff`/`finalDrainTimeout` 先例：可靠性参数不进配置。重试期间 claim 被占——数据不丢优先于立即解锁，且远小于 30min 认领 TTL。

### D5: drain-before-release（可独立取舍的实现单元）

终局批 `SaveMessagesBatch` 成功后、Finalize 前，turn goroutine 调用既有有界同步 drain（`messageDrainTimeout` 5s），使 MySQL 在 claim 释放前持有该批。drain 失败仅 WARN 并继续 Finalize（flusher ≤1s 兜底，不为可恢复的可见性延迟无限持锁）。若客户端契约接受 `generating:false` 后短暂轮询，本决策可整体裁剪而不影响 D1–D4。

### D6: 停机流程简化

`WaitAll` 等待 `Finish`，而 `Finish` 现在位于 persist+Finalize 之后——终局持久化被停机等待自然覆盖。删除 `cmd/blowball/serve.go` 的 `time.Sleep(250ms)` 与其注释。

## Risks / Trade-offs

- [双挂持续超过 30s 预算 → 终局批仍丢] → 本变更把「无感知静默丢」降级为「有 ERROR 日志的有界重试后丢」；彻底解决需 spool（Non-Goal，后续变更）。send-time 用户行的 at-least-once 兜底已恢复。
- [claim 晚释放，极端时 session 多锁 ≤30s] → 新消息 409 `SESSION_BUSY`，语义正确（上一轮数据未落地）；正常路径增量仅一次 Redis pipeline（毫秒级）。
- [取消路径 SSE 关闭推迟到持久化完成] → done 路径不受影响（done 事件已在日志中，SSE 走 sawDone 快路径）；取消路径有界 ≤30s。
- [每次 turn 终局触发全局 drain，高并发下放大] → drain 幂等、有界 5s、顺带清理并发 turn 的队列；失败不阻塞 Finalize。
- [persist 与 Finalize 之间崩溃] → Redis 双写成功即 durable（flusher 崩溃恢复语义不变）；仅在「双挂重试中」崩溃才丢，窗口较现状大幅收窄。

## Migration Plan

纯后端执行顺序变更，无 schema/配置/API 变化。部署即生效；回滚即 revert（无数据格式残留）。灰度观察点：`SESSION_BUSY` 时长分布、终局 ERROR（"save event stream failed"/双挂）日志、优雅停机耗时。

## Open Questions

- D4 的 30s/3 次/1s 数值是否需要在首版实现后按观测调整（保持常量）。
- D5 是否保留：取决于前端是否愿意在 `generating:false` 后做短暂历史轮询；tasks 中独立成项，可裁剪。
