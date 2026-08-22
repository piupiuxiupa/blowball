# mcp-call-result-spill — Design

## Context

`mcp_call` 的返回路径（`internal/tool/mcp/register.go` 的 `callTool` → `callResult{Content}` → registry `Call` → `renderToolResult` envelope → `role="tool"` 消息）今天没有任何出口大小控制。超大远端结果污染五处：当轮 LLM 上下文、SSE `tool_result` 事件、`messages` 持久化行、跨轮/跨 turn 历史回放（直到 80% compaction 才消失）、下一请求的 `llm_raw_log` `kind=request` 行。仓库已有的同型机制：`add-input-token-limit` 的 CJK 感知 token 估算（`internal/handler/tokens.go`）、`xizhi_read_file`/`xizhi_grep` 的分页读回、`xizhi_write_file` 的 write-budget steering 注入。

## Goals / Non-Goals

**Goals:**

- 超过 token 阈值的 `mcp_call` 成功结果自动全量落盘到 workspace 内，模型收到 path + preview + hint 小信封，**不需要重调远端工具**。
- 模型可经可选 `output_path` 参数主动无条件落盘。
- 落盘文件对既有 `xizhi_read_file`（分页）/ `xizhi_grep`（正则）直接可读 —— 读回侧零新代码。
- 配置镜像 `messages.max_input_tokens` 先例：`*int` 区分未设/显式 `0`，默认 20000，负数 load 拒绝。

**Non-Goals:**

- `mcp_list_tools` 超大返回、operator 全局 MCP 代理工具（同病，helper 可后续复用）。
- 远端 `isError` 超大错误体的 spill（错误路径维持现状）。
- `tmp/mcp-outputs/` 清理任务/TTL（模型可 `xizhi_delete`；前端文件浏览器可见可下载，视为 feature）。
- 精确 tokenizer（不引入 BPE 依赖）。

## Decisions

### D1: 估算器复用而非新写，估算对象是 marshal 后的 callResult JSON

复用 `estimateTokens`（CJK 每字符 1 token、其余 ceil(chars/4)）——网关模型（glm 系）无公开 BPE，先例（add-input-token-limit）已确立"启发式、不计量"的定位。估算对象 = `json.Marshal(callResult)` 后的字符串（即模型实际看到、进入上下文的准确形态），而不是原始 text 长度——信封 JSON 化的开销也计入。

提取位置：`internal/handler/tokens.go` → 新共享包 `internal/tokens`（导出 `Estimate(s string) int`），`internal/handler` 改为薄引用，行为零变化。理由：`internal/tool/mcp` import `internal/handler` 是错误依赖方向（handler 是 HTTP 层）。

精度注记：启发式**高估** CJK、**低估**密集 JSON（真实 BPE 常 ~3 chars/token）。阈值（20000）与窗口（128K 级）间有量级裕度，误差被裕度吸收；沿用 "conservative heuristic, never exact" 措辞。

### D2: 自动 spill 为主、`output_path` 主动参数为辅 —— 永不要求重调

三个候选：仅参数（模型须预知结果大，意外超大时已进上下文，补救=重调 71s ❌）；自动+参数（✅）；仅截断（丢数据，重调才能看全量 ❌）。自动判定兜住意外，`output_path` 给预知大结果的模型省一次"撞阈值"，同时是模型控制落盘路径/文件名的口子。

### D3: 阈值 `tools.user_mcp.max_inline_result_tokens`，默认 20000

`UserMCPConfig` 增加 `MaxInlineResultTokens *int`；nil → 20000，显式 `0` → 关闭（byte-for-byte 现状），负数 load 时拒绝（yaml 分数截断不必 shadow-check——整数语义同 `max_input_tokens`，但沿用其 `*int` 手法）。接线沿 `tools.user_mcp` 现有路径：serve → per-turn `ManagerOptions` → `Manager`/`Tools`。20000 的量级依据：约为 128K 窗口的 15%，单次工具调用占用的合理上限。

### D4: 落盘位置 `{workspace}/tmp/mcp-outputs/{server}/{tool}-{trace_id}-{seq}.json`

