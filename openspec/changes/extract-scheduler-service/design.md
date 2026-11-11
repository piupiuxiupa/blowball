## Context

本变更的用户确认范围是：一个 Git 仓库内按目录组织独立服务，首期拆出 Scheduler；同时提供数据库物理分表的任务会话、用户工作空间 `.cron` 资源管理，以及类似现有功能的任务配置编辑。用户要编辑的是所属任务的计划指令、模型和时间规则，不是修改历史执行或在任务会话中自由聊天。

当前仓库已有 `all/api/agent` 角色、`internal/scheduler`、纯时间规则包、任务 CRUD 和 transport-independent turn 执行器。Scheduler 仍直接依赖 SessionService、执行 JWT 签发器、Run manager，以及带 `BeforeStart` / `OnTerminal` 回调的进程内 `turn.Submit`。`messages`、`tool_calls`、标题、用量、压缩、子 Agent、附件等表的外键均指向普通 `sessions`。

当前有效持久化路径是 send-time 用户消息及 round-boundary 终稿的 MySQL 事务写入，再更新读缓存；全局 chunk ingest queue/flusher 已退役。任务会话应复用这一现行路径，不复活旧 Redis 入库队列。`current_status` / `tool_budget` 的 cache-only 约束、压缩锚点和工具活动统计必须保持。

本设计中的单 Go module、表组名称、不可变资源版本、内部协议和工具名称是供审阅的工程选择，不宣称是用户逐项指定的实现。此次仅生成 OpenSpec 文档，不创建服务代码、protobuf、迁移或前端。

## Goals / Non-Goals

**Goals:**

- Scheduler 可独立构建、部署、启停和扩容，API/Agent 不再内嵌调度。
- 新任务执行使用物理独立的任务会话表组，普通会话接口不混入任务数据。
- 固定每次执行的配置和资源版本，支持编辑当前任务、查看历史真实快照。
- HTTP 与 Agent 工具使用同一任务管理能力，保留用户身份、凭证及文件隔离。
- 明确跨服务接收、启动、终态和失败证明，避免网络重试导致重复 Agent 执行。
- 保留既有 at/cron、漏触发合并、任务不重叠、运行时限及普通聊天行为。

**Non-Goals:**

- 多 Git 仓库、一次性搬迁主服务、全面多 Go module 化或拆出所有业务层。
- Gateway、WebSocket、新消息渠道、Firecracker、风险审查 Agent或新的 bash 网络策略。
- 任务会话自由对话、手动重跑、外部消息通知和前端代码实现。
- 通用工作流引擎、任意系统 crontab、外部副作用 exactly-once、自动重跑不确定执行。
- 自动把遗留普通会话搬表，或重建用户跨会话记忆系统。

## Decisions

### D1. 渐进式 monorepo，首期保留单 Go module

```text
blowball/
├── go.mod
├── cmd/blowball/                     现有主服务入口
├── internal/                         主服务业务及少量既有纯函数包
├── services/scheduler/
│   ├── cmd/scheduler/                独立二进制入口
│   ├── internal/                     任务、调度、存储、RPC、配置
│   └── config.example.yaml
└── api/
    ├── proto/scheduler/v1/
    ├── proto/scheduledruntime/v1/
    └── gen/go/                       生成的传输类型/客户端
```

调度业务和专属 MySQL store 移入 Scheduler 私有目录。可以复用现有纯 `schedule`、日志等小包；不能为了少改文件继续依赖全量主服务 Store、SessionService、Agent、OpenAI、MCP 或 executor。公开共享内容限于传输 DTO/客户端及无业务所有权的纯函数，增加包依赖边界测试。

备选：每个服务独立 `go.mod` 加 `go.work`，依赖发布边界更强，但首期需同时搬迁/重命名主服务包；暂不采用。只新增 `serve --role scheduler` 虽最少改动，却没有形成用户要求的独立服务目录和依赖边界，也不采用。

### D2. 服务职责与表所有权

```text
现有 API HTTP ──gRPC──> Scheduler <──gRPC── Agent 定时管理工具
    │                     │                         │
    │ 资源/历史            ├──gRPC──> API 内部能力    │
    │                     └──gRPC──> Agent 执行入口 ─┘
    │                                  │
    └── 主服务存储              共用 turn / Agent / bwrap
```

