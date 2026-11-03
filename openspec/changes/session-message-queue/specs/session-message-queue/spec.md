# session-message-queue Delta

## ADDED Requirements

### Requirement: 会话消息排队准入

系统 SHALL 在 `messages.queue.enabled: true` 时，对已有运行中 turn 的会话接受消息请求并将消息加入该会话的 FIFO 队列，而非拒绝。全部入口校验（请求体字节上限、输入 token 上限、model/effort 解析、附件校验）SHALL 在入队之前完成：被校验拒绝的请求 SHALL NOT 入队、SHALL NOT 占用队列深度、SHALL NOT 触发任何 LLM 调用。队列深度达到 `messages.queue.max_depth` 时，请求 SHALL 返回 409 `QUEUE_FULL` 且队列不变。每个队列条目 SHALL 携带该请求的完整已解析快照（content、已解析 model selection、附件引用、user_id、run_id、入队时间），使接续执行不依赖入队进程的后续状态。入队 SHALL 与该 run 的可观测初始状态原子发布：meta（queued，含归属与 model 字段）与 queued 事件在 RPUSH 的同一原子步骤内写入——紧跟其后赢得竞争的 pump 永远不会观察到迟到的初始状态覆盖 running 或终态。入队路径因 Redis 故障不可用（admit 原子操作无法执行）时，系统 SHALL 记录 WARN 后按无锁语义直接起轮执行（与既有「认领传输失败降级放行」一致），SHALL NOT 拒绝请求。

#### Scenario: 运行中发送消息进入队列

- **WHEN** 会话存在运行中 turn 且队列深度未达上限，用户向该会话 POST 消息
- **THEN** 请求被接受，消息追加到队尾
- **AND** 运行中 turn 不受影响

#### Scenario: 队列满时拒绝

- **WHEN** 队列深度已达 `messages.queue.max_depth`，用户再 POST 消息
- **THEN** 系统返回 HTTP 409，错误码 `QUEUE_FULL`
- **AND** 队列内容与深度不变，运行中 turn 不受影响

#### Scenario: 校验拒绝不入队

- **WHEN** `content` 估算 token 数超过上限（或任何入口校验失败）
- **THEN** 请求返回 400，队列不变、深度不被占用
- **AND** 未触发任何 LLM 调用或附件引用变更

#### Scenario: Redis 不可达时降级放行

- **WHEN** 入队原子操作因 Redis 故障无法执行
- **THEN** 系统 WARN 后直接起轮执行该消息（无锁），请求不被拒绝
- **AND** 该行为与既有认领降级约定一致

#### Scenario: 入队状态与条目原子发布

- **WHEN** 消息入队后，上一轮的终局路径在入队调用返回前立即弹出并接续该条目
- **THEN** 该 run 的 meta 与 queued 事件在条目可被消费前已存在（同一步骤写入）
- **AND** 不会出现后写的初始状态覆盖 running 或终态、或 queued 事件晚于执行事件落日志

### Requirement: FIFO 顺序密封

系统 SHALL 保证队列顺序的密封性：队列非空时，新到达的消息 SHALL 只能排到队尾（即使此刻会话认领短暂空闲也不得插队认领）；会话认领空闲且队列为空时，新消息才 SHALL 被允许直接认领并立即起轮。队头条目的弹出与认领移交 SHALL 在同一原子操作内完成；认领被占时 SHALL 将弹出的队头推回队首。并发消息请求（含跨副本）下，队列内的顺序 SHALL 唯一确定。

#### Scenario: 多条排队消息按序接续

- **WHEN** 队列为 [B, C]，当前运行中的 turn A 到达终局
- **THEN** B 首先被接续为下一轮；B 终局后 C 被接续
- **AND** 任意时刻该会话至多一个 turn 处于 running

#### Scenario: 新消息不插队

- **WHEN** 队列非空，此刻会话认领恰好空闲（如上一轮刚终局、接续尚未完成），新消息 POST 到达
- **THEN** 该消息被排到既有全部条目之后
- **AND** 既有队头照常获得下一个运行槽

### Requirement: 排队等待期的 SSE 响应

