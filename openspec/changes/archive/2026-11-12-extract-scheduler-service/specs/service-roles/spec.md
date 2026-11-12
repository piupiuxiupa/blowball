## MODIFIED Requirements

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

## ADDED Requirements

### Requirement: Role-scoped internal RPC listeners

API/Agent SHALL 按角色提供独立配置的内部 RPC 能力；all 可以在同一内部监听器合并两组。内部监听 SHALL 不改变公共 HTTP 角色/端口，并遵循 mTLS/服务身份授权。缺少 Scheduler endpoint 或 Scheduler 故障 SHALL 不阻止普通主服务启动；旧启用内嵌调度配置 SHALL 给出明确迁移错误/指引，不能启动第二调度器。

#### Scenario: Scheduler is offline at main service startup
- **WHEN** Scheduler 暂时不可达
- **THEN** 普通主服务可启动；任务调用明确不可用，无本地 fallback

#### Scenario: Legacy embedded scheduling configuration remains
- **WHEN** 主服务仍配置旧 scheduled_tasks.enabled=true
- **THEN** 明确提示迁移且不启动内嵌实例，禁止静默双调度
