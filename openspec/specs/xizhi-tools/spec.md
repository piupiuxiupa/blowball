# xizhi-tools Specification

## Purpose

定义 Xizhi 工具集能力，包括工具注册表、文件读/写/改工具，以及通过 go-landlock 实现的进程级文件沙箱与应用层路径前缀校验。

## Requirements

### Requirement: Xizhi tool registry
系统 SHALL 提供工具注册表，根据 Agent 配置中的 tools 列表动态构建 OpenAI function calling 的 tools 参数。

#### Scenario: Build tools for agent
- **WHEN** Agent 需要调用 OpenAI API
- **THEN** 系统根据 Agent 配置的 tools 列表，从注册表中查找对应的 tool definition，构造 tools 参数；注册表中除包含 read/write/modify 外，还包含 list_files、tree、glob_files 等 workspace-scoped 工具

#### Scenario: Tool not found in registry
- **WHEN** Agent 配置引用了不存在的 tool name
- **THEN** 服务启动时报错并拒绝启动

### Requirement: Xizhi tool configuration
系统 SHALL 在 `tools.xizhi` 配置下为每个 Xizhi 工具提供独立的 `enabled` 开关。

#### Scenario: Enable list files tool
- **WHEN** 配置中 `tools.xizhi.list_files.enabled` 为 true
- **THEN** 系统将 `xizhi_list_files` 注册到工具注册表，可被 Agent 使用

#### Scenario: Enable tree tool
- **WHEN** 配置中 `tools.xizhi.tree.enabled` 为 true
- **THEN** 系统将 `xizhi_tree` 注册到工具注册表，可被 Agent 使用

#### Scenario: Enable glob files tool
- **WHEN** 配置中 `tools.xizhi.glob_files.enabled` 为 true
- **THEN** 系统将 `xizhi_glob_files` 注册到工具注册表，可被 Agent 使用

### Requirement: Xizhi read file
Xizhi SHALL 提供读取用户工作空间文件的工具，只能读取 data/{user_uuid}/workspace/ 下的文件。

#### Scenario: Read existing file
- **WHEN** Chongzhi 调用 xizhi_read_file，path 为 "src/main.go"
- **THEN** 系统读取 data/{user_uuid}/workspace/src/main.go 的内容，作为 tool response 返回

#### Scenario: Read file outside workspace
- **WHEN** Chongzhi 调用 xizhi_read_file，path 为 "../../etc/passwd"
- **THEN** 系统拒绝操作，返回错误 "path outside workspace"

#### Scenario: Read non-existent file
- **WHEN** Chongzhi 调用 xizhi_read_file，文件不存在
- **THEN** 系统返回错误 "file not found"

### Requirement: Xizhi write file
Xizhi SHALL 提供写入文件到用户工作空间的工具，只能在 data/{user_uuid}/workspace/ 下创建或覆盖文件。

#### Scenario: Write new file
- **WHEN** Chongzhi 调用 xizhi_write_file，path 为 "src/main.go"，content 为文件内容
- **THEN** 系统在 data/{user_uuid}/workspace/src/main.go 写入内容，自动创建中间目录

#### Scenario: Overwrite existing file
- **WHEN** Chongzhi 调用 xizhi_write_file，文件已存在
- **THEN** 系统覆盖文件内容

#### Scenario: Write outside workspace blocked
- **WHEN** Chongzhi 调用 xizhi_write_file，解析后的绝对路径不在 workspace 目录下
- **THEN** 系统拒绝操作，返回错误 "path outside workspace"

### Requirement: Xizhi modify file
Xizhi SHALL 提供修改已有文件部分内容的工具，通过 old_content/new_content 替换。

#### Scenario: Modify file with matching content
- **WHEN** Chongzhi 调用 xizhi_modify_file，old_content 在文件中存在且唯一
- **THEN** 系统将 old_content 替换为 new_content

#### Scenario: Old content not found
- **WHEN** Chongzhi 调用 xizhi_modify_file，old_content 在文件中不存在
- **THEN** 系统返回错误 "old content not found"

#### Scenario: Old content matches multiple locations
- **WHEN** Chongzhi 调用 xizhi_modify_file，old_content 在文件中出现多次
- **THEN** 系统返回错误 "old content is ambiguous, found multiple matches"

### Requirement: Landlock process-level restriction
服务进程 SHALL 通过 go-landlock 在启动时限制文件访问范围。限制的目录来源于 `sandbox-directory-configuration` 配置（见 `sandbox-directory-configuration` 规格）：RW 目录为派生的运行时子目录 `{data-dir}/data`、`{data-dir}/logs`、`{data-dir}/skills` 并附加 `landlock.extra_read_write`；RO 目录为 `{data-dir}/tools` 并附加 `landlock.extra_read_only`；系统只读基线（默认 `/etc`、`/usr`、`/bin`、`/lib`、`/lib64`、`/proc`）来自 `landlock.system_read_only`，且经 `os.Stat` 守卫——缺失条目被跳过并记录告警。未配置任何字段时行为与配置化之前一致（防御性地与沙箱内 `--ro-bind` 并行）。

