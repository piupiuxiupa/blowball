## Why

现有定时任务的调度、恢复和 Agent 执行位于同一进程，任务产生的会话也存入普通会话表，难以独立部署调度系统或在产品中区分后台执行与实时聊天。本变更在同一个 Git 仓库内先拆出 Scheduler，并同时提供独立任务会话存储和用户工作空间中的 `.cron` 资源管理，让用户能够维护任务配置并查看每次执行的真实历史。

## What Changes

- 在 monorepo 中新增 `services/scheduler/` 独立服务目录、启动入口、配置和部署单元；首期保留根 Go module 和现有 `cmd/blowball/` 主服务入口，不搬动无关模块。
- 使用版本化 gRPC 契约连接 Scheduler 与现有 API/Agent 角色。现有定时任务 HTTP 路径保留，由 API 适配到 Scheduler；调度服务不可用时不降级为本地调度或直接数据库写入。
- Scheduler 独占任务定义、时间规则、触发记录和调度恢复；主服务继续负责用户授权、任务资源、任务会话内容、LLM 凭证和 Agent 执行。
- 新增独立的 `scheduled_sessions` 及消息、工具调用、标题、用量、压缩、子 Agent 和所需附件/归档关联表；复用现有 turn 引擎，不复制一套 Agent 系统。
- 新触发固定采用“一次触发一个任务会话”；任务会话不进入普通会话列表，首期仅提供执行历史读取、删除和所属任务配置管理，不提供自由对话或手动重跑。
- 在用户工作空间的 `.cron/<task_id>/` 中管理可编辑草稿和不可原地修改的发布版本。触发快照固定任务配置、资源版本和 manifest 摘要；资源变更不改变已经物化的执行。
- 保留名称、主指令、模型、推理强度、at/cron 时间规则、时区和启停配置，增加资源版本引用和并发编辑保护；历史执行配置与当前可编辑配置分开返回。
- 新增 Agent 定时任务管理工具，与 HTTP 使用同一授权、校验及配置更新能力。
- 增加 Runtime 持久化执行凭证、幂等提交、跨服务启动授权和终态对账；执行是否停止不明时持久化任务阻塞，防止后续触发与旧执行重叠；不承诺外部工具副作用 exactly-once。
- **BREAKING**：`all/agent` 不再内嵌调度循环；启用定时执行必须部署独立 Scheduler，并迁移旧调度配置。
- **BREAKING**：禁止新任务或任务更新采用 `session.mode=existing`。已有此类任务暂停并要求显式迁移；不自动把普通聊天转换为任务会话。
- **BREAKING**：`.cron` 成为受管理命名空间。通用文件入口仅可修改授权草稿；bash 对 `.cron` 只读；发布区及受保护祖先不可经通用接口覆盖、移动或删除。
- 不实施 Gateway、WebSocket 接入、Firecracker、风险审查 Agent，也不改变现有 bash 风险分级或网络默认策略。

## Capabilities

### New Capabilities

- `scheduled-task-service`: 独立 Scheduler、monorepo 边界、内部 gRPC 身份与权限、持久化幂等执行、启动授权、终态对账和故障隔离。
- `scheduled-task-sessions`: 物理独立的任务会话表组、一次触发一个会话、类型明确的持久化适配、只读历史接口、删除归档与遗留历史兼容。
- `scheduled-task-resources`: `.cron` 草稿、不可变发布版本、内容清单、发布与配置引用、执行快照、写保护及资源生命周期。

### Modified Capabilities

- `scheduled-agent-tasks`: 调度服务所有权、任务配置与资源引用、专属会话策略、执行身份、恢复/停机和历史语义，以及 Agent 管理工具。
- `service-roles`: 保留 API/Agent HTTP 角色，新增角色对应的内部 gRPC 能力，移除主服务中的调度实例与后台循环。
- `workspace-api`: 增加 `.cron` 的用户作用域、草稿编辑、发布版本读取及受保护路径写入限制。
- `executor-tools`: 在既有 bwrap 中把 `.cron` 绑定为只读，防止脚本绕过文件入口改写任务资源；其余执行策略保持不变。

## Impact

- 代码影响：`cmd/blowball/`、新 `services/scheduler/`、`internal/scheduler`、任务 handler/service/store、session/message/compaction/subagent/attachment 持久化、turn 入口、运行恢复、工作空间及文件工具、executor 挂载和工具注册。
- 契约影响：新增 `api/proto/` 内部服务定义与生成 Go 客户端；扩展 `api/openapi.yaml` 中的任务配置/运行 DTO，新增任务会话及资源发布接口。保持现有普通聊天 HTTP/SSE 合同。
- 数据影响：新增顺序 SQL migrations、任务资源引用、独立任务会话表组和 Runtime 执行凭证；旧会话不自动迁移，遗留历史显式标记来源。Scheduler 与主服务首期可以共用 MySQL 实例，但各自独占业务表写入。
- 运行影响：新增 gRPC/protobuf 依赖、受限服务身份、调度配置与构建/部署目标；多 API/Agent 节点继续使用已有共享 POSIX 工作空间，Scheduler 不挂载用户工作空间。
- 前端影响：提供“定时任务 → 执行历史 → 任务会话”的独立查询契约、当前配置编辑与历史快照展示。本仓库没有前端源码，前端实现不在本变更范围内。
- 验证影响：增加启动/提交故障矩阵、数据表隔离、资源发布并发与路径防护、旧任务迁移、HTTP/工具一致性和普通聊天回归测试。
