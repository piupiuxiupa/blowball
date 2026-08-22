# user-mcp-configuration — Delta Spec

## MODIFIED Requirements

### Requirement: On-demand MCP invocation tool
系统 SHALL 提供 `mcp_call(server, tool, args, output_path?)` 元工具，按需调用某 per-user server 的某工具。远端成功结果按 token 估算判定返回形态：估算值在 inline 阈值内且未指定 `output_path` 时原样返回给 agent；超过阈值或指定 `output_path` 时按 mcp-call-result-spill 能力落盘并返回信封（path + preview + hint）。

#### Scenario: Successful remote tool call
- **WHEN** agent 调用 `mcp_call` 指向已配置 server 的已知工具且 args 合法
- **THEN** 系统连接该 server、转发 `tools/call`，并将远端成功结果返回给 agent（阈值内且未指定 `output_path` 时为原样 inline）

#### Scenario: Unknown tool rejected before call
- **WHEN** `mcp_call` 的 `tool` 不在该 server 缓存的 `tools/list` 中
- **THEN** 系统在发起远端调用前即拒绝并返回明确错误

#### Scenario: Args validated against cached schema
- **WHEN** `mcp_call` 的 `args` 违反该 server 缓存的入参 schema
- **THEN** 系统在发起远端调用前即拒绝并返回明确错误，提示 schema 不符

#### Scenario: Remote tool error surfaced
- **WHEN** 远端 `tools/call` 返回 error 或 `isError=true`
- **THEN** `mcp_call` 返回错误，agent 层将其作为 tool_error 事件流式输出（错误路径不参与 spill 判定）

#### Scenario: Oversized success result returned as spill envelope
- **WHEN** 远端成功结果的估算 token 数超过 inline 阈值
- **THEN** 系统将全量结果落盘到请求用户 workspace 的 `tmp/mcp-outputs/` 下，并向 agent 返回含 `path`/`preview`/`hint` 的信封而非全量内容（详见 mcp-call-result-spill 能力）
