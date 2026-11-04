## ADDED Requirements

### Requirement: User-owned scheduled task management

系统 SHALL 提供已鉴权的用户级任务管理端点：`POST /api/v1/scheduled-tasks`、`GET /api/v1/scheduled-tasks`、`GET /api/v1/scheduled-tasks/:task_id`、`PATCH /api/v1/scheduled-tasks/:task_id`、`DELETE /api/v1/scheduled-tasks/:task_id`。任务所有者 SHALL 来自 JWT 鉴权上下文，不接受客户端指定他人身份。任务 SHALL 包含非空 name/prompt、时间规则、会话策略、可选 model/reasoning_effort 和默认 true 的 enabled；prompt SHALL 遵循交互文本输入的大小及 token 限制，model/effort SHALL 遵循现有模型目录校验。创建成功 SHALL 返回 201，修改成功 SHALL 返回 200，删除成功 SHALL 返回 204。

#### Scenario: Create a user-owned task
- **WHEN** 已鉴权用户提交合法任务配置
- **THEN** 系统保存归属于该用户的任务并返回 201、task_id 和 next_run_at
- **AND** 创建接口不直接执行 Agent

#### Scenario: Reject another user's resources
- **WHEN** 用户读取、修改、删除他人任务或指定他人目标会话
- **THEN** 系统返回 404，不泄露资源存在性，不修改数据

#### Scenario: Reject invalid input and missing authentication
- **WHEN** 未携带有效 JWT，或任务文本、模型、会话策略或时间规则非法
- **THEN** 未鉴权请求返回 401，已鉴权非法请求返回 400
- **AND** 不保存任务、不占用运行槽位、不调用模型

### Requirement: Structured at and cron schedules

系统 SHALL 支持单次 `at` 和标准五字段 `cron`。两种规则 SHALL 保存明确的 IANA timezone；at 的 run_at SHALL 为带偏移的 RFC3339 时间，创建或改变时间规则时 SHALL 在未来；cron SHALL 使用独立 expression 字段，SHALL NOT 接受秒字段、宏、`every` 或表达式内时区前缀。绝对计划时间 SHALL 按 UTC 保存和返回，不依赖进程本地时区。

#### Scenario: Calculate a cron slot in the task timezone
- **WHEN** 任务时区不同于服务器本地时区，任务使用合法五字段 cron
- **THEN** next_run_at 根据任务时区计算并以 UTC 返回

#### Scenario: Reject an invalid schedule
- **WHEN** 创建时 run_at 已过去，timezone 不存在，或 expression 不满足五字段契约
- **THEN** 系统返回 400，既有任务及其 next_run_at 不变

#### Scenario: Handle daylight-saving transitions
- **WHEN** cron 的本地计划时间遇到夏令时不存在或重复的时间
- **THEN** 不存在的本地时间跳过，重复时间的不同 UTC 槽位可分别触发
- **AND** 每个 UTC 槽位仍遵循唯一触发及不重叠规则

### Requirement: Durable and atomic slot materialization

任务定义与单次触发记录 SHALL 保存在 MySQL，SHALL NOT 依赖内存任务表或 Redis Run TTL。系统 SHALL 在同一事务中领取到期任务、保存配置快照及稳定执行身份、插入触发记录并推进 next_run_at。`(task_id, scheduled_for)` SHALL 唯一；单次规则物化后 SHALL 清空 next_run_at，cron SHALL 推进到严格晚于当前时间的下一槽位。

#### Scenario: Concurrent scheduler replicas scan the same slot
- **WHEN** 多个 Agent 副本同时扫描同一任务的同一 scheduled_for
- **THEN** 最多物化一条该槽位的触发记录，使用同一个稳定 Run 身份

#### Scenario: Crash around materialization
- **WHEN** 进程在物化事务提交前或提交后、执行提交前崩溃
- **THEN** 提交前整笔回滚，提交后的触发记录仍可恢复
- **AND** 不存在已推进 next_run_at 却未保存触发记录的情况

### Requirement: Coalesced recovery of missed schedules

系统 SHALL 在启动及正常扫描时处理未消费的到期任务。停机期间错过多个 cron 槽位时 SHALL 仅物化数据库保存的最早未处理槽位作为一次补跑，并从当前时间计算下一槽位，SHALL NOT 逐个重放历史槽位；未消费的 at SHALL 仅补跑一次。

#### Scenario: Recover several missed cron slots
- **WHEN** 服务恢复时某 cron 任务已错过多个计划时间，且该任务没有非终态触发
- **THEN** 系统只安排一次补跑，下一时间严格晚于当前时间

#### Scenario: Recover an overdue one-shot task
- **WHEN** 服务恢复时 at 已到期但尚未物化
- **THEN** 系统物化一次触发，并清空该规则的 next_run_at
- **AND** 后续重启不重新安排已消费槽位

