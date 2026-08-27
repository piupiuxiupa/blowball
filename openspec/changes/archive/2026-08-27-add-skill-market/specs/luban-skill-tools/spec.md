# Delta Spec: luban-skill-tools

## MODIFIED Requirements

### Requirement: luban_list_skills tool registration

系统 SHALL 提供一个名为 `luban_list_skills` 的内置工具，递归扫描全局 `skills/` 目录和当前用户 `data/{userID}/skills/` 目录，返回合并后的可用 skill 元数据列表。当 `skill_market.url` 启用时，返回 SHALL 再并入市场 allowlist 条目（见 `skill-market` 能力）：市场条目的 `name`/`description` 取自市场 API、`location` 为 `"skill_market"`；名字冲突时本地版本胜出（user > global > market）。市场拉取失败时按 fail-closed 降级为仅本地列表（工具调用本身不失败）。

#### Scenario: List all available skills

- **WHEN** agent 调用 `luban_list_skills`
- **THEN** 返回全局 skills 和用户 skills 的合并列表，用户 skill 覆盖全局同名 skill

#### Scenario: User skill overrides global in list

- **WHEN** 全局目录和用户目录同时存在同名 skill
- **THEN** 返回的列表中只出现用户版本的元数据

#### Scenario: Empty skills directories

- **WHEN** 全局目录和用户目录都不存在任何有效 skill，且市场功能关闭或 allowlist 为空
- **THEN** 返回空列表

#### Scenario: Market skills merged and labeled

- **WHEN** `skill_market.url` 已启用，市场 allowlist 含技能 `fund-promo-sentiment-v2`
- **THEN** 返回列表包含该条目，`location` 为 `"skill_market"`，`description` 为市场 API 返回值

#### Scenario: Local skill overrides market skill in list

- **WHEN** 用户目录存在技能 `foo`，市场 allowlist 亦含名为 `foo` 的技能
- **THEN** 列表中 `foo` 只出现用户目录版本（`location: "user"`），市场条目被覆盖

#### Scenario: Market failure degrades to local-only list

- **WHEN** 市场服务不可达或返回非 200
- **THEN** 工具返回仅含本地（global+user）技能的列表，调用成功，系统记 WARN

### Requirement: luban_read_skill tool registration

系统 SHALL 提供一个名为 `luban_read_skill` 的内置工具，参数为 skill 名称（`name`，简单标识符，非路径）与可选的相对路径（`path`）。`name`→技能目录的解析顺序 SHALL 为 `user > global > market`：本地 miss 后 SHALL 查市场 allowlist（见 `skill-market` 能力；市场关闭或未命中即最终 miss）。当 `path` 省略时，返回与 `name` 匹配的 skill 的 `SKILL.md` 文本内容（已剥离 YAML frontmatter，若有），行为与既有版本一致（向后兼容）。当提供 `path` 时，将其解析为相对于该 skill 目录根的相对路径，读取该文件——可读取 skill 目录树内**任意文本文件**（不再限于 `.md`）；检测到二进制文件（含 NUL 字节）时拒绝并返回明确错误。读取的具体行为与安全约束详见 `luban_read_skill sub-document path reading` 要求；市场技能目录内的子路径限定与本地技能完全一致。

#### Scenario: Read user skill (default SKILL.md)

- **WHEN** 调用 `luban_read_skill(name="using-git-worktrees")` 且用户目录存在该 skill，未提供 `path`
- **THEN** 返回用户目录下该 skill 的 `SKILL.md` 文本内容

#### Scenario: Read global skill as fallback

- **WHEN** 调用 `luban_read_skill(name="using-git-worktrees")` 且用户目录不存在、全局目录存在，未提供 `path`
- **THEN** 返回全局目录下该 skill 的 `SKILL.md` 文本内容

#### Scenario: Read market skill as third fallback

- **WHEN** 调用 `luban_read_skill(name="fund-promo-sentiment-v2")` 且用户/全局目录均不存在该名称，市场 allowlist 含该名称且磁盘已同步，未提供 `path`
- **THEN** 返回 `{data-dir}/skills-market{path}` 下该技能的 `SKILL.md` 文本内容（同样剥离 frontmatter、受大小上限约束）

#### Scenario: Market skill dir missing on disk errors

- **WHEN** 市场 allowlist 含技能 `bar` 但对应目录尚未同步到磁盘，调用 `luban_read_skill(name="bar")`
- **THEN** 返回明确的 "directory not found" 错误

#### Scenario: Unknown skill

- **WHEN** 调用 `luban_read_skill(name="nonexistent")` 且全局和用户目录都不存在，市场功能关闭或 allowlist 亦无该名称
- **THEN** 返回明确的 "skill not found" 错误

#### Scenario: Reject oversized skill