- **必须在 workspace 内**：xizhi 工具作用域是 workspace root；系统 `/tmp` 或 `.blowball/`（被 `ValidatePath` 屏蔽）都会使模型读不回，闭环自废。
- **文件名唯一性跨 turn**：`seq` = Manager（turn-scoped）上的原子计数器，加 ctx 里的 `trace_id`（每 turn 唯一）保证跨 turn 不碰撞——历史上下文里引用的 path 不会被后续 turn 静默覆盖。`trace_id` 缺失时退化为纳秒时间戳（防御，正常路径不会发生）。
- **内容格式**：单个 `type:"text"` content item → 写原始文本（`.txt`，grep/分页最友好）；多个 item 或含非 text item → 写保结构的 JSON 数组（`.json`）。`mcp_call` 走到 spill 的一律是成功结果（`isError` 见 Non-Goals）。
- **写语义**：自动 spill = create（唯一名，不覆盖）；`output_path` 指定 = create-or-overwrite（对齐 `xizhi_write_file` write 模式）。父目录 `MkdirAll`。

### D5: `output_path` 的路径约束与 xizhi 同规

`output_path` 是模型可控输入，必须做与 `xizhi` 一致的 confinement：拒绝绝对路径、`..`、symlink escape、`.blowball` 前缀，清理后限制在 workspace 内。优先**导出复用** xizhi 的校验 helper（避免两份规则漂移）；`internal/tool/mcp` → `internal/tool/xizhi` 的包依赖方向无环、可接受。校验失败 = 工具错误（status 1 envelope），不发起远端调用（参数错误在 call 前拒绝，与现有 "args validated before call" 语义一致）。

### D6: 信封形状（riding 在既有 `renderToolResult` envelope 内）

```json
{"status":0,"result":{
  "spilled":true,
  "path":"tmp/mcp-outputs/gildata/query_stock-<trace>-1.json",
  "tokens_est":41230,
  "size":1834222,
  "items":1,
  "preview":"<头 1KB，rune 安全截取>",
  "hint":"Result too large for inline context; full output saved to `path`. Inspect with xizhi_read_file (paginated) or xizhi_grep."}}
```

- `toolEnvelope` 外壳不变（`tool-result-envelope` 能力无需求变更）；SSE `tool_result`、`messages` 行、历史回放随信封自动变小。
- preview 是普通字符串，可能是残缺 JSON 片段（仅头 1KB）——字段名已表达语义。
- 阈值内 + 未指定 `output_path` → 返回形状与今天完全一致（零变化路径）。

### D7: 落盘失败降级 = 截断 inline + 显式注记

写失败（磁盘满/只读）时返回 `{truncated:true, size, preview:"头 1KB", note:"full output (N bytes) could not be saved to file: <err>"}`（status 0 —— 远端调用本身成功）。绝不回退为返回全量（那会击穿本变更要保护的上下文）。

### D8: steering 句注入 `mcp_call` description

先例：write-budget 句子注入 `xizhi_write_file` 描述。新增一句说明：超大结果会自动落盘并返回 path（用 xizhi_read_file/xizhi_grep 读回）；预知大结果时传 `output_path` 主动落盘。schema 同步加 `output_path` property（`additionalProperties: false` 下必须显式声明；不传 = 现状）。

### D9: spill 判定与写入的位置

在 `callTool` 返回前完成（`callResult` 构造集中一处）：估算 → 判定 → 写文件 → 构造信封。文件写入是本地小 IO，不占 `totalCtx` 超时预算（`callTool` 内 `defer cancel` 在返回后才触发，写入发生在返回前但用父 ctx/无 ctx 均可——本地写不需要超时保护）。

## Risks / Trade-offs

- [估算低估密集 JSON，阈值边缘结果仍偏大] → 阈值-窗口间量级裕度吸收；操作员可下调阈值；文档明示 heuristic。
- [`tmp/mcp-outputs/` 无限累积] → Non-goal；模型可 `xizhi_delete`，用户可在文件浏览器删除/下载；后续可加清理任务。
- [`output_path` 被模型指向敏感路径] → D5 与 xizhi 同规 confinement + `.blowball` 屏蔽 + workspace 作用域，攻击面等同 `xizhi_write_file`（既有接受面）。
- [前端渲染新信封形态] → 前端对 tool_result 按普通 JSON 渲染，无硬契约；spill 后事件反而更小。
- [默认开启改变既有部署行为（大结果从 inline 变信封）] → 这是变更目的本身；回退 = 配置 `0`。
- [跨 turn path 引用失效（用户手动删文件）] → 与模型引用任何 workspace 文件同风险，非新增。

## Migration Plan

无 schema 迁移、无数据迁移。部署即生效（默认 20000）；回退 = `tools.user_mcp.max_inline_result_tokens: 0`（byte-for-byte 现状）。

## Open Questions

无 —— 阈值默认值（20000）、主动参数、preview 1KB、isError 缓期、scope 只做 `mcp_call` 均已在探索阶段拍板。