- **Scheduler** 独占 `scheduled_tasks`、`scheduled_task_runs` 的业务写入、时间物化、领取、启动授权和调度状态投影；不构造 Agent，也不挂载用户工作空间。
- **API/all** 保留既有任务 HTTP 路径，使用 gRPC 适配；提供用户/模型校验、资源管理、任务会话历史/删除等不调用 LLM 的主服务能力。
- **Agent/all** 提供内部 ScheduledExecution RPC、持久化执行凭证、Run registry/取消、执行 watchdog、现行 turn/工具及定时管理工具。
- 主服务独占任务会话表组、资源版本元数据和执行凭证。首期共用 MySQL 实例，但不跨服务直接 SQL 修改或 JOIN 对方业务表；跨边界关联使用逻辑 ID 和 RPC，不增加跨服务级联删除。
- `all` 仍合并 API/Agent 的能力，但绝不包含 Scheduler。Scheduler 停机不阻止普通聊天启动，也不触发本地 CRUD/执行 fallback。

备选：让 Scheduler 同时管理会话和运行 Agent，将复制大量编排与凭证逻辑；只拆定时循环但保留 CRUD/存储在 API，则调度业务所有权仍分散，均不采用。

### D3. 版本化 gRPC 与调用身份

内部能力分为三组，初期使用有界 unary RPC；Scheduler 不依赖 SSE 或长连接等待模型执行完成。

| 端点所有者 | 能力 |
| --- | --- |
| Scheduler | Create/Get/List/Update/DeleteTask、ListOccurrences、GetOccurrence、AuthorizeStart/GetStartGrant |
| API/all | ValidateTaskActorAndConfig、发布/校验/读取资源版本、读取/删除任务会话 |
| Agent/all | SubmitScheduledRun、GetScheduledExecution、CancelScheduledRun |

API/all 和 Agent/all 使用各自的内部监听配置；`all` 可以在同一内部监听器注册两组主服务能力。Scheduler 具有独立监听器及主服务 API/Agent endpoint 配置。具体端口由部署配置决定，不改变现有 HTTP 端口或角色归属。

内部调用要求 mTLS 与每方法的受限服务身份：外部请求不能注入内部 actor 元数据；API/Agent 只从已鉴权上下文构造 actor。Scheduler 对管理请求校验 task owner；Runtime 对执行请求从 Scheduler 读取权威 occurrence，重新校验用户 active、目标来源和允许的模型，不能仅信任提交体中的 user_id/prompt。

Scheduler 不持有用户登录 JWT、用户 LLM token、全局 LLM key 或 JWT 签名密钥。需要市场身份时由主服务在检查后短期签发并仅在内存传递，不放入 proto 业务消息、任务快照、资源、日志或工具参数。复用连接，所有 RPC 有 deadline；RPC 断线不等价于取消已接收的执行。

### D4. Runtime 持久化执行凭证与启动边界

仅把 `Submit` 替换成 RPC 不足以证明“没有启动”。新增主服务所有的 `scheduled_execution_receipts`，每个 occurrence 唯一，绑定稳定 run/session ID、非秘密配置 fingerprint、attempt generation、执行 worker generation、owner lease、start grant 和终态事实。Scheduler 的 occurrence 状态是调度投影；执行事实以 Runtime 凭证为准。凭证不依赖 Redis TTL，查询使用权威存储而非陈旧副本缓存。

```text
Scheduler: pending → preparing → launching → running → terminal
Runtime:   accepted → preparing → start_intent → running → terminal
                                      └──────→ uncertain（可能仍在执行）
                                  ↑
                       此后不确定状态禁止自动重执行
```

