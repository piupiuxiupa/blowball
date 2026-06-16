## Why

Blowball 已经通过 `add-mcp-client-support` 接入了 SSE 和 stdio 两种 MCP 传输，并能把远端 MCP 工具注册到进程级 registry 供 agent 调用。但当前所有 agent 共享同一套工具集，且系统提示词是静态的，无法让模型感知到：

1. 哪些 MCP 服务和工具对它可用；
2. 哪些 skill（全局 + 当前用户）对它可用；
3. 如何通过 `read_skill` 工具加载 skill 指令。

本变更在保留现有能力的基础上，新增 Streamable HTTP MCP 传输、按 agent 配置的 MCP 与 skill 权限、以及把可用能力注入系统提示词的能力，使模型能在运行时正确选择外部工具和 skill。

## What Changes

- **新增 MCP Streamable HTTP 传输**：在 `mcpclient` 中实现 `transport: http`，支持 `initialize` / `tools/list` / `tools/call`，并完整实现 `Mcp-Session-Id` 的缓存与过期自动重初始化。
- **新增 Agent 级 MCP 配置**：每个 agent 可独立配置允许访问的 MCP server 及该 server 下的具体工具列表，支持 `"*"` 通配全部工具。
- **新增 Agent 级 skill 配置**：每个 agent 可独立配置允许使用的 skill 名称列表。skill 来源包括全局 `{project_root}/skills/` 和当前用户 `{data}/{userID}/skills/`。
- **对齐 Agent Skills 规范**：skill 采用 `{skill-name}/SKILL.md` 子目录结构，`SKILL.md` 顶部包含 YAML frontmatter（`name`、`description`）。
- **新增 `read_skill` 工具**：agent 可通过该工具按 name 加载 skill 完整内容，替代直接文件读取。
- **系统提示词动态注入**：在 `AgentFactory.Build` 阶段，根据 agent 配置、当前用户、workspace 构建完整 system prompt，包含环境信息、可用工具（内置 + MCP）、可用 skill XML 列表及使用说明。
- **改造 `AgentFactory.Build` 签名**：从 `Build(workspaceRoot string)` 改为 `Build(workspaceRoot, userID string)`，以支持加载用户级 skill。
- **调整 `GET /api/v1/skills` 扫描逻辑**：从扫描 skill 目录下的平铺文件改为扫描子目录中的 `SKILL.md`。

## Capabilities

### New Capabilities

- `mcp-http-transport`: 定义 blowball 作为 MCP 客户端通过 Streamable HTTP 连接外部 MCP 服务的能力，包括 session 生命周期管理。
- `agent-mcp-configuration`: 定义按 agent 配置 MCP server 及工具白名单的能力。
- `agent-skill-configuration`: 定义按 agent 配置可用 skill、发现全局与用户 skill、以及把 skill 目录注入系统提示词的能力。
- `read-skill-tool`: 定义 `read_skill` 工具的接口与行为，使 agent 能按名称加载 skill 完整指令。

### Modified Capabilities

- `agent-orchestration`: `AgentFactory.Build` 需要 `userID` 以加载用户 skill；system prompt 构建从静态变为动态，整合工具与 skill 目录。
- `mcp-client`: 新增 HTTP transport；需要维护 server 到工具列表的映射，以支持按 agent 配置过滤工具。

## Impact

- **配置变更**：`config.yaml` 的 `agents.*` 段新增 `mcp` 和 `skills` 字段；`mcp.servers` 新增 `transport: http` 选项。
- **代码变更**：
  - `internal/tool/mcpclient/`：新增 HTTP transport、session 管理。
  - `internal/config/config.go`：新增 agent MCP 与 skill 配置结构。
  - `internal/agent/orchestrator.go`、`prompts.go`、`tools.go`：动态构建 system prompt，过滤 MCP 工具。
  - `internal/tool/skill/`（新增）：skill 发现、读取、`read_skill` 工具实现。
  - `internal/handler/skill.go`：适配新的 skill 目录结构。
  - `cmd/server/main.go`：注入 skill store，调整 orchestrator 构建。
- **API 行为变更**：`GET /api/v1/skills` 返回的 skill 标识与扫描方式改变（子目录 + SKILL.md）。
- **迁移**：现有 per-user 平铺 skill 文件需要手动迁移到 `{skill-name}/SKILL.md` 结构。
