# dynamic-subagents Specification

## Purpose

定义通用子 Agent 派发契约：主 Agent 通过单一 spawn_subagent 工具按任务需要动态创建任意多个通用子 Agent，子 Agent 能力来自父 Agent 派发时写的任务 prompt 与工具收窄，而非预定义角色；覆盖授权边界、全树预算、历史快照与续跑、子 Agent 间完全隔离。

## Requirements

### Requirement: Single spawn_subagent dispatch tool
主 Agent SHALL 通过唯一的 `spawn_subagent` function-calling 工具派发子 Agent，取代固定角色的 `invoke_chongzhi` / `invoke_liang`。工具参数 SHALL 为：`task`（必填字符串）、`context`（可选字符串）、`name`（可选归因标签）、`tools`（可选工具名数组）、`preset`（可选配置模板名）、`resume_agent_id`（可选续跑目标）。缺少 `task` 或 `task` 为空白时，派发 SHALL 以参数错误（bad_args）失败并将错误文本作为 tool result 返回主 Agent，不中断主 Agent 循环。

#### Scenario: Spawn with task only
- **WHEN** 主 Agent 调用 `spawn_subagent` 且参数仅含非空 `task`
- **THEN** 系统创建一个通用子 Agent 执行该任务并返回结构化结果

#### Scenario: Missing task rejected
- **WHEN** `spawn_subagent` 调用缺少 `task` 或 `task` 为空白
- **THEN** 该 tool call 返回 `isError` 为 true 的 bad_args 结果，主 Agent 循环继续

#### Scenario: Legacy invoke tools no longer exist
- **WHEN** 主 Agent 的工具列表被构建
- **THEN** 列表包含 `spawn_subagent` 且不包含 `invoke_chongzhi` / `invoke_liang`

### Requirement: Structured spawn result contract
每次 spawn 派发 SHALL 返回结构化结果：最终回答文本、系统分配的 `agent_instance_id`（稳定实例标识，跨续跑不变）、以及 `status`（`completed` / `capped` / `error`）。结果内容 SHALL 末尾附带机器可读的状态标记（agent id 与 status），使主 Agent 能在后续轮次引用该实例续跑。`capped` SHALL 表示子 Agent 因 round 上限未自然完成、可通过 `resume_agent_id` 继续；`error` 结果 SHALL 保持「错误文本 + 已积累部分输出」的既有合并语义并同样附带 agent id（若已分配）。

#### Scenario: Completed result carries instance identity
- **WHEN** 子 Agent 自然完成并产出最终回答
- **THEN** tool result content 为回答文本 + `agent_instance_id` 与 `status: completed` 标记

#### Scenario: Capped result is marked resumable
- **WHEN** 子 Agent 命中 max_rounds 上限受控终止
- **THEN** tool result 携带 `status: capped` 与 `agent_instance_id`，主 Agent 可据此发起续跑

#### Scenario: Failed result keeps partial output semantics
- **WHEN** 子 Agent 执行失败被放弃
- **THEN** tool result 保持错误文本 + 部分输出的合并文本与 `isError` 语义，并附带 `status: error`

### Requirement: Generic sub-agent execution
子 Agent SHALL 为通用实现：统一的 tool-calling 循环、通用 system prompt（来自配置默认值或 preset 模板）、独立上下文。子 Agent 的「能力」SHALL 完全由派发参数决定：任务 prompt、context、工具收窄与 preset 模板；系统 SHALL NOT 内置按角色区分的调度画像。主 Agent 一轮内的多个 `spawn_subagent` tool call SHALL 并行执行，且各自获得相互隔离的 run 状态。

#### Scenario: Capability comes from the dispatch prompt
- **WHEN** 两次 spawn 分别携带「只读分析代码」与「修改文件并跑测试」的任务 prompt
- **THEN** 两个子 Agent 使用同一通用执行循环，行为差异仅来自任务 prompt 与工具收窄

#### Scenario: Parallel spawns execute concurrently
- **WHEN** 主 Agent 一轮返回 3 个 `spawn_subagent` tool call
- **THEN** 三次派发并行执行，互不共享可变 run 状态

