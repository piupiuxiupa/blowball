## ADDED Requirements

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