- **WHEN** 目标文件（`SKILL.md` 或 `path` 指向的文件）大小超过配置上限（默认 500KB）
- **THEN** 返回错误，不加载内容

#### Scenario: Read skill sub-document by path

- **WHEN** 调用 `luban_read_skill(name="my-skill", path="examples/guide.md")` 且该 skill 存在、`examples/guide.md` 位于其目录根内
- **THEN** 返回 `examples/guide.md` 的文本内容（有 frontmatter 则剥离、无则原样）

#### Scenario: Read non-markdown text asset by path

- **WHEN** 调用 `luban_read_skill(name="my-skill", path="examples/demo.py")` 且该文件位于技能目录根内、为文本文件
- **THEN** 返回 `examples/demo.py` 的文本内容（无 frontmatter 剥离，原样返回）

#### Scenario: path omitted is backward compatible

- **WHEN** 调用 `luban_read_skill(name="my-skill")` 不带 `path`
- **THEN** 返回该 skill 的 `SKILL.md` 内容，与既有行为完全一致

### Requirement: luban_list_skill_files tool registration

系统 SHALL 提供一个名为 `luban_list_skill_files` 的内置工具，列出由 `name` 解析出的 skill 目录的直接子条目（一层，不递归），镜像 `xizhi_list_files` 的形状。参数：`name`（必填，简单标识符，非路径，复用 `luban_read_skill` 的 `validateSkillName` 校验）、`path`（可选，相对该 skill 目录根的子目录，默认根）、`include_hidden`（可选布尔，默认 false）。返回 SHALL 为 `{path, entries: [{name, type, size}]}`，其中 `type` 为 `"file"` 或 `"dir"`，`size` 仅对文件有意义。`name`→目录的解析 SHALL 先复用 `skill.Loader`（用户 skill 覆盖全局同名 skill），本地 miss 后查市场 allowlist（优先级与 `luban_read_skill` 完全一致：user > global > market；市场来源见 `skill-market` 能力）。

`path`（当提供时）SHALL 经与 `luban_read_skill` 子文档读取相同的目录内限制校验：拒绝绝对路径、经 `filepath.Clean` 后逃逸出 skill 目录根的 `..`、以及经 `filepath.EvalSymlinks` 解析后落在 skill 目录根之外的符号链接。目标非目录或不存在 SHALL 返回明确错误。未知 skill（`name` 在本地目录与市场 allowlist 均不存在，或市场功能关闭且本地不存在）SHALL 返回 "skill not found" 错误；市场 allowlist 命中但磁盘未同步 SHALL 返回 "directory not found" 错误。该工具的写操作为空（只读枚举）。`include_hidden` 默认 false SHALL 隐藏以 `.` 开头的条目（因此 git-clone 安装的 skill 自带的 `.git` 目录默认不出现）。

#### Scenario: List a skill's root directory

- **WHEN** 调用 `luban_list_skill_files(name="my-skill")`，未提供 `path`
- **THEN** 返回该 skill 目录根下的直接子条目（文件与目录），按名称排序

#### Scenario: List a sub-directory of a skill

- **WHEN** 调用 `luban_list_skill_files(name="my-skill", path="examples")`
- **THEN** 返回该 skill 目录下 `examples/` 子目录的直接子条目

#### Scenario: User skill overrides global in listing

- **WHEN** 全局目录和用户目录同时存在同名 skill，调用 `luban_list_skill_files(name="shared")`
- **THEN** 列出的是用户版本 skill 目录的内容

#### Scenario: List a market skill's directory

- **WHEN** 本地不存在名为 `fund-promo-sentiment-v2` 的技能，市场 allowlist 含该名称且磁盘已同步，调用 `luban_list_skill_files(name="fund-promo-sentiment-v2")`
- **THEN** 返回 `{data-dir}/skills-market{path}` 下该技能目录根的直接子条目，形状与本地技能一致

#### Scenario: Market skill dir missing on disk errors in listing

- **WHEN** 市场 allowlist 含技能 `bar` 但磁盘未同步，调用 `luban_list_skill_files(name="bar")`
- **THEN** 返回明确的 "directory not found" 错误

#### Scenario: Hidden entries hidden by default

- **WHEN** skill 目录由 git-clone 安装、含 `.git` 子目录，调用 `luban_list_skill_files(name="my-skill")` 未设 `include_hidden`
- **THEN** 返回的条目中不含 `.git`

#### Scenario: Reject path traversal escape

- **WHEN** 调用 `luban_list_skill_files(name="my-skill", path="../../shared")` 且解析后逃逸出 skill 目录根
- **THEN** 返回错误，拒绝列举

#### Scenario: Unknown skill returns error

- **WHEN** 调用 `luban_list_skill_files(name="nonexistent")` 且本地与市场 allowlist 均无该名称
- **THEN** 返回明确的 "skill not found" 错误

