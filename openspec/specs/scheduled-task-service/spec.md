# scheduled-task-service Specification

## Purpose

Define standalone Scheduler deployment and data ownership, authenticated RPC, durable execution receipts and start grants, conservative runtime recovery and limits, execution holds, and terminal fact ordering.

## Requirements

### Requirement: Independent scheduler in a monorepo

系统 SHALL 在同一 Git 仓库的 `services/scheduler/` 提供独立 Scheduler 启动、配置、构建和部署入口，首期 SHALL 保留根 Go module 和现有主服务入口。Scheduler SHALL 只依赖任务/调度所需组件、纯函数和传输契约，SHALL NOT 构造或依赖主服务会话服务、全量业务 Store、Agent、OpenAI、MCP、executor 或用户工作空间挂载。

#### Scenario: Start management without an Agent process
- **WHEN** Scheduler 自身数据库、配置和服务身份有效，但 Agent 执行服务暂时不可用
- **THEN** Scheduler 的健康及已保存任务/触发历史读取可用，不启动本地 Agent
- **AND** 需要主服务校验或执行的操作返回明确的依赖不可用错误

#### Scenario: Verify package and deployment boundaries
- **WHEN** 构建 Scheduler 并检查其依赖与配置
- **THEN** 不需要 LLM key、用户 JWT 签名密钥、工作空间挂载或 Agent 包

### Requirement: Exclusive scheduling data ownership

Scheduler SHALL 独占任务定义、时间规则、触发物化、领取、启动授权和调度状态投影的写入；主服务 SHALL 独占任务会话、资源版本和 Runtime 执行凭证。两者可以共用 MySQL 实例，但 SHALL NOT 直接写入或 JOIN 对方业务表，也 SHALL NOT 用跨服务级联删除代替生命周期协议。

#### Scenario: Manage through the existing HTTP API
- **WHEN** 已鉴权用户通过现有任务 HTTP 接口更新任务
- **THEN** API 调用 Scheduler RPC 完成业务更新，不直接修改 scheduled_tasks

#### Scenario: Scheduler is unavailable
- **WHEN** Scheduler RPC 不可用
- **THEN** API 返回 503，不降级为本地任务存储/调度
- **AND** 普通聊天、会话和工作空间能力不因 Scheduler 的不可用而被停用

### Requirement: Versioned and authenticated internal RPC

系统 SHALL 提供版本化的任务管理、occurrence/启动授权、主服务 actor/模型/资源/历史及 Agent 执行 RPC。每个普通 RPC SHALL 有 deadline、经所选安全模式认证的调用者身份与每方法的服务身份授权。内部 RPC SHALL 默认使用 mTLS；只有显式配置 `plaintext_token` 时 SHALL 使用明文传输及服务 Token 认证，SHALL NOT 提供匿名模式或因 TLS 失败而降级。actor SHALL 来自可信 API/Agent 的鉴权上下文；客户端提供的内部身份头 SHALL NOT 被信任。执行端 SHALL 获取权威 occurrence 并重查用户、来源和模型权限，SHALL NOT 将提交体当作任意代理执行授权。

#### Scenario: Reject a forged internal request
- **WHEN** 外部客户端或未授权服务调用内部 RPC，或伪造 actor 元数据
- **THEN** 请求在业务操作前被拒绝，不读取用户文件或启动 Agent

#### Scenario: Verify execution ownership at the receiver
- **WHEN** 提交参数中的用户、配置 fingerprint 或资源引用与权威 occurrence 不符
- **THEN** Runtime 拒绝接收，不创建运行或修改他人的数据

#### Scenario: Preserve method authorization in plaintext mode
- **WHEN** 使用绑定到 `blowball-scheduler` 的有效 Token 调用只允许主服务身份使用的任务管理方法
- **THEN** 返回 `PERMISSION_DENIED`，不执行任务管理操作

### Requirement: Explicit authenticated RPC transport selection

