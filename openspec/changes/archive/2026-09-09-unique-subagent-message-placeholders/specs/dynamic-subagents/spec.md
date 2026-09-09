## MODIFIED Requirements

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

## REMOVED Requirements

### Requirement: Resume with persisted history
**Reason**: Model-driven instance reuse conflicts with the new isolation policy. Persisted per-run transcripts remain available through `subagent-run-transcripts` for frontend lazy loading and auditing.
**Migration**: Callers that need additional work must spawn a fresh sub-agent and include the required prior findings in its self-contained task/context. Existing per-run transcript APIs continue to read historical rows.
