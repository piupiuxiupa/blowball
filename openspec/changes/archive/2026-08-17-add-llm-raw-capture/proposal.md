# Proposal: add-llm-raw-capture

## Why

LLM 原始输入/输出在 `OpenAIClient.StreamChat` 内被聚合后即丢弃——今天唯一的留存是截断的 debug 日志预览(`llm-debug-logging`),排查网关/模型行为问题(如网关 400 的 raw body、空 `tools[]` 仍返回 `tool_calls`、字段缺失/扩展字段异常)时没有可回溯的原始数据。需要一个默认常开、不阻塞主路径的原始数据入库旁路,用于事后排查与观测。

## What Changes

- 在 `OpenAIClient.StreamChat` 内部(唯一咽喉点,覆盖 Confucius/Chongzhi/Liang/roundcap 收尾轮/title 全部 5 个调用方)新增原始数据捕获:
  - `kind=request`:params 组装完成后捕获**实发全量 JSON**(messages + tools + 全部参数),调用开始即落缓冲——进程中途崩溃时请求侧已留存;
  - `kind=response`:流结束时捕获缝合重构的完整 `chat.completion` 等价 JSON(含目前被丢弃的字段);
  - `kind=error`:失败时捕获 HTTP 状态码 + 网关 raw error body;
  - 每次 LLM 调用产两行(request + response/error),共享 `call_id` + `seq`。
- 写入路径为 **write-behind 缓冲**:capture → Redis LIST(`RPUSH`,非阻塞,失败仅 log 不影响 turn)→ agent/all role 后台 flusher(`LMPop` 批量弹出 → MySQL 批量 INSERT;定时 ticker ∥ 队列长度阈值双触发;优雅停机 final flush)。
- 新增 MySQL 表 `llm_raw_log`(migration 011):含 `call_id`/`seq`(排序)、`session_id`/`trace_id`/`user_id`/`agent`(关联与冗余查询列)、`model`/`finish_reason`/`http_status`/`duration_ms`(冗余物化列)、`raw MEDIUMTEXT`、`msg_time`/`update_time`(冗余时间列)。
- 表**不挂 sessions FK**(会话删除后 raw 行保留为孤儿行,取证优先);**永不清理**(无 retention、无后台 DELETE);**零配置默认常开**。
- 新增 `session_id` 与 agent 名的 context 注入(`SendMessage` + `title.go` 后台 ctx;`trace_id`/`user_id` 已在 ctx);sink 为 agent 包内 `RawCaptureSink` 接口、serve.go wiring,`OpenAIClient` 保持零 store 依赖。
- 主路径零改动:`messages` 三层写入、`turn_usage`、SSE 流式路径均不受影响;MySQL-only 观测旁路,at-most-once 语义(崩溃窗口最多丢一个已弹出的批次)。

## Capabilities

### New Capabilities
- `llm-raw-capture`: 将每次 LLM 调用的原始请求/响应/错误 payload 经 Redis 缓冲批量写入 MySQL `llm_raw_log` 表,含捕获内容、上下文关联列、write-behind 写入语义、排序与不清理策略。

### Modified Capabilities

(无——`llm-debug-logging` 的日志预览行为不变;`agent-orchestration`/`turn-cost-tracking` 等既有能力的需求无变更。)

## Impact

- **代码**:
  - `internal/agent/openai_client.go`(捕获点)、`internal/agent/`(sink 接口 + ctx 注入)、`internal/handler/message_stream.go`(session_id 注入)、`internal/service/title.go`(bg ctx 注入 session_id + agent 名);
  - 新 flusher 组件(agent/all role 启动,`cmd/blowball/serve.go` wiring + 优雅停机挂钩);
  - `internal/store/redis/`(buffer key 家族:`llm_raw:buffer` RPUSH/LMPop)、`internal/store/mysql/`(`llm_raw_log` 批量 INSERT);
  - `migrations/011_llm_raw_log.sql`。
- **API**:无新 HTTP 端点(运维直接 SQL 查询);OpenAPI 契约不变。
- **依赖**:无新外部依赖;`LMPop` 需 Redis ≥ 7.0(需与部署基线确认,见 design)。
- **存储**:MySQL `llm_raw_log` 无清理增长(运维备份策略管理);Redis 增加一个缓冲 key 家族。
- **运行语义**:agent/all 角色新增后台 flusher 协程;api 角色不受影响(无 LLM 调用)。
