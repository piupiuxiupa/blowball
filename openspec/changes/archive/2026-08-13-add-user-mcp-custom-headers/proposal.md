## Why

per-user MCP（`user-mcp-configuration`）目前向远端 server 发出的每个 HTTP 请求，其请求头**完全由 `auth` 派生**（`authHeaders` → `Authorization` / `X-API-Key` / Basic），用户无法注入任何额外的自定义头。`Server` 结构没有 `headers` 字段，`DefaultTransportFactory` 也只喂 `authHeaders(server.Auth)`。

但不少 MCP server 除认证外还要求额外的请求头作为"传参"——租户标识（`X-Tenant-ID`）、来源标识（`X-Source`）、网关/路由参数、非标准鉴权（如 `X-Api-Token`，配合 `auth.type: none`）。这些是非敏感业务参数，当前无处可填，导致这类 server 无法在 per-user 路径接入。

底层 `HTTPTransport` 本就接受任意 `headers map[string]string`（`NewHTTPTransport`），卡点只在 per-user config 层：`Server` 无字段、工厂未合并。所以补齐这一层改动集中、风险可控。

## What Changes

- per-user MCP server 配置新增可选的 `headers` 字段（`map[string]string`），随 `{name}/config.json` 持久化、随连接注入。
- `mcp_add_server` 增加可选 `headers` 入参，允许 agent 配置自定义头。
- **自定义头是非敏感通道（按约定，不做脱敏）**：在 `mcp_list_servers` 输出中以明文呈现、可被 `mcp_add_server` 设置、以明文存于 `config.json`。`auth` 仍是**唯一**的敏感通道——用户不得在 `headers` 中放置密钥。这是文档化约定，而非脱敏机制。
- 注入合并次序：先施加自定义头，再用 `authHeaders` 覆盖（**同名时 auth 胜出**），保证显式 auth 类型始终权威、密钥通道单一。需要私有/非标准鉴权方案的用户用 `auth.type: none` + 自定义头。
- 校验：头名须为合法 HTTP token；头值禁止含 CR/LF（防 CRLF 注入）；一组传输层自管头名（`Content-Type` / `Content-Length` / `Accept` / `Mcp-Session-Id` / `Host`）在 per-user 层被拒，避免破坏 JSON-RPC 成帧。
- 传输层（`internal/tool/mcpclient/http.go`）**不改**：其 `post` 已按请求施加 `t.headers`，且 `Mcp-Session-Id` 最后施加（天然防覆盖）。`Content-Type`/`Accept` 在自定义头之前施加，故靠校验层黑名单兜底。

## Capabilities

### New Capabilities
<!-- 无新能力 -->

### Modified Capabilities
- `user-mcp-configuration`: 新增 per-user server 的非敏感自定义请求头能力（`headers` 字段、注入合并、保留头名校验、`mcp_list_servers`/`mcp_add_server` 适配）；明确脱敏范围仅限 `auth`，自定义头按明文呈现。

## Impact

- `internal/tool/mcp/config.go`：`Server` 增 `Headers map[string]string \`json:"headers,omitempty"\``；`validateServer` 增头名 / 头值 / 保留头名校验。
- `internal/tool/mcp/manager.go`：`DefaultTransportFactory` 由 `headers := authHeaders(server.Auth)` 改为先复制 `server.Headers` 再用 `authHeaders` 覆盖（auth 胜），传入 `NewHTTPTransport`。
- `internal/tool/mcp/auth.go`：`serverView` 增 `Headers map[string]string \`json:"headers,omitempty"\``；`serverViewFrom` 回填（明文，不脱敏）。
- `internal/tool/mcp/register.go`：`mcp_add_server` 的 `ParametersJSON` 增可选 `headers` 对象；Execute 闭包解析并传入；`addServer` 签名增 `headers map[string]string` 参数并写入 `Server.Headers`；工具描述声明 `headers` 非敏感、密钥须放 `auth`。
- `internal/tool/mcp/mcp_test.go`：覆盖自定义头注入与 auth 覆盖、保留头名拒绝、CRLF 拒绝、`list` 明文回显、`add` 往返持久化；确认未削弱 `auth` 脱敏。
- 不改 `internal/tool/mcpclient/`；不改 operator MCP 路径；不改传输 / 认证 / 隔离 / turn-scoped 连接等既有不变量；不新增 HTTP API。
