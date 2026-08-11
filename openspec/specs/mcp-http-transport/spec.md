# mcp-http-transport Specification

## Purpose

定义 MCP Streamable HTTP 传输层的声明、JSON-RPC 通信、session header 管理及 session 失效恢复。

## Requirements

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

### Requirement: Large SSE response handling
Streamable HTTP transport SHALL 在读取 SSE 响应时不以固定单行长度为上限截断消息；承载整条 JSON-RPC 响应的单条 `data:` 消息 SHALL 被完整读取。仅当单条消息的字节数超过显式上限时，系统 SHALL 返回清晰的超限错误，且 SHALL NOT 以 `bufio.Scanner: token too long` 的形式把扫描器内部错误暴露给调用方。

#### Scenario: tools/call result larger than the prior 1 MiB scanner cap
- **WHEN** HTTP MCP server 以单条 `data:` 行返回一个超过 1 MiB 但未超过显式上限的 `tools/call` 成功结果
- **THEN** 系统完整读取该响应并将结果返回给 agent，不返回 token-too-long 类错误

#### Scenario: Oversized SSE response yields a clear error
- **WHEN** HTTP MCP server 返回的单条 `data:` 消息字节数超过显式上限
- **THEN** 系统 DID NOT 静默截断，且返回一个明确的超限错误（可被 `http tools/call:` 上下文包装定位），而非 `bufio.Scanner: token too long`
