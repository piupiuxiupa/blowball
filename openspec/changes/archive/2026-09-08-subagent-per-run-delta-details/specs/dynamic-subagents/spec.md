## MODIFIED Requirements

### Requirement: Resume with persisted history
系统 SHALL 为每个子 Agent 实例持久化稳定元数据与线性 per-run 消息 delta 链，作为续跑的权威历史；每次 run 的 `messages_json` SHALL 只保存该次执行新增的消息。携带 `resume_agent_id` 的 spawn SHALL 以目标实例的 system prompt 与按序拼接的 run delta 为起点追加新任务消息继续执行；目标实例不存在、不属于当前会话、run 链不可读或拼接后的上下文超过续跑限制时 SHALL 以 bad_args 失败。续跑 SHALL 分配新的 run 身份但沿用同一 `agent_instance_id`；已折叠进历史的孙 Agent 派发 SHALL NOT 复活，续跑实例可在剩余深度与预算内派发新的孙 Agent。

#### Scenario: Resume continues from stitched history
- **WHEN** spawn 携带先前返回的 `resume_agent_id` 与新 `task`
- **THEN** 子 Agent 的初始消息列表为实例 system prompt、按顺序拼接的所有历史 run delta 与新任务 user message

#### Scenario: Same instance across resume
- **WHEN** 同一实例被续跑
- **THEN** 两次执行的流事件携带不同 run 身份但相同 `agent_instance_id`

#### Scenario: Unknown resume target rejected
- **WHEN** `resume_agent_id` 指向不存在或跨会话的实例
- **THEN** 派发以 bad_args 失败并返回错误文本

#### Scenario: Oversized stitched context rejected
- **WHEN** 拼接目标实例的完整模型上下文超过配置的子 Agent 快照大小限制
- **THEN** 派发以 bad_args 失败，且不发起子 Agent LLM 调用

#### Scenario: Grandchildren are not revived
- **WHEN** 被续跑实例的历史中含已完成的孙 Agent 派发
- **THEN** 续跑不重新执行这些孙 Agent；其结果以 tool result 形式保留在历史 delta 中
