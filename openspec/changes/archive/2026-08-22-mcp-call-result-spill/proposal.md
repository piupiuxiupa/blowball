# mcp-call-result-spill

## Why

`mcp_call` 是唯一返回内容完全没有出口限制的工具路径：远端 `tools/call` 的超大结果今天原样进入当轮 LLM 上下文、SSE `tool_result` 事件、`messages` 持久化行，并在之后每一轮、每一个 turn 被回放（直到 80% 阈值触发 compaction 才消失）。单个超过模型窗口的结果（如 500KB ≈ 十万级 token）会让该轮当场不可恢复；慢服务器（GilData 单次 `tools/call` 71s+）使"发现过大后重调一次"的补救代价高到不可接受。

## What Changes

- `mcp_call` 在返回前对结果做 **token 估算**：复用 `add-input-token-limit` 的 CJK 感知启发式估算器（从 `internal/handler/tokens.go` 提取到共享包，保持依赖方向正确），估算对象是 marshal 后、模型实际会看到的 `callResult` JSON。
- 新增配置 `tools.user_mcp.max_inline_result_tokens`：默认 **20000**，`*int` 区分未设/显式 `0`（镜像 `messages.max_input_tokens` 先例），`0` = 关闭，负数在 config load 时拒绝。
- 超过阈值时**自动落盘**：全量内容写入 `{workspace}/tmp/mcp-outputs/{server}/{tool}-{seq}.json`（必须在 workspace 内 —— xizhi 读回作用域要求；不能是 `.blowball/` 或系统 `/tmp`），返回小信封 `{spilled, path, tokens_est, size, preview(头 1KB), hint}` 引导模型用 `xizhi_read_file`（已有分页）/ `xizhi_grep`（已有正则检索）读回 —— **读回侧零新代码**。
- `mcp_call` 新增可选参数 `output_path`（workspace 相对路径）：指定即**无条件**落盘到该路径（覆盖语义对齐 `xizhi_write_file` 的 write 模式），供模型预知大结果时主动控制。
- `mcp_call` description 增加 steering 句子（先例：write-budget 注入 `xizhi_write_file` 描述）。
- 落盘写失败降级为**截断 inline + 显式注记**（不返回全量）。
- 二阶收益：spill 后 `messages` 行与跨 turn 回放同步变小。

**Out of scope（本次不做）**：`mcp_list_tools` 的超大返回；operator 全局 MCP 代理工具（`internal/tool/mcpclient/register.go`，同病，helper 可后续复用）；远端 `isError` 超大错误体；`tmp/mcp-outputs/` 的清理任务（模型可 `xizhi_delete`，文件在前端文件浏览器可见可下载，视为 feature）。

## Capabilities

### New Capabilities

- `mcp-call-result-spill`: `mcp_call` 返回结果的 token 估算、阈值判定、超大结果自动落盘（位置/命名/内容格式）、小信封返回形状、主动 `output_path` 参数、落盘失败降级、配置项 `tools.user_mcp.max_inline_result_tokens`。

### Modified Capabilities

- `user-mcp-configuration`: "On-demand MCP invocation tool" 需求变更 —— `mcp_call` 契约扩展：新增可选 `output_path` 参数；远端成功结果的返回不再是"原样返回 agent"的单一形态，超大结果以落盘信封（path + preview + hint）形态返回。

## Impact

- `internal/tool/mcp/register.go` — `mcp_call` 的 schema（新参数）、description（steering 句）、`callTool` 返回路径接入 spill 判定。
- `internal/tool/mcp/` 新增 spill 落盘实现（文件写入、命名唯一性、preview 截取）。
- `internal/handler/tokens.go` → 提取到共享包（如 `internal/tokens`），`internal/handler` 改为引用 —— 行为零变化。
- `internal/config/config.go` — `UserMCPConfig` 增加 `max_inline_result_tokens`（`*int`），load 校验（负数拒绝）。
- `config.example.yaml` — 注释示例块。
- `cmd/blowball/serve.go` — 将配置值传入 per-turn `mcp.Manager`/`Tools` 构造（沿 `tools.user_mcp` 现有接线）。
- 模型面契约：`mcp_call` 的 `result` 载荷新增 spill 信封形态（riding 在既有 `{"status":0,"result":…}` envelope 内，envelope 本身不变，`tool-result-envelope` 能力无需求变更）。
- 前端：`tool_result` 事件变小；spill 信封按普通 JSON 渲染即可，无硬契约破坏。
- `llm_raw_log`：下一请求的 `kind=request` 不再携带超大工具结果（行变小，无结构变化）。
