## MODIFIED Requirements

### Requirement: Gin HTTP server
系统 SHALL 使用 Gin 框架为当前角色启动 HTTP 服务，监听端口由角色决定：`api` 角色与 `all` 角色监听 `server.port`，`agent` 角色监听 `server.agent_port`。

#### Scenario: Server starts on configured port
- **WHEN** 服务以 `api` 或 `all` 角色启动，config.yaml 中 server.port 为 8080
- **THEN** HTTP 服务监听 0.0.0.0:8080

#### Scenario: Agent role starts on agent port
- **WHEN** 服务以 `agent` 角色启动，config.yaml 中 server.agent_port 为 8081
- **THEN** HTTP 服务监听 0.0.0.0:8081

### Requirement: API routing
系统 SHALL 按当前角色注册对应的 API 路由组（角色与路由的对应关系见 `service-roles`）：`all` 角色注册下列全部路由，`api` 角色注册其中的 CRUD 子集，`agent` 角色仅注册流式消息端点与 MCP 工具列表。下列路由清单描述 `all` 角色注册的完整目录。

#### Scenario: Auth routes
- **WHEN** 服务以注册 auth 路由的角色（`api`、`all`）启动
- **THEN** 注册 POST /api/v1/auth/login（无需鉴权）

#### Scenario: Session routes
- **WHEN** 服务以 `api` 或 `all` 角色启动
- **THEN** 注册以下需要鉴权的路由：
  - GET /api/v1/sessions
  - POST /api/v1/sessions
  - GET /api/v1/sessions/:session_id/messages

- **AND** 当且仅当角色为 `all` 时，额外注册 POST /api/v1/sessions/:session_id/messages（流式端点；`agent` 角色独立注册此路由）

#### Scenario: Workspace routes
- **WHEN** 服务以 `api` 或 `all` 角色启动
- **THEN** 注册以下需要鉴权的路由：
  - GET /api/v1/workspace/files
  - POST /api/v1/workspace/upload
  - GET /api/v1/workspace/files/*path
  - GET /api/v1/workspace/files/*path/content

#### Scenario: Tool and skill routes
- **WHEN** 服务以 `api` 或 `all` 角色启动
- **THEN** 注册需要鉴权的 GET /api/v1/skills
- **AND** GET /api/v1/mcp/tools 由 `agent` 角色注册（`all` 角色亦注册）

### Requirement: Command-line interface
系统 SHALL 通过 cobra 提供统一 CLI，包含 `serve` 与 `seed` 两个子命令；`-f`/`--config`（配置文件路径，默认 `config.yaml`）与 `-d`/`--data-dir`（运行时数据根目录，默认当前工作目录）为在根命令上定义、两个子命令均可用的持久化标志。`serve` 子命令 SHALL 额外接受 `--role` 本地标志，取值 `all`（默认）、`api`、`agent`，用于选择进程运行角色（见 `service-roles`）。

#### Scenario: serve 子命令启动 HTTP 服务
- **WHEN** 执行 `serve`（可选附带 `-f`/`-d`/`--role`）
- **THEN** 系统按角色（默认 `all`）启动对应的 Gin HTTP 服务，行为与原 `cmd/server` 一致（含路由注册、graceful shutdown）

#### Scenario: seed 子命令创建用户
- **WHEN** 执行 `seed -username alice`（可选附带 `-password`、`-f`、`-d` 等）
- **THEN** 系统创建用户，行为与原 `bin/seed` 一致

#### Scenario: 指定配置文件路径
- **WHEN** 执行 `serve -f /etc/blowball/config.yaml`
- **THEN** 系统从 `/etc/blowball/config.yaml` 加载配置

#### Scenario: 持久化标志默认值
- **WHEN** 未传入 `-f` 或 `-d`
- **THEN** 配置文件路径默认为当前工作目录下的 `config.yaml`，数据根目录默认为当前工作目录

#### Scenario: 无子命令或未知标志打印帮助并以非零码退出
- **WHEN** 不带任何子命令执行，或传入未定义的标志
- **THEN** 系统打印 cobra 帮助信息并以非零状态码退出，不启动服务

#### Scenario: 通过 --role 选择运行角色
- **WHEN** 执行 `serve --role agent`
- **THEN** 系统以 `agent` 角色启动，仅注册 agent 侧路由并在 `server.agent_port` 监听
