# Proposal: incremental-message-persistence

## Why

消息目前只在 turn 终局一次性批量入库（send-time 用户行之外）。长 agentic turn（数分钟、几十次 LLM 调用）进行中，MySQL `messages` 完全看不到本 turn 内容：中途进入只能靠 run 事件日志回放，进程崩溃时已生成内容全部丢失（终局批从未执行）。用户需要：每当一个**完整的消息事件**（闭合的 merged 事件：一段连续 token/reasoning、tool_call、tool_result、agent 标记——与现有 messages 行格式完全一致，不是 chunk）形成，就尽快落库。

## What Changes

- turn 进行中新增**增量持久化 goroutine**：周期（默认 1s，与 flusher 节奏对齐）快照事件流，仅持久化**已闭合**的 merged 事件前缀（`len(merged)-1`，最后一条可能继续生长，绝不提前写入），按现有行格式/确定性 client_msg_id/续接 msg_index 写入。
- `TurnEventTap` 从"仅压缩配置时创建"改为**无条件创建**（增量快照依赖它）。
- `turnFlushState` 增加 persist 互斥：增量 goroutine 与 mid-turn 压缩 hook 的"取范围→写→推进游标"串行化，区间永不重叠（读缓存无重复）。
- 终局批不变，仍是完整性兜底：先停止增量 goroutine（等在途批完成），再持久化剩余后缀（含最后一条与增量失败重试漏掉的部分）。
- 客户端可见性变化：turn 进行中历史端点逐步出现本 trace 的已闭合行；与 run 回放合并时按 `trace_id == run_id` 去重。

## Capabilities

### New Capabilities

（无）

### Modified Capabilities

- `message-write-behind`: 批次结构由"至多三批"改为"send-time 批 + 增量闭合批（多次）+ （压缩时的 mid-turn 批，语义不变）+ 终局剩余批"；不变量仍为每个 key 中用户行恰好一次、区间不重叠、确定性幂等键收敛。

## Impact

- `internal/handler/message_stream.go`（tap 无条件化、增量 goroutine、persistMu）、`internal/handler/ports.go`（注释）、测试。
- 无 schema/配置/API 变化；`make test`/`make lint`。
