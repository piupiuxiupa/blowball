## MODIFIED Requirements

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