排队消息的 POST SHALL 返回 SSE 流：响应携带 `X-Run-Id` 头（值为该请求的 trace_id，即队列条目的 run_id），流上首个事件 SHALL 为 `queued`（Meta 携带 `run_id` 与队列位置），随后沿用事件日志重放 + live 尾随循环直到该 run 终局。`queued` 事件 SHALL 与其它事件一样写入 `run:{rid}:events`（重放与 `Last-Event-ID` 语义一致）。排队 run SHALL 以 `queued` 状态创建 `run:{rid}:meta`，接续起轮时置为 `running`；`queued` SHALL NOT 被终局判定或死 run 检测视为运行中。客户端在等待期断开后，SHALL 可经恢复端点按 run_id 重接同一 run。

#### Scenario: queued 事件先于生成事件

- **WHEN** 排队消息被接受
- **THEN** SSE 流的首个事件为 `queued`，其 `meta.run_id` 与 `X-Run-Id` 一致
- **AND** 接续起轮后 `agent_start` 等事件在同一流上依序出现

#### Scenario: 等待期断开重连续接同一 run

- **WHEN** 客户端在排队等待期断开 SSE 连接，随后经 `GET /turns/:run_id/events` 重新 attach
- **THEN** 端点回放 `queued` 事件后继续尾随，接续起轮后事件无丢失
- **AND** 该 run 在等待期不被判定为死 run

#### Scenario: 排队 run 的状态生命周期

- **WHEN** 消息入队后被接续起轮
- **THEN** `run:{rid}:meta` 的 status 由 `queued` 变为 `running`，终局后按既有终态流转
- **AND** 事件日志首条为 `queued` 事件

### Requirement: 排队消息的持久化时机与排序

排队消息的用户消息行（及 recall 注入对）SHALL 在接续起轮时写入——位于该轮认领完成之后、orchestrator 启动之前（沿用发送时持久化的位置与不变量）。其 `msg_time` SHALL 取接续时刻而非入队时刻，使历史排序保持 `userA < assistantA < queuedUserB < …`。等待期该消息 SHALL NOT 出现在消息历史读取（`GET /messages`、Redis 读缓存）中——等待期可见性由队列读 API 承担。运行中 turn 的持久层历史恢复（含 mid-turn compaction 的 `RecoverDurable` 路径）SHALL NOT 包含尚在排队、未起轮的消息。

#### Scenario: 等待期消息历史不可见

- **WHEN** 消息 B 入队且尚未接续起轮，客户端读取会话历史
- **THEN** 历史返回内容不含消息 B（其呈现由队列读 API 承担）

#### Scenario: 接续后历史排序正确

- **WHEN** 消息 B 在 turn A 终局后被接续并起轮
- **THEN** 历史中 B 的用户行排在 turn A 全部 assistant 行之后
- **AND** B 起轮后产生的 assistant 行排在 B 的用户行之后

#### Scenario: mid-turn 压缩不泄漏排队消息

- **WHEN** turn A 运行中触发 mid-turn compaction（flush + `RecoverDurable` + 重写 round），期间消息 B 已入队
- **THEN** 重写后的 round 上下文不含消息 B
- **AND** B 仍按原样等待接续

### Requirement: 队列接续驱动

上一轮的终局消息批次持久化完成且会话认领释放之后，turn goroutine SHALL 尝试将队头条目接续为下一轮（弹头与认领移交的原子原语；认领被占则推回）。进程优雅关闭期间系统 SHALL 跳过接续（排队条目保留在队列中，交由重启后的 reconciler 处理）。agent 角色 SHALL 运行一个后台 reconciler：周期扫描会话队列 key，对会话认领空闲的队列尝试接续（多副本安全，复用同一原子接续原语），并为 `queued` 状态的 run 重臂 run-key 家族（`run:{rid}:events` / `run:{rid}:meta`）的 TTL。reconciler 或接续路径的故障 SHALL NOT 影响运行中的 turn（WARN 降级）。

#### Scenario: 终局后自动接续

- **WHEN** 运行中 turn 到达终局、终局批次 durable、认领释放，且队列非空
- **THEN** 队头消息在无需新 HTTP 请求的情况下被接续为下一轮
- **AND** 其接续轮读取到的历史包含上一轮的完整终局批次

#### Scenario: 优雅停机不接续

- **WHEN** 进程收到关闭信号，某 turn 终局释放认领且队列非空
- **THEN** 该进程不启动接续轮，排队条目保留在队列
- **AND** 重启后由 reconciler 接续

