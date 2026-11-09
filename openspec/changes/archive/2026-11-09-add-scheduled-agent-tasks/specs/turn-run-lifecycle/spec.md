## ADDED Requirements

### Requirement: Transport-independent shared Turn submission

系统 SHALL 提供不依赖 Gin、HTTP response writer 或 SSE 订阅者的内部 Turn 提交入口，使 HTTP 直接发起、会话队列接续和定时任务共用完整执行链路。共享链路 SHALL 包括会话及模型校验、历史恢复、记忆召回、上下文压缩、Run 生命周期、Agent/工具执行、事件消费、增量和终局持久化、用量、产物与取消。HTTP 解析、响应头和事件订阅 SHALL 留在传输适配层，不复制第二套 Agent 执行实现。

#### Scenario: Submit without an HTTP connection
- **WHEN** 受信任后台入口提交合法的用户、会话、文本和模型选择
- **THEN** Turn 可以完成整个生命周期，不需要构造 HTTP 请求或 SSE 客户端
- **AND** Hub 有独立 drainer，事件和终局持久化不会因无人订阅而阻塞

#### Scenario: Preserve interactive and queued behavior
- **WHEN** 重构后由 HTTP 直接发起或会话队列接续同一输入
- **THEN** 既有输入/附件校验、模型选择、FIFO、历史、标题、记忆、压缩、用量及持久化时序保持原有契约
- **AND** HTTP 的 trace_id、X-Run-Id 及事件重放语义保持不变

### Requirement: Stable headless Run identity and admission

后台提交 SHALL 接受持久化预留的稳定 Run 身份，并以相同身份写入 Run 状态、事件、消息关联及用量，不重新签发第二个 Run ID。后台 admission SHALL 原子尊重 session 活动认领和等待队列，忙时返回明确的未启动结果，SHALL NOT 套用交互请求在 Redis 故障时的无锁降级。后台执行 SHALL 使用任务启动 fence 阻止失效或已撤权的领取者启动。

#### Scenario: Submit a preallocated Run identity
- **WHEN** 定时触发携带已保存的 Run ID 提交
- **THEN** 执行、事件及用量使用该稳定身份，用户可经既有 Run 取消/事件端点访问已发布的 Run

#### Scenario: Reject busy or unconfirmed admission
- **WHEN** 后台目标会话有运行/等待消息，或 Redis 互斥无法确认
- **THEN** 不启动 Agent，不抢占队列；忙时返回未启动结果，存储故障仅允许安全的有界重试
- **AND** HTTP 交互请求的既有 admission 和故障降级不变

#### Scenario: Startup fence is revoked
- **WHEN** 后台领取 token 失效、任务撤权或停机 gate 在启动 fence 前生效
- **THEN** 入口拒绝启动并清理已取得的临时认领，不写入执行消息或调用模型

### Requirement: Headless terminal completion and cancellation

无传输 Turn SHALL 与普通 Turn 共用事件、终局消息持久化及 session 认领释放时序，并提供有界的终局通知供后台任务保存运行事实。后台时限或显式取消 SHALL 走既有部分输出持久化和取消机制，不新增一套 Redis Run 终态。进程优雅关闭 SHALL 将已启动后台 Turn 纳入现有 Run registry 的取消和等待。

#### Scenario: Persist a headless terminal result
- **WHEN** 后台 Turn 到达 done、error 或取消
- **THEN** 先按现有规则完成终局消息持久化，再通知后台记录执行终态并释放认领
- **AND** 终局通知失败使用有界处理，不无限锁住会话

#### Scenario: Cancel or shut down a headless Run
- **WHEN** 用户取消、后台运行超时或进程优雅关闭
- **THEN** 部分输出按既有中断持久化规则保存，Run 终态和资源清理沿用现有机制
- **AND** 停机等待覆盖已启动后台 Turn 的有界收尾
