## MODIFIED Requirements

### Requirement: MCP server configuration
系统 SHALL 允许在 `config.yaml` 的 `mcp.servers` 段声明一个或多个外部 MCP 服务，每个服务至少包含 `name`、`transport` 及 transport 所需参数。transport 可选值为 `sse`、`stdio`、`http`。

#### Scenario: Valid MCP configuration loads
- **WHEN** `config.yaml` 包含格式正确的 `mcp.servers`
- **THEN** 系统启动时成功解析并为每个 server 建立连接

#### Scenario: Invalid MCP configuration fails fast
- **WHEN** `mcp.servers` 中某条目缺少 `name`、`transport`，或 SSE 缺少 `url`，或 stdio 缺少 `command`，或 HTTP 缺少 `url`
- **THEN** 系统在启动阶段返回验证错误并退出

### Requirement: Integration with agent registry
外部 MCP 代理工具 SHALL 注册在进程级 `baseRegistry` 中，并由 `Orchestrator.AgentFactory` 复制到每次请求的 `reqReg`，使 agent 可以调用。系统 SHALL 同时维护每个 server 到其工具名称列表的映射，以支持按 agent 的 MCP 配置过滤工具。

#### Scenario: Agent sees external MCP tools
- **WHEN** `agents.confuse.mcp.servers` 中声明了一个外部 MCP server 及其允许的工具
- **THEN** Confuse 的工具列表和系统提示词中仅包含这些允许的工具

## ADDED Requirements

### Requirement: HTTP transport support
系统 SHALL 支持通过 MCP Streamable HTTP 连接外部 MCP 服务，并完成 `initialize`、`tools/list`、`tools/call` JSON-RPC 调用。

#### Scenario: Connect to HTTP MCP server
- **WHEN** 配置了一个 `transport: http` 的 server 且远端可达
- **THEN** 系统发送 `initialize` 请求并收到 success 响应

#### Scenario: Call remote tool via HTTP
- **WHEN** agent 调用一个由 HTTP server 代理的工具
- **THEN** 系统通过 HTTP POST `tools/call` 将参数转发到远端，并将远端结果返回给 agent

#### Scenario: HTTP server with custom headers
- **WHEN** `mcp.servers[].headers.Authorization` 配置为 `Bearer ${MCP_TOKEN}`
- **THEN** 系统使用实际环境变量值构造请求头

### Requirement: Session ID management
HTTP transport SHALL 缓存服务端返回的 `Mcp-Session-Id`，并在后续请求中携带该 header。当服务端报告 session 失效时，系统 SHALL 自动重新 `initialize` 获取新 session 并重试一次原请求。

#### Scenario: Cache session ID after initialize
- **WHEN** HTTP MCP server 在 `initialize` 响应中返回 `Mcp-Session-Id`
- **THEN** 后续 `tools/list` 和 `tools/call` 请求均包含该 header

#### Scenario: Re-initialize on session expired
- **WHEN** HTTP MCP server 返回 session 失效错误
- **THEN** 系统自动重新 `initialize`，成功后重试原请求

#### Scenario: Handle server without session ID
- **WHEN** HTTP MCP server 不返回 `Mcp-Session-Id`
- **THEN** 系统按无 session 处理，不强制要求该 header

### Requirement: Tool ownership tracking
系统 SHALL 维护 `serverName → []toolName` 的映射，以支持按 server 过滤工具并在系统提示词中按 server 分组展示 MCP 工具。

#### Scenario: Server tool mapping available at startup
- **WHEN** 所有 MCP server 初始化完成
- **THEN** 系统能回答“某个代理工具来自哪个 MCP server”

#### Scenario: Prefixed tools tracked correctly
- **WHEN** 某 server 配置了 `prefix: remote_`
- **THEN** 映射中存储的是前缀化后的工具名
