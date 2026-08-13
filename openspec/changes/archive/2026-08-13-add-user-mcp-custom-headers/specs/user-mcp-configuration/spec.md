## MODIFIED Requirements

### Requirement: Per-user MCP config file location and schema
每个用户 SHALL 在其工作空间的 `.blowball/mcp/` 目录下，**按服务拆分**声明该用户私有的 MCP 服务：每个 server 占一个子目录 `.blowball/mcp/{name}/config.json`，文件体只包含该 server 自身的 `url`、`transport`、`auth`、`headers`（可选，见"Custom (non-secret) request headers for per-user servers"）、`description`、`tools` 缓存字段，`name` 以所在子目录名为准（不写入文件体）。系统不再维护单一顶层 `config.json`，亦不做单文件↔多文件的双读兼容。

#### Scenario: Config lives under reserved workspace namespace, per server
- **WHEN** 系统为某用户解析 per-user MCP 配置
- **THEN** 枚举 `data/{userID}/workspace/.blowball/mcp/` 的子目录，逐个读取 `{name}/config.json`，绝不读取其他用户的同名目录

#### Scenario: Missing config directory means no user servers
- **WHEN** 某用户工作空间下不存在 `.blowball/mcp/` 目录或其下无任何合法 server 子目录
- **THEN** 该用户视为无可用的 per-user MCP 服务，进程不报错

#### Scenario: Malformed single server does not crash
- **WHEN** 某个 `{name}/config.json` 内容格式错误（非法 JSON 或缺必需字段，含 `headers` 未通过校验）
- **THEN** 系统返回明确错误并令该 server 不可用，但**不**导致进程崩溃或 turn 失败（区别于 operator 配置的启动期 fail-fast）

#### Scenario: One server per directory, name taken from directory
- **WHEN** 系统加载 `.blowball/mcp/github/config.json`
- **THEN** 得到一个 `name = "github"` 的 server 条目，文件体中即便出现 `name` 字段也不被采信

#### Scenario: Enumerate skips non-server entries
- **WHEN** `.blowball/mcp/` 下存在临时文件、隐藏目录或未通过 name 校验的目录
- **THEN** 枚举时跳过这些条目，不当作 server

### Requirement: Secret redaction in management tool output
`mcp_list_servers` 及任何向模型返回配置内容的工具 SHALL 对 `auth`（认证）字段脱敏，绝不在模型可见输出中回显 key 明文。**脱敏范围仅限 `auth`**：server 的自定义 `headers`（见"Custom (non-secret) request headers for per-user servers"）是非敏感业务参数，SHALL 以明文呈现、不脱敏。

#### Scenario: List output redacts credentials
- **WHEN** agent 调用 `mcp_list_servers`
- **THEN** 返回结果中 `auth` 字段以脱敏形式呈现（如 `"***"` 或省略），不含 bearer token / API key 明文

#### Scenario: Add/remove do not echo auth secrets
- **WHEN** agent 调用 `mcp_add_server` 或 `mcp_remove_server`
- **THEN** 返回结果不含已写入或移除的 `auth` 认证明文

#### Scenario: Custom headers shown in plaintext
- **WHEN** agent 调用 `mcp_list_servers` 且某 server 配置了自定义 `headers`
- **THEN** 该 `headers` 以明文原样呈现，不脱敏（区别于 `auth`）

### Requirement: Server-side auth injection in invocation
`mcp_call`（及一切发往该 server 的出站请求）SHALL 在服务端（进程内）注入该 server 的请求头：`auth` 头（敏感，值不出现在模型文本）与自定义 `headers`（非敏感）。注入合并次序 SHALL 为：先施加自定义 `headers`，再用 `auth` 头覆盖——同名头 **auth 胜出**。`auth` 的凭据值 SHALL NOT 出现在模型的输入或输出中；自定义 `headers` 因属非敏感通道，可由 `mcp_add_server` 设置并出现在 `mcp_list_servers` 输出。

