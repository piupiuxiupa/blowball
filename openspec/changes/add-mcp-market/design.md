## Context

skill-market 已建立"远端 allowlist 授权 + operator 同步磁盘 + fail-closed + 永不目录扫描"的模式（`internal/skillmarket`）。per-user MCP 当前只有一个识别来源：`{workspaceRoot}/.blowball/mcp/{name}/config.json`，由 `mcp.LoadConfig` 读取、`mcp_*` 工具族管理。本变更为 MCP 增加与 skill-market 同构的市场来源，磁盘层级为 `{data-dir}/mcp-market/{market_user_id}/{server_name}/config.json`。

凭据决策（已拍板）：市场 `config.json` 按现有 per-user `Server` schema 解析，凭据由用户经其他入口自行处理；本变更不新增任何凭据写入/管理入口，市场目录对 blowball 严格只读、不允许删除。

## Goals / Non-Goals

**Goals:**

- `mcp_list_servers` / `mcp_call` / `mcp_list_tools` 识别市场来源；工作空间同名优先（local-first 遮蔽）。
- 市场 allowlist 复用 skillmarket 的安全语义：调用者 JWT、per-user TTL 缓存、fail-closed、路径逃逸防护、永不目录扫描兜底。
- 市场目录只读：不提供删除/修改/缓存写回路径；`mcp_remove_server` 对市场条目明确报错。
- 系统提示词广告与 `GET /api/v1/mcp/tools` 并入市场服务（后者仍零 MCP 连接）。

**Non-Goals:**

- 凭据的写入、轮换、脱敏存储管理（用户经其他入口自理）。
- operator MCP（`mcp.servers`）路径的任何变更。
- 市场服务的服务端实现与 operator 同步机制本身。

## Decisions

### D1: 新包 `internal/mcpmarket` 镜像 `internal/skillmarket`，不做泛化抽象
两个市场客户端的 Entry 语义、消费点、合并策略不同（skill 是 name→目录 fallback；MCP 是 server 合并 + 遮蔽）。复制约百行已验证的模式，比抽一个共享泛型市场客户端更短、更稳。二者共享的只有 JWT context 管道——直接复用 `skillmarket.WithToken` / `TokenFromContext`（token 是同一 login JWT，与市场种类无关）。

### D2: `LoadConfig` 保持纯本地，合并在消费侧
`mcp.LoadConfig(workspaceRoot)` 有约 30 个调用点且签名无 ctx；把市场客户端塞进去会污染全部调用方。改为在 `internal/tool/mcp` 内新增 `loadServers(ctx, workspaceRoot, market)`：先 `LoadConfig`，再按 allowlist 读市场 `config.json` 合并，工作空间同名胜出。`mcp.Tools.WithMarket(c)` 注入（镜像 luban 的 `WithMarket`）；handler 与 orchestrator 提示词收集各自走同一合并函数。市场条目解析：allowlist `path` → Join/Clean/within-root 校验 → 读该目录 `config.json` → 现有 `Server` schema + `validateServer`，server `name` 取 allowlist `name`（主键），仍过 `ValidateName` 防御。

### D3: 遮蔽而非查重拒绝
skill-market 先例是 local-first：本地同名条目胜出、市场条目隐藏。因此 `mcp_add_server` 查重仅看工作空间配置——用户可以用本地同名服务遮蔽市场服务，删除本地服务后市场条目重新可见。MCP 侧对齐该语义。

### D4: 只读三处收口
1. `mcp_remove_server`：对"仅存在于市场"的名字返回"market server cannot be removed"（不返回 not configured，避免误导）；
2. `mcp_list_tools` / `mcp_call` 的缓存写回（`persistRefreshedTools` / `WriteServer`）：市场条目跳过持久化，仅刷新 turn 内存；
3. `serve.go` 将 `{data-dir}/mcp-market` 纳入 Landlock 只读集并在启动时确保目录存在（镜像 skills-market 的 wiring）。

### D5: `GET /api/v1/mcp/tools` 并入市场缓存工具，仍零 MCP 连接
端点继续只读 `tools` 缓存；市场 `config.json` 自带 operator 同步的缓存字段，读取前经 allowlist（TTL 缓存命中时零出站；miss 时一次市场 HTTP 拉取，受 `timeout` 约束，失败 fail-closed 到仅工作空间结果）。不违反"无网络扇出到 MCP server"的既有约束——市场 allowlist 拉取不是 MCP 连接，与 skill handler 合并市场清单的先例一致。

### D6: 磁盘同步滞后与 skills 同语义
列表/广告以 allowlist 为准（目录未同步不隐藏）；`mcp_call` / `mcp_list_tools` 命中磁盘缺失时返回 "directory not found (disk sync incomplete)" 类明确错误。

## Risks / Trade-offs

- [市场 config.json 可能携带静态凭据] → 复用现有 `validateServer`（禁 OAuth/stdio）与 `serverView` 脱敏；文件信任边界等同本地配置；凭据管理明确不在范围内。
- [市场与工作空间同名冲突的行为差异] → 与 skill-market 完全一致（遮蔽），工具描述与系统提示词写明来源标注，降低 agent 困惑。
- [双市场客户端带来少量重复] → 刻意为之（D1）；若第三个市场出现再抽共享层。
- [tools 端点在 allowlist miss 时多一次市场 HTTP] → 受 `mcp_market.timeout` 约束且 fail-closed；TTL 缓存窗口内零额外出站。

## Migration Plan

1. 配置默认关闭（无 `mcp_market:` 块 = 零行为变化），可独立部署回滚。
2. operator 开始同步 `{data-dir}/mcp-market/` 后，配置 `mcp_market.url` 即启用。
3. 无数据库迁移；回滚 = 删配置块（目录残留无害，Landlock 只读）。
