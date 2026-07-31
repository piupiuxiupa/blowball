# luban-skill-tools Specification

## Purpose

定义 luban 系列内置工具（`luban_list_skills`、`luban_read_skill`、`luban_install_skill`）的注册、行为、安全限制及模型引导。

## Requirements

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
系统 SHALL 提供一个名为 `luban_install_skill` 的内置工具，支持从 URL 安装 skill 或 skill 集合到当前用户的 `data/{userID}/workspace/.blowball/skills/` 目录。

#### Scenario: Install from GitHub repo URL
- **WHEN** 调用 `luban_install_skill("https://github.com/obra/superpowers")`
- **THEN** 系统将仓库克隆到 `data/{userID}/workspace/.blowball/skills/superpowers/`，并递归发现其中的子 skills

#### Scenario: Install with explicit name
- **WHEN** 调用 `luban_install_skill(url, "my-skill")`
- **THEN** 以 `my-skill` 作为目录名安装到用户 skills 目录

#### Scenario: Install single SKILL.md URL
- **WHEN** URL 指向单个 SKILL.md 文件
- **THEN** 下载并写入 `data/{userID}/workspace/.blowball/skills/{name}/SKILL.md`

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
所有 luban 工具的写操作 SHALL 限制在当前用户的 `data/{userID}/workspace/.blowball/skills/` 目录内，禁止逃逸到上级目录或其他用户目录。

#### Scenario: Install path stays within user skills dir
- **WHEN** 调用 `luban_install_skill`
- **THEN** 所有创建的文件和目录都位于 `data/{userID}/workspace/.blowball/skills/` 下

### Requirement: Tool descriptions guide model away from xizhi
`luban_*` 工具的描述和 system prompt 中的 Skills 段落 SHALL 明确告知模型：查询、读取、安装 skill 必须使用 luban 工具，禁止使用 `xizhi_*` 文件工具访问 skills 目录。

#### Scenario: System prompt includes skill tool instruction
- **WHEN** system prompt 渲染 Skills 段落
- **THEN** 包含 "Use luban_list_skills / luban_read_skill / luban_install_skill for skill operations. Never use xizhi_* tools to access the skills directory."

### Requirement: Sub-skill selection from skill collections
`luban_install_skill` SHALL accept an optional `skill` parameter that, for git-repo sources, selects a single sub-skill from the cloned collection and installs only that sub-skill. Selection matches the `skill` value against a discovered sub-skill's frontmatter `name`; if no unique name match is found, it falls back to matching `skill` as a repo-relative subpath whose target directory contains a `SKILL.md`. Only the selected sub-skill is installed; the remainder of the clone is discarded. The existing `name` parameter overrides the installed directory name; otherwise the installed name is the selected sub-skill's frontmatter name (or the `skill` value).

#### Scenario: Select a sub-skill by frontmatter name
- **WHEN** `luban_install_skill` is called with a collection repo URL and `skill: "gildata-finance-data"`
- **AND** the repo contains a sub-skill whose frontmatter `name` is `gildata-finance-data`
- **THEN** only that sub-skill is installed into the user skills directory
- **AND** no other sub-skills from the repo are installed

#### Scenario: Select a sub-skill by repo-relative subpath
- **WHEN** `luban_install_skill` is called with `skill: "skills/gildata-finance-data"` and no sub-skill frontmatter name matches
- **AND** the repo-relative dir `skills/gildata-finance-data` contains a `SKILL.md`
- **THEN** only that sub-skill is installed

#### Scenario: Unknown sub-skill lists the available ones
- **WHEN** `luban_install_skill` is called with a `skill` value that matches no discovered sub-skill by name or subpath
- **THEN** the tool returns an error listing the discovered sub-skill names so the caller can retry with a valid value
- **AND** nothing is written to the user skills directory

#### Scenario: name overrides the installed directory for a selected sub-skill
- **WHEN** `luban_install_skill` is called with both `skill: "X"` and `name: "my-name"`
- **THEN** the selected sub-skill X is installed under the directory name `my-name`

#### Scenario: skill parameter ignored for single-file sources
- **WHEN** `luban_install_skill` is called with a single SKILL.md URL and a `skill` value
- **THEN** the single SKILL.md is installed normally
- **AND** the result notes that `skill` does not apply to single-file sources

### Requirement: Install documentation URL handling
When `luban_install_skill` fetches a single-file (`.md`) URL whose body is not a valid SKILL.md (no YAML frontmatter, or frontmatter missing `name`/`description`), the tool SHALL NOT treat this as an install failure. It SHALL return a structured, non-error result identifying the content as an install document and including the fetched body, so the calling agent can read it, determine the real skill source, and call `luban_install_skill` again with that source. A body that does carry valid skill frontmatter is still installed as a skill as before. When the single-file fetch fails with a non-200 status, a non-text body, or an unresolvable/excessive redirect chain, the tool SHALL return an error (not an install-doc result), and that error SHALL include the HTTP status code and, when any redirect occurred, the last redirect `Location` (the final response `Location` header when present, otherwise the last followed redirect target), so the calling agent can read the target and retry with the resolved HTTPS URL.

#### Scenario: Non-skill markdown returned as install doc
- **WHEN** `luban_install_skill` is called with `https://skillhub.cn/install/skillhub.md`
- **AND** the fetched body is prose describing how to install `gildata-finance-data`, with no valid skill frontmatter
- **THEN** the tool returns a result with `installed: false` and `kind: "install-doc"`
- **AND** the result includes the fetched `content` and a hint to read it and re-install from the real source
- **AND** nothing is written to the user skills directory

#### Scenario: Valid SKILL.md still installs
- **WHEN** `luban_install_skill` is called with a `.md` URL whose body has valid skill frontmatter
- **THEN** the tool installs it as a single skill as before (existing behavior unchanged)

#### Scenario: Non-200 or non-text response errors with redirect location
- **WHEN** the single-file fetch's final response is a non-200 status or a non-text body
- **THEN** the tool returns an error rather than an install-doc result
- **AND** the error includes the HTTP status code
- **AND** if any redirect occurred during the fetch, the error includes the last redirect `Location`

#### Scenario: Redirect loop or exceeded cap surfaces last location
- **WHEN** the single-file fetch follows a redirect chain that exceeds the standard cap (10) or forms a loop
- **THEN** the tool returns an error
- **AND** the error includes the last redirect `Location`, so the calling agent can retry with the resolved URL

### Requirement: luban_install_skill description guides error recovery
`luban_install_skill` 的工具描述 SHALL 告知模型：当单文件（`.md`）下载因重定向或非 200 状态而失败时，返回的错误携带 HTTP 状态码与最后一次重定向 `Location`；模型可改用解析出的 HTTPS URL 重新调用 `luban_install_skill`，必要时先调用 `webfetch` 探测最终地址与响应头以确定真实源 URL。

#### Scenario: 工具描述包含重定向/错误恢复指引
- **WHEN** `luban_install_skill` 工具被注册并渲染给模型
- **THEN** 工具描述中包含关于"失败错误带状态码与 Location、改用解析出的 HTTPS URL 重试、可先用 webfetch 探测最终地址"的指引