### Requirement: Tool narrowing is subset-only
`tools` 参数 SHALL 仅允许从父 Agent 当前有效工具集中**收窄**（取子集）；父 Agent 不持有的工具 SHALL NOT 通过 spawn 授予子 Agent。`tools` 中包含父 Agent 不持有或无法解析的工具名时，派发 SHALL 以 bad_args 失败并指明无效名称。未提供 `tools` 时，子 Agent SHALL 继承父 Agent 的完整有效工具集（扣除派发工具本身）。preset 模板声明的工具集 SHALL 同样受收窄规则约束；spawn 显式 `tools` 覆盖模板声明。

#### Scenario: Narrowed tool set granted
- **WHEN** spawn 携带 `tools: ["xizhi_read_file"]` 且该工具在父 Agent 有效工具集中
- **THEN** 子 Agent 的工具列表仅含 `xizhi_read_file`

#### Scenario: Expansion attempt rejected
- **WHEN** spawn 携带 `tools: ["some_tool_parent_lacks"]`
- **THEN** 派发以 bad_args 失败，错误文本指明该工具不可授予

#### Scenario: Default inherits parent set
- **WHEN** spawn 未提供 `tools` 且未选择声明工具的 preset
- **THEN** 子 Agent 继承父 Agent 有效工具集（不含 `spawn_subagent`，除非深度预算允许）

### Requirement: Tree-wide budgets
系统 SHALL 以每回合为根维护全树预算：`max_depth`（嵌套深度上限，默认 1）、`max_concurrent`（全树同时运行的派发数上限，含所有后代）、`max_total_per_turn`（全树派生总数上限）。深度预算未耗尽的子 Agent SHALL 获得 `spawn_subagent` 工具；深度已耗尽的子 Agent 工具列表 SHALL NOT 包含 `spawn_subagent`。超出并发预算的派发 SHALL 等待槽位释放后执行而非失败；超出总数预算的派发 SHALL 立即返回预算错误 tool result。

#### Scenario: Default depth keeps flat behavior
- **WHEN** `max_depth` 为默认值 1
- **THEN** 子 Agent 工具列表不含 `spawn_subagent`，行为等价于现行扁平拓扑

#### Scenario: Nested spawn at depth two
- **WHEN** `max_depth >= 2` 且深度 1 的子 Agent 调用 `spawn_subagent`
- **THEN** 孙 Agent 正常创建执行，事件与用量按父链归因

#### Scenario: Depth cap removes spawn tool
- **WHEN** 子 Agent 已处于 `max_depth` 深度
- **THEN** 其工具列表不含 `spawn_subagent`，模型无从发起更深派发

#### Scenario: Concurrency slots shared across tree
- **WHEN** 全树已有 `max_concurrent` 个派发在运行且某子 Agent 发起新 spawn
- **THEN** 新派发等待槽位释放后执行

#### Scenario: Total budget exhausted
- **WHEN** 本回合全树派生总数达到 `max_total_per_turn` 后再次 spawn
- **THEN** 该派发返回预算错误 tool result，主 Agent 循环继续

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

### Requirement: Full isolation between sub-agents
子 Agent 之间 SHALL 完全隔离：系统 SHALL NOT 提供子 Agent 间直接通信通道（mailbox、兄弟互发、共享黑板）；一切协调 SHALL 经父 Agent 中转——父通过任务 prompt 下发信息，子通过最终结果回报。子 Agent SHALL NOT 获得引用其他实例上下文的机制。

#### Scenario: No direct sibling channel
- **WHEN** 两个子 Agent 并行运行
- **THEN** 二者间不存在任何系统提供的通信路径，各自只见父派发的任务

#### Scenario: Coordination flows through parent
- **WHEN** 主 Agent 需要综合两个子 Agent 的发现并派生后续工作
- **THEN** 其读取两次 spawn 的返回结果后自行发起新派发，子 Agent 之间不直接交换数据

### Requirement: Sub-agent presets are configuration templates
配置 SHALL 支持可选的命名 preset 模板（system prompt、工具集声明），`preset` 参数按名引用；模板仅提供派发默认值，SHALL NOT 恢复固定角色调度。未知 preset 名 SHALL 以 bad_args 失败。

#### Scenario: Preset supplies prompt defaults
- **WHEN** spawn 携带 `preset: "reviewer"`
- **THEN** 子 Agent 使用该模板的 system prompt 与工具集（仍受收窄规则约束）

#### Scenario: Unknown preset rejected
- **WHEN** spawn 携带配置中不存在的 preset 名
- **THEN** 派发以 bad_args 失败并指明未知模板名
