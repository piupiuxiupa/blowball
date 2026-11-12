## MODIFIED Requirements

### Requirement: User-owned scheduled task management

系统 SHALL 保留已鉴权任务端点 POST/GET /api/v1/scheduled-tasks 及 GET/PATCH/DELETE /api/v1/scheduled-tasks/:task_id，由 API 通过 Scheduler RPC 执行业务。owner SHALL 来自可信鉴权上下文。任务 SHALL 包含非空 name/prompt、时间规则、专属会话策略、可选 model/reasoning_effort、资源版本引用和默认 true 的 enabled；无资源任务允许空引用。prompt、模型和时间 SHALL 遵循既有输入/目录校验。创建/修改/删除成功 SHALL 分别返回 201/200/204；不得直接启动 Agent。

#### Scenario: Create a user-owned task
- **WHEN** 已鉴权用户提交合法配置
- **THEN** Scheduler 保存 owner、task_id、revision 和 next_run_at，返回 201
- **AND** 接口不直接执行 Agent

#### Scenario: Reject another user's resources
- **WHEN** 用户读取/修改/删除他人任务或绑定他人资源
- **THEN** 返回 404，不泄露存在性或修改数据

#### Scenario: Reject invalid input and missing authentication
- **WHEN** JWT 无效或配置、模型、资源引用、会话策略或时间非法
- **THEN** 返回 401 或已鉴权的 400，不保存配置、占用运行或调用模型

#### Scenario: Task management RPC is unavailable
- **WHEN** Scheduler 不可用
- **THEN** API 返回 503，不直接写数据库或运行本地调度

### Requirement: Durable and atomic slot materialization

任务和触发 SHALL 持久化到 Scheduler 所有的 MySQL 表，不依赖内存表或 Redis Run TTL。Scheduler SHALL 在同一事务领取到期任务、保存 immutable 配置/资源快照及稳定 run/session 身份、插入触发并推进 next_run_at；(task_id, scheduled_for) SHALL 唯一。at 物化后清空 next_run_at，cron 推进到严格晚于当前时间的下一槽位。

#### Scenario: Concurrent scheduler replicas scan the same slot
- **WHEN** 多个 Scheduler 副本扫描同一任务/计划时间
- **THEN** 最多物化一条该槽位触发，固定同一 Run 和任务会话身份

#### Scenario: Crash around materialization
- **WHEN** 进程在物化提交前或提交后、执行接收前崩溃
- **THEN** 提交前回滚，提交后触发可恢复，不出现计划推进而缺失触发的情况

### Requirement: Per-task non-overlap

同一任务 SHALL 至多拥有一个可执行的非终态触发。pending/preparing/launching/running 或执行是否停止不明的 hold 存在时，后续槽位 SHALL 跳过，不排队重叠执行；普通 active 情况记录 TASK_OVERLAP，uncertain hold 记录 TASK_EXECUTION_UNCERTAIN。结果未知的 interrupted 标签 SHALL 不自动解除 hold，只有可信停止证明和受控围栏对账可以解除。

#### Scenario: Previous execution is still active
- **WHEN** 任务再次到期且前次仍未结束
- **THEN** 新槽位 skipped/TASK_OVERLAP，不影响前次执行

#### Scenario: Previous execution is unknown but may be active
- **WHEN** 前次历史已标记 interrupted，但 Runtime 未证明执行停止
- **THEN** 后续槽位 skipped/TASK_EXECUTION_UNCERTAIN，不启动新的 Agent

### Requirement: Explicit session targeting and background admission

每个新触发 SHALL 使用属于 owner 的独立 scheduled session，不附加前次完整对话、不指定普通会话。兼容 session.mode=new；新创建/更新使用 existing SHALL 被拒绝，存量 existing 任务 SHALL 暂停并要求显式迁移。后台 admission SHALL 原子尊重会话单活动 Run；Redis 不能确认时 SHALL fail-closed，不使用交互请求的无锁降级。普通聊天 FIFO/admission 不变。

#### Scenario: Execute in independent sessions
- **WHEN** 周期任务先后触发两次
- **THEN** 创建两个 scheduled session，同一触发重复准备不多建会话
- **AND** 用户 workspace、skills 和记忆能力仍遵循既有开关、策略和授权范围

#### Scenario: Reject an existing interactive target
- **WHEN** 创建/更新提交 session.mode=existing 或实时 session ID
- **THEN** 返回 400，不绑定或修改普通会话

#### Scenario: Admission storage is unavailable
- **WHEN** Redis admission 失败
- **THEN** 未确认互斥前不执行模型，明确未启动步骤仅有界恢复
- **AND** 交互消息降级规则不变

