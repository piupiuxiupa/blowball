# Design: tool-call-log-correlation

## Context

排查工具调用问题的现状：`internal/tool/registry.go` 的 `Registry.Call`（所有三个 agent 的注册表工具 + MCP 代理工具的统一咽喉）没有任何日志；仓库中现有日志的关联字段覆盖为零 `session_id`、局部 `trace_id`（`logCmd`、LLM request/response 两处 DEBUG）。而所需基础设施全部在位：`trace.FromContext`（TraceMiddleware 全链路注入）、`agent.SessionIDFromContext`（`MessageStreamHandler.SendMessage` 在 turn 入口注入，随 ctx 流经 orchestrator → agent Run → `Registry.Call` → 工具 Execute）。

阻塞点只有一个：session-id 的 ctx key 定义在 `internal/agent`，而 `internal/tool` 与 `internal/pkg/logger` 不能反向 import agent（`agent` 依赖 `tool`，会成环）。

## Goals / Non-Goals

**Goals:**

- turn 链路（handler → orchestrator → agent → tool）的日志统一携带 `session_id` + `trace_id`，缺失时优雅省略。
- `Registry.Call` 输出中央工具派发日志（工具名、耗时、成败、参数预览），一处覆盖全部注册表工具。
- `agent.WithSessionID`/`SessionIDFromContext` 公开 API 与注入点不变（薄别名）。

**Non-Goals:**

- 不改日志格式/编码器/sink 配置（`logger.Init` 不动）。
- 不改 `llm_raw_log` 表与其 capture 链路（其 attribution 机制保持独立，仅共享 ctx key 定义）。
- 不覆盖无会话上下文的后台任务日志（flusher、title、shutdown 路径）。
- 不做强制 lint（zap 字段无法静态强制），靠 helper + 约定。

## Decisions

### D1：ctx key 迁至新叶子包 `internal/pkg/reqctx`，而非塞进现有包

新包只含 `WithSessionID`/`SessionIDFromContext`（key 类型 + getter/setter），零依赖。`internal/agent` 的同名函数改为委托别名，`MessageStreamHandler`/`TitleService` 的注入点无需改动。

*备选*：塞进 `internal/pkg/trace`（本就是 per-request 身份包）—— 名不符实（trace 包装 session id），且 trace 被 store 层广泛 import，语义扩散面大。*备选*：塞进 `internal/pkg/logger`（`FromContext` 就地读 key）—— logger 已 import config，让日志包拥有业务身份 key 是层次颠倒；且未来别的消费者（如 store 层）再读 session id 就得 import logger。叶子包最干净，两个方向都能依赖。

### D2：`logger.FromContext(ctx)` 是唯一的字段拼装点

```go
func FromContext(ctx context.Context) *zap.Logger {
    l := L()
    if ctx == nil { return l }
    if sid := reqctx.SessionIDFromContext(ctx); sid != "" {
        l = l.With(zap.String("session_id", sid))
    }
    if tid := trace.FromContext(ctx); tid != "" {
        l = l.With(zap.String("trace_id", tid))
    }
    return l
}
```

空值跳过（与现有 `logCmd` 的 trace_id 优雅省略语义一致）。所有改写点从 `logger.L().Warn(...)` 变为 `logger.FromContext(ctx).Warn(...)` —— 机械替换，无行为分支。

### D3：中央派发日志放 `Registry.Call`，成功 INFO / 失败 WARN

- 位置：`Registry.Call` 是唯一咽喉 —— 三个 agent 的 `dispatchRegistryTool`、per-turn 注册的 `mcp_*` 家族全部经过它；子代理派发（`invoke_*`）不经过，但已有自己的 SSE 事件与 usage 归因，不重复加。
- 字段：`event=tool_call`、`tool`、`duration`（`time.Since` 计时）、`args_preview`（截断，复用 200 字节上限的既有 truncate 风格）、失败附 `zap.Error`。不记结果体（envelope 可能巨大且已进 messages 表）。
- 级别：成功 INFO、失败 WARN。依据：executor audit 的 INFO 先例；默认日志级别为 info，DEBUG 会让排查痛点原样保留。一个 turn 5–50 次工具调用的量级可接受。
- 日志经 `FromContext(ctx)` 输出 —— `Registry.Call` 执行时 ctx 必然携带两 id（turn 注入点在更上游）。
- 超时派生（`context.WithTimeout`）在计时起点之后、`Execute` 之前完成，日志覆盖的是含超时包装的完整执行时长。

### D4：存量补齐按「有 ctx 就换」的机械规则

替换清单（全部持有 ctx）：`internal/agent` 五文件七处、`internal/tool/executor/runner.go` 两处、`internal/store/redis/redis.go` 的 `logCmd`（改为 `FromContext` 并顺带获得 session_id）、`internal/handler` 中 turn 生命周期（message_stream、turn_run）与 compaction 的 WARN。`logCmd` 保持 DEBUG 级别不变。

### D5：args_preview 的暴露面评估

工具参数本就是模型可见内容，已完整存于 `llm_raw_log`（request 行）与 `messages` 表；per-user MCP 的凭证是服务端注入、不在 args 中。日志中给截断预览**不产生新的暴露面**。预览上限与 `openai_client.go` 现有 `truncatePreview` 一致。

## Risks / Trade-offs

- [INFO 级派发日志增加日志量（每 turn 5–50 行）] → 单行字段精简（无结果体）；lumberjack 已做尺寸轮转；量级与 executor audit 同阶。
- [约定无法 lint 强制，新代码可能忘用 `FromContext`] → `zap.go` 包注释更新为「per-request 日志必须走 FromContext」+ code review 约定；helper 门槛足够低。
- [ctx key 迁移期间新旧 key 并存导致读不到] → 迁移是同 PR 内原子替换（agent 别名指向新 key），不存在混跑窗口；`WithAgentName` 留在 agent 包不动（仅 tool 层不需要它）。
- [`Registry.Call` 计时包含 MCP 代理的远程往返] → 这是期望语义（排查的就是慢调用）；GilData 类 71s 慢调用正需要这条日志暴露。

## Migration Plan

纯代码改动，无 schema/配置/API 变更，一次部署生效。回滚 = 还原代码。日志消费方（grep/journal 查询）仅需注意新增 `session_id` 字段与 `event=tool_call` 行，无破坏性。

## Open Questions

（无 —— 级别策略、包归宿、字段集已在探索阶段定案。）