#### Scenario: Landlock applied on startup
- **WHEN** 服务启动且 `landlock.enabled` 未显式关闭
- **THEN** 系统应用 go-landlock 规则，进程只能读写 `{data-dir}/data`、`{data-dir}/logs`、`{data-dir}/skills`（及配置的 `extra_read_write`）下的文件，且只能只读访问 `{data-dir}/tools`（及配置的 `extra_read_only`）

#### Scenario: Write outside runtime dirs blocked by landlock
- **WHEN** 任何代码尝试写入运行时子目录与配置额外可写目录以外的位置
- **THEN** 操作系统级别拒绝，返回 permission denied 错误

#### Scenario: Tools directory is read-only under landlock
- **WHEN** 服务进程在 Landlock 应用后尝试写入 `{data-dir}/tools` 下的文件
- **THEN** 操作被拒绝（只读限制），返回 permission denied 错误
- **AND** 读取 `{data-dir}/tools` 下的文件仍然成功

#### Scenario: 缺失系统基线目录被跳过
- **WHEN** `landlock.system_read_only` 中的某目录在运行环境不存在（如 `/lib64` 在 aarch64）
- **THEN** 系统跳过对该目录的限制并记录告警，其余基线目录照常以只读放行

#### Scenario: 显式禁用 Landlock
- **WHEN** `landlock.enabled` 配置为 `false`
- **THEN** 系统不应用 go-landlock 规则，仅记录告警并依赖应用层路径校验

### Requirement: Xizhi path validation error guidance
Xizhi 路径校验失败返回的错误信息 SHALL 包含相对路径示例，引导模型使用相对路径。

#### Scenario: Absolute path rejected with guidance
- **WHEN** Agent 调用 xizhi_read_file，path 为 "/tmp/hello.txt"
- **THEN** 系统拒绝操作
- **AND** 返回的错误信息包含类似 "use a relative path such as tmp/hello.txt" 的示例

### Requirement: Application-level path validation
Xizhi 的每个工具调用 SHALL 在应用层验证路径前缀，确保操作在用户 workspace 内；校验失败时 SHALL 返回包含相对路径示例的错误信息，帮助模型自校正。

#### Scenario: Path traversal attack blocked
- **WHEN** 请求路径包含 ".." 或符号链接指向 workspace 外
- **THEN** 系统解析绝对路径后验证前缀，拒绝越界操作
- **AND** 返回的错误信息提示使用相对路径，例如 "use a relative path such as src/main.go"

#### Scenario: Symlink escape blocked
- **WHEN** workspace 内存在符号链接指向外部目录
- **THEN** 系统使用 filepath.EvalSymlinks 解析真实路径后验证前缀
- **AND** 返回的错误信息提示使用相对路径，例如 "use a relative path such as src/main.go"

### Requirement: Reserved workspace-internal directories are rejected
`xizhi_*` path validation SHALL reject any path whose first cleaned segment is a reserved application namespace directory (`.blowball`), so that workspace-resident application state — including per-user skills at `.blowball/skills/` — is reachable only through its dedicated tools (`luban_*`) and never through the file tools. The rejection SHALL use the same outside-workspace error style with relative-path guidance.

#### Scenario: Read under reserved directory blocked
- **WHEN** the agent calls `xizhi_read_file` with path `.blowball/skills/foo/SKILL.md`
- **THEN** the system rejects the operation with a path error
- **AND** the error guides the model to use `luban_*` tools for skills

#### Scenario: Write under reserved directory blocked
- **WHEN** the agent calls `xizhi_write_file` with path `.blowball/skills/foo/SKILL.md`
- **THEN** the system rejects the operation with a path error

#### Scenario: Non-reserved dotfiles remain allowed
- **WHEN** the agent calls `xizhi_read_file` with path `.env`
- **THEN** the system reads the file normally, because `.env` is not a reserved namespace directory

### Requirement: Xizhi tool descriptions declare result shape
`xizhi_read_file`、`xizhi_write_file`、`xizhi_modify_file` 的工具描述 SHALL 声明各自的结果结构与关键失败语义：`xizhi_read_file` 返回 `{path, content, size}`（全文、无行号前缀、无截断），缺失文件返回错误；`xizhi_write_file` 返回 `{path, size, absolute}`，自动创建父目录并覆盖既有文件；`xizhi_modify_file` 返回 `{path, old_size, new_size}`，`old_content` 必须在文件中唯一匹配，缺失或多次出现则失败。

#### Scenario: read/write/modify 描述声明结果结构
- **WHEN** `xizhi_read_file`、`xizhi_write_file`、`xizhi_modify_file` 工具被注册并渲染给模型
- **THEN** 各描述分别包含其结果字段（read 含 `content`/`size`；write 含 `absolute`；modify 含 `old_size`/`new_size`）

#### Scenario: modify 描述声明唯一匹配语义
- **WHEN** `xizhi_modify_file` 工具被注册并渲染给模型
- **THEN** 描述声明 `old_content` 必须在文件中唯一匹配，缺失或多次出现则失败
