# Design: incremental-message-persistence

## Context

终局批是唯一完整性保证；增量写入是"尽力提前"，不承担完整性——失败只 WARN 不 mark，下一 tick 或终局批兜底。因此增量路径可以做到**永不阻塞生成**（独立 goroutine、失败跳过）。

## Decisions

- D1 只写**闭合**前缀：`MergeEvents` 只合并相邻 token/reasoning，前缀里除最后一条外全部闭合（后续事件不可能并入）。截断 `merged[:closed]` 后复用现有 `buildTurnMessages`，序数/幂等键与终局视图逐字节一致，区间永不重叠 → 读缓存 `msgs:{sid}` 无重复（该 list 无去重，这是硬约束）。
- D2 周期快照而非逐事件推送：1s 与下游 flusher 节奏对齐（推得更快只会积压在 msgs:buffer，MySQL 可见性仍被 flusher 卡住）；复用 tap.Snapshot（压缩 hook 已有的原语），零新并发面。tap 因此无条件创建。
- D3 互斥：`turnFlushState` 增加 `persistMu`，增量 goroutine 与压缩 round hook 的"读游标→persist→mark"临界区串行；终局批在停止增量 goroutine（等在途批完成）后独占运行，天然无竞争。
- D4 停止时机：`<-drainDone` 后先 stop（drain 在途批）再 `persistTurnEvents`，终局批从最新 `flushed.count()` 续接，涵盖未闭合尾条与增量漏写。

## Risks / Trade-offs

- [长单段回答（无工具调用）闭合晚] → token 段只在边界（reasoning↔token 切换、agent_end）闭合，纯聊天收益小、agentic turn 收益大；可接受。
- [快照 O(n)/tick] → 与压缩 hook/RecoverMessages 同量级；1s 节奏下可忽略。
- [崩溃窗口收窄但未消除] → 最后 ≤1s 的闭合事件与未闭合尾条仍可能丢（终局批兜底不再有机会时）；较现状（整个 turn 丢）大幅改善。

## Open Questions

（无——事件级推送留作后续优化，不改正确性）
