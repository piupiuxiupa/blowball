## ADDED Requirements

### Requirement: read_skill tool registration
系统 SHALL 提供一个名为 `read_skill` 的内置工具，参数为 skill 名称，返回对应 `SKILL.md` 的完整内容。

#### Scenario: Register read_skill tool
- **WHEN** 系统启动
- **THEN** `read_skill` 被注册到进程级 tool registry，并可在 agent 配置中按需启用

#### Scenario: read_skill available only when skills configured
- **WHEN** agent 未配置任何 skill
- **THEN** 该 agent 的工具列表中不包含 `read_skill`

### Requirement: Skill lookup by name
`read_skill` SHALL 根据名称先后在当前用户 skill 目录和全局 skill 目录中查找对应的 `SKILL.md`。

#### Scenario: Read user skill
- **WHEN** 调用 `read_skill` 且当前用户目录存在同名 skill
- **THEN** 返回该用户 skill 的内容

#### Scenario: Read global skill
- **WHEN** 调用 `read_skill` 且仅全局目录存在同名 skill
- **THEN** 返回该全局 skill 的内容

#### Scenario: User skill takes precedence
- **WHEN** 全局目录和当前用户目录同时存在同名 skill
- **THEN** 返回用户 skill 的内容

### Requirement: Skill content return
`read_skill` SHALL 返回 `SKILL.md` 的 markdown body（剥离 YAML frontmatter），并可选包裹在结构化标签中。

#### Scenario: Return body without frontmatter
- **WHEN** 调用 `read_skill("coding-style")`
- **THEN** 返回的内容不包含 `---` 包裹的 YAML frontmatter

#### Scenario: Wrap content in structured tags
- **WHEN** 系统配置启用内容包装
- **THEN** 返回内容包含 skill 名称和目录信息，便于模型识别

### Requirement: Skill size limits
`read_skill` SHALL 拒绝读取超过配置大小上限的 skill 文件，防止 token 爆炸。

#### Scenario: Reject oversized skill
- **WHEN** `SKILL.md` 大小超过上限（如 500KB）
- **THEN** 返回错误，不加载内容

### Requirement: Error handling for unknown skill
当 `read_skill` 接收到不存在的 skill 名称时， SHALL 返回明确错误。

#### Scenario: Unknown skill name
- **WHEN** 调用 `read_skill("nonexistent")`
- **THEN** 返回 "skill not found" 错误
