# mcp-call-result-spill — Delta Spec

## ADDED Requirements

### Requirement: Token estimation of mcp_call results

系统 SHALL 在 `mcp_call` 返回成功结果前，对 marshal 后的 `callResult` JSON（即进入模型上下文的准确形态）做 token 数估算。估算器 SHALL 复用 `add-input-token-limit` 的 CJK 感知启发式（CJK 每字符 1 token、其余 ceil(chars/4)），提取到共享包供 handler 与 tool 层共同引用，且不引入 tokenizer 依赖。估算精度为启发式而非精确计量：高估 CJK、低估密集 JSON，由阈值与模型窗口间的裕度吸收。

#### Scenario: Estimation covers the full model-visible payload
- **WHEN** `mcp_call` 远端调用成功且配置了正的 inline 阈值
- **THEN** 系统对 marshal 后的完整 `callResult` JSON（含信封化开销）估算 token 数，用于阈值判定

#### Scenario: Shared estimator package preserves input-limit behavior
- **WHEN** 估算器从 `internal/handler/tokens.go` 提取到共享包后
- **THEN** `messages.max_input_tokens` 的既有估算行为 byte-for-byte 不变（同一实现、两个调用点）

### Requirement: Configurable inline threshold with default

`tools.user_mcp.max_inline_result_tokens` SHALL 控制 `mcp_call` 结果的 inline 上限：未设置 → 默认 **20000**；显式 `0` → 关闭**自动** spill（未指定 `output_path` 的调用结果恒 inline，byte-for-byte 现状；显式 `output_path` 的无条件落盘不受影响）；负值 → config load 时拒绝。配置 SHALL 以 `*int` 承载以区分未设与显式 `0`（镜像 `messages.max_input_tokens` 先例），并沿 `tools.user_mcp` 既有接线传入 per-turn MCP manager。

#### Scenario: Default threshold applies when unset
- **WHEN** 配置未设置 `max_inline_result_tokens` 且某成功结果的估算 token 数 ≤ 20000
- **THEN** 结果原样 inline 返回（与现状一致）

#### Scenario: Explicit zero disables the automatic spill
- **WHEN** 配置显式设置 `max_inline_result_tokens: 0` 且调用未指定 `output_path`
- **THEN** 任意大小的成功结果都原样 inline 返回，不落盘、不产生信封

#### Scenario: Negative value rejected at load
- **WHEN** 配置设置 `max_inline_result_tokens` 为负数
- **THEN** config load 失败并给出明确错误

### Requirement: Automatic spill of oversized results

当成功结果的估算 token 数超过阈值时，系统 SHALL 将全量结果自动落盘并返回小信封，而不是把全量内容返回给模型。落盘位置 SHALL 在请求用户 workspace 内的 `tmp/mcp-outputs/{server}/` 下（不得使用 `.blowball/` 命名空间或 workspace 外路径——xizhi 读回作用域要求）。自动落盘文件名 SHALL 跨 turn 唯一（`{tool}-{trace_id}-{seq}`，seq 为 turn 内原子计数器；trace_id 缺失时退化为时间戳），写语义为 create（不覆盖既有文件）。文件内容格式：单个 `type:"text"` content item → 原始文本；多个 item 或含非 text item → 保结构的 JSON 数组。

#### Scenario: Oversized result spilled with envelope
- **WHEN** 成功结果的估算 token 数超过阈值且未指定 `output_path`
- **THEN** 全量内容写入 `tmp/mcp-outputs/{server}/{tool}-{trace_id}-{seq}` 文件，模型收到含 `spilled:true`、`path`、`tokens_est`、`size`、`items`、`preview`、`hint` 的信封，全量内容不进入工具返回

#### Scenario: Parallel spills in one round get distinct files
- **WHEN** 同一 round 内并发的两个 `mcp_call` 都触发自动落盘
- **THEN** 两次落盘写入不同文件（seq 递增），互不覆盖

