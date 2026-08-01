## 1. Name 校验与配置布局核心

- [x] 1.1 在 `internal/tool/mcp/config.go` 新增 `ValidateName(name string) error`，规则 `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`，并导出供 add/load 路径复用
- [x] 1.2 重写 `ConfigPath` 族：提供 `serversDir(workspaceRoot)`、`serverConfigPath(workspaceRoot, name)`、按需保留 `ConfigPath` 兼容旧调用方或移除
- [x] 1.3 `Config` 结构去掉对单文件 `servers` 数组的依赖；`Server` 不再持久化 `name`（以目录名为准，加载时回填）
- [x] 1.4 重写 `LoadConfig`：`os.ReadDir(serversDir)` → 逐个 `serverConfigPath` 读取 → 跳过非目录/隐藏/未过 name 校验的条目 → 回填 `Server.Name` → 返回 `Config{Servers:...}`；缺失目录返回空 `Config` 不报错

## 2. 写与增删路径

- [x] 2.1 重写 `WriteConfig` 拆分为 per-server 写：`WriteServer(workspaceRoot, server)` 用 `atomicWriteFile` 写单个 `{name}/config.json`；移除整份数组序列化
- [x] 2.2 `AddServer`：先 `ValidateName`，再校验目录不存在（避免覆盖），`MkdirAll` + `WriteServer`
- [x] 2.3 `RemoveServer`：删除 `{name}/` 子目录（含 `config.json`），保留其他服务
- [x] 2.4 `Server(name)`/`index(name)`/`SortedServers()` 适配：`Server`/`index` 基于 `LoadConfig` 结果；`SortedServers` 仍按 name 排序
- [x] 2.5 保留 `Config.Validate()` 对单个 server 字段（url/transport/auth/重名）的校验；重名检查改为"目录已存在"语义

## 3. 调用方适配

- [x] 3.1 `internal/tool/mcp/manager.go`：确认 `LoadConfig`/`Server`/`Conn` 调用在新布局下行为不变（底层枚举已改）
- [x] 3.2 `internal/tool/mcp/register.go`：`addServer` 走新 `AddServer`+`WriteServer`；`removeServer` 走新 `RemoveServer`；`callTool`/`persistRefreshedTools` 的"回写某 server 缓存"改为 `WriteServer` 单文件写
- [x] 3.3 `internal/agent/orchestrator.go`：`collectUserMCPServers` 走新 `LoadConfig`，确认 `SortedServers()` 输出不变
- [x] 3.4 `mcp_list_servers` 的 `serverView` 输出字段不变（name/url/transport/description/redactedAuth/tools 计数）

## 4. 工具描述与错误文案

- [x] 4.1 `mcp_add_server` 工具描述补入 name 命名规则（`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`）与示例
- [x] 4.2 非法 name 的错误信息给出具体规则与正例/反例

## 5. 测试与验收

- [x] 5.1 重写 `internal/tool/mcp/mcp_test.go`：覆盖 per-server-dir 加载、缺失目录=空、单服务损坏=该服务不可用但不崩
- [x] 5.2 新增 name 校验测试：合法名通过；`../x`、`a/b`、`.h`、`a b`、`a.b`、>64 字符被拒
- [x] 5.3 测试 `AddServer`/`RemoveServer`/`WriteServer` 的目录创建/删除与原子写
- [x] 5.4 测试枚举跳过 temp/隐藏/非法名目录
- [x] 5.5 更新 `register_test.go`/orchestrator 相关测试以匹配新存储
- [x] 5.6 `make test` 与 `go test ./internal/tool/mcp/...` 全绿；确认未触碰 operator MCP 路径
