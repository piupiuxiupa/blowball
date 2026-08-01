# user-mcp-configuration Specification

## Purpose

定义 per-user（工作空间驻留）MCP 服务的配置存储、`mcp_*` 管理与调用工具、per-user 凭据隔离、turn-scoped 连接生命周期、系统提示词集成，以及三条泄露不变量。per-user MCP 是 operator-global MCP（`agent-mcp-configuration` / `mcp-client` / `mcp-http-transport`）的并行补充：用户在自己的工作空间 `.blowball/mcp/{name}/config.json`（按服务拆分，每服务一子目录）声明私有 MCP 服务，用自己的静态凭据认证，按需调用；传输范围限定为远程 HTTP，认证范围限定为静态凭据（bearer / api-key / basic）。

## Requirements

### Requirement: Per-user MCP config file location and schema
每个用户 SHALL 在其工作空间的 `.blowball/mcp/` 目录下，**按服务拆分**声明该用户私有的 MCP 服务：每个 server 占一个子目录 `.blowball/mcp/{name}/config.json`，文件体只包含该 server 自身的 `url`、`transport`、`auth`、`description`、`tools` 缓存字段，`name` 以所在子目录名为准（不写入文件体）。系统不再维护单一顶层 `config.json`，亦不做单文件↔多文件的双读兼容。

#### Scenario: Config lives under reserved workspace namespace, per server
- **WHEN** 系统为某用户解析 per-user MCP 配置
- **THEN** 枚举 `data/{userID}/workspace/.blowball/mcp/` 的子目录，逐个读取 `{name}/config.json`，绝不读取其他用户的同名目录

#### Scenario: Missing config directory means no user servers
- **WHEN** 某用户工作空间下不存在 `.blowball/mcp/` 目录或其下无任何合法 server 子目录
- **THEN** 该用户视为无可用的 per-user MCP 服务，进程不报错

#### Scenario: Malformed single server does not crash
- **WHEN** 某个 `{name}/config.json` 内容格式错误（非法 JSON 或缺必需字段）
- **THEN** 系统返回明确错误并令该 server 不可用，但**不**导致进程崩溃或 turn 失败（区别于 operator 配置的启动期 fail-fast）

#### Scenario: One server per directory, name taken from directory
- **WHEN** 系统加载 `.blowball/mcp/github/config.json`
- **THEN** 得到一个 `name = "github"` 的 server 条目，文件体中即便出现 `name` 字段也不被采信

#### Scenario: Enumerate skips non-server entries
- **WHEN** `.blowball/mcp/` 下存在临时文件、隐藏目录或未通过 name 校验的目录
- **THEN** 枚举时跳过这些条目，不当作 server

### Requirement: Server name path-safety validation
server `name` 是文件系统路径分量（决定其 `config.json` 所在子目录），SHALL 匹配 `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`（字母数字开头，仅含字母数字/下划线/连字符，长度 1–64）。该约束 SHALL 在 `mcp_add_server` 入口、配置加载期、配置写前校验三处统一生效。任何含路径分隔符、以点号开头、或含空格/其他特殊字符的 name SHALL 被拒绝，以杜绝路径穿越、目录嵌套或覆盖。

#### Scenario: Add server with valid name
- **WHEN** `mcp_add_server` 提供 name 为 `github`、`my-mcp`、`svc_2` 等符合规则的标识符
- **THEN** 系统接受并在 `.blowball/mcp/{name}/config.json` 创建该服务

#### Scenario: Reject traversal-like name
- **WHEN** `mcp_add_server` 提供 name 含 `..`（如 `../etc`）或路径分隔符（如 `a/b`）
- **THEN** 系统拒绝并返回明确错误，不创建任何目录或文件

#### Scenario: Reject dot-prefixed or whitespace name
- **WHEN** `mcp_add_server` 提供 name 为 `.hidden`、`my server`、`a.b` 等
- **THEN** 系统拒绝并返回明确错误，说明命名规则

#### Scenario: Reject oversize name
- **WHEN** `mcp_add_server` 提供 name 长度超过 64
- **THEN** 系统拒绝

#### Scenario: Malformed name rejected at load time
- **WHEN** 配置加载期发现某子目录名不匹配 name 规则
- **THEN** 该子目录被跳过（不当作 server），不导致整体加载失败

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
系统 SHALL 提供 `mcp_list_servers`、`mcp_add_server`、`mcp_remove_server` 三个 agent 工具（仿 `luban_*` 模式），作用于调用者本人工作空间的 `.blowball/mcp/{name}/config.json`（per-server 拆分存储）。

#### Scenario: List user's MCP servers
- **WHEN** agent 调用 `mcp_list_servers`
- **THEN** 返回该用户已配置的 server 列表（name、url、transport、description），认证字段一律脱敏

#### Scenario: Add a new MCP server
- **WHEN** agent 调用 `mcp_add_server` 并提供合法 name、url、auth、description
- **THEN** 系统将该 server 写入 `.blowball/mcp/{name}/config.json`，并缓存其 `tools/list`（工具名与入参 schema）到该文件

