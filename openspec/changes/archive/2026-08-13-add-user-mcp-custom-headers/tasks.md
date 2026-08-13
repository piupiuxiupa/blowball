## 1. 配置结构与校验

- [x] 1.1 `internal/tool/mcp/config.go`：`Server` 增 `Headers map[string]string \`json:"headers,omitempty"\``
- [x] 1.2 `validateServer` 增自定义头校验：头名合法 HTTP token；头值禁 `\r`/`\n`；保留头名（`Content-Type`/`Content-Length`/`Accept`/`Mcp-Session-Id`/`Host`，canonical 归一大小写不敏感）拒绝；错误信息列明被拒头名与保留集
- [x] 1.3 确认 `LoadConfig`/`WriteServer` 对 `Headers` 自动 JSON 往返；旧 config（无该字段）→ nil map → 零行为变更

## 2. 注入合并

- [x] 2.1 `internal/tool/mcp/manager.go` 的 `DefaultTransportFactory`：`merged := clone(server.Headers)` → 用 `authHeaders(server.Auth)` 覆盖（auth 胜）→ 传 `NewHTTPTransport`
- [x] 2.2 确认 `HTTPTransport.post` 施加次序不变（Content-Type/Accept → headers → Mcp-Session-Id），无需改传输层

## 3. 管理工具适配

- [x] 3.1 `internal/tool/mcp/auth.go`：`serverView` 增 `Headers map[string]string \`json:"headers,omitempty"\``；`serverViewFrom` 回填（明文，不脱敏）
- [x] 3.2 `internal/tool/mcp/register.go`：`mcp_add_server` 的 `ParametersJSON` 增可选 `headers`（object，string→string）；Execute 闭包解析
- [x] 3.3 `addServer` 签名增 `headers map[string]string`，写入 `Server.Headers`；`validateServer(server)` 在连接前已覆盖头校验
- [x] 3.4 `mcp_add_server` 工具描述（含新增 `headers` 参数的 description）写明引导：`headers` 为**非敏感**业务参数、不得放置密钥（密钥只能放 `auth`）、私有/非标准鉴权用 `auth.type: none` + 自定义头、传输层自管头名（`Content-Type`/`Content-Length`/`Accept`/`Mcp-Session-Id`/`Host`）会被拒、同名冲突时 auth 胜出

## 4. 测试与验收

- [x] 4.1 `internal/tool/mcp/mcp_test.go`：自定义头与 auth 同名时 auth 胜；仅自定义头时按原样注入
- [x] 4.2 保留头名（5 个，含大小写变体如 `content-type`）被 `validateServer` 拒绝
- [x] 4.3 头值含 `\r`/`\n` 或头名非法 token 被拒
- [x] 4.4 `mcp_list_servers` 明文回显自定义头（不脱敏）；`mcp_add_server` 往返持久化后重读一致
- [x] 4.5 `TestAuthHeaders` / 既有脱敏用例保持绿（确认未削弱 auth 脱敏）
- [x] 4.6 `make test` 与 `go test ./internal/tool/mcp/...` 全绿；确认未触碰 operator MCP 与传输层
