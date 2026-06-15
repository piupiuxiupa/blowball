## Why

目前 blowball 的 Agent 只能使用 `xizhi_read_file`、`xizhi_write_file`、`xizhi_modify_file` 三种文件工具。当用户要求 Agent 探索工作空间、理解项目结构或检索多个文件时，Agent 缺乏高效的目录发现和批量搜索能力；同时 Agent 也无法访问外部网页获取信息。增加目录列举、树形展示、glob 搜索和网页抓取四类工具，可以让 Agent 在无需人工逐文件指定路径的情况下自主探索工作空间和网络资源。

## What Changes

- 在 `internal/tool/xizhi/` 中新增三个 workspace-scoped 文件发现工具：
  - `xizhi_list_files`：平铺列出指定目录下的文件和子目录（默认不显示隐藏文件）。
  - `xizhi_tree`：递归输出目录树结构，默认深度 3，最大深度 10。
  - `xizhi_glob_files`：基于 `doublestar` glob 模式在工作空间内递归搜索文件和目录。
- 新增 `internal/tool/webfetch/` 包，实现进程级网络工具 `webfetch`：
  - 支持 GET/POST 等方法，允许跟随重定向，默认超时 30s。
  - 返回最终 URL、HTTP 状态码、响应头和文本响应体。
- 扩展 `config.yaml` 的 `tools` 配置：
  - `tools.xizhi` 下增加 `list_files`、`tree`、`glob_files` 的 `enabled` 开关。
  - 新增 `tools.webfetch` 配置组，包含 `enabled` 和 `timeout`。
- 更新所有 Agent 的默认 `tools` 列表，使上述新工具对 Confuse、Chongzhi、Liang 均可用。
- 更新 MCP tools 列表端点，使其包含新注册的工具定义。

## Capabilities

### New Capabilities

- `xizhi-list-files`：平铺列出用户工作空间指定目录下的文件和子目录。
- `xizhi-tree`：以树形结构递归展示用户工作空间目录，支持深度限制。
- `xizhi-glob-files`：使用 `doublestar` glob 模式在用户工作空间内递归搜索文件和目录。
- `webfetch`：抓取外部网页内容并返回文本响应，支持重定向和超时配置。

### Modified Capabilities

- `xizhi-tools`：扩展 Xizhi 工具集的定义范围，将新增的文件发现工具纳入同一注册表、路径校验和配置体系；更新 `tools.xizhi` 配置结构以支持新工具的启用开关。

## Impact

- 新增代码：`internal/tool/xizhi/list.go`、`tree.go`、`glob.go` 及测试；`internal/tool/webfetch/fetch.go` 及测试。
- 修改代码：`internal/tool/xizhi/register.go`、`internal/config/config.go`、`internal/agent/orchestrator.go` 中的 `isXizhiTool` 判断、`config.yaml`、`config.example.yaml`。
- 依赖：引入 `github.com/bmatcuk/doublestar/v4` 用于递归 glob 匹配。
- API 影响：MCP `/api/v1/mcp/tools` 返回的工具列表会增加 4 个新条目；无破坏性变更。
