# Design: add-llm-raw-capture

## Context

今天 LLM 原始数据在 `OpenAIClient.StreamChat`(`internal/agent/openai_client.go`)内被聚合为 `LLMResponse` 后,逐帧 chunk JSON(含 extra fields、provider 扩展字段、tool_call 分片形态)与网关错误 body 即被丢弃;唯一留存是 `logLLMRequest`/`logLLMResponse` 的 Debug 级截断预览(`llm-debug-logging` capability),日志轮转即失。网关怪癖(400 raw body、空 `tools[]` 返回 `tool_calls`、usage 缺失)事后无从取证。

既有约束与先例:

- `StreamChat` 是唯一咽喉点,5 个调用方全经过:`confucius.go`、`chongzhi.go`、`liang.go`、`roundcap.go`(收尾轮)、`title.go`。
- ctx 已携带 `trace_id`(TraceMiddleware)与 `userID`(AuthMiddleware/skill 包);`session_id` 与 agent 名不在 ctx。
- `turn_usage`(migration 010)确立了"MySQL-only 观测旁路、独立写入、失败不回滚业务数据"的先例。
- Redis 现有两个 key 家族(`session:{id}`/`msgs:{id}`)均为 best-effort 缓存;agent/all 角色拥有优雅停机(graceful shutdown)钩子。
- 部署形态支持多 agent-role 进程共享同一 MySQL/Redis(split-role 模式)。

## Goals / Non-Goals

**Goals:**

- 每次 LLM 调用留档:实发请求全量 JSON、缝合后的完整响应 JSON、或失败的 HTTP 状态 + raw error body。
- 可按 `session_id`/`trace_id`/`seq` 定位与排序,含 agent/user/model 等冗余查询列。
- 写入绝不阻塞、绝不影响 turn 主路径(messages/turn_usage/SSE 零改动)。
- 批量入库:不是每条一写,也不是等输出完——Redis 缓冲 + 定时/定量双触发批次落库。
- 零配置默认常开;数据永不清理。

**Non-Goals:**

- ~~不做逐帧 chunk 级捕获~~(修订:用户明确要求逐帧原始形态,已纳入 D1;缝合 response 行保留作直查便利)。
- 不做 HTTP 查询 API(运维 SQL 直查;将来需要再立项)。
- 不做 retention/清理/TTL 管理任务。
- 不做压缩(直存;缝合 response 行维持无截断,逐帧 chunk 行有 8MB/调用的截断标记防护——见 D1)。
- 不动三层消息持久化与 `llm-debug-logging` 日志行为。

## Decisions

### D1. 捕获点在 `StreamChat` 内部,每次调用产 request + 逐帧 chunk + response/error 行

`kind=request` 行在 params 组装完成、发流前捕获(存的是**实发** `ChatCompletionNewParams` 序列化 JSON,不是重建近似);流中**每个 SSE chunk 落一行 `kind=chunk`**(raw 为 `ChatCompletionChunk.RawJSON()`——网关原样 wire bytes,非重序列化);`kind=response`/`kind=error` 行在流结束/失败时捕获(缝合版保留,直查最终内容方便)。

- **排序**:同一调用的全部行共享 `call_id` 与 `seq`(调用序号),行内顺序由新列 `frame_index` 决定:request=0,chunk 递增 1..N,response/error=末位。全局排序 `ORDER BY seq, frame_index`。
- **逐帧决策(修订,推翻初版 non-goal)**:用户明确要原始分帧形态而非缝合结果。逐帧是传输级取证(tool_call arguments 分片形态、usage 缺失、扩展字段异常)的唯一依据。
- **体积与截断**:每帧带 ~200B 信封开销、几个 token 一帧,帧数据比聚合内容大 50~100×。累计超过 `frameCaptureCap`(8MB)/调用后停止逐帧捕获,补一行显式截断标记行(raw 为 `{"_truncated":true,...}`),response 行不受影响。防 MEDIUMTEXT 16MB 溢出丢整行。
- **为什么不用 NDJSON 单行**:单行塞全部帧时截断只能截尾(丢的是"后面的帧");每帧一行 + 计数截断丢的也是后面的帧,但无需在内存累积 payload(只记累计字节数),且单帧级粒度可查询。
- **热路径成本**:每帧一次 `RawJSON()` 取串 + 记录组装 + RPUSH(本地 Redis 亚毫秒),远低于帧间隔;失败路径不变(仅 log 丢弃)。
- **为什么请求/响应仍是两行而非一行(请求/响应两列)**:调用中途进程崩溃或挂死时,请求行已进缓冲——那恰是事后最需要"发了什么"的时刻。
- **为什么请求侧存全量**:排查参数类问题(thinking budget、`tools[]` 空发等)需要 ground truth;O(n²)(每轮重发全量历史)已接受——不清理、磁盘换保真。
- 备选否决:在各 agent 层捕获(有 agent 元数据但拿不到原始 payload,且 5 处重复);只在 client 层存响应(丢失中途崩溃的请求侧)。

