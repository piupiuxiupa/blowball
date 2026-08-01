## 1. mcp_list_tools 工具

- [x] 1.1 在 `internal/tool/mcp/register.go` 的 `mcpTools`/`IsMCPTool`/`AnyMCPTool` 纳入 `ToolListTools = "mcp_list_tools"` 常量
- [x] 1.2 实现 `registerListTools`：参数 `{server}`；调用 `Manager.ListServerTools(ctx, server)` 取 live 列表；返回 `[{name, description, input_schema}]`
- [x] 1.3 未配置 server / 连接失败 / `tools/list` 失败时返回明确错误，不回写
- [x] 1.4 `RegisterAll` 串联注册 `mcp_list_tools`
- [x] 1.5 工具描述写明：实时获取工具名与 schema、是发现 per-user MCP 工具的权威入口、结果会异步更新缓存

## 2. 异步缓存回写

- [x] 2.1 实现 `writeBackToolsAsync(workspaceRoot, serverName, tools)`：`go func(){ ... }()`，用 `context.Background()` + 独立写超时（如 5s）
- [x] 2.2 goroutine 内只做值拷贝（不引用 turn 级 `Manager`/可变状态），调用 per-server 单文件写（`WriteServer`，依赖 `per-user-mcp-storage`）
- [x] 2.3 回写失败仅 `logger.Warn`，不 panic、不影响已返回结果
- [x] 2.4 `mcp_list_tools` 成功取列表后**先返回**再触发 `writeBackToolsAsync`
- [x] 2.5 测试：回写不阻塞返回；turn ctx 取消后回写仍以独立 ctx 完成；回写失败不影响主流程

## 3. /mcp/tools 端点重构

- [x] 3.1 修改 `internal/handler/mcp.go` `MCPHandler`：增字段持有 server 归属映射（`map[toolName]serverName`，来自 `mcpclient.Manager.ServerTools()`）与 workspace 解析依赖
- [x] 3.2 修改 `NewMCPHandler(reg, serverTools, workspaceRootForUser)` 签名
- [x] 3.3 重写 `Tools()`：
  - 全局 MCP：遍历 `reg.List()`，仅保留在 server 归属映射中的 proxy 工具
  - 用户 MCP：解析 JWT→userID→workspaceRoot，`mcp.LoadConfig` 读各 server 缓存 `tools`，展平为工具条目
  - 剔除 `xizhi_*`/`webfetch`/`executor`/`luban_*`/`invoke_*`
  - 每条工具带来源标识（`server`）
- [x] 3.4 缺失用户 config/空缓存 → 只返回全局工具，不报错；单 server 缓存损坏 → 省略该 server，其余正常
- [x] 3.5 修改 `cmd/blowball/serve.go` `wireAgent`：向 `NewMCPHandler` 注入 `mcpManager.ServerTools()` 与 `workspaceRootForUser` 闭包

## 4. 系统提示词调用规约

- [x] 4.1 修改 `internal/prompt/render.go` 的 `## User MCP Servers` 段落：加入“先 `mcp_list_tools` 再 `mcp_call`、禁止猜测工具名/入参”的调用规约
- [x] 4.2 确认 `.blowball/mcp/` 仅由 `mcp_*` 管理、主动选择 MCP 服务等既有文案保留

## 5. OpenAPI 与前端同步

- [x] 5.1 更新 `api/openapi.yaml`：`GET /api/v1/mcp/tools` 响应 schema 改为仅 MCP 工具 + 来源字段，移除内置工具
- [x] 5.2 同步 `api/openapi.yaml` 到 `blowball-frontend` 并 `npm run generate-api`（提示前端方，本仓库不改前端）

## 6. 测试与验收

- [x] 6.1 `register_test.go`：`mcp_list_tools` live 返回工具列表（mock transport）；未配置 server 报错；连接失败报错不回写
- [x] 6.2 异步回写测试：返回先于回写；turn 取消后回写完成；回写失败仅日志
- [x] 6.3 `mcp_test.go`（handler）：端点剔除内置工具、含 operator proxy、含 per-user 缓存工具、来源标识正确；缺失/损坏用户 config 不报整体错误
- [x] 6.4 提示词渲染测试：`## User MCP Servers` 段落含调用规约
- [x] 6.5 `make test` 与 `go test ./internal/tool/mcp/... ./internal/handler/... ./internal/prompt/...` 全绿
- [x] 6.6 确认 `mcp_call` 预校验、turn-scoped 连接、凭据脱敏等既有不变量未被破坏
