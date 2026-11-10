## ADDED Requirements

### Requirement: Runtime status is excluded from durable user content
send-time 用户消息持久化 SHALL 只保存用户提交的正文及既有附件元数据，SHALL NOT 把 `agent-runtime-status` 的时间、上下文或工具轮次状态拼入用户终稿。标题生成、真实用户消息 ordinal 和后续 durable 历史重建 SHALL 使用同一原文；已有包含旧式内联时间状态的记录 SHALL 保持不变。

#### Scenario: 新用户行不包含运行时状态
- **WHEN** 一次模型请求通过 synthetic 状态收到 `<current_status>` 或 `<tool_budget>`
- **THEN** 对应 send-time 用户行只包含用户原文，且 MySQL 与现有 `msgs:{sid}` 不存在额外的状态用户行或工具行

#### Scenario: 旧内联时间状态无需迁移
- **WHEN** 会话包含本变更前已经把时间状态拼入正文的用户行
- **THEN** 读取与重建保持原字节，不执行回填、清理或格式迁移

## MODIFIED Requirements

### Requirement: Deterministic cross-table reconstruction
对话历史重建 SHALL 按 `(msg_time, msg_index)` 顺序遍历 `messages`，并将同一 round 的 assistant 终稿行与其工具占位行重建为一条 wire-format assistant 消息：该消息 SHALL 携带原 round 的 content、reasoning_content 以及按 LLM 响应顺序排列的 `tool_calls` 数组，后接按同一顺序排列的 `role=tool` 结果消息。工具占位行 SHALL 通过 `tool_call_id` join `tool_calls` 表取回 payload。除压缩显式改写上下文外，重建产出的 durable canonical 请求前缀 SHALL 与当初 live agent durable canonical round 的对应消息序列逐 byte 一致。重建的 row attribution SHALL 覆盖合并 assistant/tool 单元的全部来源行，压缩边界 SHALL NOT 将该单元的 assistant 消息与其工具结果分离。

`agent-runtime-status` 的 synthetic 状态 SHALL NOT 写入上述 durable 表或现有 `msgs:{sid}` 消息行缓存。独立 Redis 状态日志命中且全部锚点有效时，系统 SHALL 按原内容、原顺序、原锚点将其合并回 active model transcript，使状态参与后的上一完整 live 请求仍可成为下一请求前缀。Redis miss 或锚点失效 SHALL 被显式视为新缓存纪元：系统只保证 durable canonical 前缀，从干净 MySQL 历史生成新状态，不得宣称旧 cache-only 状态仍存在或影响 durable 数据正确性。

#### Scenario: 双表合并顺序确定
- **WHEN** round 1 含 assistant 回复与并行 3 个工具调用
- **THEN** 重建序列为 user、一条同时携带回复与 3 个 tool_calls 的 assistant 消息、tool×3（按 LLM 响应中的调用序）

#### Scenario: narrated tool round 不拆分
- **WHEN** 一轮 LLM 响应同时包含非空 content、非空 reasoning_content 和一个 tool_call，该 round 完成后落库并在下一轮恢复
- **THEN** 重建输出是一条同时包含 content、reasoning_content 和该 tool_call 的 assistant 消息
- **AND THEN** 后一条消息是对应的 role=tool 结果

#### Scenario: 并行调用顺序不受执行完成顺序影响
- **WHEN** 一轮响应按 `call_a`、`call_b`、`call_c` 顺序发出 3 个 tool_calls，而并行执行结果按 `call_c`、`call_a`、`call_b` 顺序完成
- **THEN** 持久化 placeholder 与重建后的 assistant tool_calls 均保持 `call_a`、`call_b`、`call_c`
- **AND THEN** 重建后的 tool result 消息也保持同一顺序

#### Scenario: 前缀缓存跨 live 与 durable 恢复连续
- **WHEN** 一个包含 narrated tool round 的 turn 完成，下一 turn 从 Redis 或 MySQL 恢复历史并发起请求
- **THEN** 新请求中对应此前 live durable canonical 请求的消息前缀与原 live durable canonical 请求逐 byte 一致
- **AND THEN** 差异只出现在其后新增的 turn 内容、召回注入、用户消息或 cache-only 状态

#### Scenario: Redis 命中重放完整 live 前缀
- **WHEN** 上一 live 请求包含 cache-only 状态，下一 turn 的独立状态日志命中且全部锚点有效
- **THEN** 状态按原字节合并到原位置，上一完整 live 请求在 system/tools 不变时是下一请求的前缀
- **AND THEN** 这些状态仍不出现在 MySQL 或 `msgs:{sid}` durable 行中

#### Scenario: Redis miss 显式开启新缓存纪元
- **WHEN** 上一 live 请求包含 cache-only 状态，但下一 turn 的状态日志缺失或锚点无效
- **THEN** 系统从逐 byte 稳定的 durable canonical 历史生成新状态并继续对话
- **AND THEN** 不部分重放旧状态，也不把供应商缓存可能失效视为 durable 数据不一致

#### Scenario: 前缀缓存跨迁移边界连续
- **WHEN** 同一会话混合遗留 chunk 行与新终稿行
- **THEN** 重建逻辑对两种输入产出相同的 durable canonical 请求消息序列

#### Scenario: 压缩边界保持工具单元完整
- **WHEN** 压缩边界落在一个包含 assistant 终稿、工具占位和已完成工具结果的 round 附近
- **THEN** 保留在尾部的历史 SHALL 将该 round 的 assistant tool_calls 与对应 tool results 作为完整单元输出
