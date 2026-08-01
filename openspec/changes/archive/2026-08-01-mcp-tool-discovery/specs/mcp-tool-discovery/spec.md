## ADDED Requirements

### Requirement: MCP tools endpoint returns only MCP-sourced tools
`GET /api/v1/mcp/tools` SHALL 仅返回 **MCP 来源**的工具，SHALL NOT 返回任何内置工具（`xizhi_*`/`webfetch`/`executor`/`luban_*`）或合成的 `invoke_*` 调度工具。返回的工具由两类构成：operator（全局）MCP 的 proxy 工具，与调用者 per-user MCP 各 server 缓存中的工具。

#### Scenario: Built-in tools excluded
- **WHEN** 调用 `GET /api/v1/mcp/tools`
- **THEN** 响应不含 `xizhi_*`、`webfetch`、executor 工具、`luban_*`、`invoke_chongzhi`、`invoke_liang` 等任何非 MCP 工具

#### Scenario: Operator MCP proxy tools included
- **WHEN** 进程 registry 中存在 operator MCP server 注册的 proxy 工具
- **THEN** 这些 proxy 工具（带 name/description/parameters）出现在响应中

#### Scenario: Per-user cached tools included
- **WHEN** 调用者工作空间的 per-user MCP 配置缓存里某 server 含 `tools` 缓存
- **THEN** 这些缓存工具（name/description/input_schema）出现在响应中

#### Scenario: Each tool attributable to its source
- **WHEN** 端点构造响应
- **THEN** 每个工具条目携带其来源标识（operator 全局 server 名 或 per-user server 名），以便区分两类来源

### Requirement: Endpoint is cache-based and makes no MCP connections
端点的用户 MCP 部分 SHALL 仅读取调用者工作空间的 config 缓存，SHALL NOT 对任何 per-user server 发起 MCP 连接或 `tools/list`。新鲜度由 `mcp_list_tools`/`mcp_add_server`/`mcp_call` 的缓存写入者维护。

#### Scenario: No network fan-out on list
- **WHEN** 调用 `GET /api/v1/mcp/tools` 且调用者配置了多个 per-user server
- **THEN** 端点不发起任何 MCP 连接，仅读 registry 与 config 缓存，响应延迟与 server 数量无关（无网络扇出）

#### Scenario: Missing user config yields only global tools
- **WHEN** 调用者工作空间无 per-user MCP 配置或缓存为空
- **THEN** 端点正常返回 operator（全局）MCP 工具（可能为空），不报错

#### Scenario: Malformed single server cache omitted
- **WHEN** 调用者某 per-user server 的缓存不可读或格式错误
- **THEN** 该 server 的工具被省略，其他 server 与全局工具仍正常返回，端点不报整体错误

### Requirement: Endpoint resolves caller workspace from JWT
端点 SHALL 从请求的 JWT 推导 `userID`，再解析到该用户的工作空间根，以读取其 per-user MCP config 缓存。该端点 SHALL 仅在 agent 角色路由分区注册。

#### Scenario: Workspace derived from authenticated user
- **WHEN** 已认证用户调用 `GET /api/v1/mcp/tools`
- **THEN** 系统读取 `data/{userID}/workspace/.blowball/mcp/` 下的 per-user 缓存，绝不读取其他用户的缓存
