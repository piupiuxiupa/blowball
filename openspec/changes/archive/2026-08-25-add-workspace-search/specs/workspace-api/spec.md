## ADDED Requirements

### Requirement: Search workspace entries

系统 SHALL 提供 `GET /api/v1/workspace/search` 端点,在认证用户的工作区内按条目名递归搜索文件与目录(JWT 鉴权;`api` 角色路由分区,`api` 与 `all` 角色注册,`agent` 角色不注册)。

**参数**(全部 query 参数):`pattern`(可选;**子串语义**,服务端转义为字面量后匹配条目名 basename;空/纯空白等价于匹配一切)、`type`(可选;枚举 `file`/`dir`/`any`,缺省 `any`)、`path`(可选;搜索根,相对工作区根,缺省为工作区根;空/纯空白等价于根;SHALL 经 `xizhi.ValidatePathAllowReserved` 校验——绝对路径、`..`、符号链接逃逸拒绝,`.blowball` 保留命名空间**允许**,与 List 需求同一校验原语)、`max_depth`(可选;非负整数,搜索根之下的组件数,缺省不限)、`ignore_case`(可选布尔,缺省 **true**)、`include_hidden`(可选布尔,缺省 false)、`head_limit`/`offset`(可选整数,缺省 200/0;`applied_limit`/`applied_offset` 回显实际生效值)。

**匹配与遍历语义** SHALL 与 `xizhi_find` 引擎一致:pattern 仅匹配条目名(basename)不匹配路径;不跟随符号链接;`.gitignore` 等忽略文件不参与;隐藏条目(名以 `.` 开头)默认排除、`include_hidden: true` 时包含;收集 SHALL 设上限(实现固定,约 10000 条),超限停止遍历并置 `truncated: true`。entries SHALL 按路径字典序稳定排序(分页骨架;系统 SHALL NOT 做相关性排序)。实现 SHALL 复用 fd/纯 Go 双引擎(`fd`/`fdfind` 在 `PATH` 时 shell out,否则纯 Go walk),两引擎产出逐字段一致。

**响应** 200 SHALL 为 `{total, truncated, applied_limit, applied_offset, entries: [...]}`;每条 entry SHALL 携带 `path`(工作区相对路径,`/` 分隔)、`name`(basename)、`type`(`"file"` 或 `"dir"`,与 List 的类型值域一致)、`size`(字节)、`update_time`(RFC3339,UTC)。`size`/`update_time` SHALL 仅对返回页(≤ `applied_limit` 条)逐条 stat,SHALL NOT 对全量命中 stat。遍历与 stat 之间条目消失(TOCTOU)时系统 SHALL 保留该条目并以 `size: 0`、`update_time: ""` 降级,SHALL NOT 静默跳过(保持本页条数与分页窗口一致)。

**超时**:整个搜索(遍历 + stat)SHALL 受服务端总超时约束(固定实现常量,约 10 秒);超时 SHALL 返回 500 与错误码 `SEARCH_TIMEOUT`。

**错误映射**:搜索根越界 SHALL 返回 403 `FORBIDDEN`;搜索根不存在 SHALL 返回 200 与空 entries(`total: 0`);搜索根为普通文件 SHALL 返回 400;`type` 非法、`max_depth` 为负、`head_limit`/`offset` 非法整数 SHALL 返回 400;未携带有效凭据 SHALL 返回 401。

#### Scenario: 子串搜索命中文件与目录

- **WHEN** 工作区存在 `reports/2026/q1.md` 与 `reports/backup/`,已认证用户请求 `GET /workspace/search?pattern=2026`
- **THEN** 系统返回 200,entries 包含 `reports/2026`(type `dir`)下名字含 `2026` 的条目;`reports/2026/q1.md` 的 basename `q1.md` 不含 `2026`,不被返回(pattern 只匹配 basename)

#### Scenario: 空 pattern 配合类型过滤枚举

- **WHEN** 请求 `GET /workspace/search?pattern=&type=dir`
- **THEN** 系统返回工作区内所有非隐藏目录(等价于按类型全量枚举),entries 按路径字典序

#### Scenario: 忽略大小写默认开启

- **WHEN** 工作区存在 `README.md`,请求 `GET /workspace/search?pattern=readme`(不带 `ignore_case`)
- **THEN** 系统返回 `README.md`;显式 `ignore_case=false` 时不返回

#### Scenario: 特殊字符按字面量匹配

- **WHEN** 工作区存在 `report(1).md` 与 `a+b.txt`,请求 `GET /workspace/search?pattern=report(1)`
- **THEN** 系统返回 `report(1).md`,不因 `(` 触发正则语义或报错

#### Scenario: 隐藏条目默认排除,include_hidden 可见

- **WHEN** 工作区存在 `.blowball/skills/x/SKILL.md`,请求 `GET /workspace/search?pattern=SKILL`
- **THEN** 默认(`include_hidden` 缺省)不返回该条目;`include_hidden=true` 时返回

#### Scenario: .blowball 可作为搜索根

- **WHEN** 请求 `GET /workspace/search?path=.blowball/skills&type=file`
- **THEN** 校验通过(AllowReserved),在该子树内搜索;agent 侧 `xizhi_find` 对同一路径仍然拒绝(两路径校验原语不同,互不影响)

#### Scenario: 搜索根越界拒绝

- **WHEN** 请求 `GET /workspace/search?path=../escape` 或 `path=/etc`
- **THEN** 系统返回 403 `FORBIDDEN`

#### Scenario: 搜索根不存在返回空 200

- **WHEN** 请求 `GET /workspace/search?path=no/such/dir`
- **THEN** 系统返回 200,`total: 0`,entries 为空数组

#### Scenario: 搜索根为普通文件拒绝

- **WHEN** 请求 `GET /workspace/search?path=notes.txt`(该路径是文件)
- **THEN** 系统返回 400,错误信息说明搜索根必须是目录

#### Scenario: 分页与截断

- **WHEN** 某 pattern 命中 350 条,请求 `head_limit=200&offset=0`
- **THEN** 返回 200 条、`total: 350`、`truncated: true`、`applied_limit: 200`、`applied_offset: 0`;以 `offset=200` 翻页取得其余 150 条,两次结果无重叠无遗漏(字典序稳定排序保证)

#### Scenario: 条目在 stat 前消失降级

- **WHEN** 某命中条目在引擎遍历之后、handler stat 之前被删除
- **THEN** 该条目仍出现在返回页中,`size: 0`、`update_time: ""`,本页条数与 `applied_limit` 一致

#### Scenario: 超时返回显式错误

- **WHEN** 搜索(遍历或 stat)超过服务端超时常量
- **THEN** 系统返回 500 与错误码 `SEARCH_TIMEOUT`,不挂死连接

#### Scenario: 非法参数拒绝

- **WHEN** 请求 `type=folder`,或 `max_depth=-1`,或 `head_limit=abc`
- **THEN** 系统返回 400

#### Scenario: 缺少鉴权

- **WHEN** 未携带有效 Bearer token 请求 `GET /workspace/search`
- **THEN** 系统返回 401

#### Scenario: stat 携带大小与修改时间

- **WHEN** 命中文件 `reports/q1.md`(4096 字节,2026-08-01T09:00:00Z 修改)
- **THEN** entry 为 `{path: "reports/q1.md", name: "q1.md", type: "file", size: 4096, update_time: "2026-08-01T09:00:00Z"}`
