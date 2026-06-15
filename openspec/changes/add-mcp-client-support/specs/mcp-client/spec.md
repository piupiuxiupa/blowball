# mcp-client Specification

## Purpose

定义 blowball 作为 MCP 客户端接入外部 MCP 服务的能力，包括配置、传输层（SSE/stdio）、工具发现、代理执行、认证、超时重连、生命周期与安全边界。

## ADDED Requirements

### Requirement: MCP server configuration
系统 SHALL 允许在 `config.yaml` 的 `mcp.servers` 段声明一个或多个外部 MCP 服务，每个服务至少包含 `name`、`transport` 及 transport 所需参数。

#### Scenario: Valid MCP configuration loads
- **WHEN** `config.yaml` 包含格式正确的 `mcp.servers`
- **THEN** 系统启动时成功解析并为每个 server 建立连接

#### Scenario: Invalid MCP configuration fails fast
- **WHEN** `mcp.servers` 中某条目缺少 `name`、`transport`，或 SSE 缺少 `url`，或 stdio 缺少 `command`
- **THEN** 系统在启动阶段返回验证错误并退出

### Requirement: SSE transport support
系统 SHALL 支持通过 SSE over HTTP 连接外部 MCP 服务，并完成 `initialize`、`tools/list`、`tools/call` JSON-RPC 调用。

#### Scenario: Connect to SSE MCP server
- **WHEN** 配置了一个 `transport: sse` 的 server 且远端可达
- **THEN** 系统建立 SSE 连接，发送 `initialize` 请求并收到 success 响应

#### Scenario: Call remote tool via SSE
- **WHEN** agent 调用一个由 SSE server 代理的工具
- **THEN** 系统通过 SSE `tools/call` 将参数转发到远端，并将远端结果返回给 agent

### Requirement: stdio transport support
系统 SHALL 支持通过启动本地子进程的方式连接 stdio MCP 服务，并向子进程注入 `env` 环境变量。

#### Scenario: Spawn stdio MCP server
- **WHEN** 配置了一个 `transport: stdio` 的 server
- **THEN** 系统启动指定命令，完成 `initialize` 握手

#### Scenario: Call remote tool via stdio
- **WHEN** agent 调用一个由 stdio server 代理的工具
- **THEN** 系统通过子进程 stdin/stdout 发送 `tools/call` 并返回结果

### Requirement: Remote tool discovery
系统 SHALL 在初始化成功后调用远端 `tools/list`，为每个返回的工具在进程级 `tool.Registry` 中注册一个代理 `ToolSpec`。

#### Scenario: Discover multiple remote tools
- **WHEN** 远端 server 返回包含 N 个工具的 `tools/list`
- **THEN** 系统在 base registry 中注册 N 个对应名称的代理工具

#### Scenario: Empty remote tool list
- **WHEN** 远端 server 返回空工具列表
- **THEN** 系统正常完成初始化，不为该 server 注册任何工具

### Requirement: Proxy tool execution
代理工具的 `Execute` callback SHALL 将参数序列化后发送给对应 MCP server 的 `tools/call`，并把远端结果或错误返回。

#### Scenario: Successful remote tool call
- **WHEN** 远端 `tools/call` 返回成功结果
- **THEN** 代理 `Execute` 返回该结果作为 JSON 序列化值

#### Scenario: Remote tool call error
- **WHEN** 远端 `tools/call` 返回 error 或返回内容中包含 `isError=true`
- **THEN** 代理 `Execute` 返回错误，agent 层将其作为 tool_error 事件流式输出

#### Scenario: Remote tool call timeout
- **WHEN** 单次 `tools/call` 执行时间超过配置的 `call_timeout`
- **THEN** 系统取消该调用并返回超时错误给 agent

### Requirement: Authentication injection
SSE server SHALL 支持通过 `headers` 注入认证信息；stdio server SHALL 支持通过 `env` 注入环境变量。二者均支持 `${VAR}` 环境变量展开。

#### Scenario: SSE server with Authorization header
- **WHEN** `mcp.servers[].headers.Authorization` 配置为 `Bearer ${MCP_TOKEN}`
- **THEN** 系统使用实际环境变量值构造请求头

#### Scenario: stdio server with API key env
- **WHEN** `mcp.servers[].env.API_KEY` 配置为 `${LOCAL_API_KEY}`
- **THEN** 启动的子进程在环境变量中看到展开后的值

### Requirement: Tool name uniqueness
远程工具名称 SHALL 在全局 registry 中保持唯一。若与内置工具或其他 MCP server 工具重名，系统 SHALL 在启动时失败。可通过为 server 配置 `prefix` 解决冲突。

#### Scenario: Name collision fails startup
- **WHEN** 远端返回的工具名与 `xizhi_read_file` 冲突
- **THEN** 系统启动失败并报告具体冲突

#### Scenario: Prefix resolves collision
- **WHEN** 为该 server 配置 `prefix: remote_`
- **THEN** 注册的工具名变为 `remote_<original_name>`，不再冲突

### Requirement: Connection resilience
SSE transport SHALL 在连接断开后尝试重连；stdio transport SHALL 在子进程退出后尝试重启。重连/重启失败时后续 tool call 返回错误。

#### Scenario: SSE reconnect after disconnect
- **WHEN** SSE 连接在空闲时断开
- **THEN** 后台尝试重连，下一次 tool call 优先触发重连

#### Scenario: stdio restart after crash
- **WHEN** stdio 子进程异常退出
- **THEN** 下一次 tool call 时尝试重启子进程，重启失败则返回错误

### Requirement: Tool list caching
系统 SHALL 在初始化成功后缓存每个 server 的 `tools/list` 结果，并在进程生命周期内复用，不再动态刷新。

#### Scenario: Stable tool catalogue across requests
- **WHEN** 多个用户请求连续到达
- **THEN** 每个请求看到的工具列表与首次初始化时一致

### Requirement: Explicit allowlist only
系统 SHALL 只连接 `mcp.servers` 中显式配置的 server，禁止运行时动态发现或添加外部 MCP 服务。

#### Scenario: Unconfigured server is unreachable
- **WHEN** 一个未在 `mcp.servers` 中声明的 MCP 端点尝试被使用
- **THEN** 系统拒绝连接，该端点不会出现在工具列表中

### Requirement: Integration with agent registry
外部 MCP 代理工具 SHALL 注册在进程级 `baseRegistry` 中，并由 `Orchestrator.AgentFactory` 复制到每次请求的 `reqReg`，使 agent 可以调用。

#### Scenario: Agent sees external MCP tools
- **WHEN** `agents.confuse.tools` 中包含一个外部 MCP 代理工具名
- **THEN** Confuse 的 tool list 构建成功，agent 运行时可以正常调用
