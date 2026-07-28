## MODIFIED Requirements

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

### Requirement: Security scoping
所有 luban 工具的写操作 SHALL 限制在当前用户的 `data/{userID}/workspace/.blowball/skills/` 目录内，禁止逃逸到上级目录或其他用户目录。

#### Scenario: Install path stays within user skills dir
- **WHEN** 调用 `luban_install_skill`
- **THEN** 所有创建的文件和目录都位于 `data/{userID}/workspace/.blowball/skills/` 下