#### Scenario: Remove an MCP server
- **WHEN** agent 调用 `mcp_remove_server` 指定已存在的 name
- **THEN** 该 server 的子目录（含 `config.json`）从 `.blowball/mcp/` 移除，后续调用不再可用

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

### Requirement: On-demand MCP tool discovery tool
系统 SHALL 提供 `mcp_list_tools(server)` agent 工具，实时连接指定的 per-user server、执行 `tools/list`，并返回该 server 全部工具的 `name`/`description`/`input_schema`。这是 agent 发现 per-user MCP 工具契约（工具名与入参 schema）的权威入口。

#### Scenario: Discover a server's tools live
- **WHEN** agent 调用 `mcp_list_tools("github")` 指向一个已配置的 server
- **THEN** 系统连接该 server、执行 `tools/list`，返回其全部工具的 name/description/input_schema

#### Scenario: Unknown server rejected
- **WHEN** `mcp_list_tools` 的 `server` 不在调用者已配置的服务中
- **THEN** 系统返回明确错误，不发起连接

#### Scenario: Connect failure surfaced
- **WHEN** `mcp_list_tools` 连接或 `tools/list` 失败（超时、拒连、远端错误）
- **THEN** 返回明确错误给 agent，不回写缓存

### Requirement: Asynchronous cache write-back after discovery
`mcp_list_tools` 取得 live 工具列表后 SHALL 通过 fire-and-forget goroutine 把结果回写到该 server 的 config 缓存（`{name}/config.json` 的 `tools` 字段）。回写 SHALL 使用独立于 turn 的 context（`context.Background()` 配独立超时），SHALL NOT 阻塞 `mcp_list_tools` 的返回，SHALL NOT 影响主调用流程或 turn 生命周期。回写失败 SHALL 仅记录日志、SHALL NOT 向模型或用户报错。

#### Scenario: Live result returned before write completes
- **WHEN** agent 调用 `mcp_list_tools`
- **THEN** live 工具列表立即返回给模型，不等待缓存回写完成

#### Scenario: Write-back does not block main flow
- **WHEN** `mcp_list_tools` 触发异步回写
- **THEN** 主调用、turn 推进、turn 结束的 `Manager.Close()` 均不被回写阻塞

#### Scenario: Write-back survives turn cancellation
- **WHEN** turn 在回写完成前结束（turn ctx 被取消）
- **THEN** 回写仍以独立 context 完成（或在其独立超时内失败），不因 turn ctx 取消而中断

#### Scenario: Write-back failure is non-fatal
- **WHEN** 异步回写因 I/O 或超时失败
- **THEN** 仅记录警告日志，`mcp_list_tools` 的已返回结果与后续 turn 行为不受影响

### Requirement: mcp_list_tools joins the mcp_* tool family
`mcp_list_tools` SHALL 与 `mcp_list_servers`/`mcp_add_server`/`mcp_remove_server`/`mcp_call` 同属 per-user `mcp_*` 工具族：turn-scoped、per-user、复用同一 turn 级 `Manager`，在任一 agent 的 `tools` 列出 `mcp_*` 工具时随族注册。

#### Scenario: Registered alongside the family
- **WHEN** 任一 agent 配置了任一 `mcp_*` 工具
- **THEN** `mcp_list_tools` 与其他 `mcp_*` 工具一并注册到该 turn 的 per-agent registry

#### Scenario: Reuses turn-scoped manager
- **WHEN** agent 调用 `mcp_list_tools`
- **THEN** 复用本 turn 的 per-user MCP 连接（与 `mcp_call` 共享同一 `Manager` 与连接缓存）

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
- **THEN** 仅枚举 `data/{alice}/workspace/.blowball/mcp/` 下该用户的服务，用户 B 的配置不参与

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
系统 SHALL 在系统提示词中渲染该用户的 per-user MCP 服务（server 级描述），并引导 agent 在执行任务前主动评估并选择最合适的 skill 与 MCP 服务。提示词 SHALL 声明 `.blowball/mcp/` 仅由 `mcp_*` 工具管理（与既有 `.blowball/skills/` 约束并列）。提示词 SHALL 进一步声明 per-user MCP 的**调用规约**：在 `mcp_call` 之前 SHALL 先调用 `mcp_list_tools(server)` 了解该 server 的工具名与入参 schema，SHALL NOT 凭猜测调用工具名或构造入参。

#### Scenario: User servers described in system prompt
- **WHEN** 某用户已配置 per-user MCP 服务
- **THEN** 该 agent 的系统提示词包含这些 server 的描述（name + description）

#### Scenario: Prompt states the list-before-call convention
- **WHEN** 系统渲染系统提示词的 per-user MCP 段落
- **THEN** 段落明确指示：调用某 server 前先用 `mcp_list_tools` 获取其工具名与 schema，禁止猜测工具名或入参形状（错误会在发起远端调用前被拒）

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
