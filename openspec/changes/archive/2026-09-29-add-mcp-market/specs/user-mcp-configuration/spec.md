## MODIFIED Requirements

### Requirement: Per-user MCP configuration management tools
系统 SHALL 提供 `mcp_list_servers`、`mcp_add_server`、`mcp_remove_server` 三个 agent 工具（仿 `luban_*` 模式），作用于调用者本人工作空间的 `.blowball/mcp/{name}/config.json`（per-server 拆分存储）。`mcp_list_servers` SHALL 额外并入 mcp-market 能力授权的市场 server（标注来源），工作空间同名优先遮蔽市场条目；`mcp_remove_server` 仅作用于工作空间配置，市场条目不可删除。

#### Scenario: List user's MCP servers
- **WHEN** agent 调用 `mcp_list_servers`
- **THEN** 返回该用户已配置的 server 列表（name、url、transport、description），认证字段一律脱敏；启用 mcp-market 时市场 server 一并列出并标注来源

#### Scenario: Add a new MCP server
- **WHEN** agent 调用 `mcp_add_server` 并提供合法 name、url、auth、description
- **THEN** 系统将该 server 写入 `.blowball/mcp/{name}/config.json`，并缓存其 `tools/list`（工具名与入参 schema）到该文件

#### Scenario: Remove an MCP server
- **WHEN** agent 调用 `mcp_remove_server` 指定已存在的工作空间 server name
- **THEN** 该 server 的子目录（含 `config.json`）从 `.blowball/mcp/` 移除；若该名字同时存在于市场，市场条目在后续调用中重新可见

#### Scenario: Add server with duplicate name rejected
- **WHEN** `mcp_add_server` 的 name 与已配置的**工作空间** server 重名
- **THEN** 系统拒绝并返回明确错误，不改写既有配置

#### Scenario: Add may shadow a market server
- **WHEN** `mcp_add_server` 的 name 仅与市场 server 同名（工作空间无同名配置）
- **THEN** 系统允许创建，工作空间版本自此遮蔽市场条目（local-first）

#### Scenario: Remove market-only server rejected
- **WHEN** agent 调用 `mcp_remove_server` 指定仅存在于市场的 server
- **THEN** 返回"market server cannot be removed"类明确错误，磁盘无任何变更

### Requirement: On-demand MCP invocation tool
系统 SHALL 提供 `mcp_call(server, tool, args, output_path?)` 元工具，按需调用某 per-user server 的某工具；server 解析顺序 SHALL 为工作空间优先、市场次之（mcp-market 能力启用时）。远端成功结果按 token 估算判定返回形态：估算值在 inline 阈值内且未指定 `output_path` 时原样返回给 agent；超过阈值或指定 `output_path` 时按 mcp-call-result-spill 能力落盘并返回信封（path + preview + hint）。

#### Scenario: Successful remote tool call
- **WHEN** agent 调用 `mcp_call` 指向已配置 server 的已知工具且 args 合法
- **THEN** 系统连接该 server、转发 `tools/call`，并将远端成功结果返回给 agent（阈值内且未指定 `output_path` 时为原样 inline）

#### Scenario: Market server resolved after workspace miss
- **WHEN** agent 调用 `mcp_call(server="foo")`，工作空间无 `foo` 而市场 allowlist 授权了 `foo`
- **THEN** 系统解析到市场 `foo` 的 `config.json` 并按同一 HTTP 调用语义执行，凭据注入与脱敏规则不变

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

### Requirement: On-demand MCP tool discovery tool
系统 SHALL 提供 `mcp_list_tools(server)` agent 工具，实时连接指定的 per-user server（工作空间优先、市场次之）、执行 `tools/list`，并返回该 server 全部工具的 `name`/`description`/`input_schema`。这是 agent 发现 per-user MCP 工具契约（工具名与入参 schema）的权威入口。

