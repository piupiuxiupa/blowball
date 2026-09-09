## ADDED Requirements

### Requirement: Sub-agent placeholder history mode
系统 SHALL 支持已鉴权的 `GET /api/v1/sessions/:session_id/messages?subagent_content=placeholder` 查询模式。该模式 SHALL 保留非子 Agent 消息与主 Agent 输出；对携带动态子 Agent 身份的行，仅返回 `agent_start`、`agent_end`、`agent_error` lifecycle 占位，MUST NOT 返回其 `token`、`reasoning`、`tool_call` 或 `tool_result` 长内容。与 `subagent_runs.run_id` 对应的父 `spawn_subagent` tool result SHALL 被省略，避免重复返回子 Agent 最终输出。占位过滤 SHALL 在分页前应用，且返回的 lifecycle 行 MUST 继续携带 `agent_instance_id` 与 `run_id` 供详情 API 懒加载。

#### Scenario: Placeholder mode returns lifecycle markers only
- **WHEN** 历史中某个动态子 Agent 包含 agent_start、reasoning、token、tool_call、tool_result 与 agent_end 行
- **THEN** placeholder 模式返回该子 Agent 的 lifecycle marker，并携带相同 `agent_instance_id` / `run_id`
- **AND THEN** 不返回该子 Agent 的 reasoning、token、tool_call 或 tool_result 内容行

#### Scenario: Parent spawn result deduplicated
- **WHEN** Confucius 的 `spawn_subagent` tool result 与某个 `subagent_runs.run_id` 对应
- **THEN** placeholder 模式省略该父 tool result；前端可通过对应子 Agent run detail API 懒加载完整结果

#### Scenario: Full mode remains default
- **WHEN** 请求省略 `subagent_content` 或显式使用 `full`
- **THEN** 响应保持既有完整事件流行为

#### Scenario: Unknown mode rejected
- **WHEN** `subagent_content` 不是 `full` 或 `placeholder`
- **THEN** 系统返回 HTTP 400，且不读取消息分页数据