#### Scenario: Cross-turn path references stay stable
- **WHEN** 某 turn 落盘的文件路径出现在历史上下文中，后续 turn 再次触发同名工具的落盘
- **THEN** 新落盘使用新的 trace_id/seq 命名，既有文件不被覆盖

#### Scenario: Single text item written as raw text
- **WHEN** 被落盘的结果是单个 `type:"text"` content item
- **THEN** 文件内容为该 text 的原始文本（无 JSON 包裹），对 `xizhi_grep`/`xizhi_read_file` 直接可读

### Requirement: Spill envelope shape

自动落盘与 `output_path` 主动落盘的返回信封 SHALL riding 在既有工具结果 envelope（`{"status":0,"result":…}`）内，形如 `{spilled:true, path, tokens_est, size, items, preview, hint}`：`preview` 为全量内容的头 1KB（rune 安全截取，允许是残缺片段）；`hint` SHALL 引导模型用 `xizhi_read_file`（分页）或 `xizhi_grep`（正则检索）读回文件。阈值内且未指定 `output_path` 的结果 SHALL 保持与现状完全一致的返回形状。

#### Scenario: Envelope carries path and read-back guidance
- **WHEN** 任一落盘路径触发（自动或主动）
- **THEN** 返回信封包含落盘文件的 workspace 相对 `path` 与引导读回的 `hint`，外层 envelope 结构不变

#### Scenario: Under-threshold result shape unchanged
- **WHEN** 成功结果的估算 token 数未超阈值且未指定 `output_path`
- **THEN** 返回形状与 spill 机制引入前完全一致（无新增字段）

### Requirement: Proactive output_path parameter

`mcp_call` SHALL 提供可选参数 `output_path`（workspace 相对路径）：指定时无论结果大小**无条件**落盘到该路径（create-or-overwrite，对齐 `xizhi_write_file` 的 write 模式语义），并返回 spill 信封。`output_path` SHALL 受与 xizhi 一致的路径约束：拒绝绝对路径、`..`、symlink escape、`.blowball` 前缀，清理后必须落在 workspace 内；校验失败 SHALL 在发起远端调用前拒绝（与既有"参数错误 call 前拒绝"语义一致）。

#### Scenario: Proactive spill regardless of size
- **WHEN** 模型调用 `mcp_call` 时传入合法 `output_path` 且远端调用成功
- **THEN** 全量结果无条件写入该路径（已存在则覆盖），返回 spill 信封，即使结果远小于阈值

#### Scenario: Invalid output_path rejected before remote call
- **WHEN** `output_path` 是绝对路径、含 `..`、指向 `.blowball/` 或逃逸 workspace
- **THEN** 系统在发起远端调用前返回明确错误，不产生任何远端副作用

#### Scenario: Parent directories auto-created
- **WHEN** `output_path` 指向的父目录不存在
- **THEN** 系统自动创建父目录后完成写入

### Requirement: Spill write failure degradation

落盘写失败（磁盘满、只读等）时，系统 SHALL 降级为返回截断的 inline 结果（头 1KB preview + `truncated:true` + 显式注记说明全量大小与失败原因），SHALL NOT 回退为返回全量内容。远端调用本身成功的事实保持 status 0。

#### Scenario: Disk-full degrades to truncated inline
- **WHEN** 自动落盘写文件失败
- **THEN** 返回 `{truncated:true, size, preview, note}` 形态的截断结果并注明落盘失败原因，全量内容不返回给模型

### Requirement: mcp_call description steering

`mcp_call` 的工具 description SHALL 包含 steering 句子，说明：超大结果会自动落盘并返回 path（用 `xizhi_read_file`/`xizhi_grep` 读回）；预知大结果时可传 `output_path` 主动落盘。工具参数 schema SHALL 显式声明 `output_path` property。

#### Scenario: Schema declares the new parameter
- **WHEN** `mcp_call` 的 `tools[]` schema 渲染给模型
- **THEN** `output_path` property 存在且带用途说明，`additionalProperties: false` 下不传该参数的行为与现状一致