#### Scenario: Discover a server's tools live
- **WHEN** agent 调用 `mcp_list_tools("github")` 指向一个已配置的 server
- **THEN** 系统连接该 server、执行 `tools/list`，返回其全部工具的 name/description/input_schema

#### Scenario: Discover a market server's tools
- **WHEN** agent 调用 `mcp_list_tools` 指向市场授权的 server
- **THEN** 系统按其市场 `config.json` 连接并返回工具列表，行为与工作空间 server 一致

#### Scenario: Unknown server rejected
- **WHEN** `mcp_list_tools` 的 `server` 既不在调用者工作空间配置中，也不在市场 allowlist 中
- **THEN** 系统返回明确错误，不发起连接

#### Scenario: Connect failure surfaced
- **WHEN** `mcp_list_tools` 连接或 `tools/list` 失败（超时、拒连、远端错误）
- **THEN** 返回明确错误给 agent，不回写缓存

### Requirement: Asynchronous cache write-back after discovery
`mcp_list_tools` 取得 live 工具列表后 SHALL 通过 fire-and-forget goroutine 把结果回写到该 server 的 config 缓存（`{name}/config.json` 的 `tools` 字段）；**市场来源的 server SHALL 跳过持久化回写**（市场目录只读，刷新仅保留在 turn 内存）。回写 SHALL 使用独立于 turn 的 context（`context.Background()` 配独立超时），SHALL NOT 阻塞 `mcp_list_tools` 的返回，SHALL NOT 影响主调用流程或 turn 生命周期。回写失败 SHALL 仅记录日志、SHALL NOT 向模型或用户报错。

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

#### Scenario: Market server write-back skipped
- **WHEN** `mcp_list_tools` 刷新的是市场来源 server 的工具列表
- **THEN** 刷新结果仅更新 turn 内存，`{data-dir}/mcp-market` 下不发生任何写入

### Requirement: System prompt integration for per-user MCP
系统 SHALL 在系统提示词中渲染该用户的 per-user MCP 服务（server 级描述），并引导 agent 在执行任务前主动评估并选择最合适的 skill 与 MCP 服务。启用 mcp-market 时，广告 SHALL 并入市场授权的 server（工作空间同名优先遮蔽），并标注市场来源。提示词 SHALL 声明 `.blowball/mcp/` 仅由 `mcp_*` 工具管理（与既有 `.blowball/skills/` 约束并列）。提示词 SHALL 进一步声明 per-user MCP 的**调用规约**：在 `mcp_call` 之前 SHALL 先调用 `mcp_list_tools(server)` 了解该 server 的工具名与入参 schema，SHALL NOT 凭猜测调用工具名或构造入参。

#### Scenario: User servers described in system prompt
- **WHEN** 某用户已配置 per-user MCP 服务
- **THEN** 该 agent 的系统提示词包含这些 server 的描述（name + description）

#### Scenario: Market servers advertised with source label
- **WHEN** 市场授权某 server 且未被工作空间同名遮蔽
- **THEN** 该 server 出现在系统提示词的 per-user MCP 段落中，标注市场来源；市场拉取失败时 fail-closed 为仅工作空间服务

#### Scenario: Prompt states the list-before-call convention
- **WHEN** 系统渲染系统提示词的 per-user MCP 段落
- **THEN** 段落明确指示：调用某 server 前先用 `mcp_list_tools` 获取其工具名与 schema，禁止猜测工具名或入参形状（错误会在发起远端调用前被拒）

#### Scenario: Prompt nudges proactive selection
- **WHEN** 系统渲染系统提示词
- **THEN** 提示词包含引导 agent 在执行任务前主动选择最合适 skill 与 MCP 服务的指示

#### Scenario: Reserved-namespace management constraint stated
- **WHEN** 系统渲染系统提示词
- **THEN** 提示词声明 `.blowball/mcp/` 仅由 `mcp_*` 工具管理，不得经 `xizhi_*` 访问