#### Scenario: 停机中止已弹出的启动并原子返还

- **WHEN** pump 已弹出条目并进入启动准备（会话查找/历史恢复/发送时持久化，尚未注册），进程此时收到关闭信号
- **THEN** 准备阶段被 shutdown 上下文取消，turn SHALL NOT 启动（首个持久写之前设检查点）
- **AND** 条目以队头位置原子返还队列并释放其认领（单脚本：LPUSH + 自有认领比较删除）
- **AND** 关停等待 pop→注册窗口内的启动归零后才开始取消运行中 turn——不存在注册表不可见、取消后仍会启动的任务

#### Scenario: 崩溃后 reconciler 接续

- **WHEN** 拥有 turn 的进程在认领释放后、接续完成前死亡（或认领经 TTL/强清后空闲），队列仍有条目
- **THEN** reconciler 在扫描周期内对空闲会话执行接续，队头起轮

#### Scenario: queued run 的 TTL 在等待期被续臂

- **WHEN** 消息排队等待超过 run-key TTL 基准（1 小时量级）
- **THEN** reconciler 周期性重臂其 `run:{rid}:events` / `run:{rid}:meta` TTL，等待期订阅与重放不因 key 过期中断

#### Scenario: 接续先建心跳再发布 running

- **WHEN** 排队消息被接续起轮，发送时持久化耗时超过订阅者的轮询周期
- **THEN** running 状态发布前 run 的 alive 心跳已存在（同步首发 + drainer 周期续臂）
- **AND** 已尾随的 SSE 订阅者不会将运行中的 run 判定为死 run 并强制释放认领

#### Scenario: 启动期心跳持续续臂至 drainer 接手

- **WHEN** 发送时持久化（或存储池等待）耗时超过心跳 TTL（15 秒）且发生在 drainer 首拍之前
- **THEN** 启动期心跳保活按 drainer 节奏持续续臂，alive 心跳不因单次首发过期
- **AND** 已尾随的 SSE 订阅者在整个启动阶段都不会将 run 判定为死 run

#### Scenario: 过期 meta 在接续时完整重建

- **WHEN** 等待期超过 run-key TTL 且 reconciler 的续臂不可用，meta 已过期而队列条目仍在
- **THEN** 接续起轮时从条目快照重建完整 RunMeta（归属、model、入队时间戳）
- **AND** 取消与事件端点的归属校验继续命中，不返回 404

#### Scenario: reconciler 轮换覆盖超过单轮上限的积压

- **WHEN** 队列会话数超过单轮处理上限且扫描顺序稳定
- **THEN** 每轮的处理起点轮换推进，尾部会话在后续周期内获得 pump 尝试与 TTL 续臂
- **AND** 尾部不会因头部会话持续占用而饥饿

### Requirement: 排队 run 的取消

取消端点 SHALL 支持取消 `queued` 状态的 run：按 run_id 定位并移除队列条目、状态置 `cancelled`、按既有保留窗 retire；条目携带的附件 staged claim SHALL 被回滚。排队 run（非 terminal、不在本进程 registry、无心跳）SHALL NOT 被死 run 强清路径误判为 `interrupted`。取消端点对排队 run 与终局 run 一样幂等。取消运行中 turn SHALL NOT 影响该会话队列中其余条目的接续。

#### Scenario: 取消排队中的消息

- **WHEN** 用户对 queued 状态的 run 调用取消端点
- **THEN** 条目出队、状态置 `cancelled`、附件 staged claim 回滚
- **AND** 等待该 run 的 SSE 订阅按终局语义收流

#### Scenario: 取消与接续竞争

- **WHEN** 取消在扫描队列后、原子移除前，pump 弹出该条目并开始其 turn
- **THEN** 移除实际未发生（删除计数为 0），系统 SHALL NOT 报告 cancelled 或回滚附件
- **AND** 端点按 run 的当前状态作答，客户端重试后走运行中取消路径

#### Scenario: 取消保留其他已接受消息引用的附件

- **WHEN** 被取消的排队消息首次认领了附件 X，而另一条仍被接受的排队消息（或其后入队的消息）合法引用同一附件
- **THEN** X 的会话绑定与文件 SHALL 保留，不随被取消消息回滚到 staging
- **AND** 仅无任何剩余引用的附件被恢复到 staging 并解除绑定

