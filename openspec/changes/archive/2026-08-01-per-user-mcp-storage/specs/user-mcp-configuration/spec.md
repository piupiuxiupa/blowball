## MODIFIED Requirements

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

## ADDED Requirements

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