系统 SHALL 在未配置安全模式时使用 `mtls`，保留 CA/主机名验证、客户端证书要求和 TLS 1.3 下限。显式 `plaintext_token` 模式 SHALL 不加载 TLS 文件，并在无证书文件但其他配置有效时支持正常 RPC。每个连接 SHALL 仅使用所配置的模式；缺证书、握手失败、Token 失败或协议不匹配 SHALL NOT 触发自动切换、证书生成、匿名调用或本地调度 fallback。

#### Scenario: Missing certificate in default mode
- **WHEN** 省略安全模式且所需证书、私钥或 CA 文件缺失或无效
- **THEN** 本地 RPC 初始化失败并说明字段或文件路径，不创建明文替代连接

#### Scenario: Plaintext mode without TLS files
- **WHEN** 双方显式选择 `plaintext_token`，配置有效 Token，且 TLS 路径指向不存在的文件
- **THEN** 服务不读取这些 TLS 文件，合法 RPC 可以正常完成

#### Scenario: No protocol downgrade between modes
- **WHEN** 客户端和目标服务端分别使用不同的安全模式
- **THEN** 连接或 RPC 明确失败，不尝试另一模式，不进入业务处理

### Requirement: Token-bound caller identity

明文模式的客户端 SHALL 通过单个 `authorization: Bearer <token>` metadata 值提供出站 Token。服务端 SHALL 使用自己的 `peer_tokens` 配置与常量时间 Token 值比较认证调用者，只将匹配项绑定的服务身份交给现有方法权限表。服务端 SHALL NOT 根据身份头、actor 或业务字段选择调用者身份，也 SHALL NOT 将有效 Token 解释为不受限的业务执行授权。缺失、重复 authorization 值、错误格式或不匹配的 Token SHALL 返回 `UNAUTHENTICATED`，不得进入业务操作；Bearer scheme SHALL 大小写不敏感。

#### Scenario: Valid token establishes the configured identity
- **WHEN** 主服务使用有效 Token 调用其身份允许的 Scheduler 方法
- **THEN** 接收方采用配置中绑定的主服务身份，继续执行原方法和业务权限检查

#### Scenario: Claimed identity cannot replace credentials
- **WHEN** 请求只携带自称 `blowball-main` 的身份头而没有有效 Token
- **THEN** 返回 `UNAUTHENTICATED`，身份头不产生授权

#### Scenario: Valid token cannot impersonate another role
- **WHEN** 请求提供 Scheduler 的有效 Token，但通过身份头或业务字段自称主服务
- **THEN** 仍按配置绑定的 Scheduler 身份授权，不提升到主服务权限

#### Scenario: Reject ambiguous or malformed authorization
- **WHEN** 请求提供多个 authorization 值、非 Bearer scheme、缺少 Token 或错误 Token
- **THEN** 返回 `UNAUTHENTICATED`，不调用业务 handler

### Requirement: Authenticated internal health and bounded calls

内部 gRPC 健康检查，包括 unary `Check` 与服务端流式 `Watch`，SHALL 通过所选模式的认证：mTLS SHALL 保留握手验证客户端证书的要求，明文模式 SHALL 先验证有效服务 Token。认证后的健康检查 SHALL 继续豁免 deadline 和业务方法权限。普通 RPC SHALL 继续要求 deadline，复用既有错误码和调用超时；本变更 SHALL NOT 改变公共 HTTP `/healthz` 的行为。

#### Scenario: Authenticated plaintext health probe
- **WHEN** 有效 Token 的明文健康检查不携带 deadline
- **THEN** 可以返回健康状态，不因 deadline 或业务方法白名单被拒绝

#### Scenario: Anonymous plaintext health probe
- **WHEN** 明文健康检查缺失或携带错误 Token
- **THEN** 返回 `UNAUTHENTICATED`