1. Scheduler 在 MySQL 事务物化 immutable occurrence，预分配稳定 run/session ID，领取有时限 claim token，然后持久化 launching。
2. `SubmitScheduledRun` 仅传稳定身份、attempt 和期望 fingerprint。Runtime 拉取权威快照、校验并在事务中幂等创建任务会话和 receipt，再返回 accepted。同一身份同一 fingerprint 返回既有记录；不同内容冲突拒绝。claim token 等重试控制信息不改变配置 fingerprint。
3. Runtime 的 worker 以 CAS/lease 领取 accepted receipt，完成只读准备和资源校验，申请既有 Run admission。后台 Redis admission 不明确时 fail-closed，不执行模型。
4. 在 `BeforeStart` 边界，Runtime 先持久化 `start_intent`，再调用 Scheduler `AuthorizeStart`。该事务同时检查任务启用/未删除、claim/lease/attempt 有效，幂等保存绑定 receipt generation 的唯一 start grant、started_at 和固定 deadline。暂停/删除与此事务形成逻辑启动顺序。
5. grant 响应丢失时按同一身份查询/重试获取同一个 grant，不申请新执行。不能确认授权则不开始模型或工具副作用。Runtime 在第一项执行副作用前 CAS 保存 running/grant，并确认当前 generation；被围栏或失去 owner 的工作者不得继续启动。
6. Runtime 以 grant deadline 从启动 fence 计时；Scheduler 重启或断线不移除运行时限。超时走既有 Run 取消、部分终稿持久化并记录 TASK_TIMEOUT。
7. 主服务 finalization 在事务中保存任务会话的最终消息/工具批次与 receipt 终态，确保不能先宣布完成再落内容；用量及子 Agent 完结仍遵守现行写入顺序。Scheduler 查询可靠 receipt 后投影终态，不依赖远程回调送达或订阅者。

**安全恢复范围：**

- 没有 receipt：按原身份幂等重提交，不能重新生成 run/session ID。
- receipt 在 accepted/preparing 且未进入 start_intent：只有在旧 worker 已围栏、Runtime 给出明确未启动状态之后才能恢复或有界重试；保存 attempt generation，保留既有三次持久化重试上限。
- receipt 处于 start_intent 或以后且缺少可靠终态：查询 grant、owner/liveness 和终态证据；不能证明安全时 Scheduler 可以展示 interrupted/RESULT_UNKNOWN，但 Runtime 的执行可能性仍是 uncertain，不能把结果未知当作执行已停止。租约到期本身不证明没有副作用。
- 已启动的 worker 崩溃后不在另一节点重放 Agent；活跃的 owner 持续受监控，可信终态可以补记。失去 owner 的 Runtime 需要停止本地工作并以 generation 阻止迟到写入。

**跨 occurrence 的任务阻塞：** Task 持久化 `execution_hold_occurrence_id` 与只读 blocked_reason，在未知启动/停止状态下阻止整个任务的后续执行，不仅禁止重跑原 occurrence。历史 interrupted 不是释放任务不重叠槽位或 Runtime 容量的证明；uncertain receipt/hold 继续计入受管容量。后续到期槽位记录 skipped/TASK_EXECUTION_UNCERTAIN 并推进计划，不堆积补跑；用户可以编辑配置，但暂停/恢复、enabled=true 或资源更新不能绕过 hold。

hold 仅在 Runtime 提供绑定 occurrence/run/grant/generation 的正面停止证明时由 Scheduler CAS 清除：正常终态 finalization 已等待本地 Agent 和受管命令结束，或受控恢复确认原执行进程/命令已经终止并围栏旧 generation。心跳丢失、RPC 不可达、TTL/租约/deadline 到期以及“已发送取消”均不是该证明。仅有结果未知时保持阻塞；运维必须实际停掉可能执行的 owner 并完成受控对账，不允许通过普通 Task PATCH 清空 hold。该保护针对本系统 Agent/受管命令，不承诺停止外部服务已经接收的异步业务。

这比“依赖 Redis Run 状态 + RPC 自动重试”多一份受控执行记录，但给出了接收响应丢失、启动授权丢失和终态投影失败的独立证明。start_intent 后即使实际尚未调用模型，无法确认时也宁可中断，不冒重复副作用风险。

### D5. 独立任务会话表组，复用一套执行器

主服务新增以下表组，具体字段沿用现行模型/索引并将外键指向 `scheduled_sessions`：

- `scheduled_sessions`：含 owner、task_id、occurrence_id、trace/run 关联、压缩标志；occurrence_id 唯一。
- `scheduled_messages`、`scheduled_tool_calls`、`scheduled_titles`、`scheduled_turn_usage`、`scheduled_context_compactions`。
- `scheduled_subagent_runs`、`scheduled_subagent_instances`；所需 `scheduled_attachments` 及对应删除归档表。
- `scheduled_execution_receipts`；资源版本元数据另由主服务管理，不放进聊天消息表。

不创建空壳普通 `sessions` 来满足原外键，不将任务消息继续写入普通 `messages`。普通会话和任务会话共享 UUID 生成规范但不复用遗留身份；执行入口绑定经过校验的会话来源与类型化存储适配。表名只能来自服务端固定映射，不能由客户端输入。

