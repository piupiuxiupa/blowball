## Why

Blowball currently exposes its own tool catalogue via `GET /api/v1/mcp/tools`, but the agents can only invoke built-in tools (`xizhi_*`, `webfetch`). External MCP servers cannot be configured or called. To let agents use third-party capabilities (search, code execution, knowledge bases, etc.) without rebuilding blowball, we need first-class MCP client support.

## What Changes

- Add an `mcp.servers` section to `config.yaml` so operators can declare external MCP servers.
- Support both **SSE over HTTP** and **stdio (local subprocess)** MCP transports.
- At startup, initialize each configured server, call `tools/list`, and register proxy tools into the process-wide tool registry.
- Preserve existing per-request workspace-scoped registry behavior: external MCP proxies are carried from the base registry into each request's registry.
- Keep remote tool names unchanged by default; detect and fail fast on name collisions with local tools or other MCP servers, with an optional per-server `prefix` override.
- Add per-server auth (`headers` for SSE, `env` for stdio), connect/call timeouts, reconnect/restart behavior, and in-memory tool-list caching.
- Update the existing `/api/v1/mcp/tools` endpoint so it returns the merged catalogue including external MCP tools.

## Capabilities

### New Capabilities

- `mcp-client`: External MCP server integration — connection management, transport abstraction (SSE/stdio), tool discovery, proxy execution, auth, timeouts, and lifecycle handling.

### Modified Capabilities

- `agent-orchestration`: Agents can now be configured to invoke tools that are proxied from external MCP servers, in addition to built-in tools.
- `workspace-api`: The existing `GET /api/v1/mcp/tools` endpoint will include external MCP tools in its response, expanding the catalogue returned to clients.

## Impact

- **Config**: `internal/config/config.go` gains MCP configuration structs; `config.example.yaml` is updated.
- **Tool layer**: New `internal/tool/mcpclient` package with SSE/stdio transports, JSON-RPC client, and proxy registration.
- **Agent layer**: `Orchestrator`/`AgentFactory` already copies non-workspace tools from the base registry, so no agent dispatch changes are required.
- **HTTP layer**: `internal/handler/mcp.go` already lists the registry, so external tools appear automatically.
- **Dependencies**: May introduce an MCP Go SDK or implement a minimal JSON-RPC/SSE client in-tree (decision in `design.md`).
- **Security**: External servers gain a runtime path to execute code or access data; the proposal includes allowlist-by-config, auth injection, timeouts, and subprocess/network sandbox considerations.
