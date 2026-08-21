# xizhi-tools 变更规格

## MODIFIED Requirements

### Requirement: Xizhi write file
Xizhi SHALL 提供写入文件到用户工作空间的工具，只能在 data/{user_uuid}/workspace/ 下写文件。工具 SHALL 支持可选 `mode` 参数（枚举 `write` | `append`）：缺省或 `write` 保持现行创建或覆盖语义（行为与引入 `mode` 前逐字节一致）；`append` SHALL 在文件不存在时创建（含自动创建父目录）、存在时将 `content` 追加到文件末尾。结果 SHALL 返回 `{path, size, absolute, appended}`，其中 `size` 为写后文件总大小，`appended` 为本次调用写入的字节数（`write` 模式等于全文长度，`append` 模式等于追加内容长度）。

#### Scenario: Write new file
- **WHEN** Chongzhi 调用 xizhi_write_file，path 为 "src/main.go"，content 为文件内容
- **THEN** 系统在 data/{user_uuid}/workspace/src/main.go 写入内容，自动创建中间目录

#### Scenario: Overwrite existing file
- **WHEN** Chongzhi 调用 xizhi_write_file，文件已存在，mode 缺省或为 "write"
- **THEN** 系统覆盖文件内容

#### Scenario: Append to existing file
- **WHEN** Chongzhi 调用 xizhi_write_file，path 为已存在文件，content 为新内容，mode 为 "append"
- **THEN** 系统将 content 追加到文件末尾（原内容保留），返回的 `size` 为追加后总大小、`appended` 为本次追加字节数

#### Scenario: Append creates missing file
- **WHEN** Chongzhi 调用 xizhi_write_file，mode 为 "append"，文件不存在
- **THEN** 系统创建该文件（含父目录）并写入 content，行为等同首次写入

#### Scenario: Default mode preserves current behavior
- **WHEN** 调用不携带 mode 参数
- **THEN** 语义与引入 mode 前的 xizhi_write_file 完全一致（创建或覆盖），结果仅新增 `appended` 字段

#### Scenario: Write outside workspace blocked
- **WHEN** Chongzhi 调用 xizhi_write_file，解析后的绝对路径不在 workspace 目录下
- **THEN** 系统拒绝操作，返回错误 "path outside workspace"

#### Scenario: Invalid mode rejected
- **WHEN** 调用携带 mode 为 "write" 与 "append" 之外的值
- **THEN** 系统返回参数解析错误，不写文件

### Requirement: Xizhi tool descriptions declare result shape
`xizhi_read_file`、`xizhi_write_file`、`xizhi_modify_file` 的工具描述 SHALL 声明各自的结果结构与关键失败语义：`xizhi_read_file` 返回 `{path, content, size}`（全文、无行号前缀、无截断），缺失文件返回错误；`xizhi_write_file` 返回 `{path, size, absolute, appended}`，自动创建父目录，缺省覆盖既有文件、`mode: "append"` 时追加；`xizhi_modify_file` 返回 `{path, old_size, new_size}`，`old_content` 必须在文件中唯一匹配，缺失或多次出现则失败。此外，当某 agent 配置的工具列表包含 `xizhi_write_file` 或 `xizhi_modify_file` 时，系统 SHALL 在为该 agent 渲染 tools[] 时向这两个工具的描述追加写入预算引导：单次写入内容控制在约 `floor(该 agent 配置的 max_tokens × 0.7)` token 以内，内容过大时 SHALL 分块、少量多次写入（配合 `mode: "append"`）。预算数字 SHALL 按 agent 现算注入（工具注册表为进程级共享，注入发生在 agent 构造时渲染 tools[] 的那一层），静态描述 SHALL 保留分块写入的模式性建议。

#### Scenario: read/write/modify 描述声明结果结构
- **WHEN** `xizhi_read_file`、`xizhi_write_file`、`xizhi_modify_file` 工具被注册并渲染给模型
- **THEN** 各描述分别包含其结果字段（read 含 `content`/`size`；write 含 `absolute`；modify 含 `old_size`/`new_size`）

#### Scenario: modify 描述声明唯一匹配语义
- **WHEN** `xizhi_modify_file` 工具被注册并渲染给模型
- **THEN** 描述声明 `old_content` 必须在文件中唯一匹配，缺失或多次出现则失败

#### Scenario: 写入预算引导按 agent 注入
- **WHEN** Chongzhi 配置 `max_tokens: 8192` 且其工具列表包含 `xizhi_write_file`/`xizhi_modify_file`，系统渲染 Chongzhi 的 tools[]
- **THEN** 两个工具的描述包含写入预算引导语句，预算数字为 5734（`floor(8192 × 0.7)`），并引导大内容分块多次写入

#### Scenario: 无写入工具的 agent 不受影响
- **WHEN** 某 agent（如 Liang）的工具列表不包含 `xizhi_write_file`/`xizhi_modify_file`
- **THEN** 该 agent 渲染的 tools[] 描述不含写入预算引导语句

#### Scenario: 预算数字随 agent 配置区分
- **WHEN** 两个 agent 配置不同的 `max_tokens` 且都包含写入工具
- **THEN** 各自渲染的描述中的预算数字分别等于各自 `max_tokens × 0.7` 取整