#### Scenario: Authenticate streaming plaintext health subscriptions
- **WHEN** 明文客户端调用 `Health.Watch`
- **THEN** 无 deadline 的有效服务 Token 可以订阅健康状态；缺失、错误或重复的认证值返回 `UNAUTHENTICATED`，不进入订阅 handler

#### Scenario: Keep mutual TLS health verification
- **WHEN** mTLS 健康检查没有有效客户端证书
- **THEN** 握手或调用失败，不因健康检查豁免而绕过证书验证

#### Scenario: Ordinary RPC still requires a deadline
- **WHEN** 已认证的普通 RPC 未携带 deadline
- **THEN** 按既有语义返回 `INVALID_ARGUMENT`，不执行业务方法

### Requirement: RPC credential isolation

Token SHALL 仅用于进程配置和认证 metadata，SHALL NOT 进入业务 proto、任务/执行快照、MySQL/Redis 业务记录、SSE、标题、模型或子 Agent 上下文、工具参数。新增代码 SHALL NOT 自动向工具环境注入 Token 或扩大环境变量白名单。配置、认证和传输错误 SHALL NOT 在日志、错误响应或调试配置输出中泄露 Token、完整凭据配置或原始认证 metadata；运行程序 SHALL NOT 自动签发 Token、证书或 CA。

#### Scenario: Failed authentication does not reveal a credential
- **WHEN** 明文请求因格式或 Token 不匹配被拒绝
- **THEN** 错误和日志不包含提供的 Token 或完整 authorization metadata

#### Scenario: Task data contains no RPC credentials
- **WHEN** 已认证调用创建任务、提交执行或返回任务历史
- **THEN** 业务消息、快照、持久记录、SSE 和模型上下文只包含原业务内容，不附带 RPC Token

#### Scenario: Tool defaults do not gain credential variables
- **WHEN** 主服务加载 RPC Token 并按默认 executor 环境策略执行工具
- **THEN** 本变更不将 Token 注入工具参数/环境，也不扩展默认环境白名单

### Requirement: Durable idempotent execution acceptance

Runtime SHALL 在权威存储中保存每个 occurrence 唯一的 execution receipt，绑定稳定 run/session ID、配置 fingerprint、attempt/worker generation 和执行状态。accepted 响应 SHALL 只在 receipt 及任务会话幂等创建成功后返回。同一身份/配置重复提交 SHALL 返回同一记录；同一身份不同配置 SHALL 拒绝，SHALL NOT 生成新的执行身份。

#### Scenario: Acceptance response is lost
- **WHEN** Runtime 已提交 accepted 记录，但 Scheduler 未收到响应并查询或重复提交
- **THEN** 返回既有 receipt/run/session，不产生第二次 Agent 执行

#### Scenario: Receipt survives event retention
- **WHEN** Redis Run 事件窗口已过期或相关缓存不可用
- **THEN** Scheduler 仍可从权威执行凭证查询接收/终态，不依赖 Redis TTL 判断是否重试

### Requirement: Cross-service startup authorization

Runtime SHALL 在首个执行副作用前持久化 start_intent，并经 Scheduler 获取唯一 start grant。授权 SHALL 原子检查任务启用/未删除及有效 claim/lease/attempt，绑定 receipt generation、逻辑 started_at 和固定 deadline。重复获取同一授权 SHALL 幂等；暂停/删除先于授权生效时 SHALL 拒绝启动。Runtime SHALL 在开始副作用前确认当前 generation 和授权状态。

#### Scenario: Pause wins the startup race
- **WHEN** 暂停或删除事务先于启动授权提交
- **THEN** Runtime 无法获得可执行授权，不调用模型或执行工具

#### Scenario: Startup grant response is lost
- **WHEN** Scheduler 已提交 grant 但响应丢失
- **THEN** Runtime 仅按同一身份获取既有 grant；无法确认时不开始执行，也不申请新 Run