适配覆盖 send-time、round 事务、终局写入、标题、压缩、上下文计量、子 Agent 快照、附件、缓存重建和删除归档。子 Agent 内容仍只进 transcript 表，不回流主消息表。保持现行 canonical/active transcript 分离、逐 byte 重建、完整工具结果组、工具预算和运行状态规则；cache-only 状态不进任何 MySQL/消息缓存/SSE/标题。

保留现有以全局唯一 session ID 定位的 Run/Redis key 约定；任何缓存读取前先在正确的会话表验证 owner/source，不用“先查普通表，查不到再查任务表”的兜底来判断来源。任务删除清理其缓存，不触碰普通会话。普通会话接口收到任务 session ID 返回 404；任务会话不支持 POST message。

任务沿用用户授权范围内的 workspace、skills，以及现有开关和策略约束的跨会话记忆能力。分表需要适配记忆所需的任务身份/历史读取，但本变更不另行改变记忆读写策略；cache-only 状态及资源元数据不能被当作真实用户原文。

### D6. 配置编辑与历史快照分离

保留已有任务字段：name、prompt、model、reasoning_effort、schedule、enabled。`session.mode=new` 继续接受，但其含义明确为新任务专属会话，不能指定实时 session ID；新增 resource_revision/manifest_hash 和可选 expected_revision。

Task 的 revision 用于当前配置 CAS 更新。新客户端和工具带 expected_revision，过期编辑返回 409；兼容旧 PATCH 可省略该字段，服务端仍使用现有事务更新保护。资源发布/绑定必须显式检查当前 draft/task revision，不能静默覆盖并发编辑。

Occurrence 的 snapshot_json 固定指令、所选模型/推理强度、时间规则、任务 revision 和资源引用；主服务记录执行时实际解析出的模型，空 model 使用部署默认时也能展示真实值。更新当前配置只影响以后新物化的触发，既有 pending/running snapshot 不重写。execution hold/blocked_reason 是服务端只读状态，不接受客户端配置覆盖；阻塞期间的编辑不会产生新执行。

主指令的事实来源仍是 Task.prompt。`.cron` 文档可以是引用的长方案/附件，但不再维护第二份可独立变更的 cron 或主指令配置。修改资源需要发布版本并通过任务更新绑定；仅修改模型/时间不复制资源包。

### D7. `.cron` 资源布局、发布与执行

```text
<user-workspace>/.cron/
  <task_id>/
    draft/
      instructions.md
      scripts/
      attachments/
    revisions/<resource_revision>/
      manifest.json
      instructions.md
      scripts/
      attachments/
```

Runtime 从用户和任务身份解析根路径，客户端不得指定宿主绝对路径。task_id 必须来自当前用户所有的任务；已有非受管理 `.cron` 内容或任务路径冲突返回 RESOURCE_NAMESPACE_CONFLICT，不自动覆盖/清空。

资源可选。没有文件的任务保存空资源引用即可启用；需要附件时先创建 disabled 任务取得 task_id，编辑 draft，再发布并以 expected_revision 更新/启用任务。资源发布成功但任务更新失败只产生未引用版本，不产生可执行的半成品任务。

发布由主服务完成，使用跨 API 节点的任务级写入围栏及 draft revision，串行化草稿写入和发布。检查只包含普通文件、相对路径无逃逸；不追踪符号链接、设备或 FIFO。复制到受保护 staging，对实际复制字节计算摘要，生成按路径排序的文件路径/大小/SHA-256 manifest；确认 draft generation 未变后原子发布，保存主服务资源版本元数据，再返回 resource_revision 和 manifest_hash。RPC/进程中断后相同 publish request ID 可查询既有结果，不产生两个不同的“同一版本”。

发布有正值文件数/总字节限制，单文件复用 workspace 上传限制；建议首期默认最多 1000 个文件、总计 100 MiB，可由部署配置调节。超限/失败不替换当前已绑定版本。失败 staging 和未引用版本的清理由受控维护路径处理，不允许普通工具自行清理发布区。

