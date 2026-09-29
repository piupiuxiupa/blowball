## ADDED Requirements

### Requirement: MCP market configuration block
系统 SHALL 支持顶层 `mcp_market:` 配置块，字段：`url`（字符串）、`timeout`（duration，默认 `5s`）、`cache_ttl`（duration，默认 `60s`）。块省略或 `url` 为空 SHALL 等同于功能关闭，行为与本能力引入前逐字节一致；`url` 非空即启用，不设独立 `enabled` 开关。所有字符串值 SHALL 走既有 `${VAR}` / `${VAR:default}` 环境展开。启用状态下 `url` MUST 为绝对 `http(s)` URL；`timeout` / `cache_ttl` 为负值时配置加载 SHALL 失败。

#### Scenario: 块省略时功能关闭
- **WHEN** 配置中不含 `mcp_market` 块，或块存在但 `url` 为空
- **THEN** 不构造市场客户端、不发出任何出站请求，`mcp_*` 工具、系统提示词、`GET /api/v1/mcp/tools` 的行为与本能力引入前逐字节一致

#### Scenario: 环境变量展开
- **WHEN** 配置为 `mcp_market.url: "${MCP_MARKET_URL}"` 且环境变量 `MCP_MARKET_URL` 已设置
- **THEN** 加载后的 `url` 为该环境变量的值

#### Scenario: 非法 URL 被拒绝
- **WHEN** `mcp_market.url` 非空但不是绝对 `http(s)` URL
- **THEN** 配置加载失败，进程拒绝启动

#### Scenario: 负 duration 被拒绝
- **WHEN** `mcp_market.timeout` 或 `mcp_market.cache_ttl` 配置为负值
- **THEN** 配置加载失败，进程拒绝启动

### Requirement: Market allowlist fetch with the caller's login JWT
启用时，系统 SHALL 以发起会话用户 login 返回的 JWT 为凭证拉取该用户的 MCP 市场清单：`GET {mcp_market.url}`，请求头 `Authorization: Bearer <JWT>`，不携带其他参数；用户身份由市场服务从 token 解出。响应 SHALL 按 `{"servers":[{slug,name,description,path}]}` 解析：`name` 作为 server 名主键与遮蔽键、`description` 用于展示、`path` 用于磁盘路径拼接；`slug` 与其他未知字段 SHALL 被忽略。

#### Scenario: 成功拉取并解析清单
- **WHEN** 市场服务对该 JWT 返回 200 与 `{"servers":[{"slug":"s","name":"github-market","description":"…","path":"/019fef90-…/github-market"}]}`
- **THEN** allowlist 含一条 server：name=`github-market`，path=`/019fef90-…/github-market`

#### Scenario: 鉴权失败按 fail-closed 处理
- **WHEN** 市场服务返回 401/403
- **THEN** 记 WARN 并按空 allowlist 处理，不重试、不中断调用方工具

### Requirement: Per-user allowlist TTL cache
系统 SHALL 以 userID 为 key 缓存解析后的 allowlist，TTL 为 `mcp_market.cache_ttl`（默认 60s）；TTL 窗口内同一用户的多次访问 SHALL NOT 产生额外出站请求。缓存 SHALL 只保存 API 返回值，不掺入磁盘存在性状态。

#### Scenario: TTL 窗口内单次出站
- **WHEN** 同一用户在 TTL 窗口内先后调用 `mcp_list_servers` 与 `mcp_call`（市场 server）
- **THEN** 只向市场服务发出一次清单请求，第二次访问使用缓存

### Requirement: Fail-closed degradation
市场 URL 不可达、超时、返回非 200、响应解析失败或鉴权失败时，系统 SHALL 记一条 WARN 并按空 allowlist 继续：市场 server 不出现在任何列表、广告与工具端点中，调用返回 "not configured" 类错误。系统 SHALL NOT 在任何失败路径下回退为扫描 `{data-dir}/mcp-market` 目录。

#### Scenario: 市场不可达时降级
- **WHEN** 市场服务宕机，用户调用 `mcp_list_servers`
- **THEN** 返回仅含工作空间 server 的列表，附带 WARN 日志，工具调用本身成功

