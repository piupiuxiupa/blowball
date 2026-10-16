## MODIFIED Requirements

### Requirement: Round-boundary final content persistence
系统 SHALL 以 round 为最小持久化单位：一轮（一次完整 LLM 回复及其全部工具执行结果）完成后，将该 round 的终稿写入 MySQL。持久化批 SHALL 在单事务内跨 `messages` 与 `tool_calls` 两表写入。新数据 SHALL NOT 产生 `token`、`reasoning`、`tool_result`、`agent_start`、`agent_end` 或 `plan_updated` 事件行；例外：`agent_error` SHALL 作为错误留痕行持久化（role 为空、content 为错误文本、随所属 round 批次落库），`tool_call` 仅以占位行形式存在（见下）。当一个 tool result 之后出现下一轮 LLM 响应的 tool_call 时，系统 SHALL 将其视为新 round 并关闭前一个 round。

#### Scenario: 一轮完成即落库
- **WHEN** 主 agent 完成 round 1（assistant 回复 + 2 个工具调用及结果）后开始 round 2
- **THEN** round 1 的 assistant 终稿行与其 2 条 `tool_calls` 行已在单事务内写入 MySQL
- **AND** round 2 的流式 chunk 尚未产生任何数据库行

#### Scenario: 无工具的最终轮
- **WHEN** 最后一轮 assistant 回复不含工具调用，turn 正常结束
- **THEN** 该轮落库为一条 assistant 终稿行，无 tool_calls 行

#### Scenario: 连续 tool-only round 不合并
- **WHEN** round 1 只包含两个 tool_calls 及其结果，随后不带 token 或 reasoning 的 round 2 以新的 tool_call 开始
- **THEN** round 1 与 round 2 分别落为独立的 assistant 终稿和工具占位批次
- **AND** 重建时不得把两轮的 tool_calls 合并进同一个 assistant 消息

### Requirement: tool_calls table is the canonical tool interaction store
`tool_calls` 表 SHALL 为工具交互的唯一权威存储：每行包含 `tool_call_id`（配对键）、`name`、`arguments`、`result`、`status` 与 `turn_id`。call 与 result SHALL 同行存储。`(session_id, tool_call_id)` SHALL 为唯一键，使重试幂等。参数与结果 payload SHALL NOT 重复存储于 `messages`。对由 LLM 响应缝合而成的 arguments，系统 SHALL 保留 provider 输出的原始 JSON 字节序列，包括 insignificant whitespace，并在持久化和重建期间保持不变。

#### Scenario: call 与 result 同行配对
- **WHEN** round 内工具 `call_abc` 执行完成
- **THEN** `tool_calls` 存在一行：`tool_call_id='call_abc'`，`arguments` 与 `result` 均已填充，`status='completed'`

#### Scenario: 重试不产生重复行
- **WHEN** round 批写因网络错误重试
- **THEN** UK 使同一 `tool_call_id` 只留一行

#### Scenario: 参数空白保持不变
- **WHEN** provider 响应中的 tool call arguments 为 `{"a": 1, "b": 2}`，随后该 round 被持久化并在下一轮重建
- **THEN** 重建后的 arguments 字符串仍为 `{"a": 1, "b": 2}`
- **AND** 系统不得因其为 JSON insignificant whitespace 而将其规范化为 `{"a":1,"b":2}`

### Requirement: Deterministic cross-table reconstruction
对话历史重建 SHALL 按 `(msg_time, msg_index)` 顺序遍历 `messages`，并将同一 round 的 assistant 终稿行与其工具占位行重建为一条 wire-format assistant 消息：该消息 SHALL 携带原 round 的 content、reasoning_content 以及按 LLM 响应顺序排列的 `tool_calls` 数组，后接按同一顺序排列的 `role=tool` 结果消息。工具占位行 SHALL 通过 `tool_call_id` join `tool_calls` 表取回 payload。除压缩显式改写上下文外，重建产出的已完成请求前缀 SHALL 与当初 live agent round 发送给 provider 的对应消息序列逐 byte 一致，以保持 LLM 前缀缓存命中。重建的 row attribution SHALL 覆盖合并 assistant/tool 单元的全部来源行，压缩边界 SHALL NOT 将该单元的 assistant 消息与其工具结果分离。

#### Scenario: 双表合并顺序确定
- **WHEN** round 1 含 assistant 回复与并行 3 个工具调用
- **THEN** 重建序列为 user、一条同时携带回复与 3 个 tool_calls 的 assistant 消息、tool×3（按 LLM 响应中的调用序）

#### Scenario: narrated tool round 不拆分
- **WHEN** 一轮 LLM 响应同时包含非空 content、非空 reasoning_content 和一个 tool_call，该 round 完成后落库并在下一轮恢复
- **THEN** 重建输出是一条同时包含 content、reasoning_content 和该 tool_call 的 assistant 消息
- **AND** 后一条消息是对应的 role=tool 结果

#### Scenario: 并行调用顺序不受执行完成顺序影响
- **WHEN** 一轮响应按 `call_a`、`call_b`、`call_c` 顺序发出 3 个 tool_calls，而并行执行结果按 `call_c`、`call_a`、`call_b` 顺序完成
- **THEN** 持久化 placeholder 与重建后的 assistant tool_calls 均保持 `call_a`、`call_b`、`call_c`
- **AND** 重建后的 tool result 消息也保持同一顺序

#### Scenario: 前缀缓存跨 live 与 durable 恢复连续
- **WHEN** 一个包含 narrated tool round 的 turn 完成，下一 turn 从 Redis 或 MySQL 恢复历史并发起请求
- **THEN** 新请求中对应此前 live 请求的消息前缀与原 live 请求逐 byte 一致
- **AND** 差异只出现在其后新增的 turn 内容、召回注入或用户消息

#### Scenario: 前缀缓存跨迁移边界连续
- **WHEN** 同一会话混合遗留 chunk 行与新终稿行
- **THEN** 重建逻辑对两种输入产出相同的请求消息序列

#### Scenario: 压缩边界保持工具单元完整
- **WHEN** 压缩边界落在一个包含 assistant 终稿、工具占位和已完成工具结果的 round 附近
- **THEN** 保留在尾部的历史 SHALL 将该 round 的 assistant tool_calls 与对应 tool results 作为完整单元输出