### D2. sink 抽象:`RawCaptureSink` 接口,client 保持纯净

`internal/agent` 内定义 `RawCaptureSink` 接口(Capture 方法,接收已组装的 record),`OpenAIClient` 持可选 sink(nil 时零行为)。生产实现在 wiring 时(`cmd/blowball/serve.go`)注入:序列化 → RPUSH Redis。`OpenAIClient` 不 import store 包,保持单文件 SDK 边界与可测性。

### D3. write-behind:Redis LIST 缓冲 + LMPop 批量落库

```
StreamChat ─► sink.Capture ─► RPUSH llm_raw:buffer   (非阻塞;失败 log+drop)
                                  │
                 flusher(agent/all role, wireAgent 挂载)
                                  ▼
             LMPop count=batch_size ─► 多值 INSERT llm_raw_log
             触发: ticker(5s) ∥ LLEN≥batch(50); 停机 final flush
```

- **为什么经 Redis 而非进程内 channel**:agent 进程重启不丢已缓冲记录;多 agent-role 实例天然共享一个缓冲;调试时可 LRANGE 现场检查。
- **为什么原子弹出(LPOP count)而非 LRANGE+LTRIM**:多进程部署下两个 flusher 并发 LRANGE+LTRIM 同一 key 会因索引移位互相吃批次(丢数据);`LPOP key count`(Redis ≥ 6.2,单 key 场景与 LMPOP 等价)原子弹出,多消费者安全。代价是"弹出后、入库前"崩溃丢 ≤1 批——debug 数据接受 at-most-once。(实现期修订:设计初稿写 LMPop,但项目测试基建 miniredis 不支持 LMPOP,且 LPOP count 兼容 Redis 6.2+,语义相同,故取 LPOP count。)
- **为什么不复用三层持久化**:这是观测旁路不是业务数据;turn_usage 先例已是 MySQL-only。
- 双触发理由:纯定时让突发排查数据延迟可见;纯定量让低流量会话永等待。5s/50 是常数,不是配置(零配置决策)。

### D4. 表结构(migration `011_llm_raw_log.sql`)

```sql
CREATE TABLE llm_raw_log (
  id            BIGINT UNSIGNED AUTO_INCREMENT PRIMARY KEY,
  call_id       CHAR(36)     NOT NULL,
  seq           INT          NOT NULL,
  trace_id      VARCHAR(64)  NOT NULL,
  session_id    VARCHAR(64)  NOT NULL,
  user_id       VARCHAR(64)  NOT NULL,
  agent         VARCHAR(32)  NOT NULL,
  kind          VARCHAR(16)  NOT NULL,
  model         VARCHAR(128) NOT NULL,
  finish_reason VARCHAR(32)  NOT NULL DEFAULT '',
  http_status   SMALLINT     NOT NULL DEFAULT 0,
  duration_ms   INT          NOT NULL DEFAULT 0,
  raw           MEDIUMTEXT   NOT NULL,
  msg_time      DATETIME     NOT NULL,
  update_time   DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  KEY idx_session (session_id, seq),
  KEY idx_trace   (trace_id, seq),
  KEY idx_time    (msg_time)
);
```

