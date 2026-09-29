## MODIFIED Requirements

### Requirement: MCP tools endpoint returns only MCP-sourced tools
`GET /api/v1/mcp/tools` SHALL 仅返回 **MCP 来源**的工具，SHALL NOT 返回任何内置工具（`xizhi_*`/`webfetch`/`executor`/`luban_*`）或合成的 `invoke_*` 调度工具。返回的工具由三类构成：operator（全局）MCP 的 proxy 工具、调用者 per-user MCP 各 server 缓存中的工具，以及（启用 mcp-market 时）调用者市场授权 server 缓存中的工具（工作空间同名优先遮蔽市场条目）。

#### Scenario: Built-in tools excluded
- **WHEN** 调用 `GET /api/v1/mcp/tools`
- **THEN** 响应不含 `xizhi_*`、`webfetch`、executor 工具、`luban_*`、`invoke_chongzhi`、`invoke_liang` 等任何非 MCP 工具

#### Scenario: Operator MCP proxy tools included
- **WHEN** 进程 registry 中存在 operator MCP server 注册的 proxy 工具
- **THEN** 这些 proxy 工具（带 name/description/parameters）出现在响应中

#### Scenario: Per-user cached tools included
- **WHEN** 调用者工作空间的 per-user MCP 配置缓存里某 server 含 `tools` 缓存
- **THEN** 这些缓存工具（name/description/input_schema）出现在响应中

#### Scenario: Market cached tools included
- **WHEN** mcp-market 启用且调用者市场授权的某 server 的 `config.json` 含 `tools` 缓存
- **THEN** 这些缓存工具出现在响应中，来源归属该市场 server 名；工作空间同名 server 遮蔽时市场工具不出现

#### Scenario: Market failure degrades to workspace-only
- **WHEN** 市场 allowlist 拉取失败（不可达、超时、非 200、鉴权失败）
- **THEN** 端点仍返回 operator 与工作空间缓存工具，不报错，不回退为目录扫描

#### Scenario: Each tool attributable to its source
- **WHEN** 端点构造响应
- **THEN** 每个工具条目携带其来源标识（operator 全局 server 名、per-user server 名 或 市场 server 名），以便区分来源
