## 1. Config & Validation

- [x] 1.1 Add `MCPConfig` and `MCPServerConfig` structs to `internal/config/config.go` (name, transport, url, command, args, env, headers, timeout, call_timeout, reconnect, prefix).
- [x] 1.2 Add `mcp` field to root `Config` struct and YAML tag.
- [x] 1.3 Implement `Config.validate()` checks for MCP servers (non-empty name/transport, required fields per transport, no duplicate server names).
- [x] 1.4 Update `config.example.yaml` with SSE and stdio server examples and documentation.
- [x] 1.5 Add config unit tests covering valid, invalid, and env-substituted MCP config.

## 2. MCP Client Core

- [x] 2.1 Create `internal/tool/mcpclient` package with JSON-RPC request/response types (`InitializeRequest`, `ToolsListRequest`, `ToolsCallRequest`, etc.).
- [x] 2.2 Define `Transport` interface (`Initialize`, `ListTools`, `CallTool`, `Close`).
- [x] 2.3 Implement SSE transport: connect to `/sse`, POST messages to `/messages`, read SSE events, correlate request/response by id.
- [x] 2.4 Implement stdio transport: spawn subprocess with command/args/env, write JSON-RPC to stdin, read from stdout, manage process lifecycle.
- [x] 2.5 Implement `Client` struct wrapping a `Transport` with `initialize` → `tools/list` flow and tool schema caching.
- [x] 2.6 Add unit tests for SSE and stdio transports using a local test MCP server / mock process.

## 3. Tool Proxy Registration

- [x] 3.1 Implement `mcpclient.RegisterAll(reg *tool.Registry, cfg config.MCPConfig) error` to connect configured servers and register proxy `ToolSpec`s.
- [x] 3.2 Generate proxy `Execute` closures that forward to the correct `Client.CallTool` with timeout and JSON marshalling.
- [x] 3.3 Implement name collision detection against existing registry entries; support optional per-server `prefix`.
- [x] 3.4 Cache discovered tool schemas per client; expose list for observability.
- [x] 3.5 Add unit tests for registration, collision handling, prefix behavior, and proxy execution.

## 4. Server Startup Integration

- [x] 4.1 Call `mcpclient.RegisterAll(baseRegistry, cfg.MCP)` in `cmd/server/main.go` after `tool.NewRegistry()` and before `NewOrchestrator`.
- [x] 4.2 Ensure startup fails fast with clear error messages when MCP server connection or `tools/list` fails.
- [x] 4.3 Wire `Close()` cleanup for SSE/stdio clients into application shutdown path.

## 5. Agent & API Integration

- [x] 5.1 Verify `Orchestrator.Build` copies external MCP proxy tools from `baseRegistry` into the per-request registry.
- [x] 5.2 Verify `GET /api/v1/mcp/tools` returns external MCP tools alongside built-in tools.
- [x] 5.3 Add integration test in `test/integration` or `internal/handler/mcp_test.go` with a mock MCP server registering an external tool and listing it.
- [x] 5.4 Add agent-level integration test verifying an agent configured with an external MCP tool successfully invokes it.

## 6. Documentation & Hardening

- [x] 6.1 Update `README.md` MCP section to explain external server configuration.
- [x] 6.2 Document security considerations (allowlist-only, auth injection, subprocess/network sandboxing as future work).
- [x] 6.3 Review error handling: ensure remote MCP errors are surfaced as `agent_error`/`tool_error` stream events and do not crash the agent loop.
- [x] 6.4 Run full test suite (`go test ./...`) and fix regressions.
