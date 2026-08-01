## Why

目前 MCP 配置仅支持 operator 级别（`config.yaml` 的 `mcp.servers`），全局共享、进程启动时连接、工具固定注入。用户无法在**自己的工作空间**里接入私有的 MCP 服务（自带地址、自带认证 key）。行业内的多租户 agent 服务端（ChatGPT connectors、Claude connectors、GitHub Copilot、Vertex、Bedrock）已一致收敛到「per-user 远程 HTTP MCP」模型；blowball 现有的 per-user workspaceRoot（`{dataDir}/{userID}/workspace`）与 xizhi 对 `.blowball/` 的硬拒绝（`validatePath`）恰好使 per-user 凭据隔离**几乎免费**地成立，现在补齐这一层正是时候。

## What Changes

- **新增 per-user MCP 配置存储**：在每个用户工作空间的 `.blowball/mcp/config.json` 中声明该用户的 MCP 服务（name、url、transport、认证、描述），与现有 `.blowball/skills/` 命名空间并列。
- **新增 `mcp_*` agent 工具族**（仿 `luban_*` 模式）：`mcp_list_servers` / `mcp_add_server` / `mcp_remove_server`，让 agent（或用户通过 agent）在工作空间内管理 MCP 配置。
- **新增 `mcp_call` 元工具**：agent 按需调用某 server 的某 tool，带可配置超时（默认 10s），连接在单个 turn 内缓存复用（turn-scoped）。
- **per-user 凭据隔离**：每个用户的请求仅读取该用户工作空间下的配置、注入该用户的认证、建立该用户的连接；用户 A 的 key 永不进入用户 B 的请求路径。
- **传输范围限定**：per-user MCP **仅支持远程 HTTP（Streamable HTTP）+ 静态认证**（bearer/api-key/basic）。stdio 与 OAuth 2.1 流程**不在** per-user 范围内（stdio 维持 operator-only；OAuth 留作未来扩展）。
- **提示词更新**：渲染用户级 MCP 服务描述，并引导 agent 在执行任务前主动选择最合适的 skill 与 MCP 服务；新增「`.blowball/mcp/` 仅由 `mcp_*` 工具管理」的提示约束（与现有 `.blowball/skills/` 约束并列）。
- **三条泄露不变量（实现强制）**：(1) `mcp_*` 工具输出中脱敏认证字段，绝不把 key 回显给模型；(2) MCP 客户端日志必须脱敏认证 header；(3) `mcp_call` 在服务端注入认证，绝不向模型暴露。
- **与 operator MCP 共存**：operator 配置（`config.yaml`）维持现状、不变；per-user 路径是其并行补充。

非目标（明确排除）：per-user stdio 子进程、per-user OAuth 2.1 浏览器授权流程、resources/prompts/elicitation/sampling 等 MCP 长尾能力。

## Capabilities

### New Capabilities
- `user-mcp-configuration`: per-user MCP 服务的配置存储（工作空间文件）、`mcp_*` 管理工具、`mcp_call` 按需调用、per-user 凭据隔离、提示词集成，以及三条泄露不变量。

### Modified Capabilities
<!-- 现有 operator 侧 spec（agent-mcp-configuration / mcp-client / mcp-http-transport）的需求不变；
     per-user 路径是并行的、新增的能力，复用现有 http transport 实现但不改变其需求契约。-->
（无）

## Impact

- **新增代码**：`internal/tool/mcp/`（新工具族，仿 `internal/tool/luban/` 结构）；per-user MCP 配置加载器与 per-user turn-scoped 连接管理（复用 `internal/tool/mcpclient/` 的 http transport 与 session-id 管理）。
- **修改代码**：`internal/agent/orchestrator.go`（`buildAgentRegistry`/`collectTools` 注入 per-user MCP 工具与服务描述）、`internal/prompt/render.go`（渲染用户级 MCP 描述 + 选择引导 + `.blowball/mcp` 管理约束行）、`cmd/blowball/serve.go`（为 agent 角色装配 `mcp_*` 工具族，仿 `needsLubanTools`）。
- **复用既有**：per-user workspaceRoot 路径作用域（`message_stream.go:152`）、xizhi `.blowball/` 硬拒绝（`validate.go`）、luban 的 `userDirFn(userID)` 模式、http transport 的重连与会话管理。
- **安全**：明文 key 落盘于 per-user 工作空间文件（operator 可读 = 信任边界迁移，自托管/team 可接受，托管 SaaS 需后续加密）；三条泄露不变量须以测试守护。
- **配置/迁移**：无破坏性变更；未配置 per-user MCP 的用户行为不变。
