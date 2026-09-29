## 1. 配置与市场客户端

- [x] 1.1 `internal/config`：新增 `MCPMarketConfig`（镜像 `SkillMarketConfig`：url/timeout/cache_ttl、defaults 5s/60s、IsEnabled、applyDefaults、validate），接入 Load 流程与 `config.example.yaml` 说明块
- [x] 1.2 新建 `internal/mcpmarket` 包：镜像 `skillmarket.Client`（JWT allowlist 拉取 `{"servers":[...]}`、per-user TTL 缓存、fail-closed、Entry path Join/Clean/within-root 逃逸防护、MarketRoot），复用 `skillmarket.WithToken`/`TokenFromContext`
- [x] 1.3 `internal/mcpmarket` 单测：禁用返回 nil、成功解析、fail-closed 降级、空 token 零出站、TTL 单次出站、per-user 隔离、path 逃逸丢弃

## 2. mcp 工具识别市场路径

- [x] 2.1 `internal/tool/mcp`：新增市场合并加载 `loadServers(ctx, workspaceRoot, market)`（工作空间优先遮蔽；市场条目读 `config.json` 过 `Server` schema + `validateServer` + `ValidateName`，非法跳过记日志，磁盘缺失报 disk-sync 错误），`Tools.WithMarket` 注入
- [x] 2.2 `mcp_list_servers`：并入市场 server 并标注来源（脱敏沿用 `serverView`）
- [x] 2.3 `mcp_call` / `mcp_list_tools`：server 解析走合并加载；缓存写回（`persistRefreshedTools`/`WriteServer`）跳过市场条目
- [x] 2.4 `mcp_remove_server`：仅存在于市场的名字返回 "market server cannot be removed"；`mcp_add_server` 查重仅看工作空间（允许遮蔽市场）
- [x] 2.5 `internal/tool/mcp` 单测：合并/遮蔽、市场条目只读（remove 拒绝、写回跳过）、磁盘缺失明确报错、fail-closed 行为

## 3. 提示词与 API 合并

- [x] 3.1 `internal/agent/orchestrator.go` `collectUserMCPServers`：并入市场 server（标注来源，工作空间优先），市场失败 fail-closed
- [x] 3.2 `internal/handler/mcp.go` `GET /api/v1/mcp/tools`：并入市场缓存工具（零 MCP 连接，来源归属市场 server 名，fail-closed 到仅工作空间），补 handler 测试
- [x] 3.3 检查 `api/openapi.yaml` 的 tools 端点描述是否需要来源字段语义更新（无路径/结构变更则仅文档措辞）

## 4. 启动 wiring 与安全

- [x] 4.1 `cmd/blowball/serve.go`：`{data-dir}/mcp-market` 目录创建、`mcpmarket.New` wiring、Landlock 只读集纳入
- [x] 4.2 端到端/集成测试：禁用零行为变化；启用后 list/call 市场路径、remove 拒绝、遮蔽与恢复

## 5. 验证与收尾

- [x] 5.1 `gofmt` / `go vet ./...` / `make test` 全绿
- [x] 5.2 更新 `config.example.yaml` 与相关文档，确认无凭据入口新增
