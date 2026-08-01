## Context

per-user MCP（`user-mcp-configuration`）采用 **meta-tool 模式**：只给模型 `mcp_list_servers`/`mcp_add_server`/`mcp_remove_server`/`mcp_call` 四个工具，远端工具不作为一等公民进 `tools[]` 数组。这与 operator MCP（每个远端工具注册成独立 `ToolSpec`、模型直接看到全 schema）结构性不同。

meta-tool 模式的代价是**发现盲区**：`mcp_list_servers` 返回的 `serverView.Tools` 只是 `int`（`auth.go:84`），系统提示词的 `## User MCP Servers` 只渲染 name/description/url。模型因此不知道某 server 究竟有哪些工具、入参是什么，只能猜工具名去 `mcp_call`，而 `mcp_call` 又用缓存 schema 预校验（`register.go` 的 `callTool`）→ 猜错即拒。`mcp_add_server` 写入的 `tools` 缓存（`ToolCache`：name/description/input_schema）只服务于 `mcp_call` 的校验，从无工具把它返回给模型。

同时 `GET /api/v1/mcp/tools`（`internal/handler/mcp.go` 的 `MCPHandler.Tools`）把进程 registry 的全部工具（`xizhi_*`/`webfetch`/`executor`/`luban_*` + operator MCP proxy）外加合成的 `invoke_chongzhi`/`invoke_liang` 一股脑返回，语义杂糅；且它只遍历进程 registry，**完全看不到** per-user MCP（那些是 turn-scoped、不入 registry）。

## Goals / Non-Goals

**Goals:**
- 新增 `mcp_list_tools(server)`：live 连接单个 per-user server，返回其全部工具的 name/description/input_schema，消除发现盲区。
- `mcp_list_tools` 异步回写缓存（goroutine + 后台 ctx），不阻塞主流程、失败不影响调用结果。
- 系统提示词强制"先 list_tools 再 call、禁止猜测"的调用规约。
- `GET /api/v1/mcp/tools` 回归"只列 MCP 工具"：operator proxy（registry）+ per-user 缓存（用户 config），剔除一切内置工具。
- 端点零网络、纯读缓存，新鲜度交由 `mcp_list_tools`/`mcp_add_server`/`mcp_call` 的缓存写入者保证。

**Non-Goals:**
- **不**把 per-user MCP 工具改成一等公民（每个远端工具独立进 `tools[]`）。锁定 meta-tool + `mcp_list_tools` 路线。
- **不**让 `/mcp/tools` 端点发起任何 MCP 连接（用户 MCP 部分基于缓存）。
- **不**修改 `mcp_call` 的预校验逻辑、turn-scoped 连接生命周期、凭据隔离等既有不变量。
- **不**新增端点的鉴权模型（仍 JWT）。

## Decisions

### 1. `mcp_list_tools` 实时连单 server，复用 `Manager.ListServerTools`
- **Rationale**: 单 server 连接，无 N-server 并行问题；`Manager.ListServerTools`（`manager.go`）已实现"连/复用 + 刷新 tools/list + 更新内存快照"，直接复用，避免第三条 live 连接路径漂移。live 的两条路径（`mcp_list_tools`、`mcp_call` cache-miss 刷新）都在 turn 级 `Manager` 内、共用 `connect`，天然一致。
- **Alternative**: 返回 config 缓存、可选 `refresh:true` 才 live。被否——用户明确"实时获取"。缓存的新鲜度问题交给异步回写解决。