启动前 Runtime 校验 manifest、文件摘要、资源 owner 和 pinned revision，缺失/不一致不降级读取 draft/latest。向 Agent 提供最小的任务资源上下文（版本目录、manifest 和文档引用），内容按需经工具读取并计入既有预算，不把所有附件塞进 prompt，也不把文档当作高于系统策略的指令。数据库 user 消息仍保存原 Task.prompt。

发布区不可原地修改。现有 REST/文件工具新增统一保护：允许编辑授权 draft；拒绝改写 revisions、staging、管理元数据，以及能够覆盖/删除它们的祖先/移动目标。保护覆盖上传、文本写、创建、删除、rename、Office 回调和文件工具，读写授权不得仅凭字符串前缀。

所有 bash 的 `.cron` 绑定只读，包括普通聊天产生的 bash，以免绕过文件工具改写资源；资源生成通过受控文件工具/API 写 draft。其余用户工作空间可写，现行 bwrap 环境、技能目录和网络默认不变。Linux 以真实沙箱验证保护，非 Linux 开发环境不承诺执行隔离。

### D8. 生命周期、历史来源与 HTTP/工具合同

首期公共接口规划：

| 角色 | 路径 | 能力 |
| --- | --- | --- |
| API/all | 既有 `/api/v1/scheduled-tasks` 及 `/{task_id}` | CRUD/配置编辑，内部转 Scheduler |
| API/all | 既有 `/{task_id}/runs` | 触发历史、配置/资源快照、明确 session_source |
| API/all | `/api/v1/scheduled-sessions`、`/{session_id}`、`/{session_id}/messages` | 独立列表、详情、游标消息及删除 |
| API/all | `/api/v1/scheduled-tasks/{task_id}/resources` | 读取草稿/已发布版本元数据 |
| API/all | `/api/v1/scheduled-tasks/{task_id}/resources/publish` | 带 draft revision 的幂等发布 |
| Agent/all | `/api/v1/scheduled-sessions/{session_id}/runs/{run_id}/cancel` | 保留显式取消能力，不新增发送消息 |

列表默认 limit 20、最大 100；消息分页沿用现行 cursor/order/subagent_content 合同。Task session detail 返回 occurrence/run、历史 snapshot、实际模型和 task_id；当前任务配置通过既有 Task GET 获取，不覆盖历史 snapshot。任务已删除时历史仍可读，但配置不可编辑。

删除任务是软删除，阻止未来/未启动执行，不删除已运行会话或其资源。删除单个任务会话不删除 Task/Occurrence，不删除 `.cron`；尚未证明执行停止（包括 uncertain/hold）的会话删除返回 409，先显式取消并取得停止证明。历史保留 owner/source 和“内容已删除”信息。用户删除流程先按现有归档规则保存任务会话内容，再清理用户数据；调度事实保留且因 owner 不可用停止执行。

被 Task 或保留的 Occurrence/执行引用的发布资源不得删除，尤其不能在 active run 使用时删除。首期不提供用户强制清理发布版本接口；维护只能清理已证明未引用、未执行使用的 staging/版本。仅删会话不解除历史资源引用。

工具名建议为 `scheduled_task_create/list/get/update/pause/resume/delete`，以及 `scheduled_task_resources_get/publish`。工具 actor 仅来自当前执行上下文，使用 Scheduler/主服务 RPC，与 HTTP 共用权限及错误规则；不提供直接 SQL、crontab 文件或越权代理工具。未配置 Scheduler 不注册管理工具；配置后不可用返回清晰错误，不妨碍其他工具/聊天。

### D9. 迁移、停机与运行配置

- 独立 Scheduler 仍默认执行关闭；poll_interval 默认 5s、max_concurrent 默认每 Scheduler 实例 4、max_run_duration 默认 10m，非正值拒绝加载。
- Runtime 对 scheduled accepted/preparing/running 维持独立 admission 上限，建议每 Agent 实例默认 4；普通聊天的 admission/队列策略不改变。这是并发保护，不承诺 CPU/内存/LLM 配额物理分区。
- Scheduler 重启先恢复既有受管/待对账工作再接新工作；max_concurrent 不能因本地 goroutine 计数归零而忽略尚未证明停止的 receipt，包括历史状态为 interrupted 的 uncertain/hold 项。
- Scheduler 停机停止物化和新提交、释放明确未接收的准备，但不把已 accepted 的 Runtime 工作当作普通 RPC 取消。Agent 停机仍取消/有界持久化本地 Run，无法确认的执行终态保守处理。
- 主服务旧 `scheduled_tasks.enabled=true` 不再触发内嵌实例；迁移配置残留需明确报错或给出禁用迁移指引，不能静默双调度。新主服务连接配置使用 Scheduler endpoint，执行开关位于 Scheduler。
- 同机 API/Agent 继续共享数据根；跨机继续用已有 operator 挂载的 JuiceFS/POSIX 后端与 bwrap 健康检查。Scheduler 只需要自身配置、MySQL、RPC/健康检查，不加载 workspace/Landlock/LLM 配置。

