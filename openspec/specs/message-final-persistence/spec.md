# message-final-persistence Specification

## Purpose
定义以 round 为持久化单位的消息终稿模型：一轮完整 LLM 回复及其全部工具执行结果在单事务内直写 MySQL（`messages` 存终稿行与工具占位行、`tool_calls` 为工具交互唯一权威存储），流式 chunk 不落库；替代早期逐事件行/token 行持久化模型（遗留行的读取适配见 `agent-conversation-memory` 与 `message-storage-optimization`）。
## Requirements
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

### Requirement: messages stores final content and tool placeholders
`messages` 表 SHALL 存储 user 终稿行、assistant 终稿行（完整 content 与 reasoning），以及工具占位行（`event_type=tool_call`、仅携带 `tool_call_id` 与工具名、不携带参数/结果 payload）。占位行在其 `(msg_time, msg_index)` 位置锚定一次工具交互， SHALL 通过 `tool_call_id` join `tool_calls` 表取回 payload。SSE 实时流 SHALL 保持逐 chunk 推送不变。

#### Scenario: token chunk 不落库
- **WHEN** 一轮 assistant 流式输出 500 个 token chunk
- **THEN** SSE 消费者收到 500 个 token 事件
- **AND** 该轮结束后 `messages` 只有一行 assistant 终稿

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

### Requirement: Runtime status is excluded from durable user content
send-time 用户消息持久化 SHALL 只保存用户提交的正文及既有附件元数据，SHALL NOT 把 `agent-runtime-status` 的时间、上下文或工具轮次状态拼入用户终稿。标题生成、真实用户消息 ordinal 和后续 durable 历史重建 SHALL 使用同一原文；已有包含旧式内联时间状态的记录 SHALL 保持不变。

#### Scenario: 新用户行不包含运行时状态
- **WHEN** 一次模型请求通过 synthetic 状态收到 `<current_status>` 或 `<tool_budget>`
- **THEN** 对应 send-time 用户行只包含用户原文，且 MySQL 与现有 `msgs:{sid}` 不存在额外的状态用户行或工具行

#### Scenario: 旧内联时间状态无需迁移
- **WHEN** 会话包含本变更前已经把时间状态拼入正文的用户行
- **THEN** 读取与重建保持原字节，不执行回填、清理或格式迁移

### Requirement: Deterministic cross-table reconstruction
对话历史重建 SHALL 按 `(msg_time, msg_index)` 顺序遍历 `messages`，并将同一 round 的 assistant 终稿行与其工具占位行重建为一条 wire-format assistant 消息：该消息 SHALL 携带原 round 的 content、reasoning_content 以及按 LLM 响应顺序排列的 `tool_calls` 数组，后接按同一顺序排列的 `role=tool` 结果消息。工具占位行 SHALL 通过 `tool_call_id` join `tool_calls` 表取回 payload。除压缩显式改写上下文外，重建产出的 durable canonical 请求前缀 SHALL 与当初 live agent durable canonical round 的对应消息序列逐 byte 一致。重建的 row attribution SHALL 覆盖合并 assistant/tool 单元的全部来源行，压缩边界 SHALL NOT 将该单元的 assistant 消息与其工具结果分离。

重建 SHALL 保留已知的 assistant round 与 turn 边界。相邻的工具占位行及其关联 assistant 终稿如果属于不同的已知 TraceID，SHALL 关闭前一单元后再重建后一单元，即使前一单元没有 token/reasoning 或后续 assistant 文本。下一 turn 的 send-time synthetic recall placeholder SHALL NOT 并入上一 turn 的工具调用数组。相同 turn、相同 assistant round 内的多个 placeholder SHALL 仍合并为一个完整并行工具单元，不得因为每个 placeholder 都可取到 canonical result 而逐个拆组。缺失 TraceID 的遗留行 SHALL 沿用已有可确定的边界，不凭工具名、call ID 前缀或完成顺序推断不存在的 turn 身份。

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

