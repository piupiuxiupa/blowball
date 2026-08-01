## ADDED Requirements

### Requirement: Per-user MCP config file location and schema
每个用户 SHALL 在其工作空间的 `.blowball/mcp/config.json` 声明该用户私有的 MCP 服务。文件为 JSON 数组（或含 `servers` 数组的对象），每个 server 条目至少包含 `name`、`url`、`transport`、`auth`、`description`。

#### Scenario: Config file lives under reserved workspace namespace
- **WHEN** 系统为某用户解析 per-user MCP 配置
- **THEN** 仅读取 `data/{userID}/workspace/.blowball/mcp/config.json`，绝不读取其他用户的同名文件

#### Scenario: Missing config file means no user servers
- **WHEN** 某用户工作空间下不存在 `.blowball/mcp/config.json`
- **THEN** 该用户视为无可用的 per-user MCP 服务，进程不报错

#### Scenario: Malformed config does not crash
- **WHEN** `.blowball/mcp/config.json` 内容格式错误（非法 JSON 或缺必需字段）
- **THEN** 系统返回明确错误并令该 server 不可用，但**不**导致进程崩溃或 turn 失败（区别于 operator 配置的启动期 fail-fast）

### Requirement: Transport restricted to remote HTTP for per-user servers
per-user MCP 服务 SHALL 仅支持 `transport: http`（MCP Streamable HTTP）。`stdio` 与 `sse` 在 per-user 配置中 SHALL 被拒绝。

#### Scenario: HTTP server accepted
- **WHEN** per-user 配置声明一个 `transport: http` 的 server
- **THEN** 系统接受并通过 Streamable HTTP 连接

#### Scenario: stdio server rejected for per-user
- **WHEN** per-user 配置声明一个 `transport: stdio` 的 server
- **THEN** 系统拒绝该条目并返回明确错误，不启动任何子进程

### Requirement: Authentication restricted to static credentials for per-user servers
per-user MCP 服务的认证 SHALL 限定为静态凭据（bearer token、API key、basic auth）。OAuth 2.1 授权流程不在 per-user 范围内。

#### Scenario: Static bearer auth accepted
- **WHEN** per-user server 的 `auth` 声明为静态 bearer token
- **THEN** 系统在调用时将该 token 注入 `Authorization` header

#### Scenario: OAuth flow not supported per-user
- **WHEN** per-user server 的 `auth` 声明要求 OAuth 2.1 授权流程
- **THEN** 系统返回明确错误，指明 per-user MCP 不支持 OAuth 流程（静态凭据可用）

### Requirement: Per-user MCP configuration management tools
系统 SHALL 提供 `mcp_list_servers`、`mcp_add_server`、`mcp_remove_server` 三个 agent 工具（仿 `luban_*` 模式），作用于调用者本人工作空间的 `.blowball/mcp/config.json`。

#### Scenario: List user's MCP servers
- **WHEN** agent 调用 `mcp_list_servers`
- **THEN** 返回该用户已配置的 server 列表（name、url、transport、description），认证字段一律脱敏

#### Scenario: Add a new MCP server
- **WHEN** agent 调用 `mcp_add_server` 并提供合法 name、url、auth、description
- **THEN** 系统将该 server 写入 `.blowball/mcp/config.json`，并缓存其 `tools/list`（工具名与入参 schema）到该文件

#### Scenario: Remove an MCP server
- **WHEN** agent 调用 `mcp_remove_server` 指定已存在的 name
- **THEN** 该 server 从 `.blowball/mcp/config.json` 移除，后续调用不再可用

#### Scenario: Add server with duplicate name rejected
- **WHEN** `mcp_add_server` 的 name 与已配置 server 重名
- **THEN** 系统拒绝并返回明确错误，不改写既有配置

### Requirement: On-demand MCP invocation tool
系统 SHALL 提供 `mcp_call(server, tool, args)` 元工具，按需调用某 per-user server 的某工具。

#### Scenario: Successful remote tool call
- **WHEN** agent 调用 `mcp_call` 指向已配置 server 的已知工具且 args 合法
- **THEN** 系统连接该 server、转发 `tools/call`，并将远端成功结果返回给 agent

#### Scenario: Unknown tool rejected before call
- **WHEN** `mcp_call` 的 `tool` 不在该 server 缓存的 `tools/list` 中
- **THEN** 系统在发起远端调用前即拒绝并返回明确错误

#### Scenario: Args validated against cached schema
- **WHEN** `mcp_call` 的 `args` 违反该 server 缓存的入参 schema
- **THEN** 系统在发起远端调用前即拒绝并返回明确错误，提示 schema 不符

#### Scenario: Remote tool error surfaced
- **WHEN** 远端 `tools/call` 返回 error 或 `isError=true`
- **THEN** `mcp_call` 返回错误，agent 层将其作为 tool_error 事件流式输出

### Requirement: Turn-scoped connection lifecycle
`mcp_call` 对同一 server 的连接 SHALL 在单个 turn 内缓存复用，turn 结束时销毁。系统 SHALL NOT 跨 turn 维持 per-user MCP 连接。