#### Scenario: Auth injected server-side only
- **WHEN** agent 调用 `mcp_call`
- **THEN** 系统从该用户工作空间配置读取 `auth` 并在进程内注入到出站请求，认证值不进入模型可见的任何文本

#### Scenario: Auth wins on header-name collision
- **WHEN** 自定义 `headers` 与 `auth` 产生的头同名（如自定义 `Authorization` 与 bearer auth）
- **THEN** 出站请求中该头取 `auth` 的值，自定义头被覆盖

#### Scenario: Custom headers injected alongside auth
- **WHEN** server 同时配置了 `auth` 与自定义 `headers`
- **THEN** 二者均注入出站请求头；自定义头在无冲突时原样出现

## ADDED Requirements

### Requirement: Custom (non-secret) request headers for per-user servers
每个 per-user server MAY 声明一个可选的 `headers` 字段（`map[string]string`），承载发往该 server 的**非敏感**自定义请求头（如租户标识 `X-Tenant-ID`、来源标识、网关 / 路由参数）。`headers` SHALL 随 `{name}/config.json` 持久化，并注入到该 server 的每次出站 HTTP 请求（initialize、tools/list、tools/call）。`headers` 是与 `auth` 并列的独立通道，且是**非敏感**通道：SHALL NOT 在其中放置密钥，密钥只能放 `auth`；`mcp_list_servers` SHALL 以明文回显 `headers`（不脱敏），`mcp_add_server` SHALL 支持设置 `headers`。

#### Scenario: Optional headers persisted per server
- **WHEN** `mcp_add_server` 提供合法 `headers`（如 `{"X-Tenant-ID":"acme"}`）
- **THEN** 该 server 的 `{name}/config.json` 含 `headers` 字段，重读后一致

#### Scenario: Headers omitted means no custom headers
- **WHEN** server 配置不含 `headers`（或为空对象）
- **THEN** 出站请求仅含 `auth` 头与传输层自管头，行为与本变更前一致（零行为变更）

#### Scenario: Headers injected on every outbound request
- **WHEN** 该 server 发出 initialize / tools/list / tools/call 任一请求
- **THEN** 自定义头与 `auth` 头一并出现在出站请求头中

#### Scenario: Auth wins on header-name collision
- **WHEN** 自定义头与 `auth` 产生的头同名
- **THEN** 出站请求中该头取 `auth` 的值，自定义头被覆盖

#### Scenario: Custom headers are non-secret and shown in plaintext
- **WHEN** agent 调用 `mcp_list_servers`
- **THEN** 该 server 的 `headers` 以明文呈现，不脱敏

#### Scenario: Reserved header names rejected
- **WHEN** `headers` 含传输层自管头名（`Content-Type` / `Content-Length` / `Accept` / `Mcp-Session-Id` / `Host`，大小写不敏感）
- **THEN** 校验拒绝并返回明确错误，该 server 不落盘、不连接

#### Scenario: Invalid header name or CRLF value rejected
- **WHEN** `headers` 的某个头名为非法 HTTP token，或某头值含 `\r` / `\n`
- **THEN** 校验拒绝并返回明确错误

### Requirement: Header validation enforced at all config paths
自定义头的校验（合法 HTTP token 头名、禁 CR/LF 头值、保留头名拒绝）SHALL 在 `mcp_add_server` 入口、配置加载期、配置写前校验三处统一生效（与 server name 与 transport / auth 校验同生命周期）。

#### Scenario: Invalid headers rejected at add time
- **WHEN** `mcp_add_server` 提供 `headers` 含保留头名或非法值
- **THEN** 在连接远端之前即被拒，不发起网络请求

#### Scenario: Invalid headers in on-disk config make server unavailable
- **WHEN** 配置加载期发现某 server 的 `headers` 未通过校验
- **THEN** 该 server 不可用并返回明确错误，但不导致进程崩溃或 turn 失败（与既有"单服务损坏不崩"语义一致）