## Risks / Trade-offs

- [物理表组增加迁移与维护成本] → 复用模型/执行算法和类型化 repository；用覆盖完整 round、子 Agent、压缩、取消、删除的隔离测试防止只分主表。
- [分布式启动无法用一个本地事务包办] → 明确 start_intent、唯一 grant、generation 和持久化 receipt；不确定时不重新执行。
- [中断历史被错误当作执行已停止，后续槽位与旧 worker 重叠] → 持久化跨 occurrence execution hold，正面停止证明前不释放任务/容量；接受暂时阻塞的可用性代价。
- [普通状态缓存被误用作执行事实] → receipts/occurrence 使用权威 MySQL；运行时缓存仅用于事件/性能，不作为安全重试证明。
- [工作空间被旁路修改] → 所有文件入口保护管理路径，bash `.cron` 只读，启动前校验固定发布版本；不把文件权限说明当作已经验证的隔离。
- [遗留 existing 任务依赖普通聊天上下文] → 暂停并要求用户显式改为专属会话，不静默切换、拷贝或隐藏真实聊天。
- [历史快照长期引用资源导致空间增长] → 发布容量限制、可观测性和仅清理已证明未引用资源；不以删除一次会话偷偷删除复现材料。
- [服务化增加 RPC 故障面] → CRUD 明确 503、不本地 fallback；执行接收与启动相分离，普通聊天无 Scheduler 启动依赖。
- [分表后记忆读取误走普通会话 repository] → 传递明确任务身份/来源并保持既有记忆开关、授权和策略；记忆策略调整留给独立需求。

## Migration Plan

1. 发布新增表、字段和索引的顺序迁移；不重写普通表数据。为既有 occurrence 标记 `session_source=legacy_interactive`，新触发为 scheduled；历史读取按明确来源路由，禁止查不到时跨表猜测。
2. 停止旧 `agent/all` 调度并等待旧 non-terminal 触发收尾；无法确认结果的项按既有 interrupted 规则展示，但缺少执行停止证明时同时建立 Task hold，不能仅改历史终态就启用后续执行，不重新物化槽位。
3. 既有 `new` 任务迁移为未来触发使用新任务表组，资源引用为空；既有 `existing` 任务暂停并设置 SESSION_POLICY_MIGRATION_REQUIRED，用户显式确认新策略后恢复。
4. 部署含 RPC/存储适配的主服务，再部署执行关闭的 Scheduler；迁移任务 HTTP 管理到 RPC，核验证书、数据库表所有权、共享 workspace 和依赖隔离。
5. 验证受控任务的配置编辑、资源发布、运行、重启、超时、历史、删除及普通聊天回归，再启用 Scheduler。
6. **回滚默认只回到关闭调度**：停独立 Scheduler 的新提交，主服务保留新表组读写和已 accepted 的收尾；不删除数据、复制到普通表或启动旧内嵌 scheduler。
7. 必须回滚主服务二进制时，先关闭调度并排空/有界取消 Runtime 的 accepted/running 工作，保留 SQL/文件备份；新任务历史只能由仍支持新表组的读服务提供。不得把不确定执行或已消费 at 当作回滚后的待重试任务。

## Open Questions

以下是实现前的部署核对项，不阻塞提案审阅；首期默认已在上文明确：

- 生产 RPC 证书/服务身份的签发体系与具体监听端口，由部署方落实；不以此引入新身份服务。
- 资源数量/容量默认值需要按实际任务材料校准；超限仍可配置放宽，不能移除有界校验。
- 前端位于本仓库之外，需要与消费方核对新增 DTO、独立入口及历史快照展示；本变更交付后端/OpenAPI，不修改未知前端。
- 数据库采用增量 migration、保留旧历史；遗留会话批量搬表与资源主动保留期策略是后续独立变更，不在本次隐含实现。
