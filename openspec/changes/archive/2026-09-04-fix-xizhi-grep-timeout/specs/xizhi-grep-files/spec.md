## ADDED Requirements

### Requirement: xizhi_grep execution cancellation
`xizhi_grep` SHALL execute with the context supplied by the tool registry. The context SHALL be propagated to the selected ripgrep or pure-Go engine and to context-line reads. Cancellation SHALL surface as a tool failure rather than being interpreted as an empty result or a skipped file. The configured timeout SHALL be a total execution budget beginning at registry dispatch; it SHALL NOT be defined by lack of output.

#### Scenario: Ripgrep search respects its execution deadline
- **WHEN** ripgrep is installed and a registered `xizhi_grep` call exceeds its configured execution budget
- **THEN** the ripgrep subprocess is terminated and the tool surfaces a deadline-exceeded failure

#### Scenario: Pure-Go search respects its execution deadline
- **WHEN** ripgrep is unavailable and a pure-Go recursive search exceeds its configured execution budget
- **THEN** traversal and text scanning stop after observing cancellation and the tool surfaces a deadline-exceeded failure

#### Scenario: Context-line reads respect cancellation
- **WHEN** matches have been collected but context-line assembly is still reading matching files when the execution context is cancelled
- **THEN** no further matching file is read for context, partial search state is discarded, and the tool surfaces the cancellation error

### Requirement: Recursive searches exclude the reserved workspace namespace
When a recursive `xizhi_grep` search starts at the workspace root, it SHALL exclude the workspace-level `.blowball` directory and all descendants even when `include_hidden` is true. This exclusion SHALL NOT prevent an ordinary hidden file or directory outside the reserved namespace from being searched when `include_hidden` is true. An explicit `.blowball/...` search target SHALL continue to be rejected before either engine starts.

#### Scenario: Reserved namespace is not discovered from the workspace root
- **WHEN** Agent 调用 `xizhi_grep`，`path` 为 `"."`，`include_hidden` 为 true，且 `.blowball/mcp/config.json` 包含匹配内容
- **THEN** 系统不搜索或返回该文件

#### Scenario: Other hidden entries remain available when requested
- **WHEN** Agent 调用 `xizhi_grep`，`path` 为 `"."`，`include_hidden` 为 true，且 `.env` 与 `.git/config` 包含匹配内容
- **THEN** 系统按现有规则搜索这些非保留命名空间的隐藏条目

#### Scenario: Explicit reserved target still fails fast
- **WHEN** Agent 调用 `xizhi_grep`，`path` 为 `".blowball/mcp/config.json"`
- **THEN** 系统在启动搜索引擎前返回路径越界错误

### Requirement: xizhi_grep entry classification is deterministic
For directory traversal, `xizhi_grep` SHALL treat an entry whose basename begins with `.` as hidden: hidden files and descendants of hidden directories SHALL be skipped unless `include_hidden` is true. An explicitly requested single-file target SHALL remain searchable regardless of its hidden basename, subject to path validation. A file containing a NUL byte in its leading 8192 bytes SHALL be treated as binary and silently skipped; unreadable or non-line-oriented text files SHALL continue to produce no matches rather than aborting an otherwise valid search.

#### Scenario: Hidden descendants are skipped by default
- **WHEN** a directory search includes `.env`, `.git/config`, and `visible.txt`, and `include_hidden` is false
- **THEN** only entries outside hidden paths are searched

#### Scenario: Hidden descendants are included on request
- **WHEN** the same directory search sets `include_hidden` to true
- **THEN** non-reserved hidden text entries are searched

#### Scenario: Explicit hidden file remains searchable
- **WHEN** a single-file search targets `.env` and `include_hidden` is false
- **THEN** the file is searched because hidden filtering applies to discovered descendants, not the explicit target

#### Scenario: Leading-NUL binary file is skipped
- **WHEN** a search range contains a text file and a file whose leading 8192 bytes contain NUL
- **THEN** only the text file contributes matches, and the binary file produces neither an error nor a result
