# agent-skill-configuration Specification

## Purpose

定义 Agent 级别的 skill 配置结构、skill 目录发现、元数据解析、skill catalog 注入及引用校验。

## Requirements

### Requirement: Agent skill configuration structure
`config.yaml` 的每个 agent 段 SHALL 支持 `skills` 字段，用于声明该 agent 可使用的 skill 名称列表。

#### Scenario: Declare allowed skills
- **WHEN** `agents.confuse.skills` 包含一个或多个 skill 名
- **THEN** 系统启动时解析并校验这些配置

#### Scenario: Empty skills list
- **WHEN** `agents.confuse.skills` 为空或未配置
- **THEN** 该 agent 的系统提示词中不出现 skill catalog

### Requirement: Skill source directories
系统 SHALL 从两个来源发现 skill：全局目录 `{project_root}/skills/` 和当前用户目录 `{data}/{userID}/skills/`。

#### Scenario: Discover global skill
- **WHEN** 全局 skill 目录存在 `{skill-name}/SKILL.md`
- **THEN** 系统将其作为全局 skill 纳入 catalog

#### Scenario: Discover user skill
- **WHEN** 当前用户 skill 目录存在 `{skill-name}/SKILL.md`
- **THEN** 系统将其作为用户 skill 纳入 catalog

#### Scenario: User skill overrides global skill
- **WHEN** 全局目录和当前用户目录存在同名 skill
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
系统 SHALL 把允许的 skill 以 XML 格式注入 agent 的系统提示词，包含 name、description、location。

#### Scenario: Inject skill catalog
- **WHEN** agent 配置允许使用某些 skill
- **THEN** 其系统提示词包含 `<skills>...</skill>...</skills>` 段落

#### Scenario: Include usage instruction
- **WHEN** skill catalog 被注入
- **THEN** 系统同时注入简短说明，告知模型在任务匹配时调用 `read_skill` 加载 skill

#### Scenario: Omit location if disabled
- **WHEN** 配置关闭 location 注入
- **THEN** XML 中不包含 `<location>` 节点

### Requirement: Skill reference validation
Agent 的 `skills` 列表中每个名称 SHALL 在全局或当前用户 skill 目录中存在对应的 `SKILL.md`，否则启动失败。

#### Scenario: Reference existing skill
- **WHEN** agent 配置引用的 skill 存在
- **THEN** 校验通过

#### Scenario: Reference missing skill
- **WHEN** agent 配置引用的 skill 不存在
- **THEN** 系统启动失败并报告未知 skill