### 2. 异步 goroutine 回写缓存，用 `context.Background()` + 独立超时
- **Rationale**: 主调用应立即把 live 列表返回给模型；回写是副作用，不该阻塞。turn 一结束 turn ctx 即取消，回写必须用独立 ctx（`context.Background()` + 写超时，如 5s）才能存活于 turn 之外。失败仅 `logger.Warn`，不向模型/用户暴露——它是 best-effort 缓存维护。
- **竞态**: per-server-dir 布局（`per-user-mcp-storage`）让回写只动单个 `{name}/config.json`，窗口窄；同一 server 的并发回写以 last-write-wins 收敛，而该文件本质是缓存（权威数据在远端），最多短暂 stale，可接受，无需加锁。
- **生命周期**: 回写 goroutine 不依赖 turn 级 `Manager`（turn 可能已 close）；它只需 `workspaceRoot` + 工具列表 + 文件路径，走独立的 `WriteServer`。注意：goroutine 捕获的数据必须是值拷贝，不得引用 turn 级可变状态。

### 3. `/mcp/tools` 端点：缓存优先、零网络、剔除内置
- **Rationale**: 端点是检视/列举用途（前端、运维），调用稀疏、需快速响应——不该在每次 GET 扇出 N 条 live 连接。读 registry（operator proxy）+ 用户 config 缓存（per-user）即可，零网络、无部分失败。新鲜度由 agent 运行时的缓存写入者（`mcp_list_tools` 回写、`mcp_add_server` 初次写、`mcp_call` cache-miss 刷新）保证。
- **不对称是刻意的**: 端点（缓存，稀疏、需快）与 `mcp_list_tools`（live，agent 运行时发现）数据源策略不同，是职责分离，记录于此免后人困惑。

### 4. 端点筛 operator proxy 靠 server 归属映射，不靠名字模式
- **Rationale**: operator proxy 工具名带 per-server 配置的 prefix（`mcpclient.Client.PrefixedName`），无法靠正则可靠识别。`NewMCPHandler` 增接 `mcpclient.Manager.ServerTools()` 的归属映射来判定哪些 registry 工具是 MCP proxy。
- **wiring**: handler 还需 userID→workspaceRoot 解析（JWT）以读用户 config，复用 `wireAgent` 既有的 `workspaceRootForUser` 闭包。该端点属 agent-only 路由（`RegisterAgentRoutes`），api role 不持有这些依赖，不受影响。

### 5. 工具响应可归属到来源
- **Rationale**: 端点返回 operator proxy 与 per-user 缓存工具两类，前端/调用方需区分。每个工具条目携带来源信息（global operator server 名 / per-user server 名），具体字段名属设计细节。

## Risks / Trade-offs

- **[Risk] 模型忘记调 `mcp_list_tools` 仍会猜** → **Mitigation**: 系统提示词显式规约 + `mcp_call` 被拒错误信息提示"先调 mcp_list_tools 了解工具"；`mcp_list_tools` 工具描述强调其作用。若实际仍频繁猜，后续可考虑一等公民方案（本变更 Non-goal）。
- **[Risk] 异步回写 goroutine 泄漏或拖慢关闭** → **Mitigation**: 回写带独立超时；不持有 turn 级资源；进程退出时未完成的 best-effort 写丢失可接受（缓存而已）。
- **[Risk] 端点读到的用户缓存与 server 实际工具漂移** → **Mitigation**: 漂移只影响"列举"视图，不影响 agent 实际调用（`mcp_call` cache-miss 会 live 刷新）；用户/agent 主动 `mcp_list_tools` 即同步缓存。
- **[Risk] 端点新 shape 破坏既有前端消费者** → **Mitigation**: 内置工具从响应移除是 breaking，需同步前端；`openapi.yaml` 更新契约并同步 `blowball-frontend`。
- **[Risk] `mcp_list_tools` 频繁调用打满远端** → **Mitigation**: 单 turn 内模型通常 list 一次即可；可后续加 turn 内去重缓存（本变更 Non-goal）。

## Open Questions

- 端点响应里每个工具的"来源"字段命名（如 `source`/`server`）？倾向 `server`（operator server 名或 per-user server 名），最终实现时定。
- `mcp_list_tools` 是否在工具描述中明示"会更新缓存"？倾向是，便于模型理解后续 `mcp_call` 为何能通过校验。
