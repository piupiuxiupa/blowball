## ADDED Requirements

### Requirement: Xizhi tool descriptions declare result shape
`xizhi_read_file`、`xizhi_write_file`、`xizhi_modify_file` 的工具描述 SHALL 声明各自的结果结构与关键失败语义：`xizhi_read_file` 返回 `{path, content, size}`（全文、无行号前缀、无截断），缺失文件返回错误；`xizhi_write_file` 返回 `{path, size, absolute}`，自动创建父目录并覆盖既有文件；`xizhi_modify_file` 返回 `{path, old_size, new_size}`，`old_content` 必须在文件中唯一匹配，缺失或多次出现则失败。

#### Scenario: read/write/modify 描述声明结果结构
- **WHEN** `xizhi_read_file`、`xizhi_write_file`、`xizhi_modify_file` 工具被注册并渲染给模型
- **THEN** 各描述分别包含其结果字段（read 含 `content`/`size`；write 含 `absolute`；modify 含 `old_size`/`new_size`）

#### Scenario: modify 描述声明唯一匹配语义
- **WHEN** `xizhi_modify_file` 工具被注册并渲染给模型
- **THEN** 描述声明 `old_content` 必须在文件中唯一匹配，缺失或多次出现则失败
