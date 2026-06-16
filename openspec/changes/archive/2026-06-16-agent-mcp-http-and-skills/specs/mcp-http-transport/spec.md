## ADDED Requirements

### Requirement: HTTP transport declaration
系统 SHALL 允许在 `mcp.servers` 中声明 `transport: http` 的外部 MCP 服务，并校验其必需字段。

#### Scenario: Valid HTTP server configuration
- **WHEN** `mcp.servers` 中某条目 `transport` 为 `http` 且包含非空 `url`
- **THEN** 系统启动时成功解析该 server

#### Scenario: Invalid HTTP server configuration fails fast
- **WHEN** `transport` 为 `http` 但缺少 `url`
- **THEN** 系统启动校验失败并报告缺少 url

### Requirement: Streamable HTTP JSON-RPC
HTTP transport SHALL 使用 HTTP POST 发送 JSON-RPC 请求，支持 `initialize`、`tools/list`、`tools/call` 三个方法。

#### Scenario: Send initialize via POST
- **WHEN** 系统连接 HTTP MCP server
- **THEN** 它发送 POST 请求，body 为 `initialize` JSON-RPC

#### Scenario: Send tools/call via POST
- **WHEN** agent 调用 HTTP MCP 代理工具
- **THEN** 系统发送 POST 请求，body 为 `tools/call` JSON-RPC

### Requirement: Session header handling
HTTP transport SHALL 在 `initialize` 响应中读取 `Mcp-Session-Id` header，并在后续请求中附加该 header。

#### Scenario: Attach session ID to subsequent requests
- **WHEN** `initialize` 返回 `Mcp-Session-Id: abc123`
- **THEN** 后续 `tools/list` 和 `tools/call` 请求包含 `Mcp-Session-Id: abc123`

### Requirement: Session expiration recovery
HTTP transport SHALL 在检测到 session 失效时自动重新 `initialize` 并重试原请求一次。

#### Scenario: Retry after session expired
- **WHEN** `tools/call` 返回 session 失效错误
- **THEN** 系统重新 `initialize` 后再次发起 `tools/call`，并将最终结果返回给 agent

#### Scenario: Fail after retry still fails
- **WHEN** 重新 `initialize` 后的重试仍然失败
- **THEN** 系统返回错误给 agent，不再无限重试
