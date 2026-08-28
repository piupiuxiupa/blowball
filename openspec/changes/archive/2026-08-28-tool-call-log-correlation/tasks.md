# Tasks: tool-call-log-correlation

## 1. 基础设施：共享 ctx key + FromContext

- [x] 1.1 新建 `internal/pkg/reqctx` 叶子包：迁入 session-id 的私有 key 类型、`WithSessionID`、`SessionIDFromContext`（语义与 agent 现版一致：空 id 返回原 ctx）
- [x] 1.2 `internal/agent/rawcapture.go`：删除本地 key 定义，`WithSessionID`/`SessionIDFromContext` 改为对 `reqctx` 的委托别名（`WithAgentName` 留在 agent 包不动）；确认 `MessageStreamHandler`/`TitleService` 注入点零改动
- [x] 1.3 `internal/pkg/logger/zap.go`：新增 `FromContext(ctx) *zap.Logger`（session_id、trace_id 依次附加、空值跳过；ctx 为 nil 返回 `L()`），更新包注释「per-request 字段经 FromContext 附加」的约定表述

## 2. Registry.Call 中央派发日志

- [x] 2.1 `internal/tool/registry.go` 的 `Call`：计时（`time.Since`，覆盖含超时包装的完整执行）+ 结束后经 `logger.FromContext(ctx)` 输出 —— 成功 INFO（`event=tool_call`/`tool`/`duration`/`args_preview` 截断 ~200B），失败 WARN（附 `zap.Error`）；不记录结果体
- [x] 2.2 预览截断辅助：参考 `openai_client.go` 的 `truncatePreview` 风格在 registry 包内实现（避免 import agent）

## 3. 存量日志补齐（机械替换为 FromContext）

- [x] 3.1 `internal/agent`：`agent.go:300`、`lengthcontinue.go:134`、`roundcap.go:96`、`orchestrator.go:295/524`、`openai_client.go` 五处（LLM request/response 的 `logLLMRequest`/`logLLMResponse` 与 raw-capture marshal WARN、frame-budget WARN、idle-timeout WARN）
- [x] 3.2 `internal/tool/executor/runner.go`：`logAudit` 与 `logDangerousCommand` 改用 `FromContext`（保留既有字段，新增双 id）
- [x] 3.3 `internal/store/redis/redis.go` `logCmd`：改用 `FromContext`（保留 DEBUG 级别，顺带获得 session_id）
- [x] 3.4 `internal/handler`：turn 生命周期（message_stream、turn_run）与 compaction 相关 WARN 换 `FromContext`（仅持有请求 ctx 的日志点）

## 4. 测试

- [x] 4.1 `internal/pkg/reqctx` 单测：注入/读取往返、空 id 不写入、nil ctx 安全
- [x] 4.2 `internal/pkg/logger` 单测：`FromContext` 双 id / 单 id / 无 id 三态的字段断言（用 zap test observer）
- [x] 4.3 `internal/tool/registry` 单测：成功 INFO / 失败 WARN 的字段断言（tool、duration 存在、args_preview 截断、error 有无）、超时路径同样产日志
- [x] 4.4 既有测试修正：断言日志字段集合的用例（executor audit、logCmd、llm-debug 相关）补新字段
- [x] 4.5 全量回归：`make test`（含 `test/integration/`）通过

## 5. 文档

- [x] 5.1 CLAUDE.md：在合适段落（Testing/重要约定）补一条「turn 链路日志经 `logger.FromContext` 携带 `session_id`+`trace_id`；`Registry.Call` 输出中央工具派发日志（成功 INFO/失败 WARN）」的约定；`internal/pkg/reqctx` 在 agent orchestration 一带的 ctx 描述如有提及则同步
