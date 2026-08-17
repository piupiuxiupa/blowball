# Delta Spec: llm-raw-capture

## ADDED Requirements

### Requirement: 每次 LLM 调用的原始记录含逐帧 chunk 行

系统 SHALL 在每次 LLM 调用(所有经由 `LLMClient.StreamChat` 的调用方:Confucius、Chongzhi、Liang、round-cap 收尾轮、title 生成)产生:`kind=request` 行(调用发起时)、流中**每个 SSE chunk 一行 `kind=chunk`**、以及 `kind=response` 或 `kind=error` 行(调用结束时;缝合版保留)。同一调用的全部行 MUST 共享同一 `call_id`(uuid)与同一 `seq`(trace 内单调递增的调用序号),行内顺序由 `frame_index` 列决定(request=0,chunk 递增 1..N,response/error=末位),并携带 `trace_id`、`session_id`、`user_id`、`agent`、`model` 列。

#### Scenario: 成功的流式调用
- **WHEN** 一次 `StreamChat` 调用正常完成(网关发送了 N 个 SSE chunk 并正常结束)
- **THEN** 落库 N+2 行:`kind=request`(frame_index=0)、N 行 `kind=chunk`(frame_index 1..N,按到达顺序)、`kind=response`(frame_index=N+1),全部共享 `call_id` 与 `seq`,按 `ORDER BY seq, frame_index` 排序还原到达顺序

#### Scenario: chunk 行保真为网关原样帧
- **WHEN** 任一 SSE chunk 被捕获
- **THEN** 该行的 `raw` 为该帧的原始 wire bytes(未经重新序列化或拼接),可独立 parse 为单个 `chat.completion.chunk` 对象

#### Scenario: 调用中途进程崩溃
- **WHEN** `StreamChat` 发出请求后、流结束前进程崩溃
- **THEN** 缓冲中已存在该调用的 `kind=request` 记录及崩溃前已到达的各 `kind=chunk` 行(请求侧与已收帧不因调用未完成而缺失)

#### Scenario: 调用失败(网关错误)
- **WHEN** 网关返回非 2xx(如 400/500)导致流失败
- **THEN** 落库 `kind=request` + `kind=error` 两行(无 chunk 行——无帧到达);error 行的 `http_status` 为网关状态码,`raw` 为网关返回的原始错误 body(不可得时为 error string)

#### Scenario: 收尾轮与 title 调用的归属
- **WHEN** round-cap 收尾轮执行,或 title 生成调用 LLM
- **THEN** 前者记录的 `agent` 为命中轮次上限的所属 agent;后者为 `"title"`;两者的 `session_id`/`trace_id` 与触发它们的 turn 一致

### Requirement: 逐帧捕获的累计截断

当一次调用的 `kind=chunk` 行累计捕获字节数超过上限(8MB)时,系统 SHALL 停止为该调用追加 chunk 行,并 MUST 落一行显式截断标记行(`raw` 为含 `"_truncated": true` 及累计帧数/字节数的 JSON 对象);该调用的 `kind=response` 行 MUST NOT 因截断而缺失。

#### Scenario: 超限截断
- **WHEN** 一次超长流式调用的帧累计字节数越过 8MB 上限
- **THEN** 该调用不再产生新的 `kind=chunk` 行,最后一条 chunk 位置之后落一行截断标记行,`kind=response` 行仍正常落库

### Requirement: 原始 payload 保真入库

`kind=request` 行的 `raw` MUST 为实际发送的完整请求 JSON(messages、tools、全部参数),在 params 组装完成后捕获,而非重建近似。`kind=chunk` 行的 `raw` MUST 为该帧的原始 wire bytes(见逐帧 requirement)。`kind=response` 行的 `raw` MUST 为由流式分片缝合重构的完整响应等价 JSON,保留聚合结构(内容、tool_calls、finish_reason、usage、reasoning 及网关扩展字段)。`raw` 列 SHALL 为 MEDIUMTEXT;request/response/error 行系统 MUST NOT 压缩或截断,chunk 行仅受 8MB 累计截断防护约束。

#### Scenario: 请求侧为实发全量
- **WHEN** 一次带工具与思维参数的调用被捕获
- **THEN** `kind=request` 的 `raw` 可解析出与实发 `ChatCompletionNewParams` 一致的 messages 数组、tools 数组与参数字段

#### Scenario: 响应侧含被丢弃字段
- **WHEN** 网关在流中返回 usage 或扩展字段
- **THEN** `kind=response` 的 `raw` 含聚合后的该字段,不因现有 `LLMResponse` 结构体未承载而丢失

### Requirement: write-behind 缓冲写入

