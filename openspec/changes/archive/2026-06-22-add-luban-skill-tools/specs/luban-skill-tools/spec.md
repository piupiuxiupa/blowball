## ADDED Requirements

### Requirement: luban_list_skills tool registration
系统 SHALL 提供一个名为 `luban_list_skills` 的内置工具，递归扫描全局 `skills/` 目录和当前用户 `data/{userID}/skills/` 目录，返回合并后的可用 skill 元数据列表。

#### Scenario: List all available skills
- **WHEN** agent 调用 `luban_list_skills`
- **THEN** 返回全局 skills 和用户 skills 的合并列表，用户 skill 覆盖全局同名 skill

#### Scenario: User skill overrides global in list
- **WHEN** 全局目录和用户目录同时存在同名 skill
- **THEN** 返回的列表中只出现用户版本的元数据

#### Scenario: Empty skills directories
- **WHEN** 全局目录和用户目录都不存在任何有效 skill
- **THEN** 返回空列表

### Requirement: luban_read_skill tool registration
系统 SHALL 提供一个名为 `luban_read_skill` 的内置工具，参数为 skill 名称，返回对应 `SKILL.md` 的 markdown body（已剥离 YAML frontmatter）。

#### Scenario: Read user skill
- **WHEN** 调用 `luban_read_skill("using-git-worktrees")` 且用户目录存在该 skill
- **THEN** 返回用户目录下该 skill 的 markdown body

#### Scenario: Read global skill as fallback
- **WHEN** 调用 `luban_read_skill("using-git-worktrees")` 且用户目录不存在、全局目录存在
- **THEN** 返回全局目录下该 skill 的 markdown body

#### Scenario: Unknown skill
- **WHEN** 调用 `luban_read_skill("nonexistent")` 且全局和用户目录都不存在
- **THEN** 返回明确的 "skill not found" 错误

#### Scenario: Reject oversized skill
- **WHEN** 目标 `SKILL.md` 大小超过配置上限（默认 500KB）
- **THEN** 返回错误，不加载内容

### Requirement: luban_install_skill tool registration
系统 SHALL 提供一个名为 `luban_install_skill` 的内置工具，支持从 URL 安装 skill 或 skill 集合到当前用户的 `data/{userID}/skills/` 目录。

#### Scenario: Install from GitHub repo URL
- **WHEN** 调用 `luban_install_skill("https://github.com/obra/superpowers")`
- **THEN** 系统将仓库克隆到 `data/{userID}/skills/superpowers/`，并递归发现其中的子 skills

#### Scenario: Install with explicit name
- **WHEN** 调用 `luban_install_skill(url, "my-skill")`
- **THEN** 以 `my-skill` 作为目录名安装到用户 skills 目录

#### Scenario: Install single SKILL.md URL
- **WHEN** URL 指向单个 SKILL.md 文件
- **THEN** 下载并写入 `data/{userID}/skills/{name}/SKILL.md`

#### Scenario: Overwrite existing skill
- **WHEN** 用户目录已存在同名 skill
- **THEN** 安装操作覆盖已有内容，并在结果中返回覆盖标志

#### Scenario: Reject invalid URL
- **WHEN** URL scheme 不是 `https` 或 URL 格式不合法
- **THEN** 返回错误，不进行任何写入

#### Scenario: Reject path traversal in skill name
- **WHEN** 安装时推断或传入的 skill name 包含 `..`、路径分隔符或绝对路径
- **THEN** 返回错误，拒绝安装

### Requirement: Skill name validation in luban tools
`luban_read_skill` 和 `luban_install_skill` SHALL 把 skill name 当作标识符处理，禁止将其解析为文件路径。

#### Scenario: Reject path-like skill name in read
- **WHEN** 调用 `luban_read_skill("../workspace/secrets")`
- **THEN** 返回错误，拒绝读取

### Requirement: Security scoping
所有 luban 工具的写操作 SHALL 限制在当前用户的 `data/{userID}/skills/` 目录内，禁止逃逸到上级目录或其他用户目录。

#### Scenario: Install path stays within user skills dir
- **WHEN** 调用 `luban_install_skill`
- **THEN** 所有创建的文件和目录都位于 `data/{userID}/skills/` 下

### Requirement: Tool descriptions guide model away from xizhi
`luban_*` 工具的描述和 system prompt 中的 Skills 段落 SHALL 明确告知模型：查询、读取、安装 skill 必须使用 luban 工具，禁止使用 `xizhi_*` 文件工具访问 skills 目录。

#### Scenario: System prompt includes skill tool instruction
- **WHEN** system prompt 渲染 Skills 段落
- **THEN** 包含 "Use luban_list_skills / luban_read_skill / luban_install_skill for skill operations. Never use xizhi_* tools to access the skills directory."
