## MODIFIED Requirements

### Requirement: Compaction preserves the first message, the last five agent messages, and tool pairing boundaries
mid-turn compaction 的 flush-first 依赖 SHALL 改为 round 终稿批写：boundary cursor 指向 `messages` 终稿行；被压缩区间的 `tool_calls` 行保留但重建时按 boundary 跳过（join 与 boundary 均以 turn 为单位，语义对齐）。工具配对边界保护语义不变。

#### Scenario: boundary 指向终稿行
- **WHEN** mid-turn compaction 触发
- **THEN** cursor 引用已落库的 round 边界 `messages` 行 id，压缩区间外工具交互完整保留