#### Scenario: 不同 turn 的相邻工具占位行不合并
- **WHEN** turn A 的工具终稿及已完成 call A 后紧邻 turn B 的已完成 recall placeholder，二者之间没有 token/reasoning 或后续 assistant 文本
- **THEN** 重建输出为 `assistant[A] → tool[A] → assistant[recall-B] → tool[recall-B]`
- **AND THEN** 不产生包含 A 与 recall-B 的同一个 assistant tool_calls 数组

#### Scenario: 无文本的同轮并行占位行仍合组
- **WHEN** 一个 tool-only assistant round 的三个 placeholder 均能取到已完成的 canonical result
- **THEN** 三个调用仍重建到一条 assistant 消息，后接三个按调用序排列的结果
- **AND THEN** 不因第一个 placeholder 已有 result 而提前关闭该 round

#### Scenario: 同 turn 连续 tool-only round 保留既有边界
- **WHEN** 同一 TraceID 中 round 1 与 round 2 均无 content/reasoning，但有独立的 assistant 终稿边界及各自工具占位行
- **THEN** 两轮重建为两个独立的 assistant/tool 单元
- **AND THEN** 同 TraceID 不作为把不同 assistant round 合并的理由

#### Scenario: 拆分后来源行归属只覆盖各自工具单元
- **WHEN** 不同 turn 的相邻工具占位行被重建为两个独立单元
- **THEN** 每个单元的 assistant attribution 与结果 attribution 只覆盖该单元自己的 canonical 来源行
- **AND THEN** 基于 attribution 的压缩保留边界不会把两个 turn 合并或拆开各自的调用与结果

#### Scenario: 上一 turn 的 narration 不附着到新召回
- **WHEN** 上一 turn 的 assistant content/reasoning 或终稿后紧邻另一 TraceID 的 synthetic recall placeholder
- **THEN** 既有 assistant 内容保持自身的原 wire-format 单元，新召回保持独立 assistant/tool 对
- **AND THEN** 新召回不吸收上一 turn 的 content 或 reasoning，不改变已经发送过的历史前缀

### Requirement: Streaming chunks live in Redis until round completion
进行中 round 的流式内容 SHALL 仅存在于 Redis（复用 run event log）；round 完成批写成功后，对应 in-flight 累积 SHALL 被清理。msgflush chunk 级全局队列与 flusher SHALL 退役；用户消息 send-time 持久化 SHALL 保留。

#### Scenario: crash 丢当前 round
- **WHEN** 进程在 round N 进行中崩溃
- **THEN** round 1..N-1 已落库可见，round N 内容丢失，无半截行进入历史

#### Scenario: cancel/error 持久化截断终稿
- **WHEN** turn 被取消或 provider 返回错误时 round N 已累积部分内容
- **THEN** round N 累积内容作为截断 assistant 终稿行落库并标注 interrupted

### Requirement: Sub-agent content is excluded from messages
持久化层 SHALL 按事件 Meta 的 `agent_instance_id` 分流：非空的事件 SHALL NOT 写入 `messages` 或 `tool_calls`，其 transcript 由既有 `subagent_runs` 快照提供。

#### Scenario: 子 agent 流不进 messages，spawn 占位行携带实例链接
- **WHEN** 子 agent 运行产生 token/tool 事件
- **THEN** SSE 与 `subagent_runs` 快照正常
- **AND** `messages` 与 `tool_calls` 无该子 agent 的内容行
- **AND** 对应 spawn_subagent 占位行携带 `agent_instance_id`（该子 agent 实例），作为前端懒加载 transcript 的链接键
- **AND** 重建时该占位行仍参与 prompt 历史（它是父级调用，不是子 agent 产出行）

### Requirement: Legacy chunk rows read dual-mode without migration
存量事件行 SHALL NOT 迁移。重建路径 SHALL 按 event type 双模：终稿行直读组装，遗留 chunk 行走既有合并逻辑，二者产出一致的请求序列。

#### Scenario: 旧会话照常重建
- **WHEN** 会话历史全部为迁移前的 chunk 行
- **THEN** prompt 重建与迁移前行为一致

