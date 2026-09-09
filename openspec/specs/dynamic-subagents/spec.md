# dynamic-subagents Specification

## Purpose

定义通用子 Agent 派发契约：主 Agent 通过单一 spawn_subagent 工具按任务需要动态创建任意多个通用子 Agent，子 Agent 能力来自父 Agent 派发时写的任务 prompt 与工具收窄，而非预定义角色；覆盖授权边界、全树预算、per-run 历史持久化、子 Agent 间完全隔离。

## Requirements

### Requirement: Single spawn_subagent dispatch tool
主 Agent SHALL 通过唯一的 `spawn_subagent` function-calling 工具派发子 Agent，取代固定角色的 `invoke_chongzhi` / `invoke_liang`。工具参数 SHALL 为：`task`（必填字符串）、`context`（可选字符串）、`name`（可选归因标签）、`tools`（可选工具名数组）、`preset`（可选配置模板名）。每次 accepted dispatch SHALL 创建新的 `agent_instance_id` 与新的唯一归因名；工具契约 SHALL NOT 包含或接受 `resume_agent_id`。缺少 `task`、`task` 为空白，或参数包含 `resume_agent_id` 时，派发 SHALL 以参数错误（bad_args）失败并将错误文本作为 tool result 返回主 Agent，不中断主 Agent 循环。

#### Scenario: Spawn with task only
- **WHEN** 主 Agent 调用 `spawn_subagent` 且参数仅含非空 `task`
- **THEN** 系统创建一个全新通用子 Agent 实例执行该任务并返回结构化结果

#### Scenario: Missing task rejected
- **WHEN** `spawn_subagent` 调用缺少 `task` 或 `task` 为空白
- **THEN** 该 tool call 返回 `isError` 为 true 的 bad_args 结果，主 Agent 循环继续

#### Scenario: Resume argument rejected
- **WHEN** `spawn_subagent` 参数包含 `resume_agent_id`
- **THEN** 该 tool call 返回 bad_args 结果，不加载既有实例，也不创建子 Agent LLM 调用

#### Scenario: Legacy invoke tools no longer exist
- **WHEN** 主 Agent 的工具列表被构建
- **THEN** 列表包含 `spawn_subagent` 且不包含 `invoke_chongzhi` / `invoke_liang`

### Requirement: Structured spawn result contract
每次 spawn 派发 SHALL 返回结构化结果：最终回答文本与 `status`（`completed` / `capped` / `error`）。结果内容 SHALL NOT 向模型暴露 `agent_instance_id` 或其他可用于续跑既有实例的身份标记。`capped` SHALL 表示该次隔离执行因 round 上限未自然完成；需要继续工作时，主 Agent SHALL 发起一个包含必要上下文的全新 spawn。`error` 结果 SHALL 保持「错误文本 + 已积累部分输出」的合并语义。

#### Scenario: Completed result carries status only
- **WHEN** 子 Agent 自然完成并产出最终回答
- **THEN** tool result content 为回答文本 + `status: completed` 标记，且不包含 `agent_id`

#### Scenario: Capped result is not model-resumable
- **WHEN** 子 Agent 命中 max_rounds 上限受控终止
- **THEN** tool result 携带 `status: capped`，但不携带可传回 `spawn_subagent` 的实例身份

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
