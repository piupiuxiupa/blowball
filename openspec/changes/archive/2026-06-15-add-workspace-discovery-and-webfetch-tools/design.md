## Context

blowball 当前通过 `internal/tool/registry.go` 提供通用工具注册表，`internal/tool/xizhi/` 提供 workspace-scoped 的文件读/写/改工具。每个请求会在 `agent.orchestratorFactory.Build` 中根据用户 workspace 重建一次 Xizhi 工具注册表，以保证用户 A 无法访问用户 B 的文件。

本次变更需要新增三类文件发现工具（`list_files`、`tree`、`glob_files`）和一个网络工具（`webfetch`）。文件发现工具天然继承 Xizhi 的路径沙箱和 per-request 注册机制；`webfetch` 不依赖 workspace，是第一个注册在进程级 base registry 并被 per-request registry 继承的工具。

## Goals / Non-Goals

**Goals：**
- 让 Agent 能够自主探索用户工作空间的目录结构。
- 让 Agent 能够按 glob 模式批量定位文件。
- 让 Agent 能够抓取外部网页文本内容。
- 保持现有 Xizhi 路径安全模型不变（应用层 `validatePath` + landlock 进程级限制）。
- 所有新工具对 Confuse、Chongzhi、Liang 均可用。

**Non-Goals：**
- 不实现文件内容的模糊搜索（如 ripgrep）。
- 不对 `webfetch` 做域名白名单、IP 黑名单或响应体大小限制（按产品决策暂不设置）。
- 不实现网页内容的解析/提取（如只返回原始 HTML/文本）。

## Decisions

### 1. 文件发现工具放入 `internal/tool/xizhi/`

**理由：**
- 它们和现有文件工具共享 workspace 根目录、路径校验和 landlock 沙箱。
- 注册方式与现有工具一致，可直接复用 `xizhi.RegisterAll` 的 per-request 注册。

**替代方案：** 新建 `internal/tool/discovery/` 包。 rejected，因为会引入不必要的包边界，且 discovery 工具仍依赖 Xizhi 的 `validatePath`。

### 2. `glob_files` 使用 `doublestar/v4`

**理由：**
- 标准库 `filepath.Match` 的 `*` 不匹配路径分隔符，无法表达 `src/**/*.go` 这类递归模式。
- `doublestar` 是 Go 生态最常用的扩展 glob 库，API 稳定。

### 3. `tree` 默认深度 3、上限 10

**理由：**
- 默认 3 足以覆盖大多数项目的顶层结构，又不会一次性返回过大结果。
- 上限 10 防止模型或用户传入过大值导致深层递归遍历耗时过长。

### 4. `webfetch` 作为独立包 `internal/tool/webfetch/`

**理由：**
- 它不操作文件系统，也不属于 Xizhi workspace 沙箱。
- 独立包便于后续扩展网络相关策略（超时、重试、代理等）。

### 5. `webfetch` 注册在 base registry，per-request registry 继承

**理由：**
- `webfetch` 的执行不依赖 `workspaceRoot`，无需每个请求重建。
- `orchestratorFactory.Build` 已具备将 base registry 中非 Xizhi 工具复制到 per-request registry 的逻辑，直接复用。

### 6. 配置结构：`tools.xizhi` 扩展 + `tools.webfetch` 新组

**理由：**
- 文件发现工具与现有 Xizhi 工具同族，放在 `tools.xizhi` 下保持配置一致性。
- `webfetch` 有独立的超时配置，单独成组更清晰。

## Risks / Trade-offs

| Risk | Mitigation |
|------|-----------|
| `glob_files` 返回结果过大 | 限制匹配只返回相对路径列表，不读取内容；默认从 workspace 根开始，用户可通过 `path` 缩小范围。 |
| `tree` 在超大目录下递归过深 | 默认深度 3 + 硬上限 10；后续如需要可增加 `max_results` 限制。 |
| `webfetch` 的 SSRF/内网访问风险 | 本次按决策暂不限制；后续如需安全加固，可在 webfetch 包增加 URL 解析与内网/IP 拦截。 |
| `webfetch` 返回二进制响应体 | 当前仅返回文本内容；非 UTF-8 内容按 `string([]byte)` 转换，可能产生乱码，需在工具描述中说明。 |
| Agent 误用工具 | 工具描述中明确每个工具的用途和参数，帮助模型选择正确工具。 |

## Migration Plan

- 代码变更后，`go mod tidy` 会自动拉取 `doublestar/v4`。
- 配置文件 `config.yaml` 和 `config.example.yaml` 需要手工更新以启用新工具；未配置时新工具默认不启用（与现有工具行为一致）。
- 无需数据库迁移或数据回填。
- 回滚：还原相关文件并移除 `doublestar` 依赖即可。

## Open Questions

- 是否需要给 `webfetch` 增加最大响应体限制？当前按决策暂不设置。
- `tree` 的返回结构是否需要扁平化（如每个节点带 `depth`）以方便前端渲染？当前按嵌套 JSON 设计。
