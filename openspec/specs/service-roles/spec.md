# service-roles Specification

## Purpose

定义后端服务进程角色（`all`、`api`、`agent`）的划分与职责，包括角色选择、路由归属、独立 HTTP 监听、共享运行时数据根、按角色命名的日志文件、角色间故障隔离，以及流式对话回合管线在 `agent` 角色内的归属。

## Requirements

### Requirement: Process role selection

主服务 SHALL 保留 serve 的 all/api/agent 角色，默认 all，现有 HTTP 端口和路由分区不变。all SHALL 合并 API/Agent 能力，但 SHALL NOT 内嵌 Scheduler。独立 Scheduler SHALL 使用 services/scheduler 下的独立入口，不作为主服务的新 role 或兼容 fallback。

#### Scenario: 默认角色为 all 以保持向后兼容
- **WHEN** serve 未指定角色
- **THEN** 以 all 注册现有 API/Agent HTTP 并在 server.port 单监听，不启动调度模块

#### Scenario: 显式选择 api 角色
- **WHEN** serve --role api
- **THEN** 只注册 API HTTP 分区，在 server.port 监听，内部提供非 LLM 能力

#### Scenario: 显式选择 agent 角色
- **WHEN** serve --role agent
- **THEN** 只注册 Agent HTTP 分区，在 server.agent_port 监听，并提供任务执行内部 RPC

#### Scenario: 非法角色被拒绝
- **WHEN** serve --role foo 或把 scheduler 当作主服务 role
- **THEN** 非零退出，不启动服务

### Requirement: Role-scoped route ownership

API SHALL 保留鉴权/普通会话 CRUD/消息历史/标题/工作空间/skills/模型等 CRUD，任务 HTTP 管理和运行历史改经 Scheduler RPC，新增任务会话/资源只读或管理接口。Agent SHALL 保留普通流式消息、turn 取消/事件恢复、MCP 工具列表，并提供任务 Run 显式取消；SHALL 不注册任务 CRUD/历史 HTTP。all SHALL 合并两组，不重复注册。

#### Scenario: api 角色不注册流式消息端点
- **WHEN** 以 api 启动
- **THEN** 普通 POST /api/v1/sessions/:session_id/messages 返回 404

#### Scenario: api 角色不注册 turn 生命周期端点
- **WHEN** 以 api 启动
- **THEN** 普通 turn cancel/events 和任务 Run cancel 不注册，返回 404

#### Scenario: 模型列表由 api 角色提供
- **WHEN** api 在线
- **THEN** GET /api/v1/models 提供目录，agent 端口返回 404

#### Scenario: agent 角色不注册 CRUD 路由
- **WHEN** 以 agent 启动
- **THEN** 普通/任务会话列表历史、工作空间及模型 CRUD 均不注册

#### Scenario: turn 生命周期端点由 agent 角色提供
- **WHEN** 以 agent 启动
- **THEN** 现有普通 cancel/events 和任务 Run 显式取消可用

#### Scenario: MCP 工具列表由 agent 角色提供
- **WHEN** 以 agent 启动
- **THEN** GET /api/v1/mcp/tools 仍可用

#### Scenario: all 角色注册全部路由
- **WHEN** 以 all 启动
- **THEN** API/Agent 路由合并，包含任务管理/历史/资源及取消，无冲突

#### Scenario: api 角色管理定时任务
- **WHEN** API 与 Scheduler 可用但 Agent 离线
- **THEN** 不需要执行的任务管理/历史功能可用，主服务不启动本地 Agent 或调度

#### Scenario: agent 角色不注册任务管理路由
- **WHEN** 请求 agent 的任务 CRUD/运行历史 HTTP
- **THEN** 返回 404；其 Agent 管理工具使用内部 RPC

### Requirement: Per-role HTTP listener

`api` 角色 SHALL 在 `server.port` 上启动 HTTP 服务；`agent` 角色 SHALL 在 `server.agent_port` 上启动 HTTP 服务；`all` 角色 SHALL 在 `server.port` 上启动单一 HTTP 服务并注册全部路由。每个角色的 HTTP 服务各自独立进行 graceful shutdown。

