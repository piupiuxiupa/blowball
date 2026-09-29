## Purpose

Define how streaming assistant events are compressed before persistence to reduce storage overhead while preserving the order and semantic boundaries of the event stream.
## Requirements
### Requirement: Merge consecutive assistant token events before persistence
新数据 SHALL NOT 持久化 token 事件行：流式 chunk 留存 Redis，round 完成后写入 assistant 终稿行（见 message-final-persistence）。本需求的合并行为仅保留于遗留 chunk 行的读取适配路径。SSE 实时流仍逐 chunk 推送。

#### Scenario: 新数据无 token 行
- **WHEN** 一轮输出 500 个 token chunk
- **THEN** SSE 推送 500 个 token 事件，round 结束后数据库仅一条 assistant 终稿行

#### Scenario: 遗留 chunk 行仍可重建
- **WHEN** 读取迁移前的合并 token 行
- **THEN** 既有合并重建逻辑将其组装进请求序列

### Requirement: Preserve total event order after merging
The system SHALL assign monotonic `MsgIndex` values to merged message rows such that the original event order is recoverable when messages are ordered by `(MsgTime, MsgIndex)`.

#### Scenario: Sub-agent hand-off keeps order
- **WHEN** the event sequence is Confucius token, Confucius tool_call invoking Liang, Liang agent_start, Liang token, Liang agent_end, Confucius token
- **THEN** the persisted rows appear in the same order with indices 1, 2, 3, 4, 5, 6

### Requirement: Live SSE stream remains unmerged
The system SHALL continue to emit individual `token` events to the SSE consumer; merging SHALL apply only to the persistence path.

#### Scenario: Client receives real-time tokens
- **WHEN** an assistant turn produces five token deltas
- **THEN** the HTTP SSE response contains five separate `token` events
- **AND** the database contains one merged `token` row for that turn

### Requirement: Empty reasoning deltas produce no events or persisted rows
流式解析层 SHALL 在 reasoning delta 的解码结果为空字符串时不调用 reasoning 回调、不产生 `reasoning` StreamEvent，且不将其累积进聚合理性内容。因此，空 reasoning delta SHALL NOT 出现在 SSE 输出中，也 SHALL NOT 生成 `messages` 表持久化行。

#### Scenario: Empty reasoning field alongside content is dropped
- **WHEN** 一条 LLM SSE chunk 同时携带非空 `content` 与 `reasoning_content: ""`
- **THEN** 系统为该 chunk 仅产生 `token` 事件，不产生 `reasoning` 事件
- **AND** turn 结束后 `messages` 表不存在 `event_type='reasoning' AND content=''` 的新行

#### Scenario: Interleaved empty reasoning does not fragment token merging
- **WHEN** 一条 turn 的事件源序列为 reasoning("先分析…")，随后多个 chunk 均携带 `content` 与 `reasoning_content: ""`
- **THEN** 持久化结果为一条 merged `reasoning` 行加一条 merged `token` 行，token 行不按 chunk 碎片化

#### Scenario: Reasoning aggregation ignores empty deltas
- **WHEN** LLM 流中出现的 `reasoning_content` 字段均为空字符串
- **THEN** 聚合响应的 `ReasoningContent` 保持为空，且不触发任何 `reasoning` 回调

