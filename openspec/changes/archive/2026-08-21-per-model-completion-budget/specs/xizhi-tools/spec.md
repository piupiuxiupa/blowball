# Delta: xizhi-tools

## MODIFIED Requirements

### Requirement: Xizhi tool descriptions declare result shape
`xizhi_read_file`、`xizhi_write_file`、`xizhi_modify_file` 的工具描述 SHALL 声明各自的结果结构与关键失败语义：`xizhi_read_file` 返回 `{path, content, size}`（全文、无行号前缀、无截断），缺失文件返回错误；`xizhi_write_file` 返回 `{path, size, absolute, appended}`，自动创建父目录，缺省覆盖既有文件、`mode: "append"` 时追加；`xizhi_modify_file` 返回 `{path, old_size, new_size}`，`old_content` 必须在文件中唯一匹配，缺失或多次出现则失败。此外，当某 agent 配置的工具列表包含 `xizhi_write_file` 或 `xizhi_modify_file` 时，系统 SHALL 在为该 agent 渲染 tools[] 时向这两个工具的描述追加写入预算引导：单次写入内容控制在约 `floor(该 turn 解析目录条目的 max_completion_tokens × 0.7)` token 以内，内容过大时 SHALL 分块、少量多次写入（配合 `mode: "append"`）。预算数字 SHALL 按 turn 解析结果现算注入（工具注册表为进程级共享，注入发生在 per-turn 渲染 tools[] 的那一层），静态描述 SHALL 保留分块写入的模式性建议。

#### Scenario: read/write/modify 描述声明结果结构
- **WHEN** `xizhi_read_file`、`xizhi_write_file`、`xizhi_modify_file` 工具被注册并渲染给模型
- **THEN** 各描述分别包含其结果字段（read 含 `content`/`size`；write 含 `absolute`；modify 含 `old_size`/`new_size`）

#### Scenario: modify 描述声明唯一匹配语义
- **WHEN** `xizhi_modify_file` 工具被注册并渲染给模型
- **THEN** 描述声明 `old_content` 必须在文件中唯一匹配，缺失或多次出现则失败

#### Scenario: 写入预算引导按解析条目注入
- **WHEN** turn 解析的目录条目 `max_completion_tokens: 8192`，Chongzhi 的工具列表包含 `xizhi_write_file`/`xizhi_modify_file`，系统渲染 Chongzhi 的 tools[]
- **THEN** 两个工具的描述包含写入预算引导语句，预算数字为 5734（`floor(8192 × 0.7)`），并引导大内容分块多次写入

#### Scenario: 无写入工具的 agent 不受影响
- **WHEN** 某 agent（如 Liang）的工具列表不包含 `xizhi_write_file`/`xizhi_modify_file`
- **THEN** 该 agent 渲染的 tools[] 描述不含写入预算引导语句

#### Scenario: 预算数字随解析条目区分
- **WHEN** 同一 agent 在两个 turn 分别选择 `max_completion_tokens` 为 8192 与 4096 的条目，且其工具列表包含写入工具
- **THEN** 两个 turn 渲染的描述中的预算数字分别等于各自条目配额 × 0.7 取整（5734 与 2867）
