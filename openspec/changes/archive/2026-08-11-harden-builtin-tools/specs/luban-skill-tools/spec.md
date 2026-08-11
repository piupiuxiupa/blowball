## ADDED Requirements

### Requirement: luban_list_skill_files tool registration
系统 SHALL 提供一个名为 `luban_list_skill_files` 的内置工具，列出由 `name` 解析出的 skill 目录的直接子条目（一层，不递归），镜像 `xizhi_list_files` 的形状。参数：`name`（必填，简单标识符，非路径，复用 `luban_read_skill` 的 `validateSkillName` 校验）、`path`（可选，相对该 skill 目录根的子目录，默认根）、`include_hidden`（可选布尔，默认 false）。返回 SHALL 为 `{path, entries: [{name, type, size}]}`，其中 `type` 为 `"file"` 或 `"dir"`，`size` 仅对文件有意义。`name`→目录的解析 SHALL 复用 `skill.Loader`（用户 skill 覆盖全局同名 skill，优先级与 `luban_read_skill` 完全一致）。

`path`（当提供时）SHALL 经与 `luban_read_skill` 子文档读取相同的目录内限制校验：拒绝绝对路径、经 `filepath.Clean` 后逃逸出 skill 目录根的 `..`、以及经 `filepath.EvalSymlinks` 解析后落在 skill 目录根之外的符号链接。目标非目录或不存在 SHALL 返回明确错误。未知 skill（`name` 在全局与用户目录均不存在）SHALL 返回 "skill not found" 错误。该工具的写操作为空（只读枚举）。`include_hidden` 默认 false SHALL 隐藏以 `.` 开头的条目（因此 git-clone 安装的 skill 自带的 `.git` 目录默认不出现）。

#### Scenario: List a skill's root directory
- **WHEN** 调用 `luban_list_skill_files(name="my-skill")`，未提供 `path`
- **THEN** 返回该 skill 目录根下的直接子条目（文件与目录），按名称排序

#### Scenario: List a sub-directory of a skill
- **WHEN** 调用 `luban_list_skill_files(name="my-skill", path="examples")`
- **THEN** 返回该 skill 目录下 `examples/` 子目录的直接子条目

#### Scenario: User skill overrides global in listing
- **WHEN** 全局目录和用户目录同时存在同名 skill，调用 `luban_list_skill_files(name="shared")`
- **THEN** 列出的是用户版本 skill 目录的内容

#### Scenario: Hidden entries hidden by default
- **WHEN** skill 目录由 git-clone 安装、含 `.git` 子目录，调用 `luban_list_skill_files(name="my-skill")` 未设 `include_hidden`
- **THEN** 返回的条目中不含 `.git`

#### Scenario: Reject path traversal escape
- **WHEN** 调用 `luban_list_skill_files(name="my-skill", path="../../shared")` 且解析后逃逸出 skill 目录根
- **THEN** 返回错误，拒绝列举

#### Scenario: Unknown skill returns error
- **WHEN** 调用 `luban_list_skill_files(name="nonexistent")`
- **THEN** 返回明确的 "skill not found" 错误

#### Scenario: Sub-directory not found returns error
- **WHEN** 调用 `luban_list_skill_files(name="my-skill", path="nope")` 且该子目录不存在
- **THEN** 返回明确的 "directory not found" 错误

### Requirement: luban_tree_skill tool registration
系统 SHALL 提供一个名为 `luban_tree_skill` 的内置工具，返回由 `name` 解析出的 skill 目录的嵌套树表示，镜像 `xizhi_tree` 的形状。参数：`name`（必填，简单标识符，复用 `validateSkillName`）、`path`（可选，相对 skill 目录根的子目录，默认根）、`depth`（可选整数，默认 3，超过上限 10 时 SHALL 被钳制到 10）、`include_hidden`（可选布尔，默认 false）。返回 SHALL 为 `{path, depth, tree: [{name, type, size?, children?}]}`，其中 `type` 为 `"file"` 或 `"dir"`，目录节点携带 `children`。`name`→目录解析与 `path` 目录内限制校验 SHALL 与 `luban_list_skill_files` 完全一致（用户覆盖全局优先级；绝对路径 / `..` 逃逸 / 符号链接越界均拒绝）。未知 skill SHALL 返回 "skill not found" 错误；目标非目录或不存在 SHALL 返回明确错误。

#### Scenario: Tree of a skill with default depth
- **WHEN** 调用 `luban_tree_skill(name="my-skill")`，未提供 `path` 与 `depth`
- **THEN** 返回该 skill 目录根的嵌套树，递归到默认深度 3

#### Scenario: Depth is clamped to maximum
- **WHEN** 调用 `luban_tree_skill(name="my-skill", depth=42)`
- **THEN** 返回的树递归深度被钳制为 10，`depth` 字段为 10

#### Scenario: Tree a sub-directory of a skill
- **WHEN** 调用 `luban_tree_skill(name="my-skill", path="examples", depth=2)`
- **THEN** 返回该 skill 目录下 `examples/` 子目录的嵌套树，递归到深度 2

#### Scenario: Hidden entries hidden by default in tree
- **WHEN** skill 目录含 `.git`，调用 `luban_tree_skill(name="my-skill")` 未设 `include_hidden`
- **THEN** 树中不含 `.git` 节点

#### Scenario: Reject path traversal escape in tree
- **WHEN** 调用 `luban_tree_skill(name="my-skill", path="../../etc")` 且解析后逃逸出 skill 目录根
- **THEN** 返回错误，拒绝生成树

#### Scenario: Unknown skill returns error in tree
- **WHEN** 调用 `luban_tree_skill(name="nonexistent")`
- **THEN** 返回明确的 "skill not found" 错误
