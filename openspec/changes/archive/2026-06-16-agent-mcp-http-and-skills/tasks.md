## 1. Config and Data Model

- [x] 1.1 Add `AgentMCPConfig` and `AgentMCPServerConfig` structs to `internal/config/config.go`
- [x] 1.2 Add `Skills []string` field to `AgentConfig` in `internal/config/config.go`
- [x] 1.3 Add validation for `agents.*.mcp.servers[].name` referencing existing global MCP servers
- [x] 1.4 Add validation for `agents.*.mcp.servers[].tools` referencing existing remote tools
- [x] 1.5 Add validation for `agents.*.skills` referencing existing global or user skill directories
- [x] 1.6 Update `config.example.yaml` with new `mcp` and `skills` agent fields and `transport: http` example
- [x] 1.7 Add config tests for new validation rules

## 2. MCP HTTP Transport

- [x] 2.1 Create `internal/tool/mcpclient/http.go` implementing `Transport` interface
- [x] 2.2 Implement JSON-RPC POST request builder shared with SSE/stdio protocol layer
- [x] 2.3 Implement `Mcp-Session-Id` extraction from `initialize` response and caching
- [x] 2.4 Attach cached `Mcp-Session-Id` to subsequent `tools/list` and `tools/call` requests
- [x] 2.5 Implement session expiration detection and automatic re-initialize with one retry
- [x] 2.6 Support HTTP servers that do not return `Mcp-Session-Id`
- [x] 2.7 Wire `transport: http` into `mcpclient.TransportFactory`
- [x] 2.8 Add unit tests for HTTP transport with mock MCP server
- [x] 2.9 Add integration test for session expiration and retry

## 3. MCP Tool Ownership and Filtering

- [x] 3.1 Extend `mcpclient.Client` or `Manager` to maintain `map[serverName][]prefixedToolName`
- [x] 3.2 Update `registerServerTools` to populate the ownership mapping
- [x] 3.3 Expose the ownership mapping to `agent` package via a new interface or function
- [x] 3.4 Add `baseRegistry` tool filtering in `orchestratorFactory.Build` based on `cfg.MCP.Servers`
- [x] 3.5 Implement wildcard `"*"` support for per-server tool allowlist
- [x] 3.6 Add tests for tool filtering logic

## 4. Skill Discovery and Reading

- [x] 4.1 Create `internal/tool/skill/` package with skill discovery and metadata parsing
- [x] 4.2 Implement subdirectory scanning for `{skill-name}/SKILL.md` in global and user directories
- [x] 4.3 Implement YAML frontmatter parser extracting `name` and `description`
- [x] 4.4 Implement user skill override global skill precedence
- [x] 4.5 Implement `read_skill` tool with name lookup, size limit, and frontmatter stripping
- [x] 4.6 Register `read_skill` tool into process-wide registry when at least one agent has skills configured
- [x] 4.7 Add tests for skill discovery, parsing, and `read_skill` behavior

## 5. Skill Handler Update

- [x] 5.1 Update `internal/handler/skill.go` to scan skill subdirectories instead of flat files
- [x] 5.2 Update `skillEntry` response if needed (keep `name`, `filename`, `size`, `update_time`)
- [x] 5.3 Add handler tests for subdirectory structure

## 6. Dynamic System Prompt

- [x] 6.1 Change `AgentFactory` interface `Build(workspaceRoot string)` to `Build(workspaceRoot, userID string)`
- [x] 6.2 Update `orchestratorFactory.Build` to accept and use `userID`
- [x] 6.3 Update `Orchestrator.Handle` and handler adapter to pass `userID` to `Build`
- [x] 6.4 Implement system prompt renderer combining static prompt, env, available tools, and skill catalog
- [x] 6.5 Implement built-in tool listing for system prompt (name + description)
- [x] 6.6 Implement MCP tool listing grouped by server for system prompt
- [x] 6.7 Implement skill XML catalog injection with name, description, location
- [x] 6.8 Update `Confuse`, `Chongzhi`, `Liang` to store and return the full system prompt
- [x] 6.9 Add tests verifying system prompt content includes correct tools and skills

## 7. Main Wiring and Startup

- [x] 7.1 Create skill store/loader in `cmd/server/main.go` with global and data-root paths
- [x] 7.2 Pass skill loader to `AgentFactory` / `Orchestrator`
- [x] 7.3 Ensure `read_skill` tool is registered before `mcpclient.RegisterAll` or in correct order
- [x] 7.4 Verify startup sequence: config validation → MCP connect → tool registry → orchestrator

## 8. Integration and Verification

- [x] 8.1 Update integration test harness to include mock HTTP MCP server
- [x] 8.2 Add end-to-end test: agent with MCP config sees only allowed tools
- [x] 8.3 Add end-to-end test: agent with skills config receives skill catalog and can call `read_skill`
- [x] 8.4 Run full test suite and fix regressions
- [x] 8.5 Update README or docs with new config examples and skill migration guide