#### Scenario: Sub-directory not found returns error

- **WHEN** 调用 `luban_list_skill_files(name="my-skill", path="nope")` 且该子目录不存在
- **THEN** 返回明确的 "directory not found" 错误

### Requirement: luban_tree_skill tool registration

系统 SHALL 提供一个名为 `luban_tree_skill` 的内置工具，返回由 `name` 解析出的 skill 目录的嵌套树表示，镜像 `xizhi_tree` 的形状。参数：`name`（必填，简单标识符，复用 `validateSkillName`）、`path`（可选，相对 skill 目录根的子目录，默认根）、`depth`（可选整数，默认 3，超过上限 10 时 SHALL 被钳制到 10）、`include_hidden`（可选布尔，默认 false）。返回 SHALL 为 `{path, depth, tree: [{name, type, size?, children?}]}`，其中 `type` 为 `"file"` 或 `"dir"`，目录节点携带 `children`。`name`→目录解析与 `path` 目录内限制校验 SHALL 与 `luban_list_skill_files` 完全一致（user > global > market 优先级，市场来源见 `skill-market` 能力；绝对路径 / `..` 逃逸 / 符号链接越界均拒绝）。未知 skill SHALL 返回 "skill not found" 错误；市场 allowlist 命中但磁盘未同步 SHALL 返回 "directory not found" 错误；目标非目录或不存在 SHALL 返回明确错误。

#### Scenario: Tree a skill with default depth

- **WHEN** 调用 `luban_tree_skill(name="my-skill")`，未提供 `path` 与 `depth`
- **THEN** 返回该 skill 目录根的嵌套树，递归到默认深度 3

#### Scenario: Depth is clamped to maximum

- **WHEN** 调用 `luban_tree_skill(name="my-skill", depth=42)`
- **THEN** 返回的树递归深度被钳制为 10，`depth` 字段为 10

#### Scenario: Tree a sub-directory of a skill

- **WHEN** 调用 `luban_tree_skill(name="my-skill", path="examples", depth=2)`
- **THEN** 返回该 skill 目录下 `examples/` 子目录的嵌套树，递归到深度 2

#### Scenario: Tree a market skill's directory

- **WHEN** 本地不存在名为 `fund-promo-sentiment-v2` 的技能，市场 allowlist 含该名称且磁盘已同步，调用 `luban_tree_skill(name="fund-promo-sentiment-v2")`
- **THEN** 返回 `{data-dir}/skills-market{path}` 下该技能目录的嵌套树，形状与本地技能一致

#### Scenario: Hidden entries hidden by default in tree

- **WHEN** skill 目录含 `.git`，调用 `luban_tree_skill(name="my-skill")` 未设 `include_hidden`
- **THEN** 树中不含 `.git` 节点

#### Scenario: Reject path traversal escape in tree

- **WHEN** 调用 `luban_tree_skill(name="my-skill", path="../../etc")` 且解析后逃逸出 skill 目录根
- **THEN** 返回错误，拒绝生成树

#### Scenario: Unknown skill returns error in tree

- **WHEN** 调用 `luban_tree_skill(name="nonexistent")` 且本地与市场 allowlist 均无该名称
- **THEN** 返回明确的 "skill not found" 错误

### Requirement: luban_read_skill description declares text body return

`luban_read_skill` 的工具描述 SHALL 声明其返回目标 skill 的文本内容（`SKILL.md` 已剥离 YAML frontmatter，若有），且解析优先级为 user > global > market——本地技能优先于技能市场来源，市场技能在本地未命中时可用（见 `skill-market` 能力）。描述 SHALL 进一步说明：省略 `path` 时读取该 skill 的 `SKILL.md`；提供 `path` 时读取技能目录树内由相对路径指向的**任意文本文件**（限制在该技能目录根内，二进制文件被拒绝），`path` 为相对路径（不再限于 `.md`）。

#### Scenario: read 描述声明返回文本内容

- **WHEN** `luban_read_skill` 工具被注册并渲染给模型
- **THEN** 描述声明返回 skill 的文本内容（`SKILL.md` 已剥离 frontmatter）

#### Scenario: read 描述声明 path 可读任意文本文件

- **WHEN** `luban_read_skill` 工具被注册并渲染给模型
- **THEN** 描述说明可选 `path` 用于读取技能目录树内的任意文本文件（相对路径、二进制被拒绝、省略时读 `SKILL.md`），不再限于 `.md`

#### Scenario: read 描述声明本地优先于市场来源

- **WHEN** `luban_read_skill` 工具被注册并渲染给模型
- **THEN** 描述说明技能解析优先级为本地（用户/全局）优先，技能市场来源作为本地未命中时的回退