#### Scenario: api 角色监听 server.port
- **WHEN** 以 `api` 角色启动且 `server.port` 为 8080
- **THEN** HTTP 服务监听 0.0.0.0:8080

#### Scenario: agent 角色监听 server.agent_port
- **WHEN** 以 `agent` 角色启动且 `server.agent_port` 为 8081
- **THEN** HTTP 服务监听 0.0.0.0:8081

#### Scenario: all 角色在 server.port 上单监听
- **WHEN** 以 `all` 角色启动
- **THEN** 仅在 `server.port` 上启动一个 HTTP 服务（无第二监听），全部路由经此服务对外

#### Scenario: agent 角色端口可独立配置
- **WHEN** `server.agent_port` 未在配置中指定
- **THEN** 系统使用默认 agent 端口（如 8081）或启动时报错提示必须显式配置，二选一由实现决定并在日志中说明

### Requirement: Shared runtime data root

`api` 与 `agent` 角色 SHALL 从同一个 `-d`/`--data-dir` 派生 `data`、`logs`、`skills`、`tools`、`skills-market` 五类落盘位置，并连接同一份 MySQL、Redis 与共享 POSIX 文件系统存储；两角色之间不存在数据面隔离，三层持久化、xizhi 工作空间工具、executor 沙箱与 Landlock 策略的行为均与单进程时一致。该共享 POSIX 文件系统默认为本地磁盘；当配置为共享模式（见 `workspace-shared-storage`，实现为 MinIO 支撑的 JuiceFS 挂载）时，`data` 子树 SHALL 由 operator 在进程启动前挂载就绪，使跨机器的 `api`/`agent` 实例读写同一份 per-user 数据；`skills-market` 子树不参与该共享模式检查，由 operator 在每台主机各自同步维护（同 `tools` 约定）。当 `skill_market.url` 启用时，`api` 角色 SHALL 为 skills 列表端点构造市场客户端（仅 HTTP 客户端装配，不引入 orchestrator/OpenAI 依赖），`agent` 角色 SHALL 为 luban 工具与 executor 沙箱构造同一客户端。

#### Scenario: 两角色共享同一数据目录
- **WHEN** `api` 与 `agent` 角色以相同的 `-d /var/lib/blowball` 启动
- **THEN** 两者读写相同的 `/var/lib/blowball/data`、`/var/lib/blowball/logs`、`/var/lib/blowball/skills`、`/var/lib/blowball/tools`、`/var/lib/blowball/skills-market`

#### Scenario: 两角色共享同一存储后端
- **WHEN** 两角色同时运行
- **THEN** 两者连接配置中同一 MySQL 与 Redis；agent 角色写入的 turn 结果可被 api 角色读取

#### Scenario: 跨机器实例经共享文件系统共享数据面
- **WHEN** `storage.workspace.backend` 为 `shared`，且分别位于两台机器的 `api` 与 `agent` 实例将 JuiceFS 挂载到各自的 `{data-dir}/data`（指向同一 MinIO bucket + 同一元数据引擎）
- **THEN** 一台机器上对 `{data-dir}/data/{userID}/` 的写对另一台机器可见，工作空间与 per-user 数据跨机共享，不存在数据面隔离

#### Scenario: api 角色独立提供含市场条目的 skills 列表
- **WHEN** `skill_market.url` 已启用，仅 `api` 角色运行，用户请求 `GET /api/v1/skills`
- **THEN** api 角色以该请求的 JWT 拉取市场清单并返回合并结果，不依赖 `agent` 角色进程

### Requirement: Role-aware log file

系统 SHALL 按角色命名 `{data-dir}/logs/` 下的日志文件：`api` 角色写入 `blowball-api.log`，`agent` 角色写入 `blowball-agent.log`，`all` 角色写入 `blowball.log`，以避免两个进程的 lumberjack 实例争用同一文件。

#### Scenario: api 角色日志文件名
- **WHEN** 以 `api` 角色启动且 `logging.output` 包含文件
- **THEN** 在 `{data-dir}/logs/` 下产生并写入 `blowball-api.log`

#### Scenario: agent 角色日志文件名
- **WHEN** 以 `agent` 角色启动且 `logging.output` 包含文件
- **THEN** 在 `{data-dir}/logs/` 下产生并写入 `blowball-agent.log`