#### Scenario: 永不回退为目录扫描
- **WHEN** 市场拉取失败，但 `{data-dir}/mcp-market` 下存在已同步的 server 目录
- **THEN** 这些 server 不出现在任何列表或调用路径中

### Requirement: Market disk layout and path safety
市场 MCP 服务实体 SHALL 位于 `{data-dir}/mcp-market/{market_user_id}/{server_name}/config.json`，由 operator 同步维护；blowball SHALL NOT 写入、修改或删除该目录树。API 返回的 `path` 拼接磁盘路径时 SHALL 执行 `filepath.Join(marketRoot, path)` 后 `filepath.Clean` 并断言结果仍位于 marketRoot 之下（拒绝 `..` 等逃逸）。`config.json` SHALL 按现有 per-user `Server` JSON schema 解析并复用 `validateServer` 与 `ValidateName`（非法条目跳过并记日志，不导致整体失败）；server 名取 allowlist `name`。凭据管理不在本能力范围：config.json 中的 auth 字段按现有服务端注入与脱敏规则处理，本能力不提供任何凭据写入入口。

#### Scenario: 正常路径拼接与解析
- **WHEN** allowlist 含 `path: "/019fef90-…/github-market"` 且该目录的 `config.json` 已同步
- **THEN** 解析出一个 name=`github-market` 的 server，连接与调用语义与工作空间 server 一致

#### Scenario: 逃逸路径被拒绝
- **WHEN** allowlist 中某条目的 `path` 经 Clean 后逃逸出 marketRoot
- **THEN** 该条目被丢弃并记 WARN，任何工具不可经由它读取 marketRoot 之外的文件

#### Scenario: 非法 config.json 被跳过
- **WHEN** 某市场条目的 `config.json` 为非法 JSON 或未通过 `validateServer`
- **THEN** 该 server 不可用并被跳过，其余市场条目与工作空间 server 不受影响

### Requirement: Local-first name conflict precedence
server 名解析顺序 SHALL 为 `workspace > market`：市场 server 与工作空间 server 同名时，工作空间版本胜出，市场条目不出现；删除工作空间同名 server 后市场条目重新可见。

#### Scenario: 工作空间同名 server 覆盖市场条目
- **WHEN** 用户工作空间存在 server `foo`，市场 allowlist 亦含名为 `foo` 的 server
- **THEN** 列表与调用均解析到工作空间版本，市场条目不出现

#### Scenario: 删除本地遮蔽后市场条目恢复
- **WHEN** 用户以 `mcp_remove_server` 删除遮蔽市场条目的工作空间 server `foo`
- **THEN** 市场 `foo` 在后续列表与调用中重新可见

### Requirement: Market directory is read-only and non-removable
系统 SHALL NOT 提供任何修改、删除或写回市场目录的路径：`mcp_remove_server` 对仅存在于市场的 server SHALL 返回明确的"市场 server 不可删除"错误；`mcp_list_tools` / `mcp_call` 的 tools 缓存写回 SHALL 跳过市场条目（仅刷新 turn 内存）。进程级沙箱（Landlock）SHALL 将 `{data-dir}/mcp-market` 置为只读。

#### Scenario: 市场条目不可删除
- **WHEN** agent 对仅存在于市场的 server 调用 `mcp_remove_server`
- **THEN** 返回"market server cannot be removed"类明确错误，磁盘无任何变更

#### Scenario: 缓存写回跳过市场条目
- **WHEN** 对市场 server 调用 `mcp_list_tools` 触发 tools/list 刷新
- **THEN** 刷新结果仅更新 turn 内存，不向 `{data-dir}/mcp-market` 写入任何文件

### Requirement: Disk-sync lag tolerance
列表与系统提示词广告 SHALL 以 allowlist 为准：磁盘尚未同步的市场 server 照常出现（不因读取失败而隐藏）；`mcp_call` / `mcp_list_tools` 对磁盘缺失的市场 server SHALL 返回 "directory not found (disk sync incomplete)" 类明确错误。

#### Scenario: 看得到读不到时明确报错
- **WHEN** allowlist 含 server `bar` 但对应 `config.json` 尚未同步，调用 `mcp_call(server="bar")`
- **THEN** 返回明确的 "directory not found" 错误，而非静默隐藏或 "not configured"
