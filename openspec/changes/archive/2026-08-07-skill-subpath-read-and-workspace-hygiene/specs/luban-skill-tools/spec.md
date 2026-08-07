## MODIFIED Requirements

### Requirement: luban_read_skill tool registration
系统 SHALL 提供一个名为 `luban_read_skill` 的内置工具，参数为 skill 名称（`name`，简单标识符，非路径）与可选的相对路径（`path`）。当 `path` 省略时，返回与 `name` 匹配的 skill 的 `SKILL.md` markdown body（已剥离 YAML frontmatter），行为与既有版本一致（向后兼容）。当提供 `path` 时，将其解析为相对于该 skill 目录根的相对路径，读取该 `.md` 文件（有 frontmatter 则剥离、无则原样返回）——读取的具体行为与安全约束详见 `luban_read_skill sub-document path reading` 要求。

#### Scenario: Read user skill (default SKILL.md)
- **WHEN** 调用 `luban_read_skill(name="using-git-worktrees")` 且用户目录存在该 skill，未提供 `path`
- **THEN** 返回用户目录下该 skill 的 `SKILL.md` markdown body

#### Scenario: Read global skill as fallback
- **WHEN** 调用 `luban_read_skill(name="using-git-worktrees")` 且用户目录不存在、全局目录存在，未提供 `path`
- **THEN** 返回全局目录下该 skill 的 `SKILL.md` markdown body

#### Scenario: Unknown skill
- **WHEN** 调用 `luban_read_skill(name="nonexistent")` 且全局和用户目录都不存在
- **THEN** 返回明确的 "skill not found" 错误

#### Scenario: Reject oversized skill
- **WHEN** 目标文件（`SKILL.md` 或 `path` 指向的 `.md`）大小超过配置上限（默认 500KB）
- **THEN** 返回错误，不加载内容

#### Scenario: Read skill sub-document by path
- **WHEN** 调用 `luban_read_skill(name="my-skill", path="examples/guide.md")` 且该 skill 存在、`examples/guide.md` 位于其目录根内且为 `.md`
- **THEN** 返回 `examples/guide.md` 的 markdown body（有 frontmatter 则剥离、无则原样）

#### Scenario: path omitted is backward compatible
- **WHEN** 调用 `luban_read_skill(name="my-skill")` 不带 `path`
- **THEN** 返回该 skill 的 `SKILL.md` body，与既有行为完全一致

### Requirement: luban_read_skill description declares markdown body return
`luban_read_skill` 的工具描述 SHALL 声明其返回目标 skill 的 markdown body（已剥离 YAML frontmatter），且用户 skill 优先于全局 skill。描述 SHALL 进一步说明：省略 `path` 时读取该 skill 的 `SKILL.md`；提供 `path` 时读取技能目录树内由相对路径指向的 `.md` 文件（限制在该技能目录根内），且 `path` 必须是指向 `.md` 文件的相对路径而非 skill 名称。

#### Scenario: read 描述声明返回 markdown body
- **WHEN** `luban_read_skill` 工具被注册并渲染给模型
- **THEN** 描述声明返回（已剥离 frontmatter 的）markdown body

#### Scenario: read 描述声明 path 子文档读取
- **WHEN** `luban_read_skill` 工具被注册并渲染给模型
- **THEN** 描述说明可选 `path` 用于读取技能目录树内的 `.md` 文件，`path` 为相对路径、仅限 `.md`、省略时读 `SKILL.md`

## ADDED Requirements

### Requirement: luban_read_skill sub-document path reading
当 `luban_read_skill` 提供 `path` 参数时，系统 SHALL 把 `path` 解析为相对于匹配 skill 目录根（即该 skill 的 `SKILL.md` 所在目录）的相对路径，并在读取前执行限制在技能目录根内的路径校验：拒绝绝对路径、经 `filepath.Clean` 后逃逸出技能目录根的 `..`、以及经 `filepath.EvalSymlinks` 解析后落在技能目录根之外的符号链接。系统 SHALL 仅读取扩展名为 `.md` 的文件；非 `.md` 目标返回错误。读取内容经 frontmatter 解析（有则剥离、无则原样返回）。目标大小 SHALL 受与 `SKILL.md` 相同的上限（默认 500KB）约束。`name` 仍 MUST 为简单标识符（既有 `validateSkillName` 校验不变）；仅 `path` 作为文件相对路径。

#### Scenario: Read a nested sub-document
- **WHEN** 调用 `luban_read_skill(name="my-skill", path="references/api.md")` 且该文件位于技能目录根内
- **THEN** 返回 `references/api.md` 的 markdown body（有 frontmatter 则剥离）

#### Scenario: Reject absolute path
- **WHEN** 调用 `luban_read_skill(name="my-skill", path="/etc/passwd")`
- **THEN** 返回错误，拒绝读取

#### Scenario: Reject parent traversal escape
- **WHEN** 调用 `luban_read_skill(name="my-skill", path="../../shared.md")` 且解析后逃逸出技能目录根
- **THEN** 返回错误，拒绝读取

#### Scenario: Reject symlink escape
- **WHEN** 技能目录根内存在符号链接指向外部目录，且 `path` 经该符号链接
- **THEN** 系统使用 `filepath.EvalSymlinks` 解析真实路径后验证前缀，拒绝越界读取

#### Scenario: Reject non-markdown file
- **WHEN** 调用 `luban_read_skill(name="my-skill", path="examples/data.csv")` 且目标扩展名非 `.md`
- **THEN** 返回错误，拒绝读取非 `.md` 文件

#### Scenario: Reject oversized sub-document
- **WHEN** `path` 指向的 `.md` 文件大小超过配置上限（默认 500KB）
- **THEN** 返回错误，不加载内容

#### Scenario: Reject missing sub-document
- **WHEN** 调用 `luban_read_skill(name="my-skill", path="nope.md")` 且该路径在技能目录根内不存在
- **THEN** 返回明确的 "file not found" 错误

#### Scenario: name remains a simple identifier
- **WHEN** 调用 `luban_read_skill(name="../workspace/secrets", path="x.md")`
- **THEN** 返回错误，拒绝路径形式的 `name`（既有 `validateSkillName` 校验不变）