#### Scenario: all 角色日志文件名
- **WHEN** 以 `all` 角色启动且 `logging.output` 包含文件
- **THEN** 在 `{data-dir}/logs/` 下产生并写入 `blowball.log`（与拆分前一致）

### Requirement: Fault isolation between roles

`api` 角色 SHALL 在运行期不依赖 `agent` 角色所在进程；`agent` 角色进程的崩溃、不可用或重启 SHALL NOT 阻止 `api` 角色继续提供其自有路由（鉴权、会话 CRUD、消息历史读取、工作空间文件 CRUD、skills 列表）。反之，`api` 角色进程不可用 SHALL NOT 阻止 `agent` 角色提供流式消息端点与 MCP 工具列表。

#### Scenario: agent 角色不可用时 api 路由仍可用
- **WHEN** `agent` 角色进程未运行或已崩溃，而 `api` 角色进程正常运行
- **THEN** 对 `api` 角色自有路由（如 `GET /api/v1/sessions`）的请求仍正常返回

#### Scenario: api 角色不依赖 agent 层
- **WHEN** 以 `api` 角色启动
- **THEN** 该进程不实例化 orchestrator、OpenAI 客户端、tool registry 或 executor 沙箱，且其正常运转不需要 `agent` 角色进程在线

### Requirement: Agent role owns the streaming-turn pipeline

`agent` 角色 SHALL 在自身进程内完成一次对话回合所需的全部步骤：会话查询、历史消息恢复（`RecoverMessages`）、orchestrator 执行、SSE 流式回写、turn 结束后的三层持久化（`SaveMessagesBatch`）以及首轮标题生成（`TitleService`）。turn 的运行注册表（run registry）与 Redis run 状态 SHALL 由 `agent` 角色进程持有与维护；取消端点与恢复端点 SHALL 在 `agent` 角色进程内基于该注册表与 Redis run 状态工作。

#### Scenario: 流式端点在 agent 角色内自洽
- **WHEN** 以 `agent` 角色启动并收到 `POST /api/v1/sessions/:session_id/messages` 请求
- **THEN** 会话查询、历史恢复、agent 执行、SSE 回写与持久化均在该 agent 进程内完成，不跨进程调用 `api` 角色

#### Scenario: turn 生命周期端点在 agent 角色内自洽
- **WHEN** 以 `agent` 角色启动并收到取消或恢复端点请求
- **THEN** registry 查询、Redis run 状态读写与（取消路径的）部分持久化均在 agent 进程内或经共享 Redis 完成，不调用 `api` 角色

#### Scenario: 标题生成在 agent 角色内运行
- **WHEN** 首轮对话成功完成
- **THEN** 标题生成由 `agent` 角色进程中的 `TitleService` 异步触发并写入 MySQL，不依赖 `api` 角色进程

### Requirement: Scheduled task runtime ownership

Scheduler SHALL 独立承担后台调度与恢复；all/api/agent SHALL 均不启动该模块。Agent/all SHALL 通过内部执行 RPC 共用现行 turn/Run registry/Agent 处理任务，并持有执行凭证/时限；API/all SHALL 只提供非 LLM 内部及公共管理/历史能力，不构造 Agent 依赖。跨服务任务管理、启动授权和资源引用 SHALL 经内部 RPC。

#### Scenario: Agent role executes without an API process
- **WHEN** Agent 在线但所需 API 身份/资源校验不可用
- **THEN** 不能绕过校验启动新任务，不回退内嵌调度；已接收任务按可靠状态和既定时限处理

#### Scenario: API role remains lightweight
- **WHEN** 只有 API 与 Scheduler 在线
- **THEN** 管理/历史可用，不运行模型或加载 Agent

#### Scenario: Combined role starts one scheduler
- **WHEN** all 启动
- **THEN** 内嵌调度实例数量为零；调度必须由独立 Scheduler 提供，执行仍共用 turn 代码

### Requirement: Role-scoped internal RPC listeners

