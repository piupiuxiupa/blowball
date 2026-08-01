## ADDED Requirements

### Requirement: On-demand MCP tool discovery tool
系统 SHALL 提供 `mcp_list_tools(server)` agent 工具，实时连接指定的 per-user server、执行 `tools/list`，并返回该 server 全部工具的 `name`/`description`/`input_schema`。这是 agent 发现 per-user MCP 工具契约（工具名与入参 schema）的权威入口。

#### Scenario: Discover a server's tools live
- **WHEN** agent 调用 `mcp_list_tools("github")` 指向一个已配置的 server
- **THEN** 系统连接该 server、执行 `tools/list`，返回其全部工具的 name/description/input_schema

#### Scenario: Unknown server rejected
- **WHEN** `mcp_list_tools` 的 `server` 不在调用者已配置的服务中
- **THEN** 系统返回明确错误，不发起连接

#### Scenario: Connect failure surfaced
- **WHEN** `mcp_list_tools` 连接或 `tools/list` 失败（超时、拒连、远端错误）
- **THEN** 返回明确错误给 agent，不回写缓存

### Requirement: Asynchronous cache write-back after discovery
`mcp_list_tools` 取得 live 工具列表后 SHALL 通过 fire-and-forget goroutine 把结果回写到该 server 的 config 缓存（`{name}/config.json` 的 `tools` 字段）。回写 SHALL 使用独立于 turn 的 context（`context.Background()` 配独立超时），SHALL NOT 阻塞 `mcp_list_tools` 的返回，SHALL NOT 影响主调用流程或 turn 生命周期。回写失败 SHALL 仅记录日志、SHALL NOT 向模型或用户报错。

#### Scenario: Live result returned before write completes
- **WHEN** agent 调用 `mcp_list_tools`
- **THEN** live 工具列表立即返回给模型，不等待缓存回写完成

#### Scenario: Write-back does not block main flow
- **WHEN** `mcp_list_tools` 触发异步回写
- **THEN** 主调用、turn 推进、turn 结束的 `Manager.Close()` 均不被回写阻塞

#### Scenario: Write-back survives turn cancellation
- **WHEN** turn 在回写完成前结束（turn ctx 被取消）
- **THEN** 回写仍以独立 context 完成（或在其独立超时内失败），不因 turn ctx 取消而中断

#### Scenario: Write-back failure is non-fatal
- **WHEN** 异步回写因 I/O 或超时失败
- **THEN** 仅记录警告日志，`mcp_list_tools` 的已返回结果与后续 turn 行为不受影响

### Requirement: mcp_list_tools joins the mcp_* tool family
`mcp_list_tools` SHALL 与 `mcp_list_servers`/`mcp_add_server`/`mcp_remove_server`/`mcp_call` 同属 per-user `mcp_*` 工具族：turn-scoped、per-user、复用同一 turn 级 `Manager`，在任一 agent 的 `tools` 列出 `mcp_*` 工具时随族注册。

#### Scenario: Registered alongside the family
- **WHEN** 任一 agent 配置了任一 `mcp_*` 工具
- **THEN** `mcp_list_tools` 与其他 `mcp_*` 工具一并注册到该 turn 的 per-agent registry

#### Scenario: Reuses turn-scoped manager
- **WHEN** agent 调用 `mcp_list_tools`
- **THEN** 复用本 turn 的 per-user MCP 连接（与 `mcp_call` 共享同一 `Manager` 与连接缓存）

## MODIFIED Requirements

### Requirement: System prompt integration for per-user MCP
系统 SHALL 在系统提示词中渲染该用户的 per-user MCP 服务（server 级描述），并引导 agent 在执行任务前主动评估并选择最合适的 skill 与 MCP 服务。提示词 SHALL 声明 `.blowball/mcp/` 仅由 `mcp_*` 工具管理（与既有 `.blowball/skills/` 约束并列）。提示词 SHALL 进一步声明 per-user MCP 的**调用规约**：在 `mcp_call` 之前 SHALL 先调用 `mcp_list_tools(server)` 了解该 server 的工具名与入参 schema，SHALL NOT 凭猜测调用工具名或构造入参。

#### Scenario: User servers described in system prompt
- **WHEN** 某用户已配置 per-user MCP 服务
- **THEN** 该 agent 的系统提示词包含这些 server 的描述（name + description）

#### Scenario: Prompt states the list-before-call convention
- **WHEN** 系统渲染系统提示词的 per-user MCP 段落
- **THEN** 段落明确指示：调用某 server 前先用 `mcp_list_tools` 获取其工具名与 schema，禁止猜测工具名或入参形状（错误会在发起远端调用前被拒）

#### Scenario: Prompt nudges proactive selection
- **WHEN** 系统渲染系统提示词
- **THEN** 提示词包含引导 agent 在执行任务前主动选择最合适 skill 与 MCP 服务的指示

#### Scenario: Reserved-namespace management constraint stated
- **WHEN** 系统渲染系统提示词
- **THEN** 提示词声明 `.blowball/mcp/` 仅由 `mcp_*` 工具管理，不得经 `xizhi_*` 访问
