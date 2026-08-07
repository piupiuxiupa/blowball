## ADDED Requirements

### Requirement: xizhi_grep tool registration
系统 SHALL 提供一个名为 `xizhi_grep` 的 Xizhi 工作空间文件工具，受 `tools.xizhi.grep.enabled` 开关控制；启用时注册到工具注册表，可被配置了该工具的 Agent 使用。其注册模式 SHALL 与 `xizhi_list_files`/`xizhi_tree`/`xizhi_glob_files`/`xizhi_delete` 一致（按 `tools.xizhi.<tool>.enabled` 条件注册，per-request 绑定到调用者工作空间根）。

#### Scenario: Enable grep tool
- **WHEN** 配置中 `tools.xizhi.grep.enabled` 为 true
- **THEN** 系统将 `xizhi_grep` 注册到工具注册表，可被配置了该工具的 Agent 使用

#### Scenario: Disable grep tool
- **WHEN** 配置中 `tools.xizhi.grep.enabled` 为 false 或未配置
- **THEN** 系统不注册 `xizhi_grep`，Agent 无法调用它

### Requirement: Xizhi grep content search
`xizhi_grep` SHALL 在调用者工作空间内按文件内容搜索，参数包含：`path`（搜索起始目录，相对工作空间根，默认根）、`pattern`（RE2 正则，必填）、`glob`（可选，doublestar 文件名过滤模式，如 `*.go`，仅搜索文件名匹配的文件）、`ignore_case`（可选布尔，默认 false）、`include_hidden`（可选布尔，默认 false）、`context_before`（可选整数，默认 0，匹配行前输出行数）、`context_after`（可选整数，默认 0，匹配行后输出行数）。`path` SHALL 经 `xizhi.ValidatePath` 校验（绝对路径、`..`、符号链接逃逸、`.blowball` 保留命名空间均拒绝）；不跟随符号链接（对齐 `xizhi_glob_files`）。

匹配输出 SHALL 为 `{path, pattern, glob, ignore_case, matches: [{file, line_number, line, context_before?: [], context_after?: []}], truncated}`，每个 match 携带 `file`（相对 `path` 的路径）、`line_number`（1 基）、`line`（匹配行文本）、以及当对应 context 参数 >0 时的上下文行数组。系统 SHALL 跳过二进制文件（检测到 NUL 字节即视为二进制并跳过，不报错）。系统 SHALL 对结果设上限：匹配总数上限与每行字符截断上限（默认上限值由实现固定，约 200 条匹配 / 每行约 500 字符），超限时 SHALL 停止追加并在 `truncated` 置 `true`。正则编译失败 SHALL 返回错误。

#### Scenario: Search content with regex matches
- **WHEN** Agent 调用 `xizhi_grep`，`path` 为 `"src"`，`pattern` 为 `"func Foo\("`
- **THEN** 系统返回 `data/{user_uuid}/workspace/src` 下所有文本文件中匹配该正则的行，每条 match 携带 `file`、`line_number`、`line`

#### Scenario: Filter files by glob pattern
- **WHEN** Agent 调用 `xizhi_grep`，`pattern` 为 `"TODO"`，`glob` 为 `"*.go"`
- **THEN** 系统仅搜索文件名匹配 `*.go` 的文件，返回其中的匹配行

#### Scenario: Case-insensitive search
- **WHEN** Agent 调用 `xizhi_grep`，`pattern` 为 `"error"`，`ignore_case` 为 true
- **THEN** 系统匹配 `Error`、`ERROR`、`error` 等任意大小写组合

#### Scenario: Context lines around matches
- **WHEN** Agent 调用 `xizhi_grep`，`pattern` 为 `"def main"`，`context_before` 为 2，`context_after` 为 2
- **THEN** 每条 match 的 `context_before` 携带匹配行前 2 行、`context_after` 携带匹配行后 2 行

#### Scenario: Binary files are skipped
- **WHEN** 搜索范围内存在二进制文件（含 NUL 字节）
- **THEN** 系统跳过该文件，不报错，不返回其任何匹配

#### Scenario: Reserved .blowball namespace rejected
- **WHEN** Agent 调用 `xizhi_grep`，`path` 解析进 `.blowball` 保留命名空间
- **THEN** 系统经 `xizhi.ValidatePath` 拒绝，返回路径越界错误

#### Scenario: Result cap truncates output
- **WHEN** 搜索产生的匹配数超过结果上限（约 200 条）
- **THEN** 系统停止追加匹配，返回 `truncated: true`

#### Scenario: Invalid regex returns error
- **WHEN** Agent 调用 `xizhi_grep`，`pattern` 为非法 RE2 正则（如未闭合的 `[`）
- **THEN** 系统返回正则编译错误，不执行搜索

#### Scenario: Search from workspace root by default
- **WHEN** Agent 调用 `xizhi_grep`，未提供 `path`（或为空/`.`），`pattern` 为 `"import"`
- **THEN** 系统从工作空间根目录开始递归搜索所有非隐藏文本文件
