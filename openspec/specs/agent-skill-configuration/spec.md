# agent-skill-configuration Specification

## Purpose

定义 Agent 级别的 skill 配置结构、skill 目录发现、元数据解析、skill catalog 注入及引用校验。

## Requirements

### Requirement: Agent skill configuration structure
`config.yaml` 的每个 agent 段 SHALL 支持 `skills` 字段，用于声明该 agent 可使用的 skill 名称列表。

#### Scenario: Declare allowed skills
- **WHEN** `agents.confucius.skills` 包含一个或多个 skill 名
- **THEN** 系统启动时解析并校验这些配置

#### Scenario: Empty skills list
- **WHEN** `agents.confucius.skills` 为空或未配置
- **THEN** 该 agent 的系统提示词中不出现 skill catalog

### Requirement: Skill source directories
系统 SHALL 从两个来源发现 skill：全局目录 `{project_root}/skills/` 和当前用户目录 `{data}/{userID}/skills/`。发现过程 SHALL 递归扫描子目录。

#### Scenario: Discover nested global skill
- **WHEN** 全局目录存在 `superpowers/skills/using-git-worktrees/SKILL.md`
- **THEN** 系统将其作为名为 `using-git-worktrees` 的全局 skill 纳入 catalog

#### Scenario: Discover nested user skill
- **WHEN** 用户目录存在 `my-collection/skills/brainstorming/SKILL.md`
- **THEN** 系统将其作为名为 `brainstorming` 的用户 skill 纳入 catalog

#### Scenario: User skill overrides global skill
- **WHEN** 全局目录和用户目录存在同名 skill（无论嵌套路径如何）
- **THEN** 系统仅使用用户 skill，catalog 中只出现一次

### Requirement: Skill metadata parsing
每个 skill SHALL 采用 `{skill-name}/SKILL.md` 目录结构，且 `SKILL.md` 顶部包含 YAML frontmatter，至少包含 `name` 和 `description`。

#### Scenario: Parse valid SKILL.md
- **WHEN** `SKILL.md` 包含有效的 `name` 和 `description` frontmatter
- **THEN** 系统成功提取其元数据

#### Scenario: Reject missing description
- **WHEN** `SKILL.md` 缺少 `description`
- **THEN** 系统跳过该 skill 并记录警告

#### Scenario: Reject unparseable frontmatter
- **WHEN** `SKILL.md` 的 YAML frontmatter 完全无法解析
- **THEN** 系统跳过该 skill 并记录警告

### Requirement: Skill catalog injection
系统 SHALL 把允许的**全局** skill 以 XML 格式注入 agent 的系统提示词，包含 name、description、location。

#### Scenario: Inject only global skills
- **WHEN** agent 配置允许使用某些 skill 且这些 skill 存在于全局目录
- **THEN** 其系统提示词包含 `<skills>...</skills>` 段落，且只包含全局 skills

#### Scenario: Do not inject user skills
- **WHEN** 某 skill 仅存在于用户目录而未在全局目录出现
- **THEN** 即使该 skill 在 `agent.skills` 中，系统提示词也不预注入它

#### Scenario: Include usage instruction for luban tools
- **WHEN** skill catalog 被注入
- **THEN** 系统同时注入说明，告知模型使用 `luban_list_skills`、`luban_read_skill`、`luban_install_skill` 进行 skill 操作

### Requirement: Skill reference validation
Agent 的 `skills` 列表中每个名称 SHALL 在**全局** skill 目录中存在对应的有效 `SKILL.md`，否则启动失败。

#### Scenario: Reference existing global skill
- **WHEN** agent 配置引用的 skill 存在于全局目录
- **THEN** 校验通过

#### Scenario: Reference missing global skill
- **WHEN** agent 配置引用的 skill 在全局目录不存在
- **THEN** 系统启动失败并报告未知 skill

#### Scenario: User-only skill not allowed in agent.skills
- **WHEN** agent 配置引用的 skill 只存在于用户目录
- **THEN** 校验失败，因为 `agent.skills` 只用于声明全局 skills
