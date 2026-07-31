## MODIFIED Requirements

### Requirement: Tool descriptions guide model away from xizhi
system prompt 中的 Skills 段落 SHALL 作为权威来源明确告知模型：查询、读取、安装 skill 必须使用 luban 工具（`luban_list_skills`/`luban_read_skill`/`luban_install_skill`），禁止使用 `xizhi_*` 文件工具访问 skills 目录。`luban_read_skill` 的描述 SHALL 保留一条最简指针（用 luban 而非 `xizhi_*` 访问 skill 目录）；`luban_list_skills` 与 `luban_install_skill` 的描述 SHALL 不再逐字重复该整句，以避免跨工具协作规则在多个工具描述中冗余（权威表述仅保留在系统提示词）。

#### Scenario: System prompt includes skill tool instruction
- **WHEN** system prompt 渲染 Skills 段落
- **THEN** 包含 "Use luban_list_skills / luban_read_skill / luban_install_skill for skill operations. Never use xizhi_* tools to access the skills directory."

#### Scenario: luban_read_skill 保留最简指针
- **WHEN** `luban_read_skill` 工具被注册并渲染给模型
- **THEN** 其描述包含指向"用 luban 而非 `xizhi_*` 访问 skill 目录"的最简指针

#### Scenario: list/install 不再重复整句 cross-rule
- **WHEN** `luban_list_skills` 与 `luban_install_skill` 工具被注册并渲染给模型
- **THEN** 其描述不再逐字包含 "Never use xizhi_* tools to access the skills directory" 整句

## ADDED Requirements

### Requirement: luban_read_skill description declares markdown body return
`luban_read_skill` 的工具描述 SHALL 声明其返回目标 skill 的 `SKILL.md` markdown body（已剥离 YAML frontmatter），且用户 skill 优先于全局 skill。

#### Scenario: read 描述声明返回 markdown body
- **WHEN** `luban_read_skill` 工具被注册并渲染给模型
- **THEN** 描述声明返回（已剥离 frontmatter 的）markdown body
