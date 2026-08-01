## Why

per-user MCP 配置当前是单个 `.blowball/mcp/config.json`，所有 server 挤在一个 `servers` 数组里。当用户配置的服务变多时，该文件会变得冗长难维护、难并发编辑。同时 server `name` 目前只是 JSON key（仅校验非空），改为按服务拆分目录后 name 会成为路径分量，必须引入路径安全校验以防 `../`、`a/b`、`.hidden` 等穿越/嵌套/覆盖攻击。

## What Changes

- **BREAKING**: per-user MCP 配置存储从单文件 `.blowball/mcp/config.json` 改为**按服务拆分**：每个 server 一个目录 `.blowball/mcp/{name}/config.json`，文件内只放该 server 自身的字段（`url`/`transport`/`auth`/`description`/`tools` 缓存），`name` 以所在目录名为准，不再写入文件体。
- 列举服务 = 枚举 `.blowball/mcp/` 的子目录并各读一个 `config.json`；增删服务 = 增删一个子目录文件。
- 新增严格的 server **name 路径安全校验**：`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`（字母数字开头，仅含字母数字/下划线/连字符，最长 64，禁路径分隔符与点号开头）。该校验在 `mcp_add_server` 与配置加载时统一生效。
- 新布局是**唯一支持格式**：不做单文件→多文件的双读兼容，不承担存量迁移（存量转换由部署侧另行处理）。
- 配置原子写仍走 `atomicWriteFile`（temp + rename，同目录），但因每个 server 独立文件，写操作的竞态窗口从「整份配置」缩小到「单个 server 文件」。

## Capabilities

### New Capabilities
<!-- 无新能力 -->

### Modified Capabilities
- `user-mcp-configuration`: 配置存储位置与 schema 从单文件改为按服务拆分目录；新增 server name 的路径安全校验要求（`name` 成为路径分量后的穿越/嵌套防护）。

## Impact

- 修改 `internal/tool/mcp/config.go`：`ConfigPath` → 按服务解析的路径族；`LoadConfig` 改为枚举子目录并逐个解析；`WriteConfig` 拆为 per-server 写；`AddServer`/`RemoveServer`/`Server`/`index`/`SortedServers` 适配新布局；新增 `ValidateName`。
- 修改 `internal/tool/mcp/manager.go`：`LoadConfig` 调用语义不变，但底层枚举方式改变。
- 修改 `internal/tool/mcp/register.go`：`addServer`/`removeServer`/`callTool`/`persistRefreshedTools` 的读写路径适配 per-server 文件。
- 修改 `internal/agent/orchestrator.go`：`collectUserMCPServers`（系统提示词枚举）走新 LoadConfig。
- 修改 `internal/tool/mcp/mcp_test.go` 与相关测试：覆盖新布局、name 校验、拒绝非法 name。
- 不新增 HTTP API；不修改 operator MCP 路径；不承担存量数据迁移。