API/Agent SHALL 按角色提供独立配置的内部 RPC 能力；all 可以在同一内部监听器合并两组。内部监听 SHALL 不改变公共 HTTP 角色/端口，并遵循默认 mTLS 或显式 `plaintext_token` 模式的身份认证与原服务身份授权。主服务未启用内部 RPC 时，缺少 Scheduler endpoint SHALL NOT 阻止普通主服务启动；明确启用但配置不完整时 SHALL 拒绝 RPC 初始化，不能通过关闭认证或更换模式规避配置错误。配置完整后，Scheduler 暂时不可达 SHALL NOT 阻止普通主服务启动。旧启用内嵌调度配置 SHALL 给出明确迁移错误/指引，不能启动第二调度器。

#### Scenario: Scheduler is offline at main service startup
- **WHEN** Scheduler 暂时不可达
- **THEN** 普通主服务可启动；任务调用明确不可用，无本地 fallback

#### Scenario: Legacy embedded scheduling configuration remains
- **WHEN** 主服务仍配置旧 scheduled_tasks.enabled=true
- **THEN** 明确提示迁移且不启动内嵌实例，禁止静默双调度

#### Scenario: Disabled internal RPC preserves normal startup
- **WHEN** 主服务 `internal_rpc.enabled: false`，安全凭证或 Scheduler endpoint 未配置
- **THEN** 不创建内部监听或客户端，不校验未使用的安全凭证，普通聊天/HTTP 角色行为保持不变

#### Scenario: Explicitly enabled RPC needs complete configuration
- **WHEN** 主服务明确启用内部 RPC，但所选模式所需凭证或 endpoint 不完整
- **THEN** 配置/初始化明确失败，不跳过认证，不自动改用另一模式

### Requirement: Process-wide internal RPC security configuration

主服务 SHALL 使用 `internal_rpc.security_mode`，Scheduler SHALL 使用 `rpc.security_mode`，支持且仅支持 `mtls` 和 `plaintext_token`；启用的 RPC 在省略模式时 SHALL 默认 `mtls`，未知模式 SHALL 拒绝启动。该模式 SHALL 同时用于所属进程的内部监听与所有内部出站连接，不提供逐 endpoint 混合模式。mTLS SHALL 保留原 TLS 字段路径和校验；明文模式 SHALL 使用主服务 `internal_rpc.peer_tokens`/`internal_rpc.client_token`、Scheduler `rpc.peer_tokens`/`main_service.client_token`。

明文入站映射 SHALL 非空，键 SHALL 仅为现有 `blowball-scheduler`、`blowball-api`、`blowball-agent`、`blowball-main`；不同身份 SHALL NOT 共享 Token。入站和出站 Token SHALL 非空、只包含可打印 ASCII 字符且无空白/控制字符。程序 SHALL 在环境变量展开后校验，SHALL NOT 提供默认 Token。主服务关闭内部 RPC 时 SHALL 不要求这些凭证；Scheduler 仅关闭执行时 SHALL 仍校验 RPC 配置。endpoint、timeout 和调度正值校验 SHALL 保持原行为。

Token 环境引用 SHALL 作为字符串值只展开一次，SHALL 保留引号、反斜杠和环境变量值中的字面 `${VAR}`，不得将其解释为额外的 YAML 语法或再次展开。此处理 SHALL 保留别名解析和其他字段既有的环境展开、类型及校验语义。

#### Scenario: Existing configuration keeps mutual TLS
- **WHEN** 现有有效 TLS 配置省略 `security_mode`
- **THEN** 监听和出站连接均继续使用 mTLS，不需要迁移字段

#### Scenario: Shared mode covers separate API and Agent targets
- **WHEN** Scheduler 的 API/Agent endpoint 不同且选择 `plaintext_token`
- **THEN** 两个出站连接及自身监听均使用明文 Token 模式，出站均使用 `main_service.client_token`

#### Scenario: Token configuration fails after missing environment expansion
- **WHEN** 明文模式需要的 Token 环境变量未设置，展开后为空
- **THEN** 初始化前拒绝配置，指出字段而不输出凭证

#### Scenario: Unknown or duplicate identity binding
- **WHEN** 入站映射包含未知身份，或不同身份配置相同 Token
- **THEN** 拒绝配置，不能根据映射顺序或请求头猜测身份

