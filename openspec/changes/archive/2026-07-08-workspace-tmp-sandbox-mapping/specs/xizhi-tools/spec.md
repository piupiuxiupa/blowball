## ADDED Requirements

### Requirement: Xizhi path validation error guidance
Xizhi 路径校验失败返回的错误信息 SHALL 包含相对路径示例，引导模型使用相对路径。

#### Scenario: Absolute path rejected with guidance
- **WHEN** Agent 调用 xizhi_read_file，path 为 "/tmp/hello.txt"
- **THEN** 系统拒绝操作
- **AND** 返回的错误信息包含类似 "use a relative path such as tmp/hello.txt" 的示例

## MODIFIED Requirements

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

## REMOVED Requirements

None.