#### Scenario: A stale worker returns
- **WHEN** claim、attempt 或 Runtime worker generation 已被围栏
- **THEN** 迟到工作者不能获得新授权、开始执行或覆盖当前终态

### Requirement: Conservative recovery and runtime-owned limits

accepted/preparing 的恢复或有界重试 SHALL 有明确未启动证明并围栏旧工作者。start_intent 及以后状态不明确时 SHALL NOT 自动重放 Agent；系统 SHALL 对账可靠 grant/owner/终态，否则记录 interrupted/RESULT_UNKNOWN 并保留执行可能性 uncertain。租约到期 SHALL NOT 独自作为未执行证明。Runtime SHALL 自己执行从 grant started_at 计时的 deadline，不依赖 Scheduler 连接或进程存活。

#### Scenario: Scheduler restarts during execution
- **WHEN** Runtime 已接收或运行任务而 Scheduler 重启
- **THEN** Runtime 继续受自己的执行时限约束，Scheduler 恢复后对账，不重新调用 Agent

#### Scenario: Runtime dies in an uncertain window
- **WHEN** Runtime 在 start_intent 后死亡且无法确认终态
- **THEN** 记录结果未知/中断，不在新节点重执行同一 occurrence

#### Scenario: Runtime enforces timeout without Scheduler
- **WHEN** Scheduler 断线期间任务到达固定 deadline
- **THEN** Runtime 取消 Run，持久化部分输出和 TASK_TIMEOUT

### Requirement: Unknown execution blocks subsequent task occurrences

无法证明原执行已经停止时，Scheduler SHALL 持久化 task-level execution hold，不因历史 interrupted 或 lease/deadline 到期释放任务不重叠限制；Runtime uncertain 与 hold SHALL 继续计入受管容量。后续槽位 SHALL 跳过并记录 TASK_EXECUTION_UNCERTAIN，不重跑/堆积。普通配置编辑、恢复或 enabled=true SHALL 不能清空阻塞。只有匹配 occurrence/run/grant/generation 的正面停止证明及旧 worker 围栏，才能由受控对账 CAS 清除 hold。

#### Scenario: An unreachable worker may still be running
- **WHEN** 原执行不可达且结果未知，下一 cron 槽位到期
- **THEN** 不启动后续 occurrence，保持 hold 和容量占用，不把历史 terminal 标签当作停止证明

#### Scenario: A user resumes a blocked task
- **WHEN** 用户在没有可靠停止证明时恢复/编辑该任务
- **THEN** 配置可以按授权更新，但执行阻塞保持，不能产生新的 start grant

#### Scenario: Hold persistence fails before interrupted projection
- **WHEN** 对账已经确认执行结果未知，但 task hold 写入失败
- **THEN** occurrence 保持非终态、活跃和可对账状态，不先展示会释放不重叠限制的 interrupted
- **AND** 后续对账可以重试；只有 hold 可靠保存后才能投影 interrupted

#### Scenario: Reconciliation positively proves termination
- **WHEN** Runtime 确认原 Agent/受管命令已经结束并围栏对应 generation
- **THEN** Scheduler 仅对匹配的 hold CAS 清除，后续槽位可正常调度，不重放阻塞期间跳过槽位

### Requirement: Terminal facts precede scheduling completion

Runtime SHALL 在任务最终消息/工具批次可靠持久化后确认 execution receipt 终态，并通过事务保证终态事实不先于对应内容。Scheduler SHALL 查询可信 receipt 投影 occurrence 终态，不依赖 SSE 客户端或远程回调送达。

#### Scenario: Scheduler misses terminal notification
- **WHEN** Runtime 已保存终局内容和 receipt，但 Scheduler 尚未回写触发终态
- **THEN** 后续对账补记同一终态，不重新执行，也不猜测不同结果

#### Scenario: Final content persistence fails
- **WHEN** 最终批次未可靠保存
- **THEN** receipt 不宣称对应的可靠完成，系统按既有有界收尾和保守恢复处理