### Requirement: Per-task non-overlap

同一任务 SHALL 至多拥有一个可执行的非终态触发。已有 pending、preparing、launching 或 running 触发时，新到期槽位 SHALL 记录 `skipped` 和 `TASK_OVERLAP`，并推进计划，SHALL NOT 累积重叠执行。

#### Scenario: Previous execution is still active
- **WHEN** 某任务再次到期且前次触发仍未终结
- **THEN** 新槽位记录为 skipped，前次触发不受影响，不启动第二个 Agent Run

### Requirement: Explicit session targeting and background admission

系统 SHALL 默认按每次触发创建属于任务所有者的独立会话，不附加前次完整对话。existing 模式 SHALL 显式指定所有者的既有会话，并在执行时复查。后台提交 SHALL 原子尊重 session 单活动 Run 及既有 FIFO：有运行或待处理消息时 SHALL 记录 `skipped: SESSION_BUSY`，SHALL NOT 入队、插队、抢占或取消聊天。Redis 无法确认 admission 时 SHALL 不启动模型，SHALL NOT 使用交互请求的无锁降级。

#### Scenario: Execute in independent sessions
- **WHEN** 使用默认会话策略的周期任务先后触发两次
- **THEN** 两次使用不同、属于同一用户的会话，重复准备同一次触发不创建多个会话
- **AND** 用户 workspace、skills 和跨会话记忆继续遵循既有规则

#### Scenario: Existing session is generating or queued
- **WHEN** 目标既有会话有活动 Run 或等待消息，即使认领瞬间空闲
- **THEN** 后台触发记录为 SESSION_BUSY，不越过既有 FIFO

#### Scenario: Admission storage is unavailable
- **WHEN** Redis admission 操作失败
- **THEN** 系统仅在确认尚未启动的前提下有界重试，未确认互斥前不调用模型
- **AND** 交互消息原有降级行为不受影响

### Requirement: Runtime owner authorization and credential secrecy

任务 SHALL 只授权在该用户现有权限范围内执行保存指令。系统 SHALL 在准备及启动 fence 前重新检查任务未暂停/删除、用户存在且 active、目标会话仍归属该用户。账号或目标会话不可用时 SHALL 记录明确跳过原因并暂停任务。LLM 凭证 SHALL 在执行时遵循现有 per-user 解析与全局 fallback 规则，SHALL NOT 持久化到任务或触发快照。

后台所需用户 JWT SHALL 仅由受信任授权服务在检查后短期签发，覆盖有界运行及收尾，SHALL NOT 持久化、公开返回、写入日志/raw capture，或注入工具参数及 bash 环境。系统 SHALL NOT 保存任务创建请求的登录 JWT，也 SHALL NOT 新增公开免密码签发端点或通用特权 fallback。

#### Scenario: User logs out or the original JWT expires
- **WHEN** 创建请求的登录 JWT 已失效，但任务仍被授权且用户 active
- **THEN** 任务可以使用运行时身份执行，不依赖保存旧登录 JWT

#### Scenario: User becomes unavailable before execution
- **WHEN** 用户在任务物化后、启动 fence 前被删除或禁用
- **THEN** 系统不启动模型，记录 USER_UNAVAILABLE 并暂停任务

#### Scenario: User changes an LLM credential
- **WHEN** 用户在创建任务后更换自己的 LLM token，再触发任务
- **THEN** 任务使用执行时的当前 token；该 token 认证失败时不回退全局凭证重试

#### Scenario: Credentials remain secret
- **WHEN** 后台执行调用需要 JWT 的市场客户端并产生日志和运行历史
- **THEN** token 仅在内存认证链路中使用，任务表、触发表、响应、工具输入和日志中没有 token 本体

### Requirement: Pause resume update and delete semantics

系统 SHALL 通过 PATCH enabled 暂停/恢复任务，并通过 DELETE 软删除任务。暂停/删除 SHALL 与启动 fence 协调，阻止尚未启动的触发；已跨过 fence 的 Run SHALL 不被隐式重跑或取消，显式取消仍使用既有 Run 端点。普通配置修改 SHALL 仅影响以后物化的快照。恢复 cron SHALL 从当前时间计算下一槽位，不补主动暂停期间的槽位；恢复已消费 at SHALL NOT 重放旧槽位。

#### Scenario: Pause races with startup
- **WHEN** 暂停在启动 fence 前生效，包括过期领取者延迟返回的情况
- **THEN** 触发被取消且不能启动 Agent
- **AND** 已跨过 fence 的执行继续走既有生命周期

#### Scenario: Resume a paused cron
- **WHEN** 用户恢复已暂停一段时间的 cron 任务
- **THEN** next_run_at 为当前时间后的下一槽位，不补暂停期间的历史

