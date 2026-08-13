## Context

per-user MCP（`user-mcp-configuration`）向远端 server 发请求时，头完全由 `auth` 决定：`DefaultTransportFactory`（`internal/tool/mcp/manager.go:32`）做 `headers := authHeaders(server.Auth)` 后传给 `NewHTTPTransport`。`authHeaders`（`internal/tool/mcp/auth.go:19`）只产出 `Authorization: Bearer...` / `<Header|X-API-Key>: <key>` / `Authorization: Basic ...` 三种。`Server` 结构（`config.go:132`）只有 `URL/Transport/Auth/Description/Tools`，无 `headers` 字段。因此用户无法为某 server 附加 `X-Tenant-ID`、`X-Source`、网关路由头或私有鉴权头这类**非敏感业务参数**。

底层 `HTTPTransport`（`internal/tool/mcpclient/http.go`）其实已支持任意头：`post`（`:156`）在每个请求上 `httpReq.Header.Set(k, v)` 遍历 `t.headers`。施加次序为 `Content-Type` → `Accept` → `t.headers`（含 auth）→ `Mcp-Session-Id`（最后）。所以底层不是瓶颈，卡点在 config 层。

三条既有泄露不变量均针对 `auth`（凭据脱敏 / 日志不回显 auth 头 / 认证服务端注入不进模型文本）。引入自定义头时，需明确把它划为**独立的非敏感通道**，与 `auth` 的敏感通道并列，二者不混淆。

## Goals / Non-goals

**Goals**
- per-user server 可声明可选 `headers`（string→string），随 `{name}/config.json` 持久化、随该 server 的每次出站请求（initialize / tools/list / tools/call）注入。
- `mcp_add_server` 支持配置自定义头；`mcp_list_servers` 明文回显自定义头。
- 自定义头为非敏感通道：不脱敏、模型可见；`auth` 仍是唯一敏感通道。
- 注入合并与保留头名校验保证不破坏既有 auth 权威性与 JSON-RPC 成帧。

**Non-goals**
- **不**为自定义头做脱敏（按用户决策：自定义头按非敏感对待）。
- **不**支持 per-call 头（头是 server/连接级；每次工具调用的参数走 JSON-RPC `params`，不走头）。
- **不**改 `internal/tool/mcpclient/` 传输层。
- **不**改 operator MCP（`config.yaml` 的 `mcp.servers`）。
- **不**改传输 / 认证类型 / 凭据隔离 / turn-scoped 连接等既有不变量。
- **不**新增 HTTP API 或 agent 工具（`mcp_add_server` 仅增可选入参）。

## Decisions

### 1. 自定义头为独立非敏感通道，不做脱敏
- **Rationale**: 用户明确选择。自定义头承载租户 / 来源 / 网关等非敏感业务参数；若需密钥，走 `auth`。脱敏范围维持仅 `auth`，三条泄露不变量语义不变（它们本就只约束 `auth` / 凭据）。
- **约束**: 工具描述须声明"不得在 `headers` 放置密钥，密钥只能放 `auth`"。
- **Alternative**: 把所有自定义头值脱敏成 `***`（最安全）。被否——用户选择不脱敏，且会丧失让 agent 在 `list` 中看到非敏感头的能力。

### 2. 合并次序：自定义头先施加，auth 覆盖（auth 胜）
- **Rationale**: 显式 `auth` 类型（bearer / api-key / basic）应始终权威；同名头冲突时 auth 胜出，避免用户误用自己的自定义头覆盖掉认证。需要私有 / 非标准鉴权（如 `X-Api-Token`）的用户设 `auth.type: none` + 自定义头即可。
- **实现**: `DefaultTransportFactory` 先 `merged := clone(server.Headers)`，再 `for k,v := range authHeaders(server.Auth) { merged[k]=v }`。

### 3. 保留头名黑名单（传输层自管头）
- **Rationale**: 传输 `post` 在 `t.headers` 之**前**施加 `Content-Type`/`Accept`（`:174-178`），故自定义头若名为二者会覆盖 JSON-RPC 成帧头。`Mcp-Session-Id` 虽在最后施加（已防覆盖），但仍列入保留以保持语义清晰。`Host`/`Content-Length` 由 Go http 客户端按请求管理，列入保留避免歧义。
- **保留集**: `Content-Type`、`Content-Length`、`Accept`、`Mcp-Session-Id`、`Host`（按 `textproto.CanonicalMIMEHeaderKey` 归一后大小写不敏感比对）。
- **Alternative**: 不黑名单、允许覆盖并文档化风险。否——成帧头被覆盖会直接导致 server 解析失败，应硬拒。

### 4. 头名 / 头值校验
- **Rationale**: 头名须合法 HTTP token（防注入非法字符）；头值禁含 `\r`/`\n`（防 CRLF / header 注入，Go `net/http` 在请求期也会拒，但提前校验给出清晰错误）。校验落在 `validateServer`，与 url / transport / auth 校验同处、同生命周期（add / load / write 三路径统一）。

### 5. 持久化复用既有 per-server JSON 往返
- **Rationale**: `Server` 加 `Headers ...json:"headers,omitempty"` 后，`WriteServer` 的 `json.MarshalIndent` 与 `LoadConfig` 的反序列化自动往返；旧 config 无该字段 → nil map → 无自定义头（零行为变更）。

## Risks / Trade-offs

- **[Risk] 用户误把密钥放进 `headers` → 明文进 `config.json`、`mcp_list_servers` 输出、模型可见** → **Mitigation**: 工具描述声明 `headers` 非敏感、密钥只能放 `auth`；`auth` 仍是受脱敏保护的唯一敏感通道。这是约定约束，与现有"`.blowball/mcp/` 仅由 `mcp_*` 管理、`xizhi` 拒绝访问"同性质。
- **[Risk] 保留头名黑名单导致合理的自定义头被误拒** → **Mitigation**: 黑名单仅 5 个传输自管头；其余头名一律放行。错误信息列明被拒头名与保留集。
- **[Risk] 同名头 auth / 自定义冲突时用户预期自定义胜** → **Mitigation**: 工具描述明确"auth 胜出；私有鉴权用 `auth.type: none` + 自定义头"。行为确定、可预期。
- **[Risk] CRLF 注入绕过** → **Mitigation**: 校验层禁 `\r`/`\n`（值）与非法 token（名），与 Go `net/http` 的请求期拦截双保险。

## Open Questions
<!-- 无待决问题。原"是否在系统提示补一句引导"已决：自定义头的非敏感性质与密钥归属（密钥只能放 `auth`）、保留头名、auth 胜出等引导统一写进 `mcp_add_server` 的工具描述（见 Decisions #1 约束与 tasks 3.4），不另开系统提示段落——该描述本就承载 name 命名规则等同性质约束，集中一处更不易漂移。 -->