#### Scenario: Token is not representable as one credential value
- **WHEN** Token 含空白、控制字符或非 ASCII 字符
- **THEN** 拒绝配置，不等到 RPC 发送阶段才失败，也不自动修剪 Token

#### Scenario: Preserve opaque token contents from the environment
- **WHEN** 入站或出站 Token 环境变量包含合法的引号、反斜杠或字面 `${VAR}`，并通过 YAML 占位符提供
- **THEN** 两侧加载后的 Token 与环境变量原值完全相同，不因 YAML 解析改变凭证，也不再次展开字面引用

#### Scenario: Token expansion preserves aliases and existing scalar types
- **WHEN** Token 占位符使用 YAML 别名或合并配置，其他字段仍使用未加引号的数值/布尔或多行 YAML 环境值
- **THEN** Token 按原值解析，其他字段继续保持其既有类型、层级和校验结果

#### Scenario: Tagged token placeholder is rejected without echoing the credential
- **WHEN** 入站或出站 Token 占位符标量携带非字符串 YAML 标签（如 `!!int`、`!!bool`）并引用环境变量
- **THEN** 配置加载在恢复凭证前拒绝，错误指明字段与行号，不包含 Token 值

#### Scenario: Unknown enabled security mode
- **WHEN** 已启用 RPC 的 `security_mode` 为不支持的值
- **THEN** 启动失败，不默认明文或跳过授权

#### Scenario: Disabled execution still has authenticated RPC
- **WHEN** Scheduler 设置 `scheduler.execution_enabled: false` 并选择明文模式
- **THEN** 仍要求有效入站/出站 Token，任务管理和健康检查不变成匿名接口

### Requirement: Observable plaintext RPC startup

主服务和 Scheduler 成功启用 `plaintext_token` 时 SHALL 在启动日志记录 WARN，包含安全模式、监听/目标地址及“链路未加密、服务端身份未验证”的风险说明，SHALL NOT 包含 Token。配置和部署示例 SHALL 将明文模式标为受控开发/隔离联调用途，并使用回环地址；内网正式部署 SHALL 推荐 mTLS，SHALL NOT 将 Token 或内网位置描述为等价的 TLS 防护。

#### Scenario: Plaintext startup is explicit and observable
- **WHEN** 主服务或 Scheduler 以明文模式启动内部 RPC
- **THEN** 日志出现风险 WARN，不输出任何入站或出站 Token

#### Scenario: Mutual TLS startup is not mislabeled
- **WHEN** 主服务或 Scheduler 使用默认 mTLS 模式
- **THEN** 不记录明文启用告警，也不因残留 Token 字段改变传输模式

### Requirement: Internal mutual TLS deployment guidance

部署文档 SHALL 提供内网 CA 信任、客户端服务身份 CN、服务端 SAN 与 `server_name`、`clientAuth`/`serverAuth` 用途、证书/私钥路径和只读挂载的配置对应关系。文档 SHALL 区分 CN 的应用身份与 SAN 的服务端主机名验证，提供受控开发签发/验证指引和缺文件排查，不要求购买公网证书。CA 私钥 SHALL NOT 放入运行容器或仓库，新增示例 SHALL 不包含真实秘密；文档 SHALL 警告不得将 RPC Token 变量转交给 executor 环境白名单、环境字面量或 MCP 服务环境。

#### Scenario: Diagnose a missing Scheduler client certificate
- **WHEN** 部署出现 `main_service.tls` 客户端证书文件不存在的错误
- **THEN** 指引说明运行环境路径、挂载和配置映射的检查步骤，不建议关闭 CA/主机名验证或自动降级

#### Scenario: Distinguish service identity from hostname verification
- **WHEN** 部署方配置主服务和 Scheduler 的 mTLS 证书
- **THEN** 指引分别说明客户端 CN 身份、服务端 SAN/ServerName 匹配和必要的证书用途，CA 私钥不进入运行容器

#### Scenario: Restore mutual TLS before binary rollback
- **WHEN** 从支持明文模式的新版本回退到旧二进制
- **THEN** 指引要求先恢复 mTLS 配置与有效证书，再协调回退，不直接使用明文配置运行旧版本，也不修改任务数据