#### Scenario: Update an already materialized task
- **WHEN** 用户修改指令、模型或时间规则，而已有触发快照
- **THEN** 已物化快照不变，新配置只作用于未来触发
- **AND** 仅重新启用已消费单次规则不会再次执行，显式修改为新的未来 run_at 才能重新安排

#### Scenario: Delete a task with an active Run
- **WHEN** 用户删除已有运行中 Turn 的任务
- **THEN** 未来及未启动触发被阻止，已启动 Run 的会话内容仍按既有规则持久化
- **AND** 普通任务读取返回 404，内部历史事实不因软删除而丢失

### Requirement: Fenced preparation and conservative execution recovery

系统 SHALL 为可安全恢复的准备步骤使用租约与领取 token，过期领取者 SHALL 不能越过启动 fence。进入可能执行窗口前 SHALL 持久化 launching 状态；明确未启动的提交失败 SHALL 仅有界重试，已开始或启动结果不明确的触发 SHALL NOT 自动重新提交。重启恢复 SHALL 检查 Run 终态和存活，无法可靠确认成功或运行进程已死时 SHALL 记录 interrupted，不承诺外部副作用 exactly-once。

#### Scenario: Recover an expired preparation lease
- **WHEN** 准备租约过期且该触发尚未进入可能执行的窗口
- **THEN** 另一副本可以重新准备，旧领取 token 不能启动

#### Scenario: Crash after submission may have started a Run
- **WHEN** launching 触发的进程死亡，提交结果无法可靠确认
- **THEN** 系统先恢复可确认的 Run 状态，否则记录 interrupted
- **AND** 不为同一槽位重新调用 Agent

#### Scenario: Reconcile a running or completed Run
- **WHEN** 恢复方发现 Run 仍存活或有可靠终态记录
- **THEN** 存活 Run 被继续跟踪，终态被补记到 MySQL，不产生第二次执行

### Requirement: Durable execution history and in-app results

系统 SHALL 提供 `GET /api/v1/scheduled-tasks/:task_id/runs`，仅所有者可读，返回触发状态、原因、计划/实际时间、session_id 和 Run ID。列表 SHALL 支持 limit/offset，默认 limit 为 20、最大 100；任务按创建时间倒序，运行按 scheduled_for 和 ID 倒序。消息、工具活动、产物和用量 SHALL 使用既有持久化及查询能力；终局触发状态 SHALL 在 Turn 终局消息持久化后有界回写 MySQL。

#### Scenario: Complete without an SSE client
- **WHEN** 定时 Run 从未有 SSE 订阅者并正常完成
- **THEN** 消息、工具活动、产物声明和用量仍按既有规则持久化，运行历史保存终态和关联身份

#### Scenario: Redis replay window expires
- **WHEN** Run 的短期事件保留窗口已到期
- **THEN** MySQL 运行历史仍可读取，内容在会话未删除时可通过会话历史读取
- **AND** 不以 Redis TTL 删除任务运行事实

#### Scenario: Completion cannot be reliably confirmed
- **WHEN** 终态回写失败且恢复时已没有可靠 Run 终态证据
- **THEN** 系统记录中断/结果未知的原因，不猜测成功，不重新执行业务

### Requirement: Bounded configuration and graceful shutdown

`scheduled_tasks.enabled` SHALL 默认 false，仅控制后台运行；启用后 poll_interval SHALL 默认 5s，max_concurrent SHALL 默认每 Agent 进程 4，max_run_duration SHALL 默认 10m，非正值 SHALL 拒绝加载。运行时限 SHALL 从启动 fence 开始计时，超时通过既有取消机制中止并记录 `cancelled: TASK_TIMEOUT`。关闭开关 SHALL 不改变交互消息行为。优雅停机 SHALL 先停止新物化及投递，再配合既有 Run 停机流程取消并有界等待运行中的 Turn。

#### Scenario: Scheduling is disabled by default
- **WHEN** 配置省略 scheduled_tasks 或 enabled=false
- **THEN** 不启动后台调度/恢复循环，交互消息、队列、SSE 和凭证行为保持原样

#### Scenario: Background execution reaches capacity or timeout
- **WHEN** 本进程已有 max_concurrent 个后台运行，或某 Run 超过 max_run_duration
- **THEN** 达到容量时不新增后台运行；超时时取消该 Run 并保存部分输出和 TASK_TIMEOUT 原因

#### Scenario: Shutdown begins during preparation or execution
- **WHEN** Agent 进程进入优雅停机
- **THEN** 新触发及启动被 gate 阻止，可安全恢复的准备被释放，已启动 Run 走有界取消和终局持久化
- **AND** 停机 gate 后不出现新启动的 Turn
