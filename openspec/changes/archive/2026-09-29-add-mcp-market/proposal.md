## Why

技能侧已有 skill-market（远端授权 + operator 同步磁盘 + fail-closed），MCP 服务没有对等入口：per-user MCP 只能识别工作空间内 `.blowball/mcp/` 自建配置。需要新增 `mcp_market`，让市场下发的 MCP 服务与 skill-market 同构地进入 `mcp_*` 工具的识别路径。

## What Changes

- 新增顶层 `mcp_market:` 配置块（`url` / `timeout` / `cache_ttl`），语义与 `skill_market:` 一致：空 `url` 即关闭，行为与引入前逐字节一致。
- 新增 `{data-dir}/mcp-market/{market_user_id}/{server_name}/config.json` 磁盘布局，operator 同步维护，blowball 只读。
- 新增 per-user MCP 市场 allowlist 客户端：以调用者 login JWT 拉取（复用 skillmarket 的 turn-context token 管道）、per-user TTL 缓存、fail-closed 降级、路径逃逸防护，永不回退为目录扫描。
- `mcp_list_servers` / `mcp_call` 识别市场来源：工作空间同名优先（local-first 遮蔽市场条目），列表标注来源；`mcp_remove_server` 拒绝删除市场条目并返回明确错误。
- `mcp_list_tools` / `mcp_call` 的 tools/list 缓存写回 SHALL 跳过市场条目（目录只读，仅内存刷新）。
- 系统提示词的 per-user MCP 服务广告 SHALL 包含市场来源服务。
- `GET /api/v1/mcp/tools` SHALL 并入市场服务的缓存工具（仅读缓存，不发 MCP 连接），来源归属市场 server 名，市场失败 fail-closed 到仅工作空间结果。
- 凭据管理不在本变更范围：`config.json` 按现有 per-user `Server` schema 解析与校验，凭据由用户经其他入口自行处理；本变更不新增任何凭据写入/管理入口。

## Capabilities

### New Capabilities

- `mcp-market`: MCP 市场能力本体——配置块、JWT allowlist 拉取、TTL 缓存、fail-closed、磁盘布局与路径安全、只读不可删、local-first 名字优先级、磁盘同步滞后容忍。

### Modified Capabilities

- `user-mcp-configuration`: `mcp_list_servers` / `mcp_call` / `mcp_remove_server` / 缓存写回 / 系统提示词集成需纳入市场来源与只读约束。
- `mcp-tool-discovery`: `GET /api/v1/mcp/tools` 需并入市场服务的缓存工具并保持零 MCP 连接。

## Impact

- 代码：`internal/config`（新配置块）、新 `internal/mcpmarket`（镜像 `internal/skillmarket`）、`internal/tool/mcp`（合并加载、list/call/remove、写回跳过）、`internal/agent/orchestrator.go`（提示词广告）、`internal/handler/mcp.go`（tools 端点合并）、`cmd/blowball/serve.go`（wiring：目录创建、Landlock 只读挂载）。
- 配置/部署：`config.example.yaml` 新增 `mcp_market:` 块说明；operator 需同步市场 payload 至 `{data-dir}/mcp-market/`。
- API：`GET /api/v1/mcp/tools` 响应新增市场来源工具（无 breaking）；`api/openapi.yaml` 无路径/结构级变更，来源字段语义扩展。
- 安全：市场目录进 Landlock 只读集；allowlist 为唯一授权面；`serverView` 既有脱敏继续生效。