### Requirement: Runtime owner authorization and credential secrecy

任务 SHALL 只授权在 owner 现有权限内执行。Scheduler SHALL 在启动授权事务检查任务未暂停/删除及有效领取；Runtime SHALL 在接收和启动前重查 active owner、专属会话和资源归属。用户不可用 SHALL 记录 USER_UNAVAILABLE 并暂停任务。LLM 凭证 SHALL 在 Runtime 执行时按现有 per-user/全局规则解析，不保存到任务、快照、RPC 业务消息或资源。

市场等能力需要的执行 JWT SHALL 由主服务在检查后短期签发，仅在内存身份链路使用；SHALL NOT 公开、持久化、记录 raw/log、注入工具输入或 bash 环境。Scheduler SHALL 不持有 JWT 签名密钥，不保存创建请求的登录 JWT；SHALL NOT 增加公开免密码签发或通用特权 fallback。

#### Scenario: User logs out or the original JWT expires
- **WHEN** 创建请求 JWT 已失效，但任务仍有效且用户 active
- **THEN** Runtime 可以签发有界执行身份，不依赖保存旧 token

#### Scenario: User becomes unavailable before execution
- **WHEN** 用户在物化后、启动前被删除或禁用
- **THEN** 不启动模型，记录 USER_UNAVAILABLE 并暂停任务

#### Scenario: User changes an LLM credential
- **WHEN** 用户更换自己的 LLM token 后触发任务
- **THEN** 使用执行时当前 token；配置的用户 token 认证失败不回退全局重试

#### Scenario: Credentials remain secret
- **WHEN** 执行使用 JWT/LLM token 并产生运行日志及历史
- **THEN** Scheduler 表、RPC 业务参数、任务资源、响应、工具输入和日志没有 token 本体

### Requirement: Pause resume update and delete semantics

系统 SHALL 保留 PATCH enabled 暂停/恢复与 DELETE 软删除。暂停/删除 SHALL 与跨服务 start grant 原子协调，阻止尚未越过授权 fence 的执行；已授权 Run SHALL 不被隐式重跑或取消，显式取消使用任务 Run 入口。配置与资源引用修改 SHALL 只影响未来新物化快照。恢复 cron SHALL 从当前时间计算下一槽位；恢复已消费 at SHALL 不重放。带 expected_revision 的过期编辑 SHALL 返回 409，不覆盖当前配置。execution hold SHALL 为只读服务状态，配置编辑/恢复不能解除无停止证明的阻塞。

#### Scenario: Pause races with startup
- **WHEN** 暂停先于 start grant 生效，包含过期领取者迟到
- **THEN** 未启动触发取消且不能获得执行授权
- **AND** 已越过 fence 的 Run 继续既有生命周期

#### Scenario: Resume a paused cron
- **WHEN** 用户恢复暂停一段时间的 cron
- **THEN** 下一槽位晚于当前时间，不补暂停历史

#### Scenario: Enable an unconsumed future at task
- **WHEN** 未来 at 任务创建时禁用，或在计划时间前暂停并恢复
- **THEN** 保留原来尚未消耗的槽位，禁用时不触发，恢复后只执行该槽位一次
- **AND** 已消费 at 的空 next_run_at 不因恢复而重建

#### Scenario: Update an already materialized task
- **WHEN** 用户修改指令、模型、时间或资源版本，已有触发快照
- **THEN** 旧快照不变，仅后续新触发使用新配置
- **AND** 仅启用已消费 at 不重复执行，显式设置新未来 run_at 才重新安排

#### Scenario: Delete a task with an active Run
- **WHEN** 用户删除运行中任务
- **THEN** 未来/未授权执行被阻止，已运行内容与资源保持并收尾
- **AND** Task GET 返回 404，内部触发事实和任务会话仍保留

#### Scenario: Reject a stale configuration edit
- **WHEN** 提交的 expected_revision 不等于当前 Task revision
- **THEN** 返回 409，配置和当前资源引用不变

### Requirement: Fenced preparation and conservative execution recovery

Scheduler SHALL 保留准备 lease/claim token、稳定身份及 launching 状态，并使用 Runtime 持久化 receipt 和唯一 start grant 对账。明确未启动且旧 worker 已围栏的步骤 SHALL 保留最多三次持久化有界重试；start_intent 后或启动结果不明 SHALL NOT 自动重放 Agent。恢复 SHALL 使用可信 grant、owner 和终态事实，否则记 interrupted/RESULT_UNKNOWN，不承诺外部副作用 exactly-once。

