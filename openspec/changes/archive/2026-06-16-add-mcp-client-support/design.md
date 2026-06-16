## Context

Blowball agents today can only invoke built-in tools registered at startup: `xizhi_*` workspace file tools and `webfetch`. The existing `GET /api/v1/mcp/tools` endpoint merely lists these local tools in a MCP-shaped JSON format; there is no MCP protocol client.

The `tool.Registry` abstraction already supports arbitrary tool specs with name, description, JSON schema, and an `Execute` closure. The `Orchestrator` already maintains a process-wide `baseRegistry` and copies its non-workspace-scoped entries into a fresh per-request registry. This means external MCP tools can be registered once at startup and made available to every agent turn without touching the agent dispatch loop.

## Goals / Non-Goals

**Goals:**
- Let operators declare external MCP servers in `config.yaml`.
- Support both SSE-over-HTTP and stdio transports.
- Discover remote tools via `initialize` + `tools/list` and surface them to agents.
- Forward agent tool calls to the correct MCP server via `tools/call`.
- Preserve existing per-request workspace-scoped registry behavior.
- Include external tools in the existing `/api/v1/mcp/tools` catalogue.
- Provide auth, timeouts, reconnect/restart, and tool-list caching.

**Non-Goals:**
- Making blowball itself an MCP server (it already exposes a simple catalogue, but not the full MCP protocol).
- Supporting every MCP feature (resources, prompts, sampling, roots, notifications). Only tool discovery and invocation.
- Modifying the LLM client or agent loop beyond the existing `tool.Registry` integration.
- Runtime reconfiguration of MCP servers without a process restart.

## Decisions

### 1. In-tree minimal MCP client instead of a third-party SDK
**Decision:** Implement a small JSON-RPC/SSE/stdio client inside `internal/tool/mcpclient` rather than importing an MCP SDK.
**Rationale:**
- Keeps dependency tree small and avoids version churn.
- We only need three operations: `initialize`, `tools/list`, `tools/call`.
- The `ToolSpec` model already dictates our wire shape; a generic SDK would require adapter glue anyway.
**Alternative considered:** `github.com/mark3labs/mcp-go` would be more spec-complete but adds a dependency and surface area we do not need today.

### 2. Tool proxy registration into `baseRegistry`
**Decision:** At startup, connect every configured server, call `tools/list`, and register a proxy `ToolSpec` for each remote tool into the process-wide registry. The `Orchestrator.AgentFactory` already copies non-Xizhi specs from `baseRegistry` into each request's registry.
**Rationale:**
- Reuses the existing agent dispatch path (`dispatchRegistryTool` → `toolRegistry.Call`).
- No per-request connection cost; external servers are long-lived.
- Keeps workspace isolation untouched because external tools are not workspace-scoped.

### 3. Keep remote tool names unchanged by default, fail on collision
**Decision:** Proxy tools use the exact name returned by the remote `tools/list`. If the name collides with a built-in tool or another MCP server, startup fails with a clear error. An optional per-server `prefix` can disambiguate.
**Rationale:**
- Matches the user's stated preference for original names.
- Silent shadowing is a security and debuggability risk.
- Optional prefix keeps flexibility for multi-server deployments.

### 4. Transport abstraction
**Decision:** Define a single `Transport` interface with two implementations: `SSETransport` and `StdioTransport`. Both speak the same JSON-RPC request/response envelope.
**Rationale:**
- SSE and stdio differ only in I/O plumbing; the protocol layer is identical.
- A shared interface simplifies the manager and testing (mock transport can exercise protocol logic).

### 5. Tool-list snapshot caching at startup
**Decision:** Cache the result of `tools/list` once per server after successful initialization. Do not refresh automatically during the process lifetime.
**Rationale:**
- Agent prompts are built once per request from the registry; changing the tool set mid-flight would confuse the model.
- A future management endpoint can trigger refresh explicitly if needed.

### 6. Per-server timeouts and reconnect
**Decision:** Config carries `connect_timeout`, `call_timeout`, and `reconnect` flags. SSE uses HTTP-level retry with capped backoff; stdio restarts the subprocess on failure.
**Rationale:**
- External servers are unreliable by default; bounded timeouts prevent agent hangs.
- Reconnect keeps long-running deployments healthy without operator intervention.

### 7. Auth injected via config, never hard-coded
**Decision:** SSE auth uses `headers` map supporting `${VAR}` expansion. Stdio auth uses `env` map injected into the child process.
**Rationale:**
- Consistent with existing config env-substitution (`config.Load` already expands `${VAR}`).
- Keeps secrets out of committed YAML.

## Risks / Trade-offs

| Risk | Mitigation |
|---|---|
| External MCP server executes arbitrary code or exfiltrates data | Servers are explicitly allowlisted in config only; no dynamic discovery. Optional `trusted` flag for future policy enforcement. Stdio subprocesses inherit OS process boundaries. |
| Remote tool name collides with built-in tool | Startup validation fails loudly; optional `prefix` config. |
| MCP server is slow or unresponsive | Per-server `call_timeout`; slow calls return error results to the agent instead of hanging the turn. |
| SSE connection drops between requests | Background reconnect with capped backoff; tool call triggers reconnect attempt if needed. |
| Stdio subprocess dies | Detect on next tool call and attempt restart; fail the call if restart fails. |
| In-tree client lags MCP spec evolution | Scope is intentionally narrow (`initialize`, `tools/list`, `tools/call`). If the spec diverges significantly, migrating to an SDK later is localized to `internal/tool/mcpclient`. |
| Tool list caching hides remote updates | Documented non-goal; operators restart blowball to pick up remote tool changes. |

## Migration Plan

1. **Config update**: Add `mcp.servers` to `config.example.yaml`. Existing configs without the section remain valid because YAML unmarshalling leaves missing structs at zero values.
2. **Code changes**: Introduce `internal/tool/mcpclient`; wire it into `cmd/server/main.go` after `tool.NewRegistry()` and before `NewOrchestrator`.
3. **Validation**: Add startup validation in `config.Config.validate()` for `mcp.servers` entries (non-empty name/transport, URL for SSE, command for stdio).
4. **Tests**: Unit tests for SSE/stdio transports against a mock MCP server; integration test verifying external tools appear in `/api/v1/mcp/tools` and can be called by an agent.
5. **Rollback**: Remove or comment `mcp.servers` from config; the manager skips zero-value config and blowball reverts to built-in tools only.

## Open Questions

- Should we expose a runtime endpoint (e.g., `POST /api/v1/admin/mcp/refresh`) to reload tool lists without restart? Not in scope for the first iteration.
- Should subprocess stdio servers be spawned with additional sandboxing (e.g., `landlock`, seccomp, or chroot)? The current landlock integration only covers blowball's own filesystem access; stdio sandboxing is a future hardening task.
- Do we need per-server circuit-breaker behavior beyond simple reconnect? Start with reconnect; add circuit breaker if operational experience shows thundering retries.