#### Scenario: Repeated calls reuse connection within a turn
- **WHEN** 一个 turn 内对同一 server 连续调用多次 `mcp_call`
- **THEN** 仅首次建立连接（connect + initialize），后续调用复用同一连接

#### Scenario: No cross-turn connection state
- **WHEN** 一个 turn 结束
- **THEN** 该 turn 建立的 per-user MCP 连接被销毁，下一 turn 重新按需建立（无跨 turn 持久池）

### Requirement: Configurable timeout with separated connect and total bounds
`mcp_call` SHALL 受可配置超时约束，默认 total-call 超时为 10 秒。connect 超时与 total-call 超时 SHALL 分离配置。

#### Scenario: Total-call timeout enforced
- **WHEN** 一次 `mcp_call` 的 connect + initialize + tools/call 总耗时超过 total-call 超时
- **THEN** 系统取消该调用并返回超时错误给 agent

#### Scenario: Connect timeout avoids handshake hang
- **WHEN** TCP/TLS 握手或 initialize 阶段超过 connect 超时（但未到 total 超时）
- **THEN** 系统取消并返回连接超时错误，避免握手死等

### Requirement: Per-user credential isolation
per-user MCP 配置与连接 SHALL 严格按 userID 隔离。一个用户的认证凭据 SHALL NOT 进入任何其他用户的请求路径或连接。

#### Scenario: Each turn reads only the caller's config
- **WHEN** 系统为用户 A 的 turn 读取 MCP 配置
- **THEN** 仅读取 `data/{alice}/workspace/.blowball/mcp/config.json`，用户 B 的配置文件不参与

#### Scenario: Connections scoped to the owning user
- **WHEN** 用户 A 的 turn 建立 MCP 连接
- **THEN** 该连接仅携带用户 A 的认证、仅服务于用户 A 的 turn，turn 结束销毁，不与用户 B 共享

#### Scenario: No cross-user key leakage
- **WHEN** 用户 B 的 turn 执行
- **THEN** 用户 A 的 MCP 凭据不出现在用户 B 的配置读取、连接建立或工具调用中的任何环节

### Requirement: Secret redaction in management tool output
`mcp_list_servers` 及任何向模型返回配置内容的工具 SHALL 对认证字段脱敏，绝不在模型可见输出中回显 key 明文。

#### Scenario: List output redacts credentials
- **WHEN** agent 调用 `mcp_list_servers`
- **THEN** 返回结果中认证字段以脱敏形式呈现（如 `"***"` 或省略），不含 bearer token / API key 明文

#### Scenario: Add/remove do not echo secrets
- **WHEN** agent 调用 `mcp_add_server` 或 `mcp_remove_server`
- **THEN** 返回结果不含已写入或移除的认证明文

### Requirement: Server-side auth injection in invocation
`mcp_call` SHALL 在服务端（进程内）注入认证，认证值 SHALL NOT 出现在模型的输入或输出中。

#### Scenario: Auth injected server-side only
- **WHEN** agent 调用 `mcp_call`
- **THEN** 系统从该用户工作空间配置读取认证并在进程内注入到出站请求，认证值不进入模型可见的任何文本

### Requirement: Authentication redaction in logs
MCP 客户端的结构化日志 SHALL 对认证 header（如 `Authorization`、`X-API-Key`）脱敏，不得将认证明文写入日志。

#### Scenario: Auth header never logged in plaintext
- **WHEN** MCP 客户端记录一次携带认证 header 的出站请求
- **THEN** 日志中该 header 以脱敏形式呈现，不含 token / key 明文

### Requirement: System prompt integration for per-user MCP
系统 SHALL 在系统提示词中渲染该用户的 per-user MCP 服务（server 级描述），并引导 agent 在执行任务前主动评估并选择最合适的 skill 与 MCP 服务。提示词 SHALL 声明 `.blowball/mcp/` 仅由 `mcp_*` 工具管理（与既有 `.blowball/skills/` 约束并列）。

#### Scenario: User servers described in system prompt
- **WHEN** 某用户已配置 per-user MCP 服务
- **THEN** 该 agent 的系统提示词包含这些 server 的描述（name + description）

#### Scenario: Prompt nudges proactive selection
- **WHEN** 系统渲染系统提示词
- **THEN** 提示词包含引导 agent 在执行任务前主动选择最合适 skill 与 MCP 服务的指示

#### Scenario: Reserved-namespace management constraint stated
- **WHEN** 系统渲染系统提示词
- **THEN** 提示词声明 `.blowball/mcp/` 仅由 `mcp_*` 工具管理，不得经 `xizhi_*` 访问

### Requirement: Coexistence with operator MCP
per-user MCP 路径 SHALL 与既有 operator MCP（`config.yaml` 的 `mcp.servers`）并存，互不干扰。operator 侧的需求契约与行为不变。

#### Scenario: Operator and user servers both available
- **WHEN** 某部署同时配置了 operator MCP（全局）与某用户的 per-user MCP
- **THEN** 两者的工具/服务对 agent 均可见（分组渲染），互不覆盖、互不冲突

#### Scenario: Operator path unchanged
- **WHEN** 未配置任何 per-user MCP
- **THEN** operator MCP 行为与本变更前完全一致
