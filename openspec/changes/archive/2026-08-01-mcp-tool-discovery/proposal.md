## Why

当前 agent 面对用户的 per-user MCP 服务时存在**发现盲区**：系统提示词只渲染 server 的 name/description，`mcp_list_servers` 只返回工具**数量**（不含名字与 schema），模型只能**猜**工具名去调 `mcp_call`，而 `mcp_call` 又会用缓存 schema 预校验——猜错即被拒，陷入"猜→拒→再猜"的循环。同时 `GET /api/v1/mcp/tools` 端点返回的是 `xizhi_*`/`webfetch`/`invoke_*` 等内置工具与 MCP proxy 的大杂烩，无法清晰表达"实际可用的 MCP 工具有哪些"。需要补齐 per-user MCP 的运行时发现能力，并让端点回归"只列 MCP 工具"的纯粹语义。

## What Changes

- **新增 agent 工具 `mcp_list_tools(server)`**：实时连接指定 per-user server，跑 `tools/list`，返回该 server 全部工具的 `name`/`description`/`input_schema`。这是 agent 发现 per-user MCP 工具契约的**唯一权威入口**，消除"猜工具名"。
- **异步缓存回写**：`mcp_list_tools` 取得 live 工具列表后，通过 **fire-and-forget goroutine** 把结果回写到该 server 的 config 缓存（`{name}/config.json` 的 `tools` 字段）。回写使用 `context.Background()` + 独立超时，**不阻塞**主调用返回、**不影响**主流程；失败仅记日志、不向模型/用户报错。
- **系统提示词新增 MCP 调用规约**：在 `## User MCP Servers` 段落强制要求 agent 在 `mcp_call` 前先 `mcp_list_tools(server)` 了解工具名与入参 schema，禁止猜测。
- **重构 `GET /api/v1/mcp/tools` 端点**：**只返回 MCP 来源的工具**，剔除 `xizhi_*`/`webfetch`/`executor`/`luban_*`/`invoke_*` 等内置工具。返回内容：
  - **全局（operator）MCP**：进程 registry 中有 server 归属的 proxy 工具（带完整 schema）；
  - **用户（per-user）MCP**：读取调用者工作空间 config 缓存里每个 server 的 `tools`（`name`/`description`/`input_schema`）。
  - **基于缓存、零网络**：端点不发起任何 MCP 连接，只读 registry 与用户 config 缓存；新鲜度由 `mcp_list_tools`/`mcp_add_server`/`mcp_call` 的缓存写入者保证。
- 端点 handler 增加 userID→workspaceRoot 解析依赖（JWT 推导），用于读用户 config。

## Capabilities

### New Capabilities
- `mcp-tool-discovery`: 定义 `GET /api/v1/mcp/tools` 端点的新契约——仅返回 MCP 来源工具（operator proxy + per-user 缓存），剔除内置工具，基于缓存、零网络，工具可归属到其来源。

### Modified Capabilities
- `user-mcp-configuration`: 新增 `mcp_list_tools(server)` 发现工具（live `tools/list` + 异步 goroutine 缓存回写）；系统提示词集成新增"先 list_tools 再 call、禁止猜测"的调用规约。

## Impact

- 新增工具注册：`internal/tool/mcp/register.go` 增加 `mcp_list_tools`（复用 `Manager.ListServerTools` 取 live 列表；新增异步回写逻辑，复用 `persistRefreshedTools`/`WriteServer` 的单 server 写）。
- 修改 `internal/handler/mcp.go`：`Tools()` 改为只输出 MCP 来源工具；`NewMCPHandler` 增接 server 归属映射（`mcpclient.Manager.ServerTools()`）与 workspace 解析依赖。
- 修改 `cmd/blowball/serve.go`：向 `MCPHandler` 注入 server 归属映射与 `workspaceRootForUser` 闭包。
- 修改 `internal/prompt/render.go`：`## User MCP Servers` 段落加入 MCP 调用规约。
- 修改 `internal/tool/mcp/register.go` 的 `mcpTools` 列表与 `IsMCPTool`/`AnyMCPTool` 纳入 `mcp_list_tools`。
- 测试：端点新 shape、`mcp_list_tools` live 返回 + 异步回写不阻塞主流程、提示词规约渲染。
- **依赖**：本变更的 config 读写建立在 `per-user-mcp-storage`（per-server-dir 布局）之上，应在其之后实现。