- **排序**:`ORDER BY seq, kind`(request 字典序先于 response/error);`seq` 为 trace 内单调递增调用序号,由 sink 在 request 捕获时分配(进程内 per-trace 原子计数;同 trace 的调用必在同一进程——一个 turn 一个 HTTP 请求一个 agent 进程,title goroutine 同进程)。
- **冗余列**(`model`/`finish_reason`/`http_status`/`duration_ms`/`msg_time`/`update_time`):均可从 raw 推导,物化为免解析直查列,与 `turn_usage.total_tokens` 同思路;`update_time` 对齐既有表惯例。
- **不挂 sessions FK**:会话删除后保留孤儿行(取证优先)。与 `turn_usage` 的 CASCADE 不同是有意为之——本表的存在目的就是事后取证,被删会话的问题恰恰可能就是要查的。
- **无唯一约束/无幂等去重**:LMPop 已是单消费者取出,无重复来源;保持插入路径最简。

### D5. 上下文管道:`session_id` + agent 名走 ctx

仿 `trace`/`skill.WithUserID` 先例新增两个 context value:`SendMessage`(handler)注入 session_id;各 agent 循环层(confucius/chongzhi/liang 的 Run 入口)注入自身 agent 名——roundcap 收尾轮在同一 ctx 下运行自动继承,记为所属 agent;`title.go` 的 `GenerateTitle` 在拷贝 bg ctx 时注入 session_id 与 `"title"`。`user_id`/`trace_id` 已在 ctx,零改动。子代理通过共享 ctx 自动继承全部值。

### D6. 失败捕获的落地细节

`kind=error` 行:`http_status` 取自 openai-go `apierror`(需实现期确认 v3 对 stream 错误暴露的 raw body 字段——`APIError.JSON` 应携带网关响应体);`raw` 存原始错误 body(取不到则存 error string)。ctx 取消(`ctx.Err()`)不视为 error 行——已部分流式的输出按 response 行落档(内容为已聚合部分),取消本身不丢失。

### D7. flusher 生命周期与角色

flusher 只在 agent/all role 启动(`wireAgent` 路径),api role 无 LLM 调用不启动。协程随优雅停机终止:停机时执行一次 final flush(尽力而为,超时上限如 3s),避免常规重启丢缓冲。Redis/MySQL 不可达时 flusher 记 log 退避重试,不影响服务本身可用性。

## Risks / Trade-offs

- [多 agent 进程并发 LMPop 竞争] → LMPop 原子语义保证安全;竞争只是分摊吞吐,无正确性问题。
- [Redis maxmemory 驱逐策略逐掉 buffer key] → 接受:debug 数据 at-most-once;缓冲窗口短(≤5s),暴露面极小。在 spec 中明示该语义。
- [raw 超过 MEDIUMTEXT 16MB 导致 INSERT 失败] → 罕见(流式输出正常远低于该值);flusher 记 log 丢该条,不阻塞批次内其余行(单条错误行单独插入,隔离失败)。
- [不清理 → 表无限增长] → 已接受;运维备份/容量策略管理。request 侧全量存的 O(n²) 增长同此决策。
- [写入可见延迟 ≤5s] → 排查场景接受;紧急时可 LRANGE buffer 看未落库数据。
- [捕获的 raw 含用户会话明文] → 与 messages 表同信任边界(库内明文);无 FK 级联意味着会话删除后 raw 仍留——这是取证优先的有意决策,记录在 spec。
- [进程内 per-trace seq 计数 map 泄漏] → 以 trace_id 为键的 map 随 turn 数缓慢增长;用全局单调计数器替代(trace 内相对序不变)或限制 map 容量——实现期取简,倾向全局计数器。

## Migration Plan

1. 合入 migration `011_llm_raw_log.sql`(docker compose 首次初始化自动执行;存量库手动执行)。
2. 部署新 binary:agent/all role 启动即捕获,无需任何配置。
3. 回滚:回退 binary 即停止捕获与写入;表与已落库数据留存(只增不改,无回滚耦合)。

## Open Questions

- openai-go v3 stream 错误路径对 raw error body 的暴露程度(`apierror.APIError.JSON` 字段形状)——实现期 spike 确认;取不到时降级存 error string,不阻塞其余设计。
- ~~Redis 部署基线是否 ≥ 7.0(LMPop 所需)~~ 已解决:实现改用 `LPOP key count`(Redis ≥ 6.2),兼容性更宽且 miniredis 可测。
