## ADDED Requirements

### Requirement: xizhi_find tool registration
系统 SHALL 提供名为 `xizhi_find` 的 Xizhi 工作空间条目查找工具,受 `tools.xizhi.find.enabled` 开关控制;启用时注册到工具注册表,可被配置了该工具的 Agent 使用,注册模式与 `xizhi_list_files`/`xizhi_tree`/`xizhi_grep` 一致(按 `tools.xizhi.<tool>.enabled` 条件注册,per-request 绑定到调用者工作空间根)。配置中出现已移除的 `tools.xizhi.glob_files` key 时,配置加载 SHALL 以指向 `tools.xizhi.find` 的迁移错误失败。

#### Scenario: Enable find tool
- **WHEN** 配置中 `tools.xizhi.find.enabled` 为 true
- **THEN** 系统将 `xizhi_find` 注册到工具注册表,可被配置了该工具的 Agent 使用

#### Scenario: Disable find tool
- **WHEN** 配置中 `tools.xizhi.find.enabled` 为 false 或未配置
- **THEN** 系统不注册 `xizhi_find`,Agent 无法调用它

#### Scenario: Removed glob_files config key rejected
- **WHEN** 配置中出现 `tools.xizhi.glob_files` key
- **THEN** 配置加载失败,错误信息指明该 key 已移除、应使用 `tools.xizhi.find`

### Requirement: Xizhi find by regex name search
`xizhi_find` SHALL 在调用者工作空间内递归查找文件与目录条目。参数:`path`(可选,搜索根目录,相对工作空间根,缺省为工作空间根;必须为目录,非目录路径 SHALL 返回明确错误;经 `xizhi.ValidatePath` 校验——绝对路径、`..`、符号链接逃逸、`.blowball` 保留命名空间均拒绝)、`pattern`(可选,RE2 正则,**匹配条目名(basename)**,缺省匹配一切)、`type`(可选,枚举 `file`/`directory`/`any`,缺省 `any`)、`max_depth`(可选,正整数,缺省不限)、`ignore_case`(可选布尔,缺省 false)、`include_hidden`(可选布尔,缺省 false)、`head_limit`/`offset`(可选整数,缺省 200/0)。空/纯空白 pattern SHALL 等价于匹配一切(合法)。正则编译失败 SHALL 返回错误。隐藏条目(名以 `.` 开头)默认排除,`include_hidden: true` 时包含;不跟随符号链接;`.gitignore` 等忽略文件不参与(SHALL NOT 因忽略文件跳过条目)。

返回 SHALL 为 `{path, pattern, type, total, truncated, applied_limit, applied_offset, entries: [{path, type}]}`,entry 的 `path` 相对搜索根、`type` 为 `"file"` 或 `"directory"`。entries SHALL 按路径字典序排序。`total` SHALL 为过滤后的匹配条目总数(受收集上限约束时为下界);翻页窗口由 `applied_limit`/`applied_offset` 描述,`truncated` 在仍有未返回条目时为 true。系统 SHALL 对收集设上限(实现固定,约 10000 条),超限停止遍历并置 `truncated: true`。

#### Scenario: Find files by regex
- **WHEN** Agent 调用 `xizhi_find`,`path` 为 `"src"`,`pattern` 为 `"\.go$"`
- **THEN** 系统返回 `data/{user_uuid}/workspace/src` 下所有名以 `.go` 结尾的文件,每条 entry 携带相对路径与 `type: "file"`

#### Scenario: Find directories
- **WHEN** Agent 调用 `xizhi_find`,`path` 为 `"."`,`pattern` 为 `"test"`,`type` 为 `"directory"`
- **THEN** 系统返回工作空间内名字含 `test` 的目录(含文件名含 test 的目录被匹配、文件被排除),每条 entry `type: "directory"`

#### Scenario: Empty pattern with type filter lists directories
- **WHEN** Agent 调用 `xizhi_find`,`path` 为 `"tmp/mcp-outputs"`,`pattern` 省略,`type` 为 `"file"`
- **THEN** 系统返回该目录下所有非隐藏文件(等价于按类型枚举)

#### Scenario: Pattern matches basename not path
- **WHEN** 工作空间存在 `a/b/c.txt`,Agent 调用 `xizhi_find`,`pattern` 为 `"b/c"`
- **THEN** 系统不返回 `a/b/c.txt`(pattern 仅匹配条目名,不匹配路径)

#### Scenario: Case-insensitive search
- **WHEN** Agent 调用 `xizhi_find`,`pattern` 为 `"readme"`,`ignore_case` 为 true
- **THEN** 系统匹配 `README`、`Readme`、`readme.md` 等任意大小写组合的条目名

#### Scenario: Max depth limits traversal
- **WHEN** 工作空间存在 `a/b/c.txt`,Agent 调用 `xizhi_find`,`path` 为 `"."`,`pattern` 为 `"c\.txt"`,`max_depth` 为 1
- **THEN** 系统不返回 `a/b/c.txt`(其在深度 2);`max_depth` 为 2 时返回

#### Scenario: Hidden entries excluded by default
- **WHEN** 搜索范围内存在以 `.` 开头的条目,`include_hidden` 缺省
- **THEN** 系统不返回它们;`include_hidden: true` 时返回

#### Scenario: Path is not a directory rejected
- **WHEN** Agent 调用 `xizhi_find`,`path` 指向一个普通文件
- **THEN** 系统返回 "not a directory" 类明确错误

#### Scenario: Reserved .blowball namespace rejected
- **WHEN** Agent 调用 `xizhi_find`,`path` 解析进 `.blowball` 保留命名空间
- **THEN** 系统经 `xizhi.ValidatePath` 拒绝,返回路径越界错误

#### Scenario: Invalid regex returns error
- **WHEN** Agent 调用 `xizhi_find`,`pattern` 为非法 RE2 正则(如 `*\.go`,重复算子缺操作数)
- **THEN** 系统返回正则编译错误,不执行查找

#### Scenario: Pagination window
- **WHEN** 匹配条目超过 `head_limit`,Agent 以默认 200 首查后以 `offset = offset + head_limit` 续查
- **THEN** 每页返回对应窗口的 entries,尾页 `truncated` 为 false,结果携带 `total`/`applied_limit`/`applied_offset`

### Requirement: Xizhi find dual-engine parity
`xizhi_find` SHALL 采用双引擎:系统 PATH 上存在 `fd`(或 Debian 系 `fdfind`)时 shell out(基础 flag 固定化:`--no-ignore` 恒开、smart-case 显式禁用、`--hidden` 仅 include_hidden、`-t`/`-d` 对应 type/max_depth),否则纯 Go `filepath.WalkDir` 引擎。两引擎对同参数 SHALL 产生逐字段一致的返回(entries、排序、分页、truncated)。

#### Scenario: fd engine used when available
- **WHEN** 系统装有 `fd` 或 `fdfind`
- **THEN** 查找经由系统 fd 执行,输出解析为与 Go 引擎一致的形状

#### Scenario: Engines produce identical results
- **WHEN** 同一工作空间、同一参数分别经 fd 引擎与 Go 引擎执行
- **THEN** 两者的返回(含 entries 排序与分页窗口)逐字段一致