#### Scenario: 已弹出或已持久化消息的引用受保护

- **WHEN** 引用附件 X 的消息已被 pump 弹出（无论处于启动准备、运行中还是已完成持久化）而不在剩余队列中
- **THEN** 持久化行的附件引用 SHALL 纳入保护集合；会话认领存在但持有者尚无心跳（启动准备期，行未落库）时整体挂起回滚
- **AND** 不以「队列中无引用」推断附件无人使用

#### Scenario: 取消当前轮后队列继续

- **WHEN** 用户取消运行中 turn，队列中仍有待处理条目
- **THEN** 被取消 turn 的部分输出按 interrupted-turn 路径持久化并释放认领
- **AND** 队头条目照常接续为下一轮

### Requirement: 队列读 API

系统 SHALL 提供已鉴权的队列读取端点 `GET /api/v1/sessions/:session_id/queue`（api 分区），按入队顺序返回该会话的排队条目（`run_id`、`content`、`model`、入队时间、附件引用）。仅会话所有者可读；不存在或非所有者的会话 SHALL 返回 404。该端点 SHALL 只依赖 run 状态存储的读原语（api 角色无需 registry 或 orchestrator）。

#### Scenario: 读取会话队列

- **WHEN** 会话队列非空，所有者请求 `GET /api/v1/sessions/:session_id/queue`
- **THEN** 系统返回 HTTP 200 与按入队顺序排列的条目数组

#### Scenario: 非所有者读取队列

- **WHEN** 用户请求不属于自己会话的队列
- **THEN** 系统返回 HTTP 404，不泄露他人会话的存在性

### Requirement: 接续轮的完整 turn 语义

每条排队消息 SHALL 作为完整独立的一轮执行，并与 HTTP 直接发起的轮共用同一起轮路径：入队时解析的 model / reasoning effort / 上下文与输出预算 SHALL 在该轮全程生效；会话标题生成 cadence SHALL 在接续起轮时照常触发（用户消息序数照算）；cross-session memory recall SHALL 在接续时以该消息查询（此时上一轮终局已在历史中）；turn-start 上下文压缩检查 SHALL 读取包含上一轮 `context_tokens` 的最新值；`turn_usage` SHALL 照常按轮记录。

#### Scenario: 每条消息独立模型与预算

- **WHEN** 排队消息 A 入队时解析为模型 X、消息 B 解析为模型 Y，二者先后接续
- **THEN** A 轮全程使用 X 的 selection，B 轮全程使用 Y 的 selection

#### Scenario: 接续轮包含上一轮完整历史

- **WHEN** 队头消息被接续起轮
- **THEN** 其 LLM 上下文包含上一轮的完整用户消息与 assistant 输出（终局批次已 durable）
- **AND** title cadence、recall、turn-start 压缩检查按该轮语义照常执行

### Requirement: 队列配置与回滚

`messages.queue.enabled` SHALL 默认为 false：省略或关闭时，消息发送路径行为 SHALL 与既有行为完全一致（运行中 turn 的会话 POST 返回 409 `SESSION_BUSY`，无队列读写、无 queued 状态与 `queued` 事件、无 reconciler）。`messages.queue.max_depth` SHALL 为正整数（默认 8），非正值 SHALL 在配置加载时被拒绝。回滚 SHALL 只需关闭开关并重启 agent 角色进程。

#### Scenario: 默认关闭行为不变

- **WHEN** 配置不含 `messages.queue`（或 `enabled: false`）且会话存在运行中 turn
- **THEN** POST 返回 409 `SESSION_BUSY`，行为与队列能力引入前 byte-for-byte 一致

#### Scenario: 非法深度配置拒绝加载

- **WHEN** 配置设置 `messages.queue.max_depth` 为零或负数
- **THEN** 配置加载失败，服务拒绝启动

### Requirement: 会话删除清空队列

会话删除 SHALL 在既有删除序列（持久化 drain → 归档清理 → 缓存清理）中清除该会话的队列 key。排队条目 SHALL NOT 进入删除归档镜像（尚非消息行）。删除后队列读端点 SHALL 对该会话返回空。

#### Scenario: 删除会话清空队列

- **WHEN** 会话队列非空时该会话被删除
- **THEN** 队列条目全部清除，不再被接续
- **AND** 对应 queued run 的等待期订阅按终局语义收流
