## ADDED Requirements

### Requirement: Xizhi tool registry
系统 SHALL 提供工具注册表，根据 Agent 配置中的 tools 列表动态构建 OpenAI function calling 的 tools 参数。

#### Scenario: Build tools for agent
- **WHEN** Agent 需要调用 OpenAI API
- **THEN** 系统根据 Agent 配置的 tools 列表，从注册表中查找对应的 tool definition，构造 tools 参数

#### Scenario: Tool not found in registry
- **WHEN** Agent 配置引用了不存在的 tool name
- **THEN** 服务启动时报错并拒绝启动

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
服务进程 SHALL 通过 go-landlock 限制文件写操作范围为 data/ 目录。

#### Scenario: Landlock applied on startup
- **WHEN** 服务启动
- **THEN** 系统应用 go-landlock 规则，限制进程只能读写 data/ 目录下的文件

#### Scenario: Write outside data dir blocked by landlock
- **WHEN** 任何代码尝试写入 data/ 目录以外的位置
- **THEN** 操作系统级别拒绝，返回 permission denied 错误

### Requirement: Application-level path validation
Xizhi 的每个工具调用 SHALL 在应用层验证路径前缀，确保操作在用户 workspace 内。

#### Scenario: Path traversal attack blocked
- **WHEN** 请求路径包含 ".." 或符号链接指向 workspace 外
- **THEN** 系统解析绝对路径后验证前缀，拒绝越界操作

#### Scenario: Symlink escape blocked
- **WHEN** workspace 内存在符号链接指向外部目录
- **THEN** 系统使用 filepath.EvalSymlinks 解析真实路径后验证前缀