捕获的记录 SHALL 先写入 Redis 缓冲(`llm_raw:buffer` LIST,`RPUSH`),由后台 flusher 批量写入 MySQL,而非逐条写库、也非等输出完成后再写。捕获路径(RPUSH)MUST 为非阻塞且失败安全:Redis 不可达时记日志并丢弃该记录,turn 的执行与 SSE 流式路径 MUST NOT 因此受阻或报错。`messages` 三层持久化、`turn_usage` 写入、SSE 事件流 MUST 保持零改动。

#### Scenario: Redis 不可达
- **WHEN** 捕获时 Redis 连接失败
- **THEN** 仅输出 WARN 日志,当次 LLM 调用与整 turn 行为与关闭捕获时完全一致

#### Scenario: 主路径隔离
- **WHEN** 捕获与缓冲路径任意环节失败
- **THEN** `messages` 表、`turn_usage` 表与 SSE 事件序列不受影响

### Requirement: 批量落库触发与原子弹出

后台 flusher(仅在 agent/all 角色启动;api 角色不启动)SHALL 以双触发落库:定时 ticker(约 5s)或缓冲长度达到批量阈值(约 50 条)先到者触发;批量弹出 MUST 使用原子弹出语义(`LMPop`),保证多 agent 进程并发消费同一缓冲时无批次互相覆盖或索引移位丢失。系统优雅停机时 SHALL 执行一次尽力而为的 final flush。

#### Scenario: 定时触发
- **WHEN** 缓冲中仅有少量记录且 ticker 到期
- **THEN** 现有缓冲记录被批量落库

#### Scenario: 数量触发
- **WHEN** 缓冲长度在 ticker 间隔内达到批量阈值
- **THEN** 提前触发一次批量落库,无需等待 ticker

#### Scenario: 多进程并发消费安全
- **WHEN** 两个 agent-role 进程的 flusher 同时消费同一 Redis 缓冲
- **THEN** 每条记录恰被一个批次弹出并落库,无丢失、无互相覆盖

#### Scenario: 优雅停机
- **WHEN** agent/all 角色进程收到停机信号
- **THEN** flusher 在退出前执行一次 final flush(有超时上限),缓冲中已捕获记录尽可能落库

### Requirement: llm_raw_log 表结构与查询

系统 SHALL 提供 MySQL 表 `llm_raw_log`,列含 `id`(自增主键)、`call_id`、`seq`、`frame_index`、`trace_id`、`session_id`、`user_id`、`agent`、`kind`、`model`、`finish_reason`、`http_status`、`duration_ms`、`raw`(MEDIUMTEXT)、`msg_time`、`update_time`。表 MUST 建立 `(session_id, seq, frame_index)`、`(trace_id, seq)`、`(msg_time)` 索引。`model`、`finish_reason`、`http_status`、`duration_ms`、`msg_time`、`update_time` 为可从 raw 推导的冗余物化列,支持免解析直查。单条 INSERT 失败(如超出 MEDIUMTEXT 上限)SHALL 仅丢弃该条并记日志,MUST NOT 阻塞批次内其余记录落库。

#### Scenario: 按会话回放调用序列
- **WHEN** 运维按 `session_id` 查询并 `ORDER BY seq, frame_index`
- **THEN** 得到该会话各 LLM 调用的有序请求/逐帧/响应记录,无需解析 raw 即可经 `agent`/`model`/`kind`/`finish_reason`/`duration_ms` 列定位与筛选

#### Scenario: 单条超限隔离
- **WHEN** 批次中某条 raw 超过 MEDIUMTEXT 上限导致该行插入失败
- **THEN** 该条被丢弃并记 ERROR 日志,批次内其余行正常落库

### Requirement: 会话删除不联动、数据不清理

`llm_raw_log` MUST NOT 对 `sessions` 表建立外键或级联删除;会话删除后其原始记录 SHALL 保留为孤儿行。系统 MUST NOT 提供或执行任何针对该表的时间性清理、TTL 或后台删除任务。

#### Scenario: 会话删除后取证
- **WHEN** 某会话被删除(purge)后运维查询其 `session_id` 的原始记录
- **THEN** 记录仍完整存在且可查

#### Scenario: 无清理任务
- **WHEN** 系统长期运行
- **THEN** `llm_raw_log` 无任何自动清理行为,增长仅由运维容量/备份策略管理

### Requirement: 零配置默认常开

原始捕获 SHALL 默认启用,MUST NOT 提供开关、保留期或批参数的配置项;常量(ticker 间隔、批量阈值)固化在实现中。

#### Scenario: 无配置部署
- **WHEN** 使用不含任何新配置字段的既有 `config.yaml` 启动 agent/all 角色
- **THEN** 原始捕获与落库自动生效,配置校验行为不变