#### Scenario: Recover an expired preparation lease
- **WHEN** 准备 lease 过期且 Runtime 证明尚未进入不确定启动窗口
- **THEN** 围栏旧 worker 后可恢复准备，保留相同 run/session 身份

#### Scenario: Crash after submission may have started a Run
- **WHEN** launching 提交响应丢失或 worker 死亡
- **THEN** 先查询 receipt/grant；不确定则中断，不新建 Run 重执行该槽位

#### Scenario: Reconcile a running or completed Run
- **WHEN** receipt 显示受管存活 Run 或可靠终态
- **THEN** 继续跟踪或补记终态，不产生第二次 Agent 执行

### Requirement: Durable execution history and in-app results

系统 SHALL 保留 owner-only GET /api/v1/scheduled-tasks/:task_id/runs，返回触发状态、原因、时间、session/run ID、明确 session_source、历史配置/资源快照和实际执行模型。任务与运行列表 SHALL 保留默认 limit 20、最大 100 及现有排序。新消息/工具/产物/用量 SHALL 使用独立任务会话存储及接口；Scheduler 终态 SHALL 依据 Runtime 可靠持久化事实回写。

#### Scenario: Complete without an SSE client
- **WHEN** 任务无订阅者并完成
- **THEN** 任务表组保存内容/用量，receipt 和触发历史保存关联身份及终态

#### Scenario: Redis replay window expires
- **WHEN** Run 短期事件到期
- **THEN** MySQL 触发历史和未删除的任务会话仍可读，不以 TTL 删除事实

#### Scenario: Completion cannot be reliably confirmed
- **WHEN** 恢复时无法获得可靠终态证据
- **THEN** 记录结果未知/中断，不猜测成功或重新执行

#### Scenario: View historical configuration after editing the task
- **WHEN** 当前任务已被编辑，用户读取旧运行
- **THEN** 仍返回旧 snapshot 和实际模型；当前配置通过 Task GET 分开取得

### Requirement: Bounded configuration and graceful shutdown

独立 Scheduler 的执行开关 SHALL 默认 false，poll_interval 默认 5s、max_concurrent 默认每 Scheduler 实例 4、max_run_duration 默认 10m，非正值 SHALL 拒绝加载。恢复未证明停止的执行 SHALL 计入容量，包括 interrupted 历史关联的 uncertain/hold，不仅计本地 goroutine。Runtime SHALL 从 start grant 计时并强制 timeout，保留 cancelled/TASK_TIMEOUT 与部分输出；普通聊天行为不变。Scheduler 停机 SHALL 停物化/提交、释放明确未接收准备，不取消已接收 Runtime 工作；Agent 停机仍有界取消/保存自己的 Run。

#### Scenario: Scheduling is disabled by default
- **WHEN** Scheduler 执行开关省略或 false
- **THEN** 不进行物化/执行，任务管理仍可用，主服务不启动内嵌调度

#### Scenario: Background execution reaches capacity or timeout
- **WHEN** 容量已满，或 Run 达到 grant deadline
- **THEN** 不新增超容量执行；Runtime 超时取消并保存 TASK_TIMEOUT，不依赖 Scheduler 在线

#### Scenario: Scheduler shuts down during execution
- **WHEN** Scheduler 优雅停机
- **THEN** 停止新提交，Runtime 已接收的工作继续受有界生命周期控制，重启后对账

#### Scenario: Agent shuts down during execution
- **WHEN** Agent 进入停机 gate
- **THEN** 不新启 Run，已运行任务有界取消并持久化，未知状态不重放

## ADDED Requirements

### Requirement: Agent scheduled task management tools

配置了 Scheduler 时系统 SHALL 提供 scheduled_task_create/list/get/update/pause/resume/delete 及资源 get/publish 工具。actor SHALL 仅来自执行上下文；HTTP 与工具 SHALL 共用所有者检查、输入校验、revision 和资源发布规则。工具 SHALL NOT 暴露直接 SQL、系统 crontab、任意用户代理执行或自由重跑。未配置不注册；依赖不可用时明确失败，不影响普通工具。

#### Scenario: Manage through an Agent tool
- **WHEN** Agent 代表已授权用户更新模型/指令/时间或发布资源
- **THEN** 与 HTTP 相同的校验、revision 和未来触发生效规则适用，不绕过权限

#### Scenario: Attempt to supply another actor
- **WHEN** 工具参数试图指定他人 user_id/task/resource
- **THEN** 不采用伪造 actor，越权资源操作被拒绝且无副作用
