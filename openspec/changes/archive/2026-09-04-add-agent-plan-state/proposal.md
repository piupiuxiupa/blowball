## Why

动态子 Agent 架构已经把执行身份、执行状态、resume 快照、拓扑预算和用量归因结构化，但 Confucius 的任务分解与语义进度仍只能存在于模型文本中。前端无法稳定渲染当前计划，模型在多轮派发后也只能依赖自然语言记忆；这会削弱并行子 Agent 编排的可观测性和可靠性。

## What Changes

- 新增 Confucius 专属合成工具 `update_plan`：模型以 whole-plan snapshot 提交语义计划，host 校验、归一化并分配递增 revision。
- 新增 turn 级语义计划状态：步骤状态为 `pending` / `in_progress` / `completed`；为匹配并行 `spawn_subagent`，允许多个步骤同时处于 `in_progress`。
- 计划更新在调度层先行处理：同一 assistant round 中的 `update_plan` 必须先于 `spawn_subagent` 与普通工具的并行执行，保证计划事件与执行事件的语义顺序一致。
- 新增 SSE 事件 `plan_updated`：`Content` 携带 host 归一化后的 canonical plan JSON，`Meta.revision` 携带快照版本；事件随现有消息持久化路径落库。
- `update_plan` 的 tool result 返回 host 归一化后的计划快照，使后续 LLM round 看到的是 host 确认状态而非模型原始输入。
- 保持子 Agent 上下文隔离：子 Agent 不能调用 `update_plan`，不能修改根计划；Confucius 必须通过 `spawn_subagent.task` 与 `context` 显式传递必要计划状态。
- 保持语义计划状态与执行实例状态分离：`subagent_runs.status` 的 `completed` / `capped` / `error` 不得被自动映射为计划步骤状态；步骤完成必须由 Confucius 验收后通过 `update_plan` 声明。

## Capabilities

### New Capabilities

- `agent-plan-state`: Confucius 语义计划工具、host 权威快照、多步骤并行进行中、计划更新调度顺序、`plan_updated` 事件与持久化、计划状态向子 Agent 的显式传递边界，以及语义计划状态与动态子 Agent 执行状态的分离。

### Modified Capabilities

_None._ The behavior is additive and is specified as a new orchestration control-plane capability; existing dynamic sub-agent execution, identity, resume, and cost-tracking requirements remain unchanged.

## Impact

- `internal/agent`: 新增计划状态模型与 `update_plan` 合成工具；调整 Confucius tool list 与 dispatch 顺序；更新提示词纪律。
- `internal/stream` / `internal/model` / `internal/handler`: 新增 `plan_updated` 事件类型、事件构造器、消息映射与持久化行为。
- `api/openapi.yaml`: 新增客户端可见的 `plan_updated` SSE 事件契约。
- 前端：可按 `plan_updated` 渲染计划面板，并在历史重放时使用最后一个有效快照。
- 无数据库 schema 变更：V1 使用 turn 内权威状态与既有消息事件持久化；跨 session 的持久任务库不在本变更范围内。
