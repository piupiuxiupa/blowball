# Proposal: tool-call-log-correlation

## Why

工具调用出问题时日志文件几乎帮不上忙：中央派发点 `Registry.Call` 是所有 agent 所有注册表工具的唯一咽喉，却**零日志**（工具名/参数/耗时/成败无痕）；已有日志又缺关联字段 —— 全仓库没有一条日志带 `session_id`，`trace_id` 只覆盖 LLM request/response 与 redis 命令两处 DEBUG。排查只能绕道 `llm_raw_log` 表或 SSE 事件。需要让 turn 链路上的日志可按 `session_id` + `trace_id` 直接定位。

## What Changes

- **session-id ctx key 下沉共享包**：`SessionIDFromContext`/`WithSessionID` 的 key 从 `internal/agent`（`rawcapture.go`）迁至新叶子包 `internal/pkg/reqctx`；`internal/agent` 保留薄别名（公开 API 不变）。解决 `internal/tool` 不能反向 import agent 的环。
- **`logger.FromContext(ctx) *zap.Logger`**：返回 `L().With(session_id, trace_id)`（空值跳过字段），成为 turn 链路日志的统一出口；`zap.go` 包注释预留的 per-request field 机制正式落地。
- **`Registry.Call` 中央派发日志（新增）**：每次注册表工具调用输出一条结构化日志，字段 `event=tool_call` / `tool` / `duration` / 截断的 `args_preview` / 失败时附 `error`；成功 INFO、失败 WARN（与 executor audit 的 INFO 先例一致；默认日志级别 info，用 DEBUG 则排查时根本看不到）。覆盖三个 agent 的全部注册表工具与 MCP 代理工具。
- **存量 agent 路径日志补齐两字段**（机械替换为 `FromContext`）：
  - `internal/agent`：`agent.go:300`（unexpected finish_reason WARN）、`lengthcontinue.go:134`、`roundcap.go:96`、`orchestrator.go:295/524`、`openai_client.go` 的 LLM request/response/capture/idle-timeout 五处；
  - `internal/tool/executor/runner.go`：audit 与 dangerous-command 日志（现仅 user_id）；
  - `internal/store/redis/redis.go` `logCmd`：补 `session_id`（现有 trace_id 保留）;
  - `internal/handler` turn 生命周期与 compaction 路径的 WARN 日志。
- **边界**：后台任务（msgflush/llmraw flusher、title 服务等）天然无会话上下文，不在范围内；`llm_raw_log` 表结构不动（已有 session_id/trace_id 列）。

## Capabilities

### New Capabilities

- `tool-call-log-correlation`: turn 链路日志的关联字段约定 —— context 感知 logger（`session_id`+`trace_id`）、`Registry.Call` 中央工具派发日志、agent/tool/handler 存量日志的关联字段补齐。

### Modified Capabilities

- `llm-debug-logging`: 「Trace correlation」要求由仅 `trace_id` 扩展为同时携带 ctx 中的 `session_id`（有则带、无则优雅省略，语义不变）。
- `executor-tools`: 「Audit logging」要求的审计字段增加 `trace_id` 与 `session_id`（来自调用 ctx）。

## Impact

- 新增 `internal/pkg/reqctx`（session-id key 迁移目标，叶子包）。
- `internal/pkg/logger/zap.go`：新增 `FromContext`。
- `internal/agent/rawcapture.go`：key 迁走、保留别名。
- `internal/tool/registry.go`：`Call` 增加派发日志（含 duration 计时）。
- 存量日志替换点：`internal/agent/{agent,lengthcontinue,roundcap,orchestrator,openai_client}.go`、`internal/tool/executor/runner.go`、`internal/store/redis/redis.go`、`internal/handler/`（turn 生命周期、compaction）。
- 测试：reqctx 单测、registry 派发日志单测（字段断言）、logCmd/audit 字段断言更新。
- 文档：CLAUDE.md Testing/conventions 无需结构变更；logging 相关段落补充 `FromContext` 约定。
